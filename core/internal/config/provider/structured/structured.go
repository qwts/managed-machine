// Package structured is the key-level configuration provider for JSON and
// TOML files (S4 story C03): it creates or updates only the dot-separated key
// paths an operation declares, preserves unrelated keys, their order and the
// destination's line-ending convention, rejects JSON with comments as
// malformed, refuses to overwrite an existing key path that has no key entry
// in the ownership ledger, reports per-key drift on check, and records key
// ledger entries only. TOML comments are not preserved (recorded limitation).
package structured

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/file"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/plan"
)

// Name is the provider name operations declare.
const Name = "structured"

// Version is the provider version recorded in plan bindings.
const Version = "1"

// The formats a structured operation may declare. The format is never
// inferred from the path of the destination.
const (
	FormatJSON = "json"
	FormatTOML = "toml"
)

// Provider is the structured configuration provider.
type Provider struct{}

// New returns the structured provider.
func New() *Provider { return &Provider{} }

func (*Provider) Name() string    { return Name }
func (*Provider) Version() string { return Version }

// document is an order-preserving JSON or TOML configuration document.
type document interface {
	get(segs []string) (val any, exists bool, blocked bool)
	set(segs []string, val any) error
	del(segs []string)
	marshal() ([]byte, error)
}

func newDocument(format string) (document, error) {
	switch format {
	case FormatJSON:
		return newJSONDoc(), nil
	case FormatTOML:
		return newTOMLDoc(), nil
	default:
		return nil, fmt.Errorf("unknown format %q; supported: %s, %s", format, FormatJSON, FormatTOML)
	}
}

func parseDocument(format string, data []byte) (document, error) {
	switch format {
	case FormatJSON:
		return parseJSONDoc(data)
	case FormatTOML:
		return parseTOMLDoc(data)
	default:
		return nil, fmt.Errorf("unknown format %q; supported: %s, %s", format, FormatJSON, FormatTOML)
	}
}

