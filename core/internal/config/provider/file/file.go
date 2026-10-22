// Package file is the whole-file configuration provider (S4 story C02): it
// copies a pinned source asset from the definition's source to the resolved
// destination path, refuses to replace an existing file that has no file
// entry in the ownership ledger, plans no operation when the owned file on
// disk already matches the desired bytes, and independently re-reads and
// hashes the file after writing before recording ownership.
package file

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/plan"
)

// Name is the provider name operations declare.
const Name = "file"

// Version is the provider version recorded in plan bindings.
const Version = "1"

// Provider is the file configuration provider.
type Provider struct{}

// New returns the file provider.
func New() *Provider { return &Provider{} }

func (*Provider) Name() string    { return Name }
func (*Provider) Version() string { return Version }

// Plan inspects the destination without changing it and returns the pinned
// source asset, the destination precondition, and the planned operation.
func (p *Provider) Plan(fsys fs.FS, dir string, op plan.Operation, _ render.Values, owned *ledger.Snapshot) ([]plan.Asset, []plan.Precondition, []plan.Operation, error) {
	src := op.Params["source"]
	if fsys == nil {
		return nil, nil, nil, fmt.Errorf("source %q: no source filesystem", src)
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, src))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("source %q: %w", src, err)
	}
	assets := []plan.Asset{{Name: src, SHA256: SHA256Hex(data)}}
	pre, ops, err := PlanBytes(op, data, owned)
	if err != nil {
		return nil, nil, nil, err
	}
	return assets, pre, ops, nil
}

// Check compares the destination on disk with the source asset bytes.
func (p *Provider) Check(fsys fs.FS, dir string, op plan.Operation, _ render.Values) ([]ledger.Drift, error) {
	src := op.Params["source"]
	if fsys == nil {
		return nil, fmt.Errorf("source %q: no source filesystem", src)
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, src))
	if err != nil {
		return nil, fmt.Errorf("source %q: %w", src, err)
	}
	return CheckBytes(op, data)
}

// Step returns the performer for one planned file operation.
func (p *Provider) Step(op plan.Operation, record func(ledger.Record) error) (execute.Step, error) {
	return StepFor(Name, op, record)
}

// PlanBytes plans a whole-file operation for the desired bytes data against
// the destination named by op.Params["path"]. It is shared by the file and
// template providers, which both own the whole destination file.
func PlanBytes(op plan.Operation, data []byte, owned *ledger.Snapshot) ([]plan.Precondition, []plan.Operation, error) {
	destPath := op.Params["path"]
	cfgID := op.Params["config"]
	if destPath == "" {
		return nil, nil, errors.New("operation is missing destination path")
	}
	wantSum := SHA256Hex(data)
	_, statErr := os.Stat(destPath)
	switch {
	case statErr == nil:
		if owned == nil || !owned.IsOwnedBy(cfgID, op.Target, destPath, ledger.KindFile, "") {
			conflict := "replace-file " + destPath
			return nil, []plan.Operation{{
				ID:        op.ID,
				Operation: op.Operation,
				Target:    op.Target,
				Action:    "conflict " + conflict,
				Provider:  op.Provider,
				Params: map[string]string{
					"config":   cfgID,
					"path":     destPath,
					"kind":     ledger.KindFile,
					"conflict": conflict,
				},
			}}, nil
		}
		cur, err := os.ReadFile(destPath)
		if err != nil {
			return nil, nil, err
		}
		curSum := SHA256Hex(cur)
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: "sha256:" + curSum,
			Observed:    "sha256:" + curSum,
		}}
		if bytes.Equal(cur, data) {
			return pre, nil, nil
		}
		return pre, []plan.Operation{{
			ID:        op.ID,
			Operation: op.Operation,
			Target:    op.Target,
			Action:    "update",
			Provider:  op.Provider,
			Params: map[string]string{
				"config":  cfgID,
				"path":    destPath,
				"kind":    ledger.KindFile,
				"content": string(data),
				"sha256":  wantSum,
			},
		}}, nil
	case errors.Is(statErr, os.ErrNotExist):
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: "absent",
			Observed:    "absent",
		}}
		return pre, []plan.Operation{{
			ID:        op.ID,
			Operation: op.Operation,
			Target:    op.Target,
			Action:    "create",
			Provider:  op.Provider,
			Params: map[string]string{
				"config":  cfgID,
				"path":    destPath,
				"kind":    ledger.KindFile,
				"content": string(data),
				"sha256":  wantSum,
			},
		}}, nil
	default:
		return nil, nil, statErr
	}
}

