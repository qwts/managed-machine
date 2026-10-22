// Package policy parses and validates fleet authorization policy documents
// from a managed-machine-config checkout (S5 story L01; decisions D2 and D6,
// ADR-0010): access groups ("kind: access-group" under
// fleet/access-groups/*.yaml), the key-directory publication source
// ("kind: key-directory-source" at fleet/key-directory.yaml), and the
// one-way cutover marker ("kind: fleet-cutover" at fleet/cutover.yaml).
//
// Loading is read-only and never runs git or reads .git: every policy
// document's SHA-256 digest is recorded as "policy:<relative path>" for
// plan asset bindings.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/schema"
)

// SchemaVersion is the only policy document format version accepted.
const SchemaVersion = 1

// PolicyEnv names an explicit policy checkout directory, taking precedence
// over the default location under the platform state directory.
const PolicyEnv = "MANAGED_MACHINE_POLICY_DIR"

// DefaultSubdir is the directory name of the policy checkout under the
// platform state directory when PolicyEnv is unset.
const DefaultSubdir = "managed-machine-config"

// Policy document kinds (D2, D6).
const (
	KindAccessGroup        = "access-group"
	KindKeyDirectorySource = "key-directory-source"
	KindCutover            = "fleet-cutover"
	KindKeyDirectory       = "key-directory"
)

// Standard relative paths inside the policy checkout.
const (
	FleetDir            = "fleet"
	AccessGroupsDir     = "fleet/access-groups"
	KeyDirectoryRelPath = "fleet/key-directory.yaml"
	CutoverRelPath      = "fleet/cutover.yaml"
)

var (
	fingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	accountPattern     = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)
	ownerIDPattern     = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	hexPattern         = regexp.MustCompile(`^[0-9a-f]+$`)
)

// ValidFingerprint reports whether s matches SHA256: followed by 43
// unpadded standard base64 characters.
func ValidFingerprint(s string) bool { return fingerprintPattern.MatchString(s) }

// ValidAccount reports whether s matches the D2 OS account name grammar.
func ValidAccount(s string) bool { return accountPattern.MatchString(s) }

// ValidOwnerID reports whether s matches the D2 key-directory owner id
// grammar.
func ValidOwnerID(s string) bool { return ownerIDPattern.MatchString(s) }

// ValidHex reports whether s is a non-empty lowercase hexadecimal string.
func ValidHex(s string) bool { return hexPattern.MatchString(s) }

// ValidRFC3339UTC reports whether s is an RFC 3339 timestamp in UTC ("Z").
func ValidRFC3339UTC(s string) bool {
	if !strings.HasSuffix(s, "Z") {
		return false
	}
	_, err := time.Parse(time.RFC3339, s)
	return err == nil
}

// Sentinels for policy errors so callers can branch with errors.Is.
var (
	ErrUnavailable = errors.New("fleet policy unavailable")
	ErrNotCutOver  = errors.New("fleet is not cut over")
)

// UnavailableError reports a missing policy directory or a checkout without
// fleet/key-directory.yaml (MM_FLEET_POLICY_UNAVAILABLE).
type UnavailableError struct {
	Message string
}

func (e *UnavailableError) Error() string { return e.Message }
func (e *UnavailableError) Is(target error) bool {
	return target == ErrUnavailable
}

// NotCutOverError reports a missing or invalid fleet/cutover.yaml when an
// operation requires cutover (MM_FLEET_NOT_CUT_OVER).
type NotCutOverError struct {
	Message string
}

func (e *NotCutOverError) Error() string { return e.Message }
func (e *NotCutOverError) Is(target error) bool {
	return target == ErrNotCutOver
}

// Diagnostic maps an error from this package to its accepted diagnostic
// identifier.
func Diagnostic(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable):
		return diag.FleetPolicyUnavailable
	case errors.Is(err, ErrNotCutOver):
		return diag.FleetNotCutOver
	default:
		return diag.FleetPolicyInvalid
	}
}

// ToDiagnostic builds a diag.Diagnostic for err using Diagnostic(err).
func ToDiagnostic(err error) diag.Diagnostic {
	return diag.New(Diagnostic(err), err.Error())
}

