// Package block is the managed-block configuration provider (S4 story C04):
// it renders an operation's inline content through the logic-free
// substitution engine (D4) and manages a marked block delimited by
// "<comment> managed-machine:begin <block_id>" and
// "<comment> managed-machine:end <block_id>" (D2) inside the destination
// file.
//
// Unlike the whole-file providers, block owns only its marked region and
// records a "block" entry in the ownership ledger (D3). An absent
// destination is created holding only the block; a new block in an existing
// file is appended using the file's existing line-ending convention; an
// owned block is replaced in place; delete removes only the owned block and
// its markers; and every byte outside the markers is preserved verbatim.
package block

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/file"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/plan"
)

// Name is the provider name operations declare.
const Name = "managed-block"

// Version is the provider version recorded in plan bindings.
const Version = "1"

// DefaultComment is the default marker prefix when an operation omits comment.
const DefaultComment = "#"

// Provider is the managed-block configuration provider.
type Provider struct{}

// New returns the managed-block provider.
func New() *Provider { return &Provider{} }

func (*Provider) Name() string    { return Name }
func (*Provider) Version() string { return Version }

// Plan renders the operation's content and inspects the destination without
// changing it. Markers with the same block_id and no matching block ledger
// entry plan "conflict foreign-block <path>:<block_id>".
func (p *Provider) Plan(_ fs.FS, _ string, op plan.Operation, v render.Values, owned *ledger.Snapshot) ([]plan.Asset, []plan.Precondition, []plan.Operation, error) {
	destPath := op.Params["path"]
	cfgID := op.Params["config"]
	if destPath == "" {
		return nil, nil, nil, errors.New("operation is missing destination path")
	}
	blockID := blockIDOf(op)
	if blockID == "" {
		return nil, nil, nil, errors.New("operation is missing block_id")
	}
	comment, err := commentOf(op)
	if err != nil {
		return nil, nil, nil, err
	}
	rendered, err := renderContent(op.Params["content"], v)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("block %q: %w", blockID, err)
	}

	cur, readErr := os.ReadFile(destPath)
	switch {
	case errors.Is(readErr, os.ErrNotExist):
		wantBlock := FormatBlock(blockID, comment, rendered, "\n")
		wantSum := file.SHA256Hex(wantBlock)
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: "absent",
			Observed:    "absent",
		}}
		return nil, pre, []plan.Operation{plannedOp(op, "create", cfgID, destPath, blockID, comment, wantBlock, wantSum)}, nil
	case readErr != nil:
		return nil, nil, nil, readErr
	default:
		eol := DetectEOL(cur)
		wantBlock := FormatBlock(blockID, comment, rendered, eol)
		wantSum := file.SHA256Hex(wantBlock)
		curSum := file.SHA256Hex(cur)
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: "sha256:" + curSum,
			Observed:    "sha256:" + curSum,
		}}
		loc := locateBlock(cur, blockID, comment)
		switch loc.state {
		case stateAbsent:
			return nil, pre, []plan.Operation{plannedOp(op, "create", cfgID, destPath, blockID, comment, wantBlock, wantSum)}, nil
		case stateMalformed:
			return nil, nil, []plan.Operation{conflictOp(op, cfgID, destPath, blockID)}, nil
		case stateFound:
			if owned == nil || !owned.IsOwnedBy(cfgID, op.Target, destPath, ledger.KindBlock, blockID) {
				return nil, nil, []plan.Operation{conflictOp(op, cfgID, destPath, blockID)}, nil
			}
			if bytes.Equal(cur[loc.start:loc.end], wantBlock) {
				return nil, pre, nil, nil
			}
			return nil, pre, []plan.Operation{plannedOp(op, "update", cfgID, destPath, blockID, comment, wantBlock, wantSum)}, nil
		default:
			return nil, nil, nil, fmt.Errorf("unexpected block state for %q", blockID)
		}
	}
}