// Plan inspects the destination without changing it and returns the target
// precondition and the planned operation. An existing key path without a
// matching key ledger entry plans a foreign-key conflict even when its value
// already matches; an unchanged owned key set plans zero operations.
func (p *Provider) Plan(_ fs.FS, _ string, op plan.Operation, v render.Values, owned *ledger.Snapshot) ([]plan.Asset, []plan.Precondition, []plan.Operation, error) {
	destPath := op.Params["path"]
	cfgID := op.Params["config"]
	if destPath == "" {
		return nil, nil, nil, errors.New("operation is missing destination path")
	}
	format, keys, keyOrder, err := parseOpKeys(op, v)
	if err != nil {
		return nil, nil, nil, err
	}
	renderedKeysJSON, err := marshalKeys(keys)
	if err != nil {
		return nil, nil, nil, err
	}

	cur, readErr := os.ReadFile(destPath)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		doc, err := newDocument(format)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, k := range keyOrder {
			if err := doc.set(strings.Split(k, "."), keys[k]); err != nil {
				return nil, nil, nil, fmt.Errorf("key %q: %w", k, err)
			}
		}
		out, err := doc.marshal()
		if err != nil {
			return nil, nil, nil, err
		}
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: "absent",
			Observed:    "absent",
		}}
		return nil, pre, []plan.Operation{{
			ID:        op.ID,
			Operation: op.Operation,
			Target:    op.Target,
			Action:    "create",
			Provider:  op.Provider,
			Params: map[string]string{
				"config": cfgID,
				"path":   destPath,
				"kind":   ledger.KindKey,
				"format": format,
				"keys":   renderedKeysJSON,
				"sha256": file.SHA256Hex(out),
			},
		}}, nil

	case readErr != nil:
		return nil, nil, nil, readErr

	default:
		crlf := hasCRLF(cur)
		doc, err := parseDocument(format, cur)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", destPath, err)
		}
		anyExistingOwned := false
		anyChanged := false
		for _, k := range keyOrder {
			segs := strings.Split(k, ".")
			got, exists, blocked := doc.get(segs)
			isOwned := owned != nil && owned.IsOwnedBy(cfgID, op.Target, destPath, ledger.KindKey, k)
			if exists || blocked {
				if !isOwned {
					conflict := "foreign-key " + destPath + ":" + k
					return nil, nil, []plan.Operation{{
						ID:        op.ID,
						Operation: op.Operation,
						Target:    op.Target,
						Action:    "conflict " + conflict,
						Provider:  op.Provider,
						Params: map[string]string{
							"config":   cfgID,
							"path":     destPath,
							"kind":     ledger.KindKey,
							"id":       k,
							"format":   format,
							"conflict": conflict,
						},
					}}, nil
				}
				anyExistingOwned = true
				if blocked || !valuesEqual(got, keys[k]) {
					anyChanged = true
				}
			} else {
				if isOwned {
					anyExistingOwned = true
				}
				anyChanged = true
			}
		}

		var removedKeys []string
		if owned != nil {
			for _, r := range owned.OwnedByOperation(cfgID, op.Target)[op.Operation] {
				if r.Kind == ledger.KindKey && filepath.Clean(r.Path) == filepath.Clean(destPath) {
					if _, ok := keys[r.ID]; !ok {
						anyExistingOwned = true
						anyChanged = true
						removedKeys = append(removedKeys, r.ID)
					}
				}
			}
		}

		curSum := file.SHA256Hex(cur)
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: "sha256:" + curSum,
			Observed:    "sha256:" + curSum,
		}}
		if !anyChanged {
			return nil, pre, nil, nil
		}

		for _, rk := range removedKeys {
			doc.del(strings.Split(rk, "."))
		}
		for _, k := range keyOrder {
			if err := doc.set(strings.Split(k, "."), keys[k]); err != nil {
				return nil, nil, nil, fmt.Errorf("key %q: %w", k, err)
			}
		}
		out, err := doc.marshal()
		if err != nil {
			return nil, nil, nil, err
		}
		if crlf {
			out = toCRLF(out)
		}

		action := "create"
		if anyExistingOwned {
			action = "update"
		}
		params := map[string]string{
			"config": cfgID,
			"path":   destPath,
			"kind":   ledger.KindKey,
			"format": format,
			"keys":   renderedKeysJSON,
			"sha256": file.SHA256Hex(out),
		}
		if len(removedKeys) > 0 {
			b, err := json.Marshal(removedKeys)
			if err != nil {
				return nil, nil, nil, err
			}
			params["release_keys"] = string(b)
		}
		return nil, pre, []plan.Operation{{
			ID:        op.ID,
			Operation: op.Operation,
			Target:    op.Target,
			Action:    action,
			Provider:  op.Provider,
			Params:    params,
		}}, nil
	}
}

// Check compares the declared keys against the destination on disk and
// reports one Drift record per differing or absent key.
func (p *Provider) Check(_ fs.FS, _ string, op plan.Operation, v render.Values) ([]ledger.Drift, error) {
	destPath := op.Params["path"]
	if destPath == "" {
		return nil, errors.New("operation is missing destination path")
	}
	format, keys, keyOrder, err := parseOpKeys(op, v)
	if err != nil {
		return nil, err
	}
	cur, err := os.ReadFile(destPath)
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(format, cur)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", destPath, err)
	}
	var drifts []ledger.Drift
	for _, k := range keyOrder {
		segs := strings.Split(k, ".")
		got, exists, blocked := doc.get(segs)
		if !exists || blocked {
			drifts = append(drifts, ledger.Drift{
				Subject: k,
				Detail:  "actual=absent desired=" + formatDriftValue(keys[k]),
			})
			continue
		}
		if !valuesEqual(got, keys[k]) {
			drifts = append(drifts, ledger.Drift{
				Subject: k,
				Detail:  "actual=" + formatDriftValue(got) + " desired=" + formatDriftValue(keys[k]),
			})
		}
	}
	return drifts, nil
}