// CheckBytes compares the file at op.Params["path"] with the desired bytes.
func CheckBytes(op plan.Operation, want []byte) ([]ledger.Drift, error) {
	destPath := op.Params["path"]
	cur, err := os.ReadFile(destPath)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(cur, want) {
		return nil, nil
	}
	subj := op.Params["destination"]
	if subj == "" {
		subj = destPath
	}
	return []ledger.Drift{{
		Subject: subj,
		Detail:  "actual=sha256:" + SHA256Hex(cur) + " desired=sha256:" + SHA256Hex(want),
	}}, nil
}

// StepFor builds the execute.Step for a whole-file operation of providerName.
func StepFor(providerName string, op plan.Operation, record func(ledger.Record) error) (execute.Step, error) {
	if op.Provider != providerName {
		return nil, fmt.Errorf("provider %s cannot run %s operation", providerName, op.Provider)
	}
	destPath := op.Params["path"]
	if destPath == "" {
		return nil, errors.New("operation is missing path")
	}
	switch op.Action {
	case "create", "update":
		wantSum := op.Params["sha256"]
		if wantSum == "" {
			return nil, errors.New("operation is missing sha256")
		}
		return &writeStep{
			op:      op,
			path:    destPath,
			content: []byte(op.Params["content"]),
			sha256:  wantSum,
			record:  record,
		}, nil
	case "delete":
		return &deleteStep{
			op:     op,
			path:   destPath,
			record: record,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported action %q", op.Action)
	}
}

type writeStep struct {
	op      plan.Operation
	path    string
	content []byte
	sha256  string
	record  func(ledger.Record) error
}

func (s *writeStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".mm-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(s.content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
	_ = os.Chmod(tmpName, 0o644)
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return nil, err
	}
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
	gotSum := SHA256Hex(got)
	if gotSum != s.sha256 {
		return execute.Evidence{
			"path":            s.path,
			"observed_sha256": gotSum,
			"expected_sha256": s.sha256,
		}, fmt.Errorf("verify %s: digest %s does not match planned %s", s.path, gotSum, s.sha256)
	}
	if s.record != nil {
		if err := s.record(ledger.Record{
			Kind:      ledger.KindFile,
			Path:      s.path,
			SHA256:    gotSum,
			Config:    s.op.Params["config"],
			Operation: s.op.Operation,
			Target:    s.op.Target,
			State:     ledger.Owned,
		}); err != nil {
			return nil, err
		}
	}
	return execute.Evidence{"path": s.path, "sha256": gotSum}, nil
}

type deleteStep struct {
	op     plan.Operation
	path   string
	record func(ledger.Record) error
}

func (s *deleteStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return execute.Evidence{"path": s.path}, nil
}

func (s *deleteStep) Verify(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.path); err == nil {
		return nil, fmt.Errorf("verify %s: file still exists after delete", s.path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("verify %s: %w", s.path, err)
	}
	if s.record != nil {
		if err := s.record(ledger.Record{
			Kind:      ledger.KindFile,
			Path:      s.path,
			Config:    s.op.Params["config"],
			Operation: s.op.Operation,
			Target:    s.op.Target,
			State:     ledger.Released,
		}); err != nil {
			return nil, err
		}
	}
	return execute.Evidence{"path": s.path}, nil
}

// SHA256Hex returns the lowercase hexadecimal SHA-256 digest of data.
func SHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