// Check renders the operation's content and compares the desired block with
// the block on disk. Content outside the markers is ignored.
func (p *Provider) Check(_ fs.FS, _ string, op plan.Operation, v render.Values) ([]ledger.Drift, error) {
	destPath := op.Params["path"]
	if destPath == "" {
		return nil, errors.New("operation is missing destination path")
	}
	blockID := blockIDOf(op)
	if blockID == "" {
		return nil, errors.New("operation is missing block_id")
	}
	comment, err := commentOf(op)
	if err != nil {
		return nil, err
	}
	rendered, err := renderContent(op.Params["content"], v)
	if err != nil {
		return nil, fmt.Errorf("block %q: %w", blockID, err)
	}
	cur, err := os.ReadFile(destPath)
	if err != nil {
		return nil, err
	}
	eol := DetectEOL(cur)
	wantBlock := FormatBlock(blockID, comment, rendered, eol)
	wantSum := file.SHA256Hex(wantBlock)

	loc := locateBlock(cur, blockID, comment)
	switch loc.state {
	case stateAbsent:
		return []ledger.Drift{{
			Subject: blockID,
			Detail:  "actual=absent desired=sha256:" + wantSum,
		}}, nil
	case stateMalformed:
		return []ledger.Drift{{
			Subject: blockID,
			Detail:  "actual=malformed desired=sha256:" + wantSum,
		}}, nil
	case stateFound:
		gotBlock := cur[loc.start:loc.end]
		if bytes.Equal(gotBlock, wantBlock) {
			return nil, nil
		}
		return []ledger.Drift{{
			Subject: blockID,
			Detail:  "actual=sha256:" + file.SHA256Hex(gotBlock) + " desired=sha256:" + wantSum,
		}}, nil
	default:
		return nil, nil
	}
}

// Step returns the performer for one planned managed-block operation.
func (p *Provider) Step(op plan.Operation, record func(ledger.Record) error) (execute.Step, error) {
	if op.Provider != Name {
		return nil, fmt.Errorf("provider %s cannot run %s operation", Name, op.Provider)
	}
	destPath := op.Params["path"]
	if destPath == "" {
		return nil, errors.New("operation is missing path")
	}
	blockID := blockIDOf(op)
	if blockID == "" {
		return nil, errors.New("operation is missing block_id")
	}
	switch op.Action {
	case "create", "update":
		comment, err := commentOf(op)
		if err != nil {
			return nil, err
		}
		wantSum := op.Params["sha256"]
		if wantSum == "" {
			return nil, errors.New("operation is missing sha256")
		}
		raw := []byte(op.Params["content"])
		if locateBlock(raw, blockID, comment).state != stateFound {
			raw = FormatBlock(blockID, comment, string(raw), "\n")
		}
		return &writeStep{
			op:      op,
			path:    destPath,
			blockID: blockID,
			comment: comment,
			block:   raw,
			sha256:  wantSum,
			record:  record,
		}, nil
	case "delete":
		return &deleteStep{
			op:      op,
			path:    destPath,
			blockID: blockID,
			comment: op.Params["comment"],
			record:  record,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported action %q", op.Action)
	}
}

type writeStep struct {
	op      plan.Operation
	path    string
	blockID string
	comment string
	block   []byte
	sha256  string
	record  func(ledger.Record) error
}

func (s *writeStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cur, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeAtomic(s.path, s.block, 0o644); err != nil {
			return nil, err
		}
		return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
	}
	if err != nil {
		return nil, err
	}
	perm := fileMode(s.path)
	loc := locateBlock(cur, s.blockID, s.comment)
	var next []byte
	switch loc.state {
	case stateMalformed:
		return nil, fmt.Errorf("malformed markers for block %q in %s", s.blockID, s.path)
	case stateFound:
		next = make([]byte, 0, loc.start+len(s.block)+len(cur)-loc.end)
		next = append(next, cur[:loc.start]...)
		next = append(next, s.block...)
		next = append(next, cur[loc.end:]...)
	case stateAbsent:
		var sep []byte
		if len(cur) > 0 && cur[len(cur)-1] != '\n' {
			sep = []byte(DetectEOL(cur))
		}
		next = make([]byte, 0, len(cur)+len(sep)+len(s.block))
		next = append(next, cur...)
		next = append(next, sep...)
		next = append(next, s.block...)
	}
	if err := writeAtomic(s.path, next, perm); err != nil {
		return nil, err
	}
	return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
}

func (s *writeStep) Verify(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	got, err := os.ReadFile(s.path)
	if err != nil {
		return nil, fmt.Errorf("verify %s: %w", s.path, err)
	}
	loc := locateBlock(got, s.blockID, s.comment)
	if loc.state != stateFound {
		return nil, fmt.Errorf("verify %s: block %q not found", s.path, s.blockID)
	}
	gotBlock := got[loc.start:loc.end]
	gotSum := file.SHA256Hex(gotBlock)
	if gotSum != s.sha256 {
		return execute.Evidence{
			"path":            s.path,
			"block_id":        s.blockID,
			"observed_sha256": gotSum,
			"expected_sha256": s.sha256,
		}, fmt.Errorf("verify %s: block %q digest %s does not match planned %s", s.path, s.blockID, gotSum, s.sha256)
	}
	if s.record != nil {
		if err := s.record(ledger.Record{
			Kind:      ledger.KindBlock,
			Path:      s.path,
			ID:        s.blockID,
			SHA256:    gotSum,
			Config:    s.op.Params["config"],
			Operation: s.op.Operation,
			Target:    s.op.Target,
			State:     ledger.Owned,
		}); err != nil {
			return nil, err
		}
	}
	return execute.Evidence{"path": s.path, "block_id": s.blockID, "sha256": gotSum}, nil
}