// Dir resolves the policy directory path from getenv and stateDir without
// touching the filesystem. When MANAGED_MACHINE_POLICY_DIR is set it wins;
// otherwise "<state>/managed-machine-config" is returned.
func Dir(getenv func(string) string, stateDir string) (string, error) {
	if getenv != nil {
		if p := strings.TrimSpace(getenv(PolicyEnv)); p != "" {
			return filepath.Clean(p), nil
		}
	}
	if strings.TrimSpace(stateDir) == "" {
		return "", &UnavailableError{Message: "state directory is required to locate the policy checkout"}
	}
	return filepath.Join(filepath.Clean(stateDir), DefaultSubdir), nil
}

// AccessGroup is a normalized "kind: access-group" policy document (D2).
// Field order is the JSON key order of the normalized record.
type AccessGroup struct {
	SchemaVersion        int      `json:"schema_version"`
	Kind                 string   `json:"kind"`
	ID                   string   `json:"id"`
	Description          string   `json:"description,omitempty"`
	MaintainedBy         []string `json:"maintained_by"`
	Accounts             []string `json:"accounts"`
	ApprovedFingerprints []string `json:"approved_fingerprints"`
}

type rawAccessGroup struct {
	SchemaVersion        *int     `yaml:"schema_version"`
	Kind                 string   `yaml:"kind"`
	ID                   string   `yaml:"id"`
	Description          string   `yaml:"description"`
	MaintainedBy         []string `yaml:"maintained_by"`
	Accounts             []string `yaml:"accounts"`
	ApprovedFingerprints []string `yaml:"approved_fingerprints"`
}

// KeyDirectorySource is a validated "kind: key-directory-source" document
// (D2). Exactly one of Gist or Path is non-empty.
type KeyDirectorySource struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Gist          string `json:"gist,omitempty"`
	Path          string `json:"path,omitempty"`
}

type rawKeyDirectorySource struct {
	SchemaVersion *int   `yaml:"schema_version"`
	Kind          string `yaml:"kind"`
	Gist          string `yaml:"gist"`
	Path          string `yaml:"path"`
}

// Cutover is a validated "kind: fleet-cutover" document (D6).
type Cutover struct {
	SchemaVersion int      `json:"schema_version"`
	Kind          string   `json:"kind"`
	CutOverAt     string   `json:"cut_over_at"`
	ReviewedBy    []string `json:"reviewed_by"`
	Note          string   `json:"note,omitempty"`
}

type rawCutover struct {
	SchemaVersion *int     `yaml:"schema_version"`
	Kind          string   `yaml:"kind"`
	CutOverAt     string   `yaml:"cut_over_at"`
	ReviewedBy    []string `yaml:"reviewed_by"`
	Note          string   `yaml:"note"`
}

// Policy is the validated fleet policy read from a checkout.
type Policy struct {
	KeyDirectory KeyDirectorySource `json:"key_directory"`
	Groups       []AccessGroup      `json:"groups"`
	Cutover      *Cutover           `json:"cutover,omitempty"`
	Assets       []plan.Asset       `json:"assets"`
}

// RequireCutover checks that a valid fleet/cutover.yaml document was loaded.
func (p *Policy) RequireCutover() error {
	if p == nil || p.Cutover == nil {
		return &NotCutOverError{Message: "fleet/cutover.yaml is absent; fleet cutover is required before planning or applying"}
	}
	return nil
}

// probeKind reads the top-level "kind" field of a YAML document before
// strict struct decoding so a wrong or missing kind always fails on the
// "kind" field rather than on an unrelated field of another schema.
func probeKind(data []byte) (string, bool) {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return "", false
	}
	return strings.TrimSpace(probe.Kind), true
}

func checkKind(doc string, data []byte, want string) error {
	if k, ok := probeKind(data); ok {
		if k == "" {
			return schema.Errorf(schema.Invalid, doc, "kind", "missing; expected kind: %s", want)
		}
		if k != want {
			return schema.Errorf(schema.Invalid, doc, "kind", "expected kind %q; got %q", want, k)
		}
	}
	return nil
}