// Step returns the performer for one planned structured operation.
func (p *Provider) Step(op plan.Operation, record func(ledger.Record) error) (execute.Step, error) {
	if op.Provider != Name {
		return nil, fmt.Errorf("provider %s cannot run %s operation", Name, op.Provider)
	}
	destPath := op.Params["path"]
	if destPath == "" {
		return nil, errors.New("operation is missing path")
	}
	switch op.Action {
	case "create", "update":
		format, keys, keyOrder, err := parseOpKeys(op, render.Values{})
		if err != nil {
			return nil, err
		}
		var releaseKeys []string
		if rawRel := op.Params["release_keys"]; rawRel != "" {
			if err := json.Unmarshal([]byte(rawRel), &releaseKeys); err != nil {
				return nil, fmt.Errorf("release_keys: %w", err)
			}
		}
		return &writeStep{
			op:          op,
			path:        destPath,
			format:      format,
			keys:        keys,
			keyOrder:    keyOrder,
			releaseKeys: releaseKeys,
			sha256:      op.Params["sha256"],
			record:      record,
		}, nil
	case "delete":
		format := strings.TrimSpace(op.Params["format"])
		if format != FormatJSON && format != FormatTOML {
			return nil, fmt.Errorf("unknown format %q; supported: %s, %s", format, FormatJSON, FormatTOML)
		}
		keyID := op.Params["id"]
		if keyID == "" {
			return nil, errors.New("delete operation is missing key id")
		}
		return &deleteStep{
			op:     op,
			path:   destPath,
			format: format,
			keyID:  keyID,
			record: record,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported action %q", op.Action)
	}
}

type writeStep struct {
	op            plan.Operation
	path          string
	format        string
	keys          map[string]any
	keyOrder      []string
	releaseKeys   []string
	sha256        string
	writtenSHA256 string
	record        func(ledger.Record) error
}

func (s *writeStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var doc document
	crlf := false
	cur, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		doc, err = newDocument(s.format)
		if err != nil {
			return nil, err
		}
	case err != nil:
		return nil, err
	default:
		crlf = hasCRLF(cur)
		doc, err = parseDocument(s.format, cur)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.path, err)
		}
	}
	for _, rk := range s.releaseKeys {
		doc.del(strings.Split(rk, "."))
	}
	for _, k := range s.keyOrder {
		if err := doc.set(strings.Split(k, "."), s.keys[k]); err != nil {
			return nil, fmt.Errorf("key %q: %w", k, err)
		}
	}
	out, err := doc.marshal()
	if err != nil {
		return nil, err
	}
	if crlf {
		out = toCRLF(out)
	}
	if err := writeAtomic(s.path, out); err != nil {
		return nil, err
	}
	s.writtenSHA256 = file.SHA256Hex(out)
	return execute.Evidence{"path": s.path}, nil
}

func (s *writeStep) Verify(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	got, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", s.path, err)
	}
	gotSum := file.SHA256Hex(got)
	wantSum := s.writtenSHA256
	if wantSum == "" {
		wantSum = s.sha256
	}
	if wantSum != "" && gotSum != wantSum {
		return execute.Evidence{
			"path":            s.path,
			"observed_sha256": gotSum,
			"expected_sha256": wantSum,
		}, fmt.Errorf("verify %s: digest %s does not match planned %s", s.path, gotSum, wantSum)
	}
	doc, err := parseDocument(s.format, got)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", s.path, err)
	}
	for _, rk := range s.releaseKeys {
		if _, exists, _ := doc.get(strings.Split(rk, ".")); exists {
			return nil, fmt.Errorf("verify %s: key %s still exists after release", s.path, rk)
		}
	}
	for _, k := range s.keyOrder {
		val, exists, blocked := doc.get(strings.Split(k, "."))
		if !exists || blocked || !valuesEqual(val, s.keys[k]) {
			return nil, fmt.Errorf("verify %s: key %s does not match desired value", s.path, k)
		}
	}
	if s.record != nil {
		for _, rk := range s.releaseKeys {
			if err := s.record(ledger.Record{
				Kind:      ledger.KindKey,
				Path:      s.path,
				ID:        rk,
				Format:    s.format,
				Config:    s.op.Params["config"],
				Operation: s.op.Operation,
				Target:    s.op.Target,
				State:     ledger.Released,
			}); err != nil {
				return nil, err
			}
		}
		for _, k := range s.keyOrder {
			if err := s.record(ledger.Record{
				Kind:      ledger.KindKey,
				Path:      s.path,
				ID:        k,
				Format:    s.format,
				SHA256:    valueSHA256(s.keys[k]),
				Config:    s.op.Params["config"],
				Operation: s.op.Operation,
				Target:    s.op.Target,
				State:     ledger.Owned,
			}); err != nil {
				return nil, err
			}
		}
	}
	return execute.Evidence{"path": s.path, "sha256": gotSum}, nil
}