type deleteStep struct {
	op      plan.Operation
	path    string
	blockID string
	comment string
	record  func(ledger.Record) error
}

func (s *deleteStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cur, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
	}
	if err != nil {
		return nil, err
	}
	loc := locateBlock(cur, s.blockID, s.comment)
	switch loc.state {
	case stateAbsent:
		return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
	case stateMalformed:
		return nil, fmt.Errorf("malformed markers for block %q in %s", s.blockID, s.path)
	case stateFound:
		perm := fileMode(s.path)
		next := make([]byte, 0, loc.start+len(cur)-loc.end)
		next = append(next, cur[:loc.start]...)
		next = append(next, cur[loc.end:]...)
		if err := writeAtomic(s.path, next, perm); err != nil {
			return nil, err
		}
		return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
	}
	return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
}

func (s *deleteStep) Verify(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	got, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("verify %s: %w", s.path, err)
	}
	if err == nil {
		if loc := locateBlock(got, s.blockID, s.comment); loc.state != stateAbsent {
			return nil, fmt.Errorf("verify %s: block %q still exists after delete", s.path, s.blockID)
		}
	}
	if s.record != nil {
		if err := s.record(ledger.Record{
			Kind:      ledger.KindBlock,
			Path:      s.path,
			ID:        s.blockID,
			Config:    s.op.Params["config"],
			Operation: s.op.Operation,
			Target:    s.op.Target,
			State:     ledger.Released,
		}); err != nil {
			return nil, err
		}
	}
	return execute.Evidence{"path": s.path, "block_id": s.blockID}, nil
}

// Inspect locates blockID in data and returns its SHA-256 hex digest when the
// block is present and well-formed, ("", false, nil) when the block is absent,
// or an error when its markers are malformed.
func Inspect(data []byte, blockID string) (sha256Hex string, present bool, err error) {
	loc := locateBlock(data, blockID, "")
	switch loc.state {
	case stateAbsent:
		return "", false, nil
	case stateMalformed:
		return "", false, fmt.Errorf("malformed markers for block %q", blockID)
	case stateFound:
		return file.SHA256Hex(data[loc.start:loc.end]), true, nil
	default:
		return "", false, fmt.Errorf("unexpected block state for %q", blockID)
	}
}

// DetectEOL returns "\r\n" when data uses CRLF line endings and "\n"
// otherwise (including when data is empty or has no line breaks).
func DetectEOL(data []byte) string {
	var crlf, lf int
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			if i > 0 && data[i-1] == '\r' {
				crlf++
			} else {
				lf++
			}
		}
	}
	if crlf > 0 && crlf >= lf {
		return "\r\n"
	}
	return "\n"
}

// FormatBlock wraps renderedContent in the D2 begin and end markers for
// blockID using comment and the line ending eol ("\n" or "\r\n").
func FormatBlock(blockID, comment, renderedContent, eol string) []byte {
	if comment == "" {
		comment = DefaultComment
	}
	if eol == "" {
		eol = "\n"
	}
	norm := strings.ReplaceAll(renderedContent, "\r\n", "\n")
	norm = strings.TrimSuffix(norm, "\n")
	norm = strings.ReplaceAll(norm, "\n", eol)

	var b strings.Builder
	b.WriteString(comment)
	b.WriteString(" managed-machine:begin ")
	b.WriteString(blockID)
	b.WriteString(eol)
	if norm != "" {
		b.WriteString(norm)
		b.WriteString(eol)
	}
	b.WriteString(comment)
	b.WriteString(" managed-machine:end ")
	b.WriteString(blockID)
	b.WriteString(eol)
	return []byte(b.String())
}

func renderContent(raw string, v render.Values) (string, error) {
	return render.Render(raw, render.Values{
		Inputs:       v.Inputs,
		Destinations: v.Destinations,
	})
}

func blockIDOf(op plan.Operation) string {
	if id := op.Params["block_id"]; id != "" {
		return id
	}
	return op.Params["id"]
}

func commentOf(op plan.Operation) (string, error) {
	c := op.Params["comment"]
	if c == "" {
		return DefaultComment, nil
	}
	if strings.ContainsAny(c, "\r\n") {
		return "", errors.New("comment must be one line")
	}
	return c, nil
}