// ParseAccessGroup decodes and validates one "kind: access-group" document.
func ParseAccessGroup(doc string, data []byte) (AccessGroup, error) {
	if err := checkKind(doc, data, KindAccessGroup); err != nil {
		return AccessGroup{}, err
	}
	var raw rawAccessGroup
	if err := schema.Decode(doc, data, &raw); err != nil {
		return AccessGroup{}, err
	}
	if err := checkSchemaVersion(doc, raw.SchemaVersion); err != nil {
		return AccessGroup{}, err
	}
	if strings.TrimSpace(raw.Kind) != KindAccessGroup {
		return AccessGroup{}, schema.Errorf(schema.Invalid, doc, "kind", "expected kind %q; got %q", KindAccessGroup, raw.Kind)
	}
	if !schema.ValidID(raw.ID) {
		return AccessGroup{}, schema.Errorf(schema.Invalid, doc, "id", "invalid access group id %q", raw.ID)
	}
	maintainers, err := validateOwnerIDs(doc, "maintained_by", raw.MaintainedBy)
	if err != nil {
		return AccessGroup{}, err
	}
	accounts, err := validateAccounts(doc, "accounts", raw.Accounts)
	if err != nil {
		return AccessGroup{}, err
	}
	fps, err := validateFingerprints(doc, "approved_fingerprints", raw.ApprovedFingerprints)
	if err != nil {
		return AccessGroup{}, err
	}
	return AccessGroup{
		SchemaVersion:        SchemaVersion,
		Kind:                 KindAccessGroup,
		ID:                   raw.ID,
		Description:          raw.Description,
		MaintainedBy:         maintainers,
		Accounts:             accounts,
		ApprovedFingerprints: fps,
	}, nil
}

// Normalize renders one AccessGroup as a compact JSON line with fixed key
// order and no HTML escaping.
func (g AccessGroup) Normalize() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// NormalizeGroups renders access groups as compact JSON Lines in id order.
func NormalizeGroups(groups []AccessGroup) ([]byte, error) {
	sorted := append([]AccessGroup(nil), groups...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, g := range sorted {
		if err := enc.Encode(g); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ParseKeyDirectorySource decodes and validates one
// "kind: key-directory-source" document (D2).
func ParseKeyDirectorySource(doc string, data []byte) (KeyDirectorySource, error) {
	if err := checkKind(doc, data, KindKeyDirectorySource); err != nil {
		return KeyDirectorySource{}, err
	}
	var raw rawKeyDirectorySource
	if err := schema.Decode(doc, data, &raw); err != nil {
		return KeyDirectorySource{}, err
	}
	if err := checkSchemaVersion(doc, raw.SchemaVersion); err != nil {
		return KeyDirectorySource{}, err
	}
	if strings.TrimSpace(raw.Kind) != KindKeyDirectorySource {
		return KeyDirectorySource{}, schema.Errorf(schema.Invalid, doc, "kind", "expected kind %q; got %q", KindKeyDirectorySource, raw.Kind)
	}
	switch {
	case raw.Gist == "" && raw.Path == "":
		return KeyDirectorySource{}, schema.Errorf(schema.Invalid, doc, "gist", "exactly one of gist or path is required")
	case raw.Gist != "" && raw.Path != "":
		return KeyDirectorySource{}, schema.Errorf(schema.Invalid, doc, "gist", "gist and path are mutually exclusive; specify exactly one")
	case raw.Gist != "":
		if !ValidHex(raw.Gist) {
			return KeyDirectorySource{}, schema.Errorf(schema.Invalid, doc, "gist", "invalid gist id %q; must be lowercase hex", raw.Gist)
		}
		return KeyDirectorySource{
			SchemaVersion: SchemaVersion,
			Kind:          KindKeyDirectorySource,
			Gist:          raw.Gist,
		}, nil
	default:
		if !filepath.IsAbs(raw.Path) {
			return KeyDirectorySource{}, schema.Errorf(schema.Invalid, doc, "path", "path %q must be an absolute directory", raw.Path)
		}
		return KeyDirectorySource{
			SchemaVersion: SchemaVersion,
			Kind:          KindKeyDirectorySource,
			Path:          raw.Path,
		}, nil
	}
}

// ParseCutover decodes and validates one "kind: fleet-cutover" document (D6).
func ParseCutover(doc string, data []byte) (Cutover, error) {
	if err := checkKind(doc, data, KindCutover); err != nil {
		return Cutover{}, err
	}
	var raw rawCutover
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Cutover{}, err
	}
	if err := checkSchemaVersion(doc, raw.SchemaVersion); err != nil {
		return Cutover{}, err
	}
	if strings.TrimSpace(raw.Kind) != KindCutover {
		return Cutover{}, schema.Errorf(schema.Invalid, doc, "kind", "expected kind %q; got %q", KindCutover, raw.Kind)
	}
	if strings.TrimSpace(raw.CutOverAt) == "" {
		return Cutover{}, schema.Errorf(schema.Invalid, doc, "cut_over_at", "missing")
	}
	if !ValidRFC3339UTC(raw.CutOverAt) {
		return Cutover{}, schema.Errorf(schema.Invalid, doc, "cut_over_at", "invalid timestamp %q; must be RFC 3339 UTC", raw.CutOverAt)
	}
	reviewers, err := validateOwnerIDs(doc, "reviewed_by", raw.ReviewedBy)
	if err != nil {
		return Cutover{}, err
	}
	return Cutover{
		SchemaVersion: SchemaVersion,
		Kind:          KindCutover,
		CutOverAt:     raw.CutOverAt,
		ReviewedBy:    reviewers,
		Note:          raw.Note,
	}, nil
}

// Load reads and validates the fleet policy tree from dir on the OS
// filesystem. A missing directory or one without fleet/key-directory.yaml
// returns ErrUnavailable (MM_FLEET_POLICY_UNAVAILABLE).
func Load(dir string) (*Policy, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, &UnavailableError{Message: "policy directory is not configured"}
	}
	fi, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, &UnavailableError{Message: fmt.Sprintf("policy directory %q does not exist", dir)}
		}
		return nil, &UnavailableError{Message: fmt.Sprintf("policy directory %q: %v", dir, err)}
	}
	if !fi.IsDir() {
		return nil, &UnavailableError{Message: fmt.Sprintf("policy path %q is not a directory", dir)}
	}
	return LoadFS(os.DirFS(dir))
}