type deleteStep struct {
	op     plan.Operation
	path   string
	format string
	keyID  string
	record func(ledger.Record) error
}

func (s *deleteStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cur, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return execute.Evidence{"path": s.path, "id": s.keyID}, nil
	}
	if err != nil {
		return nil, err
	}
	crlf := hasCRLF(cur)
	doc, err := parseDocument(s.format, cur)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	doc.del(strings.Split(s.keyID, "."))
	out, err := doc.marshal()
	if err != nil {
		return nil, err
	}
	if crlf {
		out = toCRLF(out)
	}
	if err := writeAtomic(s.path, out); err != nil {
		return nil, err
	}
	return execute.Evidence{"path": s.path, "id": s.keyID}, nil
}

func (s *deleteStep) Verify(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cur, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("verify %s: %w", s.path, err)
	}
	if err == nil {
		doc, err := parseDocument(s.format, cur)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", s.path, err)
		}
		if _, exists, _ := doc.get(strings.Split(s.keyID, ".")); exists {
			return nil, fmt.Errorf("verify %s: key %s still exists after delete", s.path, s.keyID)
		}
	}
	if s.record != nil {
		if err := s.record(ledger.Record{
			Kind:      ledger.KindKey,
			Path:      s.path,
			ID:        s.keyID,
			Format:    s.format,
			Config:    s.op.Params["config"],
			Operation: s.op.Operation,
			Target:    s.op.Target,
			State:     ledger.Released,
		}); err != nil {
			return nil, err
		}
	}
	return execute.Evidence{"path": s.path, "id": s.keyID}, nil
}

func writeAtomic(destPath string, content []byte) error {
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mm-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(tmpName, 0o644)
	if err := os.Rename(tmpName, destPath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// parseOpKeys validates the format and decodes the declared keys of op,
// substituting any {{ inputs.<name> }} or {{ destinations.<name> }} tokens
// in string values when v is populated.
func parseOpKeys(op plan.Operation, v render.Values) (string, map[string]any, []string, error) {
	format := strings.TrimSpace(op.Params["format"])
	if format == "" {
		return "", nil, nil, fmt.Errorf("missing format; a structured operation declares format: %s or %s", FormatJSON, FormatTOML)
	}
	if format != FormatJSON && format != FormatTOML {
		return "", nil, nil, fmt.Errorf("unknown format %q; supported: %s, %s", format, FormatJSON, FormatTOML)
	}
	rawKeys := strings.TrimSpace(op.Params["keys"])
	if rawKeys == "" {
		return "", nil, nil, errors.New("structured operation declares no keys")
	}
	dec := json.NewDecoder(strings.NewReader(rawKeys))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return "", nil, nil, fmt.Errorf("keys: %w", err)
	}
	if len(m) == 0 {
		return "", nil, nil, errors.New("structured operation declares no keys")
	}
	keyOrder := make([]string, 0, len(m))
	for k := range m {
		keyOrder = append(keyOrder, k)
	}
	sort.Strings(keyOrder)

	rv := render.Values{
		Inputs:       v.Inputs,
		Destinations: v.Destinations,
	}
	out := make(map[string]any, len(m))
	for _, k := range keyOrder {
		if err := validKeyPath(k); err != nil {
			return "", nil, nil, err
		}
		val, err := normalizeDesiredValue(k, m[k], rv)
		if err != nil {
			return "", nil, nil, err
		}
		out[k] = val
	}
	return format, out, keyOrder, nil
}

func validKeyPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errors.New("the key path is empty")
	}
	for _, seg := range strings.Split(p, ".") {
		if seg == "" {
			return fmt.Errorf("the key path %q has an empty segment", p)
		}
	}
	return nil
}