func plannedOp(op plan.Operation, action, cfgID, destPath, blockID, comment string, wantBlock []byte, wantSum string) plan.Operation {
	return plan.Operation{
		ID:        op.ID,
		Operation: op.Operation,
		Target:    op.Target,
		Action:    action,
		Provider:  op.Provider,
		Params: map[string]string{
			"config":   cfgID,
			"path":     destPath,
			"kind":     ledger.KindBlock,
			"id":       blockID,
			"block_id": blockID,
			"comment":  comment,
			"content":  string(wantBlock),
			"sha256":   wantSum,
		},
	}
}

func conflictOp(op plan.Operation, cfgID, destPath, blockID string) plan.Operation {
	conflict := "foreign-block " + destPath + ":" + blockID
	return plan.Operation{
		ID:        op.ID,
		Operation: op.Operation,
		Target:    op.Target,
		Action:    "conflict " + conflict,
		Provider:  op.Provider,
		Params: map[string]string{
			"config":   cfgID,
			"path":     destPath,
			"kind":     ledger.KindBlock,
			"id":       blockID,
			"block_id": blockID,
			"conflict": conflict,
		},
	}
}

type blockState int

const (
	stateAbsent blockState = iota
	stateFound
	stateMalformed
)

type blockLocation struct {
	state blockState
	start int
	end   int
}

type lineSpan struct {
	start  int
	end    int
	text   string
	ending string
}

func splitLines(data []byte) []lineSpan {
	var lines []lineSpan
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			end := i + 1
			ending := "\n"
			textEnd := i
			if i > start && data[i-1] == '\r' {
				ending = "\r\n"
				textEnd = i - 1
			}
			lines = append(lines, lineSpan{
				start:  start,
				end:    end,
				text:   string(data[start:textEnd]),
				ending: ending,
			})
			start = end
		}
	}
	if start < len(data) {
		lines = append(lines, lineSpan{
			start:  start,
			end:    len(data),
			text:   string(data[start:]),
			ending: "",
		})
	}
	return lines
}

func locateBlock(data []byte, blockID, comment string) blockLocation {
	lines := splitLines(data)
	if comment != "" {
		if loc := locateWithComment(lines, blockID, comment); loc.state != stateAbsent {
			return loc
		}
	}
	return locateAnyComment(lines, blockID)
}

func locateWithComment(lines []lineSpan, blockID, comment string) blockLocation {
	beginLine := comment + " managed-machine:begin " + blockID
	endLine := comment + " managed-machine:end " + blockID
	var begins, ends []int
	for i, l := range lines {
		switch l.text {
		case beginLine:
			begins = append(begins, i)
		case endLine:
			ends = append(ends, i)
		}
	}
	if len(begins) == 0 && len(ends) == 0 {
		return blockLocation{state: stateAbsent}
	}
	if len(begins) != 1 || len(ends) != 1 || begins[0] >= ends[0] {
		return blockLocation{state: stateMalformed}
	}
	return blockLocation{
		state: stateFound,
		start: lines[begins[0]].start,
		end:   lines[ends[0]].end,
	}
}

func locateAnyComment(lines []lineSpan, blockID string) blockLocation {
	var begins, ends []int
	var beginPrefix, endPrefix string
	for i, l := range lines {
		if p, ok := matchAnyCommentMarker(l.text, "begin", blockID); ok {
			begins = append(begins, i)
			beginPrefix = p
		} else if p, ok := matchAnyCommentMarker(l.text, "end", blockID); ok {
			ends = append(ends, i)
			endPrefix = p
		}
	}
	if len(begins) == 0 && len(ends) == 0 {
		return blockLocation{state: stateAbsent}
	}
	if len(begins) != 1 || len(ends) != 1 || begins[0] >= ends[0] || beginPrefix != endPrefix {
		return blockLocation{state: stateMalformed}
	}
	return blockLocation{
		state: stateFound,
		start: lines[begins[0]].start,
		end:   lines[ends[0]].end,
	}
}

func matchAnyCommentMarker(lineText, kind, blockID string) (string, bool) {
	suffix := " managed-machine:" + kind + " " + blockID
	if !strings.HasSuffix(lineText, suffix) {
		return "", false
	}
	prefix := strings.TrimSuffix(lineText, suffix)
	if strings.TrimSpace(prefix) == "" {
		return "", false
	}
	return prefix, true
}

func fileMode(path string) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return 0o644
}

func writeAtomic(destPath string, content []byte, perm os.FileMode) error {
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
	_ = os.Chmod(tmpName, perm)
	if err := os.Rename(tmpName, destPath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
