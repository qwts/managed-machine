package block

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/config/provider/file"
	"github.com/qwts/managed-machine/internal/config/render"
	"github.com/qwts/managed-machine/internal/plan"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func TestBlockGoldens(t *testing.T) {
	p := New()
	vals := render.Values{
		Inputs: map[string]string{
			"editor": "nvim",
			"pager":  "less -FRX",
			"theme":  "solarized",
		},
	}

	type tc struct {
		name        string
		initial     string
		ownedBefore bool
		action      string // "" means Plan+Apply, "delete" means delete step
		blockID     string
		comment     string
		content     string
		crlf        bool
		wantPrefix  string
		wantSuffix  string
	}

	cases := []tc{
		{
			name:    "create-absent",
			blockID: "managed-machine",
			content: "export EDITOR={{ inputs.editor }}\nexport PAGER=\"{{ inputs.pager }}\"\n",
		},
		{
			name: "append-lf",
			initial: "# User shell configuration\nexport HISTSIZE=10000\n\n" +
				"# >>> conda initialize >>>\n__conda_setup=1\n# <<< conda initialize <<<\n",
			blockID: "managed-machine",
			content: "export EDITOR={{ inputs.editor }}\n",
			wantPrefix: "# User shell configuration\nexport HISTSIZE=10000\n\n" +
				"# >>> conda initialize >>>\n__conda_setup=1\n# <<< conda initialize <<<\n",
		},
		{
			name: "append-crlf",
			initial: "# Windows profile\r\n" +
				"export HISTSIZE=10000\r\n",
			blockID: "managed-machine",
			content: "export EDITOR={{ inputs.editor }}\nexport PAGER=\"{{ inputs.pager }}\"\n",
			crlf:    true,
			wantPrefix: "# Windows profile\r\n" +
				"export HISTSIZE=10000\r\n",
		},
		{
			name: "replace-in-place",
			initial: "# Header before\nexport A=1\n" +
				"# other-tool:begin managed-machine\nkeep=true\n# other-tool:end managed-machine\n" +
				"# managed-machine:begin other-block\nexport OTHER=keep\n# managed-machine:end other-block\n" +
				"# managed-machine:begin managed-machine\nexport EDITOR=vim\n# managed-machine:end managed-machine\n" +
				"# Footer after\nexport Z=9\n",
			ownedBefore: true,
			blockID:     "managed-machine",
			content:     "export EDITOR={{ inputs.editor }}\nexport THEME={{ inputs.theme }}\n",
			wantPrefix: "# Header before\nexport A=1\n" +
				"# other-tool:begin managed-machine\nkeep=true\n# other-tool:end managed-machine\n" +
				"# managed-machine:begin other-block\nexport OTHER=keep\n# managed-machine:end other-block\n",
			wantSuffix: "# Footer after\nexport Z=9\n",
		},
		{
			name: "replace-in-place-crlf",
			initial: "# Header CRLF\r\nexport A=1\r\n" +
				"# managed-machine:begin managed-machine\r\nexport EDITOR=vim\r\n# managed-machine:end managed-machine\r\n" +
				"# Footer CRLF\r\nexport Z=9\r\n",
			ownedBefore: true,
			blockID:     "managed-machine",
			content:     "export EDITOR={{ inputs.editor }}\nexport PAGER=\"{{ inputs.pager }}\"\n",
			crlf:        true,
			wantPrefix:  "# Header CRLF\r\nexport A=1\r\n",
			wantSuffix:  "# Footer CRLF\r\nexport Z=9\r\n",
		},
		{
			name: "delete-owned",
			initial: "# Header before\nexport A=1\n" +
				"# managed-machine:begin managed-machine\nexport EDITOR=vim\n# managed-machine:end managed-machine\n" +
				"# managed-machine:begin other-block\nexport OTHER=keep\n# managed-machine:end other-block\n" +
				"# Footer after\n",
			ownedBefore: true,
			action:      "delete",
			blockID:     "managed-machine",
			wantPrefix:  "# Header before\nexport A=1\n",
			wantSuffix: "# managed-machine:begin other-block\nexport OTHER=keep\n# managed-machine:end other-block\n" +
				"# Footer after\n",
		},
		{
			name:       "custom-comment",
			initial:    "// Top header\nconst existing = true;\n",
			blockID:    "js-settings",
			comment:    "//",
			content:    "# inner line is not a marker\nconst editor = \"{{ inputs.editor }}\";\n",
			wantPrefix: "// Top header\nconst existing = true;\n",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			dest := filepath.Join(dir, "rc")
			if c.initial != "" {
				if err := os.WriteFile(dest, []byte(c.initial), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var snap *ledger.Snapshot
			if c.ownedBefore {
				snap = ledger.NewSnapshot([]ledger.Record{{
					SchemaVersion: ledger.SchemaVersion,
					Kind:          ledger.KindBlock,
					Path:          dest,
					ID:            c.blockID,
					SHA256:        "prior",
					Config:        "shell",
					Operation:     "rc",
					Target:        "user",
					Account:       "tester",
					Plan:          "plan-20261021T120000Z-0001",
					At:            "2026-10-21T12:00:00Z",
					State:         ledger.Owned,
				}})
			} else {
				snap = ledger.NewSnapshot(nil)
			}

			if c.action == "delete" {
				delOp := plan.Operation{
					ID:        "rc",
					Operation: "rc",
					Target:    "user",
					Action:    "delete",
					Provider:  Name,
					Params: map[string]string{
						"config": "shell",
						"path":   dest,
						"kind":   ledger.KindBlock,
						"id":     c.blockID,
					},
				}
				st, err := p.Step(delOp, nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.Apply(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := st.Verify(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				inOp := plan.Operation{
					ID:        "rc",
					Operation: "rc",
					Target:    "user",
					Provider:  Name,
					Params: map[string]string{
						"config":      "shell",
						"destination": "rc",
						"path":        dest,
						"block_id":    c.blockID,
						"comment":     c.comment,
						"content":     c.content,
					},
				}
				assets, _, ops, err := p.Plan(nil, "", inOp, vals, snap)
				if err != nil {
					t.Fatal(err)
				}
				if len(assets) != 0 {
					t.Fatalf("unexpected assets: %+v", assets)
				}
				if len(ops) != 1 {
					t.Fatalf("expected 1 planned operation, got %+v", ops)
				}
				st, err := p.Step(ops[0], nil)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.Apply(context.Background()); err != nil {
					t.Fatal(err)
				}
				if _, err := st.Verify(context.Background()); err != nil {
					t.Fatal(err)
				}
			}

			gotBytes, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if c.wantPrefix != "" && !bytes.HasPrefix(gotBytes, []byte(c.wantPrefix)) {
				t.Errorf("prefix outside markers changed:\ngot:  %q\nwant prefix: %q", string(gotBytes), c.wantPrefix)
			}
			if c.wantSuffix != "" && !bytes.HasSuffix(gotBytes, []byte(c.wantSuffix)) {
				t.Errorf("suffix outside markers changed:\ngot:  %q\nwant suffix: %q", string(gotBytes), c.wantSuffix)
			}
			if c.crlf {
				if DetectEOL(gotBytes) != "\r\n" {
					t.Errorf("expected CRLF line endings, got %q", string(gotBytes))
				}
				for i := 0; i < len(gotBytes); i++ {
					if gotBytes[i] == '\n' && (i == 0 || gotBytes[i-1] != '\r') {
						t.Fatalf("bare LF at byte %d in CRLF file: %q", i, string(gotBytes))
					}
				}
			}

			// Encode CRLF visibly in the golden file so .gitattributes (* text=auto eol=lf)
			// keeps golden files byte-identical across git checkouts on all platforms.
			goldenText := string(gotBytes)
			if c.crlf {
				goldenText = strings.ReplaceAll(goldenText, "\r\n", "<CRLF>\n")
			}
			goldenPath := filepath.Join("testdata", c.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, []byte(goldenText), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			wantGolden, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatal(err)
			}
			if goldenText != string(wantGolden) {
				t.Errorf("differs from %s:\ngot:\n%s\nwant:\n%s", goldenPath, goldenText, string(wantGolden))
			}
		})
	}
}

func TestBlockProviderLifecycle(t *testing.T) {
	p := New()
	dir := t.TempDir()
	dest := filepath.Join(dir, "sub", ".bashrc")
	vals := render.Values{
		Inputs:       map[string]string{"editor": "vim"},
		Destinations: map[string]string{"shell-rc": dest},
	}
	baseOp := plan.Operation{
		ID:        "rc",
		Operation: "rc",
		Target:    "user",
		Provider:  Name,
		Params: map[string]string{
			"config":      "shell",
			"destination": "shell-rc",
			"path":        dest,
			"block_id":    "managed-machine",
			"content":     "export EDITOR={{ inputs.editor }}\n",
		},
	}

	// 1. Absent destination plans create and creates the file holding only the block.
	assets, pre, ops, err := p.Plan(nil, "", baseOp, vals, ledger.NewSnapshot(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 0 {
		t.Fatalf("assets: %+v", assets)
	}
	if len(pre) != 1 || pre[0].Requirement != "absent" {
		t.Fatalf("preconditions: %+v", pre)
	}
	if len(ops) != 1 || ops[0].Action != "create" {
		t.Fatalf("ops: %+v", ops)
	}

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

	wantBlock := "# managed-machine:begin managed-machine\nexport EDITOR=vim\n# managed-machine:end managed-machine\n"
	gotDisk, err := os.ReadFile(dest)
	if err != nil || string(gotDisk) != wantBlock {
		t.Fatalf("disk content = %q, want %q (%v)", gotDisk, wantBlock, err)
	}
	// Ledger records a "block" entry, never a "file" entry.
	if len(recorded) != 1 || recorded[0].Kind != ledger.KindBlock || recorded[0].ID != "managed-machine" || recorded[0].State != ledger.Owned || recorded[0].SHA256 != file.SHA256Hex([]byte(wantBlock)) {
		t.Fatalf("recorded ledger entry: %+v", recorded)
	}

	// 2. Reapply with unchanged content is a zero-operation plan.
	snap := ledger.NewSnapshot(recorded)
	_, _, ops, err = p.Plan(nil, "", baseOp, vals, snap)
	if err != nil || len(ops) != 0 {
		t.Fatalf("reapply no-op: %+v (%v)", ops, err)
	}

	// 3. Editing outside the markers causes neither drift nor a planned operation.
	withOutside := "# user prefix\n" + wantBlock + "# user suffix\n"
	if err := os.WriteFile(dest, []byte(withOutside), 0o644); err != nil {
		t.Fatal(err)
	}
	drifts, err := p.Check(nil, "", baseOp, vals)
	if err != nil || len(drifts) != 0 {
		t.Fatalf("check with outside edits: %+v (%v)", drifts, err)
	}
	_, _, ops, err = p.Plan(nil, "", baseOp, vals, snap)
	if err != nil || len(ops) != 0 {
		t.Fatalf("plan with outside edits: %+v (%v)", ops, err)
	}

	// 4. Editing inside the markers causes Check to report block drift and Plan to plan update.
	driftedFile := "# user prefix\n# managed-machine:begin managed-machine\nexport EDITOR=nano\n# managed-machine:end managed-machine\n# user suffix\n"
	if err := os.WriteFile(dest, []byte(driftedFile), 0o644); err != nil {
		t.Fatal(err)
	}
	drifts, err = p.Check(nil, "", baseOp, vals)
	if err != nil || len(drifts) != 1 || drifts[0].Subject != "managed-machine" || !strings.Contains(drifts[0].Detail, "actual=sha256:") {
		t.Fatalf("check after block edit: %+v (%v)", drifts, err)
	}
	_, _, ops, err = p.Plan(nil, "", baseOp, vals, snap)
	if err != nil || len(ops) != 1 || ops[0].Action != "update" {
		t.Fatalf("plan after block edit: %+v (%v)", ops, err)
	}

	// 5. Applying update restores the block in place while keeping the outside lines byte-identical.
	upStep, err := p.Step(ops[0], recFn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := upStep.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := upStep.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	gotDisk, err = os.ReadFile(dest)
	if err != nil || string(gotDisk) != withOutside {
		t.Fatalf("after update: got %q, want %q (%v)", gotDisk, withOutside, err)
	}

	// 6. Delete removes the owned block and its markers only, leaving outside lines byte-identical,
	// and appends a released "block" ledger entry.
	delOp := plan.Operation{
		ID:        "rc",
		Operation: "rc",
		Target:    "user",
		Action:    "delete",
		Provider:  Name,
		Params: map[string]string{
			"config": "shell",
			"path":   dest,
			"kind":   ledger.KindBlock,
			"id":     "managed-machine",
		},
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
	gotDisk, err = os.ReadFile(dest)
	if err != nil || string(gotDisk) != "# user prefix\n# user suffix\n" {
		t.Fatalf("after delete: got %q, want %q (%v)", gotDisk, "# user prefix\n# user suffix\n", err)
	}
	last := recorded[len(recorded)-1]
	if last.Kind != ledger.KindBlock || last.ID != "managed-machine" || last.State != ledger.Released {
		t.Fatalf("released record: %+v", last)
	}
}

func TestForeignBlockAndOtherToolMarkers(t *testing.T) {
	p := New()
	dir := t.TempDir()
	dest := filepath.Join(dir, ".bashrc")
	vals := render.Values{
		Inputs: map[string]string{"editor": "vim"},
	}
	baseOp := plan.Operation{
		ID:        "rc",
		Operation: "rc",
		Target:    "user",
		Provider:  Name,
		Params: map[string]string{
			"config":      "shell",
			"destination": "shell-rc",
			"path":        dest,
			"block_id":    "managed-machine",
			"content":     "export EDITOR={{ inputs.editor }}\n",
		},
	}

	// 1. Markers with the same block_id and no block ledger entry plan
	// "conflict foreign-block <path>:<block_id>" even when the content already matches.
	exactBlock := "# managed-machine:begin managed-machine\nexport EDITOR=vim\n# managed-machine:end managed-machine\n"
	if err := os.WriteFile(dest, []byte(exactBlock), 0o644); err != nil {
		t.Fatal(err)
	}
	wantConflict := "conflict foreign-block " + dest + ":managed-machine"
	_, _, ops, err := p.Plan(nil, "", baseOp, vals, ledger.NewSnapshot(nil))
	if err != nil || len(ops) != 1 || ops[0].Action != wantConflict {
		t.Fatalf("foreign block with no ledger entry: %+v (%v)", ops, err)
	}

	// 2. A file ledger entry (KindFile) does not satisfy block ownership.
	fileSnap := ledger.NewSnapshot([]ledger.Record{{
		SchemaVersion: ledger.SchemaVersion,
		Kind:          ledger.KindFile,
		Path:          dest,
		SHA256:        file.SHA256Hex([]byte(exactBlock)),
		Config:        "shell",
		Operation:     "rc",
		Target:        "user",
		Account:       "tester",
		Plan:          "plan-20261021T120000Z-0001",
		At:            "2026-10-21T12:00:00Z",
		State:         ledger.Owned,
	}})
	_, _, ops, err = p.Plan(nil, "", baseOp, vals, fileSnap)
	if err != nil || len(ops) != 1 || ops[0].Action != wantConflict {
		t.Fatalf("foreign block with file ledger entry: %+v (%v)", ops, err)
	}

	// 3. A block ledger entry owned by another config still plans foreign-block conflict.
	otherCfgSnap := ledger.NewSnapshot([]ledger.Record{{
		SchemaVersion: ledger.SchemaVersion,
		Kind:          ledger.KindBlock,
		Path:          dest,
		ID:            "managed-machine",
		SHA256:        file.SHA256Hex([]byte(exactBlock)),
		Config:        "other-config",
		Operation:     "rc",
		Target:        "user",
		Account:       "tester",
		Plan:          "plan-20261021T120000Z-0001",
		At:            "2026-10-21T12:00:00Z",
		State:         ledger.Owned,
	}})
	_, _, ops, err = p.Plan(nil, "", baseOp, vals, otherCfgSnap)
	if err != nil || len(ops) != 1 || ops[0].Action != wantConflict {
		t.Fatalf("foreign block owned by another config: %+v (%v)", ops, err)
	}

	// 4. Unbalanced/malformed markers for the same block_id plan conflict and report malformed drift.
	for _, malformed := range []string{
		"# managed-machine:begin managed-machine\nexport EDITOR=vim\n",
		"export EDITOR=vim\n# managed-machine:end managed-machine\n",
		"# managed-machine:end managed-machine\n# managed-machine:begin managed-machine\n",
	} {
		if err := os.WriteFile(dest, []byte(malformed), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, ops, err = p.Plan(nil, "", baseOp, vals, ledger.NewSnapshot(nil))
		if err != nil || len(ops) != 1 || ops[0].Action != wantConflict {
			t.Fatalf("malformed block (%q): %+v (%v)", malformed, ops, err)
		}
		drifts, err := p.Check(nil, "", baseOp, vals)
		if err != nil || len(drifts) != 1 || !strings.Contains(drifts[0].Detail, "actual=malformed") {
			t.Fatalf("check malformed (%q): %+v (%v)", malformed, drifts, err)
		}
	}

	// 5. Another tool's markers (even with the same block_id) are never touched or counted.
	otherTool := "# BEGIN ANSIBLE MANAGED BLOCK\nexport A=1\n# END ANSIBLE MANAGED BLOCK\n" +
		"# other-tool:begin managed-machine\nexport B=2\n# other-tool:end managed-machine\n"
	if err := os.WriteFile(dest, []byte(otherTool), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ops, err = p.Plan(nil, "", baseOp, vals, ledger.NewSnapshot(nil))
	if err != nil || len(ops) != 1 || ops[0].Action != "create" {
		t.Fatalf("other tool's markers should plan create: %+v (%v)", ops, err)
	}
	st, err := p.Step(ops[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.HasPrefix(got, []byte(otherTool)) {
		t.Fatalf("other tool's markers were altered:\ngot:  %q\nwant prefix: %q", got, otherTool)
	}
}

func TestVerifyRejectsTamperedBlockAndDoesNotRecordOwnership(t *testing.T) {
	p := New()
	dest := filepath.Join(t.TempDir(), ".bashrc")
	tampered := "# managed-machine:begin managed-machine\nexport EDITOR=tampered\n# managed-machine:end managed-machine\n"
	if err := os.WriteFile(dest, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	expected := FormatBlock("managed-machine", "#", "export EDITOR=vim\n", "\n")
	var recorded []ledger.Record
	st, err := p.Step(plan.Operation{
		ID:        "rc",
		Operation: "rc",
		Target:    "user",
		Action:    "create",
		Provider:  Name,
		Params: map[string]string{
			"config":   "shell",
			"path":     dest,
			"block_id": "managed-machine",
			"content":  string(expected),
			"sha256":   file.SHA256Hex(expected),
		},
	}, func(r ledger.Record) error {
		recorded = append(recorded, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Fatal("Verify accepted tampered block")
	}
	if len(recorded) != 0 {
		t.Fatalf("Verify recorded ownership on mismatch: %+v", recorded)
	}
}