// LoadFS reads and validates the fleet policy tree from fsys.
func LoadFS(fsys fs.FS) (*Policy, error) {
	if fsys == nil {
		return nil, &UnavailableError{Message: "policy filesystem is nil"}
	}
	if _, err := fs.Stat(fsys, FleetDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &UnavailableError{Message: "policy directory is missing " + KeyDirectoryRelPath}
		}
		return nil, &UnavailableError{Message: err.Error()}
	}
	kdData, err := fs.ReadFile(fsys, KeyDirectoryRelPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, &UnavailableError{Message: "policy directory is missing " + KeyDirectoryRelPath}
		}
		return nil, schema.Errorf(schema.Invalid, KeyDirectoryRelPath, "", "read: %v", err)
	}
	kd, err := ParseKeyDirectorySource(KeyDirectoryRelPath, kdData)
	if err != nil {
		return nil, err
	}
	assets := []plan.Asset{{
		Name:   "policy:" + KeyDirectoryRelPath,
		SHA256: sha256Hex(kdData),
	}}

	var cutover *Cutover
	if coData, err := fs.ReadFile(fsys, CutoverRelPath); err == nil {
		co, err := ParseCutover(CutoverRelPath, coData)
		if err != nil {
			return nil, err
		}
		cutover = &co
		assets = append(assets, plan.Asset{
			Name:   "policy:" + CutoverRelPath,
			SHA256: sha256Hex(coData),
		})
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, schema.Errorf(schema.Invalid, CutoverRelPath, "", "read: %v", err)
	}

	// Inspect the rest of fleet/ so any misplaced document (such as a
	// kind: key-directory document under fleet/) is rejected as
	// MM_FLEET_POLICY_INVALID naming the document and field.
	var groupFiles []string
	err = fs.WalkDir(fsys, FleetDir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return schema.Errorf(schema.Invalid, p, "", "read: %v", walkErr)
		}
		if p == FleetDir || p == AccessGroupsDir {
			return nil
		}
		// Legacy fleet/machines/*.toml records are frozen history (D6),
		// but any .yaml file under fleet/machines is still refused.
		if p == "fleet/machines" && d.IsDir() {
			return nil
		}
		if strings.HasPrefix(p, "fleet/machines/") {
			if !d.IsDir() && path.Ext(p) == ".yaml" {
				return rejectUnexpectedPolicyYAML(fsys, p)
			}
			return nil
		}
		if p == KeyDirectoryRelPath || p == CutoverRelPath {
			if !d.Type().IsRegular() {
				return schema.Errorf(schema.Invalid, p, "", "must be a regular file")
			}
			return nil
		}
		if path.Dir(p) == AccessGroupsDir {
			if d.IsDir() || !d.Type().IsRegular() || path.Ext(p) != ".yaml" {
				return schema.Errorf(schema.Invalid, p, "", "not a .yaml access-group file")
			}
			groupFiles = append(groupFiles, p)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if path.Ext(p) == ".yaml" {
			return rejectUnexpectedPolicyYAML(fsys, p)
		}
		return schema.Errorf(schema.Invalid, p, "", "unexpected file under fleet/")
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(groupFiles)
	groups := make([]AccessGroup, 0, len(groupFiles))
	seenID := make(map[string]string, len(groupFiles))
	for _, gf := range groupFiles {
		data, err := fs.ReadFile(fsys, gf)
		if err != nil {
			return nil, schema.Errorf(schema.Invalid, gf, "", "read: %v", err)
		}
		g, err := ParseAccessGroup(gf, data)
		if err != nil {
			return nil, err
		}
		if prev, dup := seenID[g.ID]; dup {
			return nil, schema.Errorf(schema.Conflict, gf, "id", "access group %q is already defined by %s", g.ID, prev)
		}
		seenID[g.ID] = gf
		groups = append(groups, g)
		assets = append(assets, plan.Asset{
			Name:   "policy:" + gf,
			SHA256: sha256Hex(data),
		})
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].ID < groups[j].ID })
	sort.Slice(assets, func(i, j int) bool { return assets[i].Name < assets[j].Name })

	return &Policy{
		KeyDirectory: kd,
		Groups:       groups,
		Cutover:      cutover,
		Assets:       assets,
	}, nil
}

