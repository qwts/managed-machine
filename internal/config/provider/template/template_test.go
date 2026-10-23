package template

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/config/provider/file"
	"github.com/qwts/managed-machine/internal/config/render"
	"github.com/qwts/managed-machine/internal/plan"
)

func TestTemplateProviderRendersSettingsFirstAndVerifies(t *testing.T) {
	p := New()
	dir := t.TempDir()
	dest := filepath.Join(dir, ".gitconfig")
	rawTmpl := "[user]\n\tname = {{ settings.label }}\n\teditor = {{ inputs.editor }}\n\tpath = {{ destinations.gitconfig }}\n"
	fsys := fstest.MapFS{
		"templates/gitconfig.tmpl": {Data: []byte(rawTmpl)},
	}
	vals := render.Values{
		Inputs:       map[string]string{"editor": "nvim", "handle": "alice"},
		Destinations: map[string]string{"gitconfig": dest},
		Settings:     map[string]string{"label": "Managed ({{ inputs.handle }})"},
	}
	op := plan.Operation{
		ID:        "config",
		Operation: "config",
		Target:    "user",
		Provider:  Name,
		Params: map[string]string{
			"config":      "gitconfig",
			"destination": "gitconfig",
			"path":        dest,
			"source":      "templates/gitconfig.tmpl",
		},
	}

	assets, pre, ops, err := p.Plan(fsys, ".", op, vals, ledger.NewSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 1 || assets[0].SHA256 != file.SHA256Hex([]byte(rawTmpl)) {
		t.Fatalf("pinned asset should hash raw template: %+v", assets)
	}
	if len(pre) != 1 || pre[0].Requirement != "absent" {
		t.Fatalf("preconditions: %+v", pre)
	}
	wantRendered := "[user]\n\tname = Managed (alice)\n\teditor = nvim\n\tpath = " + dest + "\n"
	if len(ops) != 1 || ops[0].Action != "create" || ops[0].Params["content"] != wantRendered || ops[0].Params["sha256"] != file.SHA256Hex([]byte(wantRendered)) {
		t.Fatalf("planned op: %+v", ops)
	}

	var recorded []ledger.Record
	st, err := p.Step(ops[0], func(r ledger.Record) error {
		r.SchemaVersion = ledger.SchemaVersion
		r.Account = "tester"
		r.Plan = "plan-20261021T120000Z-0001"
		r.At = "2026-10-21T12:00:00Z"
		recorded = append(recorded, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotBytes, err := os.ReadFile(dest)
	if err != nil || string(gotBytes) != wantRendered {
		t.Fatalf("written bytes = %q, want %q (err %v)", gotBytes, wantRendered, err)
	}

	// Re-planning with same inputs and owned ledger entry plans zero operations.
	snap := ledger.NewSnapshot(recorded)
	_, _, ops, err = p.Plan(fsys, ".", op, vals, snap)
	if err != nil || len(ops) != 0 {
		t.Fatalf("reapply no-op: %+v %v", ops, err)
	}

	// Check matches rendered output.
	drifts, err := p.Check(fsys, ".", op, vals)
	if err != nil || len(drifts) != 0 {
		t.Fatalf("check: %+v %v", drifts, err)
	}
}