func normalizeDesiredValue(key string, raw any, v render.Values) (any, error) {
	switch val := raw.(type) {
	case string:
		if strings.Contains(val, "{{") && (len(v.Inputs) > 0 || len(v.Destinations) > 0) {
			s, err := render.Render(val, v)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", key, err)
			}
			return s, nil
		}
		return val, nil
	case bool:
		return val, nil
	case int64:
		return val, nil
	case float64:
		return val, nil
	case json.Number:
		s := val.String()
		if !strings.ContainsAny(s, ".eE") {
			i, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("key %q: %w", key, err)
			}
			return i, nil
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return nil, fmt.Errorf("key %q: %w", key, err)
		}
		return f, nil
	case []string:
		out := make([]string, len(val))
		for i, item := range val {
			if strings.Contains(item, "{{") && (len(v.Inputs) > 0 || len(v.Destinations) > 0) {
				s, err := render.Render(item, v)
				if err != nil {
					return nil, fmt.Errorf("key %q: %w", key, err)
				}
				out[i] = s
			} else {
				out[i] = item
			}
		}
		return out, nil
	case []any:
		out := make([]string, len(val))
		for i, elem := range val {
			s, ok := elem.(string)
			if !ok {
				return nil, fmt.Errorf("key %q: must be a list of strings", key)
			}
			if strings.Contains(s, "{{") && (len(v.Inputs) > 0 || len(v.Destinations) > 0) {
				rendered, err := render.Render(s, v)
				if err != nil {
					return nil, fmt.Errorf("key %q: %w", key, err)
				}
				s = rendered
			}
			out[i] = s
		}
		return out, nil
	default:
		return nil, fmt.Errorf("key %q: must be a scalar or a list of strings", key)
	}
}

func marshalKeys(keys map[string]any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(keys); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

// valuesEqual reports whether an observed value from a parsed document
// equals a normalized desired value (string, bool, int64, float64, []string).
func valuesEqual(got, want any) bool {
	switch w := want.(type) {
	case string:
		g, ok := got.(string)
		return ok && g == w
	case bool:
		g, ok := got.(bool)
		return ok && g == w
	case int64:
		switch g := got.(type) {
		case int64:
			return g == w
		case json.Number:
			s := g.String()
			if strings.ContainsAny(s, ".eE") {
				return false
			}
			gi, err := strconv.ParseInt(s, 10, 64)
			return err == nil && gi == w
		}
		return false
	case float64:
		switch g := got.(type) {
		case float64:
			return g == w
		case json.Number:
			gf, err := strconv.ParseFloat(g.String(), 64)
			return err == nil && gf == w
		}
		return false
	case []string:
		switch g := got.(type) {
		case []string:
			if len(g) != len(w) {
				return false
			}
			for i := range w {
				if g[i] != w[i] {
					return false
				}
			}
			return true
		case []any:
			if len(g) != len(w) {
				return false
			}
			for i := range w {
				s, ok := g[i].(string)
				if !ok || s != w[i] {
					return false
				}
			}
			return true
		}
		return false
	}
	return false
}

// formatDriftValue renders an observed or desired key value as a compact
// deterministic token for `--check` detail (`actual=... desired=...`).
func formatDriftValue(v any) string {
	switch val := v.(type) {
	case *jsonObject:
		b, err := val.marshalCompact()
		if err == nil {
			return string(b)
		}
	case *tomlTable:
		b, err := val.marshalCompactJSON()
		if err == nil {
			return string(b)
		}
	case []*tomlTable:
		arr := make([]any, len(val))
		for i, t := range val {
			arr[i] = t.toMap()
		}
		return jsonToken(arr)
	case tomlRaw:
		return jsonToken(string(val))
	}
	return jsonToken(v)
}

func jsonToken(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// Inspect parses data in format ("json" or "toml"), looks up the dot-separated
// keyPath, and returns the canonical value SHA-256 hex digest when the key is
// present, ("", false, nil) when the key is absent or blocked by a scalar, or
// an error when format is unknown or data is malformed.
func Inspect(format string, data []byte, keyPath string) (sha256Hex string, present bool, err error) {
	if err := validKeyPath(keyPath); err != nil {
		return "", false, err
	}
	doc, err := parseDocument(strings.TrimSpace(format), data)
	if err != nil {
		return "", false, err
	}
	got, exists, blocked := doc.get(strings.Split(keyPath, "."))
	if !exists || blocked {
		return "", false, nil
	}
	return valueSHA256(got), true, nil
}

func valueSHA256(v any) string {
	return file.SHA256Hex([]byte(formatDriftValue(v)))
}

func hasCRLF(data []byte) bool {
	return bytes.Contains(data, []byte("\r\n"))
}

func toCRLF(data []byte) []byte {
	lf := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
}