func rejectUnexpectedPolicyYAML(fsys fs.FS, relPath string) error {
	if data, err := fs.ReadFile(fsys, relPath); err == nil {
		if k, ok := probeKind(data); ok && k != "" {
			return schema.Errorf(schema.Invalid, relPath, "kind", "document of kind %q is not allowed at %s in the policy tree", k, relPath)
		}
	}
	return schema.Errorf(schema.Invalid, relPath, "kind", "unexpected policy document at %s", relPath)
}

func checkSchemaVersion(doc string, v *int) error {
	if v == nil {
		return schema.Errorf(schema.InvalidVersion, doc, "schema_version", "missing")
	}
	if *v != SchemaVersion {
		return schema.Errorf(schema.InvalidVersion, doc, "schema_version", "unsupported version %d; supported: %d", *v, SchemaVersion)
	}
	return nil
}

func validateOwnerIDs(doc, field string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, schema.Errorf(schema.Invalid, doc, field, "must not be empty")
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if !ValidOwnerID(s) {
			return nil, schema.Errorf(schema.Invalid, doc, field, "invalid owner id %q", s)
		}
		if seen[s] {
			return nil, schema.Errorf(schema.Invalid, doc, field, "duplicate owner id %q", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func validateAccounts(doc, field string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, schema.Errorf(schema.Invalid, doc, field, "at least one account is required")
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if !ValidAccount(s) {
			return nil, schema.Errorf(schema.Invalid, doc, field, "invalid account name %q", s)
		}
		if seen[s] {
			return nil, schema.Errorf(schema.Invalid, doc, field, "duplicate account %q", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func validateFingerprints(doc, field string, in []string) ([]string, error) {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		if !ValidFingerprint(s) {
			return nil, schema.Errorf(schema.Invalid, doc, field, "invalid fingerprint %q; must match SHA256:<43 base64 characters>", s)
		}
		if seen[s] {
			return nil, schema.Errorf(schema.Invalid, doc, field, "duplicate fingerprint %q", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
