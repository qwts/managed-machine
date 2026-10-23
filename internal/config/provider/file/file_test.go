package file

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/config/render"
	"github.com/qwts/managed-machine/internal/plan"
)

func TestFileProviderLifecycle(t *testing.T) {
	p := New()
	dir := t.TempDir()
	dest := filepath.Join(dir, "sub", "ignore")
	fsys := fstest.MapFS{
		"files/gitignore": {Data: []byte("*.swp\n")},
	}
	baseOp := plan.Operation{
		ID:        "ignore",
		Operation: "ignore",
		Target:    "user",
		Provider:  Name,
		Params: map[string]string{
			"config":      "git",
			"destination": "gitignore",
			"path":        dest,
			"source":      "files/gitignore",
		},
	}

	// 1. Absent destination plans create and pins the source asset.
	assets, pre, ops, err := p.Plan(fsys, ".", baseOp, render.Values{}, ledger.NewSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].Name != "files/gitignore" || assets[0].SHA256 != SHA256Hex([]byte("*.swp\n")) {
		t.Fatalf("assets: %+v", assets)
	}
	if len(pre) != 1 || pre[0].Requirement != "absent" {
		t.Fatalf("preconditions: %+v", pre)
	}
	if len(ops) != 1 || ops[0].Action != "create" {
		t.Fatalf("ops: %+v", ops)
	}

	// 2. Apply writes the file, Verify re-reads its digest and records owned.
	var recorded []ledger.Record
	recFn := func(r ledger.Record) error {
		r.SchemaVersion = ledger.SchemaVersion
		r.Account = "tester"
		r.Plan = "plan-20261021T120000Z-0001"
		r.At = "2026-10-21T12:00:00Z"
		recorded = append(recorded, r)
		return nil
	}
	st, err := p.Step(ops[0], recFn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(recorded) != 1 || recorded[0].State != ledger.Owned || recorded[0].SHA256 != SHA256Hex([]byte("*.swp\n")) {
		t.Fatalf("recorded: %+v", recorded)
	}

	// 3. Reapply with unchanged content plans zero operations.
	snap := ledger.NewSnapshot(recorded)
	_, _, ops, err = p.Plan(fsys, ".", baseOp, render.Values{}, snap)
	if err != nil || len(ops) != 0 {
		t.Fatalf("reapply no-op: %+v %v", ops, err)
	}

	// 4. Check succeeds with zero drift when content matches, and reports drift after external edit.
	drifts, err := p.Check(fsys, ".", baseOp, render.Values{})
	if err != nil || len(drifts) != 0 {
		t.Fatalf("check clean: %+v %v", drifts, err)
	}
	if err := os.WriteFile(dest, []byte("drifted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	drifts, err = p.Check(fsys, ".", baseOp, render.Values{})
	if err != nil || len(drifts) != 1 || drifts[0].Subject != "gitignore" || !strings.Contains(drifts[0].Detail, "actual=sha256:") {
		t.Fatalf("check drift: %+v %v", drifts, err)
	}

	// 5. Owned file whose content differs plans update.
	_, _, ops, err = p.Plan(fsys, ".", baseOp, render.Values{}, snap)
	if err != nil || len(ops) != 1 || ops[0].Action != "update" {
		t.Fatalf("update plan: %+v %v", ops, err)
	}

	// 6. Existing file without a ledger entry plans conflict replace-file <path>.
	_, _, ops, err = p.Plan(fsys, ".", baseOp, render.Values{}, ledger.NewSnapshot(nil))
	if err != nil || len(ops) != 1 || ops[0].Action != "conflict replace-file "+dest {
		t.Fatalf("conflict plan: %+v %v", ops, err)
	}

	// 7. Delete removes the file and records released.
	delOp := plan.Operation{
		ID:        "ignore",
		Operation: "ignore",
		Target:    "user",
		Action:    "delete",
		Provider:  Name,
		Params:    map[string]string{"config": "git", "path": dest, "kind": ledger.KindFile},
	}
	delStep, err := p.Step(delOp, recFn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := delStep.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := delStep.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("file still exists after delete: %v", err)
	}
	if recorded[len(recorded)-1].State != ledger.Released {
		t.Fatalf("expected released record, got %+v", recorded[len(recorded)-1])
	}
}

func TestVerifyRejectsTamperedFileAndDoesNotRecordOwnership(t *testing.T) {
	p := New()
	dest := filepath.Join(t.TempDir(), "file.conf")
	if err := os.WriteFile(dest, []byte("wrong"), 0o644); err != nil {
		t.Fatal(err)
	}
	var recorded []ledger.Record
	st, err := p.Step(plan.Operation{
		ID:        "op1",
		Operation: "op1",
		Target:    "user",
		Action:    "create",
		Provider:  Name,
		Params: map[string]string{
			"config":  "cfg",
			"path":    dest,
			"content": "expected",
			"sha256":  SHA256Hex([]byte("expected")),
		},
	}, func(r ledger.Record) error {
		recorded = append(recorded, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Fatal("Verify accepted mismatched digest")
	}
	if len(recorded) != 0 {
		t.Fatalf("Verify recorded ownership on mismatch: %+v", recorded)
	}
}
