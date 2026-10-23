package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/target"
)

type execEnv struct {
	t         *testing.T
	dir       string
	sourceDir string
	stateDir  string
	homeDir   string
	binDir    string
	desired   string
	account   string
	machine   string
	platform  target.Target
	now       time.Time
	seq       int
	writable  func(string) bool
	secrets   func(context.Context, string) (string, error)
	compose   func(Definition, Desired) (Definition, error)
}

func newExecEnv(t *testing.T) *execEnv {
	t.Helper()
	dir := t.TempDir()
	srcDir := filepath.Join(dir, "source")
	stateDir := filepath.Join(dir, "state")
	homeDir := filepath.Join(dir, "home")
	binDir := filepath.Join(dir, "bin")
	for _, d := range []string{srcDir, homeDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &execEnv{
		t:         t,
		dir:       dir,
		sourceDir: srcDir,
		stateDir:  stateDir,
		homeDir:   homeDir,
		binDir:    binDir,
		desired:   filepath.Join(dir, "desired.yaml"),
		account:   "alice",
		machine:   "machine-1",
		platform:  target.DarwinARM64,
		now:       time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC),
	}
}

func (e *execEnv) writeSource(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.sourceDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *execEnv) writeDesired(content string) {
	e.t.Helper()
	if err := os.WriteFile(e.desired, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *execEnv) writeTool(dir, name string) {
	e.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	file := name
	if runtime.GOOS == "windows" {
		file = name + ".exe"
	}
	p := filepath.Join(dir, file)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
}

func (e *execEnv) run(args ...string) (string, string, int) {
	e.t.Helper()
	var out, errb bytes.Buffer
	envMap := map[string]string{
		DesiredEnv: e.desired,
		"HOME":     e.homeDir,
		"PATH":     e.binDir,
		"PATHEXT":  ".EXE;.CMD;.BAT",
	}
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &out,
		Stderr: &errb,
		Detect: func() (target.Host, error) { return target.Host{Target: e.platform}, nil },
		Commands: []cli.Command{Command(Deps{
			Catalog: func() ([]Entry, error) {
				snap, err := Load(source.Identity{Kind: "local", Location: "fixtures/local"}, os.DirFS(e.sourceDir), ".")
				if err != nil {
					return nil, err
				}
				return Catalog(snap)
			},
			Getenv: func(k string) string { return envMap[k] },
			Environ: func() []string {
				return []string{
					"PATH=" + envMap["PATH"],
					"PATHEXT=" + envMap["PATHEXT"],
				}
			},
			GOOS:     "darwin",
			StateDir: func() (string, error) { return e.stateDir, nil },
			Account:  func() (string, error) { return e.account, nil },
			Machine:  func(context.Context) (string, error) { return e.machine, nil },
			Now: func() time.Time {
				e.seq++
				return e.now.Add(time.Duration(e.seq) * time.Second)
			},
			NewPlanID: func(now time.Time) (string, error) {
				return fmt.Sprintf("plan-%s-%04x", now.UTC().Format("20060102T150405Z"), e.seq), nil
			},
			Writable: e.writable,
			Secrets:  e.secrets,
			Compose:  e.compose,
		})},
	}
	code := app.Run(append([]string{"config"}, args...))
	return out.String(), errb.String(), code
}

// TestFileAndTemplateLifecycle proves the full --plan -> --apply -> --check ->
// --status lifecycle for a configuration combining both the file and template
// providers, activation records and idempotent re-planning.
func TestFileAndTemplateLifecycle(t *testing.T) {
	e := newExecEnv(t)
	e.writeTool(e.binDir, "git")
	e.writeSource("files/gitignore", "*.swp\n.DS_Store\n")
	e.writeSource("templates/gitconfig.tmpl", "[user]\n\tname = {{ inputs.name }}\n[core]\n\texcludesfile = {{ destinations.ignore }}\n\tcomment = {{ settings.header }}\n")
	e.writeSource("git.yaml", `kind: config
schema_version: 1
id: git
description: Git configuration and ignore file.
targets: [user, custom]
inputs:
  name: Alice
operations:
  - id: ignore
    provider: file
    destination: ignore
    source: files/gitignore
  - id: config
    provider: template
    destination: config
    source: templates/gitconfig.tmpl
    settings:
      header: "managed for {{ inputs.name }}"
activation:
  gitconfig: "{{ destinations.config }}"
requires:
  - command: git
`)
	ignoreDest := filepath.Join(e.homeDir, ".config", "git", "ignore")
	configDest := filepath.Join(e.homeDir, ".gitconfig")
	e.writeDesired(fmt.Sprintf(`target: user
config:
  git:
    inputs:
      name: Bob
    destinations:
      ignore: %s
      config: %s
`, filepath.ToSlash(ignoreDest), filepath.ToSlash(configDest)))

	// 1. Before apply, --status reports absent and creates no state directory.
	out, errOut, code := e.run("git", "--status", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("status before apply: exit %d %q %q", code, out, errOut)
	}
	wantStatusAbsent := `{"id":"git","operation":"ignore","target":"user","state":"absent"}` + "\n" +
		`{"id":"git","operation":"config","target":"user","state":"absent"}` + "\n"
	if out != wantStatusAbsent {
		t.Errorf("status before apply:\ngot:  %s\nwant: %s", out, wantStatusAbsent)
	}

	// 2. Before apply, destination does not exist so --check exits 4 (MM_CONFIG_CHECK_INCOMPLETE).
	out, errOut, code = e.run("git", "--check", "--user")
	if code != 4 || out != "" || !strings.Contains(errOut, "MM_CONFIG_CHECK_INCOMPLETE") {
		t.Fatalf("check before apply: exit %d %q %q", code, out, errOut)
	}

	// 3. --apply without a saved plan fails with MM_CONFIG_NO_PLAN (exit 2).
	out, errOut, code = e.run("git", "--apply", "--user")
	if code != 2 || out != "" || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Fatalf("apply without plan: exit %d %q %q", code, out, errOut)
	}

	// 4. --plan is read-only: writes the plan in stateDir, touches no destination.
	out, errOut, code = e.run("git", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan: exit %d %q %q", code, out, errOut)
	}
	planID := "plan-20261021T120001Z-0001"
	wantPlanOut := fmt.Sprintf(`{"id":"git","target":"user","plan":%q}`+"\n", planID) +
		`{"id":"git","operation":"ignore","target":"user","action":"create"}` + "\n" +
		`{"id":"git","operation":"config","target":"user","action":"create"}` + "\n"
	if out != wantPlanOut {
		t.Errorf("plan output:\ngot:  %s\nwant: %s", out, wantPlanOut)
	}
	if _, err := os.Stat(ignoreDest); !os.IsNotExist(err) {
		t.Fatal("--plan created ignore destination")
	}
	if _, err := os.Stat(configDest); !os.IsNotExist(err) {
		t.Fatal("--plan created config destination")
	}

	// Inspect the saved plan document: assets pin git.yaml, files/gitignore and templates/gitconfig.tmpl.
	p := readPlan(t, e.stateDir, planID)
	if p.Command != "config" || p.Config != "git" || p.State != plan.Saved || len(p.Bindings.Assets) != 3 || len(p.Operations) != 2 {
		t.Fatalf("saved plan: %+v", p)
	}

	// 5. --apply executes the plan, writes both files, appends to the ownership ledger, and emits apply + activation records.
	out, errOut, code = e.run("git", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply: exit %d %q %q", code, out, errOut)
	}
	wantApplyOut := `{"id":"git","operation":"ignore","target":"user","state":"applied"}` + "\n" +
		`{"id":"git","operation":"config","target":"user","state":"applied"}` + "\n" +
		fmt.Sprintf(`{"id":"git","target":"user","activation":"gitconfig","value":%q}`+"\n", filepath.ToSlash(configDest))
	if out != wantApplyOut {
		t.Errorf("apply output:\ngot:  %s\nwant: %s", out, wantApplyOut)
	}
	gotIgnore, err := os.ReadFile(ignoreDest)
	if err != nil || string(gotIgnore) != "*.swp\n.DS_Store\n" {
		t.Errorf("ignore content: %q (%v)", gotIgnore, err)
	}
	wantConfig := "[user]\n\tname = Bob\n[core]\n\texcludesfile = " + filepath.ToSlash(ignoreDest) + "\n\tcomment = managed for Bob\n"
	gotConfig, err := os.ReadFile(configDest)
	if err != nil || string(gotConfig) != wantConfig {
		t.Errorf("config content:\ngot:  %q\nwant: %q (%v)", gotConfig, wantConfig, err)
	}

	// 6. A plan runs at most once: re-applying without a new plan fails with MM_CONFIG_NO_PLAN,
	// and re-applying with --from-plan fails with MM_CONFIG_NO_PLAN (not eligible).
	if _, errOut, code := e.run("git", "--apply", "--user"); code != 2 || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Errorf("second apply without plan: exit %d %q", code, errOut)
	}
	if _, errOut, code := e.run("git", "--apply", "--user", "--from-plan", planID); code != 2 || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Errorf("second apply with --from-plan: exit %d %q", code, errOut)
	}

	// 7. After apply, --check exits 0 with no output.
	out, errOut, code = e.run("git", "--check", "--user")
	if code != 0 || out != "" || errOut != "" {
		t.Errorf("check after apply: exit %d %q %q", code, out, errOut)
	}

	// 7b. After an external edit, --check exits 3 with one drift record per difference,
	// and restoring the file returns --check to exit 0.
	if err := os.WriteFile(ignoreDest, []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = e.run("git", "--check", "--user")
	if code != 3 || !strings.Contains(errOut, "MM_CONFIG_DRIFT_FOUND") || !strings.Contains(out, `"operation":"ignore","target":"user","drift":"ignore"`) {
		t.Fatalf("check after external edit: exit %d %q %q", code, out, errOut)
	}
	if err := os.WriteFile(ignoreDest, []byte("*.swp\n.DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 8. After apply, --status reports applied for both operations.
	out, errOut, code = e.run("git", "--status", "--user")
	wantStatusApplied := `{"id":"git","operation":"ignore","target":"user","state":"applied"}` + "\n" +
		`{"id":"git","operation":"config","target":"user","state":"applied"}` + "\n"
	if code != 0 || out != wantStatusApplied || errOut != "" {
		t.Errorf("status after apply: exit %d %q %q", code, out, errOut)
	}

	// 9. Re-planning when destinations already match produces a plan with 0 operations,
	// and applying it still returns the activation record.
	out, errOut, code = e.run("git", "--plan", "--user")
	if code != 0 || errOut != "" || strings.Count(out, "\n") != 1 {
		t.Fatalf("no-op plan: exit %d %q %q", code, out, errOut)
	}
	out, errOut, code = e.run("git", "--apply", "--user")
	wantNoopApply := fmt.Sprintf(`{"id":"git","target":"user","activation":"gitconfig","value":%q}`+"\n", filepath.ToSlash(configDest))
	if code != 0 || out != wantNoopApply || errOut != "" {
		t.Fatalf("no-op apply: exit %d %q %q", code, out, errOut)
	}
}

// TestSupersessionAndFromPlanMismatch proves that a newer --plan for the same
// (config, target, account, inputs) supersedes an earlier saved plan, while a
// saved plan for a different target/name/account is rejected by --from-plan
// with MM_CONFIG_PLAN_MISMATCH, and a saved plan whose inputs changed is
// rejected by --from-plan with MM_CONFIG_PLAN_REJECTED.
func TestSupersessionAndFromPlanMismatch(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("templates/rc.tmpl", "EDITOR={{ inputs.editor }}\n")
	e.writeSource("editor.yaml", `kind: config
schema_version: 1
id: editor
description: Editor configuration.
targets: [global, user]
inputs:
  editor: vim
operations:
  - id: rc
    provider: template
    destination: rc
    source: templates/rc.tmpl
`)
	dest := filepath.Join(e.homeDir, ".editorrc")
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  editor:\n    inputs:\n      editor: vim\n    destinations:\n      rc: %s\n", filepath.ToSlash(dest)))

	// First plan (vim) -> plan 1.
	if _, errOut, code := e.run("editor", "--plan", "--user"); code != 0 {
		t.Fatalf("first plan: exit %d %q", code, errOut)
	}
	firstID := "plan-20261021T120001Z-0001"

	// Second plan with identical inputs -> plan 2, supersedes plan 1.
	if _, errOut, code := e.run("editor", "--plan", "--user"); code != 0 {
		t.Fatalf("second plan: exit %d %q", code, errOut)
	}
	secondID := "plan-20261021T120002Z-0002"

	p1 := readPlan(t, e.stateDir, firstID)
	if p1.State != plan.Superseded {
		t.Errorf("first plan state = %q", p1.State)
	}

	// Applying the superseded plan by ID fails with MM_CONFIG_NO_PLAN (not eligible).
	if _, errOut, code := e.run("editor", "--apply", "--user", "--from-plan", firstID); code != 2 || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Errorf("apply superseded plan: exit %d %q", code, errOut)
	}

	// Applying plan 2 under a different account fails with MM_CONFIG_PLAN_MISMATCH (exit 2).
	e.account = "bob"
	if _, errOut, code := e.run("editor", "--apply", "--user", "--from-plan", secondID); code != 2 || !strings.Contains(errOut, "MM_CONFIG_PLAN_MISMATCH") {
		t.Errorf("apply --from-plan with mismatched account: exit %d %q", code, errOut)
	}
	e.account = "alice"

	// Changing desired input (vim -> nvim) makes plan 2's inputs_digest mismatch.
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  editor:\n    inputs:\n      editor: nvim\n    destinations:\n      rc: %s\n", filepath.ToSlash(dest)))

	// Without --from-plan, no saved plan matches the new inputs_digest -> MM_CONFIG_NO_PLAN.
	if _, errOut, code := e.run("editor", "--apply", "--user"); code != 2 || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Errorf("apply after input change: exit %d %q", code, errOut)
	}
	// With --from-plan naming plan 2 (whose inputs_digest was for vim) -> MM_CONFIG_PLAN_REJECTED (exit 1).
	if _, errOut, code := e.run("editor", "--apply", "--user", "--from-plan", secondID); code != 1 || !strings.Contains(errOut, "MM_CONFIG_PLAN_REJECTED") {
		t.Errorf("apply --from-plan with changed inputs: exit %d %q", code, errOut)
	}
	if p2 := readPlan(t, e.stateDir, secondID); p2.State != plan.Rejected {
		t.Errorf("plan 2 state: %+v", p2)
	}
}

// TestPlanRejectedOnAssetOrDestinationChange proves that execute.Run re-verifies
// pinned source assets and destination preconditions before modifying anything,
// transitioning the plan to rejected and returning MM_CONFIG_PLAN_REJECTED (exit 1).
func TestPlanRejectedOnAssetOrDestinationChange(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("files/conf", "v1\n")
	e.writeSource("app.yaml", `kind: config
schema_version: 1
id: app
description: Application config.
targets: [user]
operations:
  - id: conf
    provider: file
    destination: conf
    source: files/conf
`)
	dest := filepath.Join(e.homeDir, ".appconf")
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  app:\n    destinations:\n      conf: %s\n", filepath.ToSlash(dest)))

	// Case 1: source asset changes between --plan and --apply.
	if _, errOut, code := e.run("app", "--plan", "--user"); code != 0 {
		t.Fatalf("plan 1: exit %d %q", code, errOut)
	}
	plan1 := "plan-20261021T120001Z-0001"
	e.writeSource("files/conf", "v2\n")

	if _, errOut, code := e.run("app", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_PLAN_REJECTED") {
		t.Fatalf("apply after asset change: exit %d %q", code, errOut)
	}
	if p1 := readPlan(t, e.stateDir, plan1); p1.State != plan.Rejected {
		t.Errorf("plan 1 after asset change: %+v", p1)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("rejected plan wrote destination")
	}

	// Case 2: destination appears on disk between --plan (action: create) and --apply.
	if _, errOut, code := e.run("app", "--plan", "--user"); code != 0 {
		t.Fatalf("plan 2: exit %d %q", code, errOut)
	}
	plan2 := "plan-20261021T120003Z-0003"
	if err := os.WriteFile(dest, []byte("unmanaged\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run("app", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_PLAN_REJECTED") {
		t.Fatalf("apply after destination appeared: exit %d %q", code, errOut)
	}
	if p2 := readPlan(t, e.stateDir, plan2); p2.State != plan.Rejected {
		t.Errorf("plan 2 after destination change: %+v", p2)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "unmanaged\n" {
		t.Errorf("rejected plan overwrote unmanaged file: %q", got)
	}
}

// TestConflictRefusal proves that an existing destination without a matching
// file ledger entry plans `conflict replace-file <path>` and --apply refuses
// with MM_CONFIG_CONFLICT (exit 1) before writing, leaving the file byte-identical.
func TestConflictRefusal(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("files/a", "alpha\n")
	e.writeSource("files/b", "beta\n")
	e.writeSource("first.yaml", `kind: config
schema_version: 1
id: first
description: First config.
targets: [user]
operations:
  - id: main
    provider: file
    destination: dest
    source: files/a
`)
	e.writeSource("second.yaml", `kind: config
schema_version: 1
id: second
description: Second config.
targets: [user]
operations:
  - id: main
    provider: file
    destination: dest
    source: files/b
`)
	sharedDest := filepath.Join(e.homeDir, ".shared")
	e.writeDesired(fmt.Sprintf(`target: user
config:
  first:
    destinations:
      dest: %s
  second:
    destinations:
      dest: %s
`, filepath.ToSlash(sharedDest), filepath.ToSlash(sharedDest)))

	// 1. Pre-existing unmanaged file plans `conflict replace-file <path>` and --apply refuses with MM_CONFIG_CONFLICT.
	if err := os.WriteFile(sharedDest, []byte("handwritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run("first", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"conflict replace-file `) {
		t.Fatalf("plan on unmanaged file: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("first", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") {
		t.Fatalf("apply on unmanaged file: exit %d %q", code, errOut)
	}
	if got, _ := os.ReadFile(sharedDest); string(got) != "handwritten\n" {
		t.Fatalf("unmanaged file modified: %q", got)
	}

	// 2. Remove the unmanaged file, apply "first", then verify "second" plans conflict and apply refuses with MM_CONFIG_CONFLICT.
	if err := os.Remove(sharedDest); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run("first", "--plan", "--user"); code != 0 {
		t.Fatalf("first plan: exit %d %q", code, errOut)
	}
	if _, errOut, code := e.run("first", "--apply", "--user"); code != 0 {
		t.Fatalf("first apply: exit %d %q", code, errOut)
	}
	out, errOut, code = e.run("second", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"conflict replace-file `) {
		t.Fatalf("second plan on owned file: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("second", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") {
		t.Errorf("second apply on owned file: exit %d %q", code, errOut)
	}
	if got, _ := os.ReadFile(sharedDest); string(got) != "alpha\n" {
		t.Errorf("owned file modified by conflicting apply: %q", got)
	}
}

// TestOrphanedOperationDeleteAndCustomLedger proves that:
//  1. Custom targets write their ownership ledger to <root>/.managed-machine/ownership.jsonl
//     and never touch <state>/ownership.jsonl.
//  2. When an operation is removed from a definition, --plan plans a "delete"
//     operation, --apply deletes the file and records "released" in the ledger,
//     and --status reports "released" while the remaining operation stays "applied".
func TestOrphanedOperationDeleteAndCustomLedger(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("files/one", "one\n")
	e.writeSource("files/two", "two\n")
	e.writeSource("pair.yaml", `kind: config
schema_version: 1
id: pair
description: Two files in a custom root.
targets: [custom]
operations:
  - id: one
    provider: file
    destination: one
    source: files/one
  - id: two
    provider: file
    destination: two
    source: files/two
`)
	customRoot := filepath.Join(e.dir, "soul")
	destOne := filepath.Join(customRoot, "etc", "one.conf")
	destTwo := filepath.Join(customRoot, "etc", "two.conf")
	desiredCustom := filepath.Join(customRoot, ".managed-machine", "desired.yaml")
	if err := os.MkdirAll(filepath.Dir(desiredCustom), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(desiredCustom, []byte(fmt.Sprintf(`target: custom
root: %s
config:
  pair:
    destinations:
      one: %s
      two: %s
`, filepath.ToSlash(customRoot), filepath.ToSlash(destOne), filepath.ToSlash(destTwo))), 0o644); err != nil {
		t.Fatal(err)
	}
	// Clear DesiredEnv so DesiredPath uses <customRoot>/.managed-machine/desired.yaml.
	e.desired = ""

	if _, errOut, code := e.run("pair", "--plan", "--custom", customRoot); code != 0 {
		t.Fatalf("plan pair: exit %d %q", code, errOut)
	}
	if _, errOut, code := e.run("pair", "--apply", "--custom", customRoot); code != 0 {
		t.Fatalf("apply pair: exit %d %q", code, errOut)
	}

	// Custom target ledger lives inside customRoot, not stateDir.
	customLedger := filepath.Join(customRoot, ".managed-machine", "ownership.jsonl")
	if _, err := os.Stat(customLedger); err != nil {
		t.Fatalf("custom ledger missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "ownership.jsonl")); !os.IsNotExist(err) {
		t.Fatal("custom apply wrote to stateDir ownership.jsonl")
	}
	recs, err := ledger.Read(customLedger)
	if err != nil || len(recs) != 2 {
		t.Fatalf("custom ledger records: %+v (%v)", recs, err)
	}

	// Remove operation "two" from pair.yaml and its destination mapping from desired.yaml.
	e.writeSource("pair.yaml", `kind: config
schema_version: 1
id: pair
description: One file remaining.
targets: [custom]
operations:
  - id: one
    provider: file
    destination: one
    source: files/one
`)
	if err := os.WriteFile(desiredCustom, []byte(fmt.Sprintf(`target: custom
root: %s
config:
  pair:
    destinations:
      one: %s
`, filepath.ToSlash(customRoot), filepath.ToSlash(destOne))), 0o644); err != nil {
		t.Fatal(err)
	}

	// Before delete is applied, --status still reports "two" as applied because it is owned in the ledger.
	out, errOut, code := e.run("pair", "--status", "--custom", customRoot)
	if code != 0 || !strings.Contains(out, `"operation":"two"`) || !strings.Contains(out, `"state":"applied"`) {
		t.Fatalf("status with orphan before delete: exit %d %q %q", code, out, errOut)
	}

	// --plan plans a "delete" action for operation "two".
	out, errOut, code = e.run("pair", "--plan", "--custom", customRoot)
	if code != 0 || !strings.Contains(out, `"operation":"two"`) || !strings.Contains(out, `"action":"delete"`) {
		t.Fatalf("plan delete orphan: exit %d %q %q", code, out, errOut)
	}

	// --apply removes destTwo and records state "released" in the custom ledger.
	out, errOut, code = e.run("pair", "--apply", "--custom", customRoot)
	if code != 0 || errOut != "" {
		t.Fatalf("apply delete orphan: exit %d %q %q", code, out, errOut)
	}
	if _, err := os.Stat(destTwo); !os.IsNotExist(err) {
		t.Fatal("orphaned file was not removed by apply")
	}

	// Verify the custom ledger now ends with a released record for operation "two".
	recs, err = ledger.Read(customLedger)
	if err != nil || len(recs) != 3 || recs[2].Operation != "two" || recs[2].State != ledger.Released {
		t.Fatalf("custom ledger after release: %+v (%v)", recs, err)
	}
}

// TestPrerequisitesAndDiagnostics covers MM_CONFIG_MISSING_PREREQUISITE (both
// command and software), MM_CONFIG_PRIVILEGE_REQUIRED, MM_CONFIG_SECRET_UNRESOLVED,
// MM_CONFIG_CHECK_INCOMPLETE, MM_CONFIG_BUSY and the C07 flavor composition seam.
func TestPrerequisitesAndDiagnostics(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("files/rc", "content\n")
	e.writeSource("tool.yaml", `kind: config
schema_version: 1
id: tool
description: Config with prerequisites and inputs.
targets: [user]
inputs:
  token: plain
operations:
  - id: rc
    provider: file
    destination: rc
    source: files/rc
requires:
  - command: mycmd
  - software: ripgrep
`)
	dest := filepath.Join(e.homeDir, ".toolrc")
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  tool:\n    destinations:\n      rc: %s\n", filepath.ToSlash(dest)))

	// 1. Missing command prerequisite plans `missing-prerequisite mycmd` and apply refuses with MM_CONFIG_MISSING_PREREQUISITE (exit 1).
	out, errOut, code := e.run("tool", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"missing-prerequisite mycmd"`) {
		t.Fatalf("plan missing command: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("tool", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_MISSING_PREREQUISITE") || !strings.Contains(errOut, "mycmd") {
		t.Errorf("apply missing command: exit %d %q", code, errOut)
	}

	// Put mycmd on PATH; ripgrep software requirement is still missing.
	e.writeTool(e.binDir, "mycmd")
	out, errOut, code = e.run("tool", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"missing-prerequisite ripgrep"`) {
		t.Fatalf("plan missing software: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("tool", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_MISSING_PREREQUISITE") || !strings.Contains(errOut, "ripgrep") {
		t.Errorf("apply missing software: exit %d %q", code, errOut)
	}

	// Satisfy software requirement via an applied "software --install" plan and receipt in stateDir.
	store, err := plan.Open(e.stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	softPlan := &plan.Document{
		Plan:           "plan-20261021T110000Z-aaaa",
		SchemaVersion:  plan.SchemaVersion,
		Command:        "software --install",
		Software:       "ripgrep",
		Target:         "user",
		Account:        e.account,
		SourceRevision: "sha256:0000",
		CreatedAt:      "2026-10-21T11:00:00Z",
		State:          plan.Saved,
		Bindings: plan.Bindings{
			Machine:      e.machine,
			Platform:     string(e.platform),
			InputsDigest: "sha256:0000",
			Assets:       []plan.Asset{},
			Providers:    []plan.Provider{},
		},
		Preconditions: []plan.Precondition{},
		Operations:    []plan.Operation{},
	}
	if err := store.Save(softPlan); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Move(softPlan, plan.Executing); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Move(softPlan, plan.Applied); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Append(plan.Record{
		Plan:    softPlan.Plan,
		Action:  plan.ActionReceipt,
		Outcome: plan.OutcomeApplied,
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()

	// Now both requirements are satisfied, so --plan plans only the file operation.
	out, errOut, code = e.run("tool", "--plan", "--user")
	if code != 0 || strings.Contains(out, "missing-prerequisite") {
		t.Fatalf("plan with satisfied prerequisites: exit %d %q %q", code, out, errOut)
	}

	// 2. Privilege required when destination is not writable -> MM_CONFIG_PRIVILEGE_REQUIRED (exit 1).
	e.writable = func(string) bool { return false }
	if _, errOut, code := e.run("tool", "--plan", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_PRIVILEGE_REQUIRED") {
		t.Errorf("privilege required: exit %d %q", code, errOut)
	}
	e.writable = func(string) bool { return true }

	// 3. Unresolved secret:<name> input -> MM_CONFIG_SECRET_UNRESOLVED (exit 1).
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  tool:\n    inputs:\n      token: secret:api-key\n    destinations:\n      rc: %s\n", filepath.ToSlash(dest)))
	if _, errOut, code := e.run("tool", "--plan", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_SECRET_UNRESOLVED") {
		t.Errorf("unresolved secret: exit %d %q", code, errOut)
	}
	// When the Secrets seam resolves the secret, planning succeeds.
	e.secrets = func(_ context.Context, name string) (string, error) {
		if name == "api-key" {
			return "resolved-value", nil
		}
		return "", errors.New("not found")
	}
	if _, errOut, code := e.run("tool", "--plan", "--user"); code != 0 {
		t.Fatalf("plan with secret seam: exit %d %q", code, errOut)
	}

	// 4. Busy plan store -> MM_CONFIG_BUSY (exit 1).
	heldStore, err := plan.Open(e.stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := e.run("tool", "--plan", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_BUSY") {
		heldStore.Close()
		t.Fatalf("busy store: exit %d %q", code, errOut)
	}
	heldStore.Close()

	// 5. --check incomplete when destination cannot be read because its parent is a regular file -> MM_CONFIG_CHECK_INCOMPLETE (exit 4).
	blocker := filepath.Join(e.homeDir, "file-not-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	subDest := filepath.Join(blocker, "rc")
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  tool:\n    destinations:\n      rc: %s\n", filepath.ToSlash(subDest)))
	if _, errOut, code := e.run("tool", "--check", "--user"); code != 4 || !strings.Contains(errOut, "MM_CONFIG_CHECK_INCOMPLETE") {
		t.Errorf("check incomplete: exit %d %q", code, errOut)
	}

	// 6. C07 flavor composition seam is invoked before resolution.
	composedCalled := false
	e.compose = func(def Definition, _ Desired) (Definition, error) {
		composedCalled = true
		return def, nil
	}
	e.writeDesired(fmt.Sprintf("target: user\nconfig:\n  tool:\n    destinations:\n      rc: %s\n", filepath.ToSlash(dest)))
	if _, errOut, code := e.run("tool", "--plan", "--user"); code != 0 {
		t.Fatalf("plan with compose seam: exit %d %q", code, errOut)
	}
	if !composedCalled {
		t.Error("compose seam was not called")
	}
}

// TestManagedBlockLifecycle proves the end-to-end --plan -> --apply -> --check ->
// --status lifecycle for the managed-block provider (C04): absent creation,
// appending to an existing CRLF file, replacing an owned block in place,
// foreign-block conflict refusal, and orphan delete removing only the block
// and its markers while recording block entries in the ownership ledger.
func TestManagedBlockLifecycle(t *testing.T) {
	e := newExecEnv(t)
	e.writeTool(e.binDir, "sh")
	e.writeSource("shell.yaml", `kind: config
schema_version: 1
id: shell
description: Shell configuration blocks.
targets: [user]
inputs:
  editor: vim
operations:
  - id: env
    provider: managed-block
    destination: env
    block_id: mm-env
    content: |
      export EDITOR={{ inputs.editor }}
  - id: rc
    provider: managed-block
    destination: rc
    block_id: mm-rc
    comment: "//"
    content: |
      source {{ destinations.env }}
activation:
  shell: /bin/sh
requires:
  - command: sh
`)
	envDest := filepath.Join(e.homeDir, ".zshenv")
	rcDest := filepath.Join(e.homeDir, ".zshrc")
	// Pre-populate rcDest with CRLF content and another tool's block.
	rcInitial := "// User header\r\n// other-tool:begin mm-rc\r\nkeep = 1;\r\n// other-tool:end mm-rc\r\n"
	if err := os.WriteFile(rcDest, []byte(rcInitial), 0o644); err != nil {
		t.Fatal(err)
	}
	e.writeDesired(fmt.Sprintf(`target: user
config:
  shell:
    inputs:
      editor: nvim
    destinations:
      env: %s
      rc: %s
`, filepath.ToSlash(envDest), filepath.ToSlash(rcDest)))

	// 1. Plan and apply creates envDest holding only the block and appends to rcDest with CRLF.
	out, errOut, code := e.run("shell", "--plan", "--user")
	if code != 0 || errOut != "" || !strings.Contains(out, `"operation":"env","target":"user","action":"create"`) || !strings.Contains(out, `"operation":"rc","target":"user","action":"create"`) {
		t.Fatalf("plan shell: exit %d %q %q", code, out, errOut)
	}
	out, errOut, code = e.run("shell", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply shell: exit %d %q %q", code, out, errOut)
	}

	gotEnv, err := os.ReadFile(envDest)
	wantEnv := "# managed-machine:begin mm-env\nexport EDITOR=nvim\n# managed-machine:end mm-env\n"
	if err != nil || string(gotEnv) != wantEnv {
		t.Fatalf("envDest: got %q, want %q (%v)", gotEnv, wantEnv, err)
	}
	cleanEnv := filepath.ToSlash(envDest)
	gotRC, err := os.ReadFile(rcDest)
	wantRC := rcInitial + "// managed-machine:begin mm-rc\r\nsource " + cleanEnv + "\r\n// managed-machine:end mm-rc\r\n"
	if err != nil || string(gotRC) != wantRC {
		t.Fatalf("rcDest: got %q, want %q (%v)", gotRC, wantRC, err)
	}

	// 2. Ownership ledger records "block" entries only, never "file" entries.
	recs, err := ledger.Read(filepath.Join(e.stateDir, ledger.FileName))
	if err != nil || len(recs) != 2 {
		t.Fatalf("ledger records: %+v (%v)", recs, err)
	}
	for _, r := range recs {
		if r.Kind != ledger.KindBlock || r.State != ledger.Owned {
			t.Fatalf("expected owned block ledger entry, got %+v", r)
		}
	}

	// 3. Reapply with unchanged content plans zero operations; --check passes.
	out, errOut, code = e.run("shell", "--plan", "--user")
	if code != 0 || errOut != "" || strings.Count(out, "\n") != 1 {
		t.Fatalf("reapply no-op plan: exit %d %q %q", code, out, errOut)
	}
	if out, errOut, code := e.run("shell", "--check", "--user"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("check clean: exit %d %q %q", code, out, errOut)
	}

	// 4. Editing inside the block causes --check to exit 3 (MM_CONFIG_DRIFT_FOUND);
	// updating inputs replaces the owned block in place while keeping outside bytes identical.
	driftedRC := rcInitial + "// managed-machine:begin mm-rc\r\nsource tampered\r\n// managed-machine:end mm-rc\r\n// Footer CRLF\r\n"
	if err := os.WriteFile(rcDest, []byte(driftedRC), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = e.run("shell", "--check", "--user")
	if code != 3 || !strings.Contains(errOut, "MM_CONFIG_DRIFT_FOUND") || !strings.Contains(out, `"drift":"mm-rc"`) {
		t.Fatalf("check drifted block: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("shell", "--plan", "--user"); code != 0 {
		t.Fatalf("plan update: exit %d %q", code, errOut)
	}
	if _, errOut, code := e.run("shell", "--apply", "--user"); code != 0 {
		t.Fatalf("apply update: exit %d %q", code, errOut)
	}
	gotRC, err = os.ReadFile(rcDest)
	wantRCAfterUpdate := rcInitial + "// managed-machine:begin mm-rc\r\nsource " + cleanEnv + "\r\n// managed-machine:end mm-rc\r\n// Footer CRLF\r\n"
	if err != nil || string(gotRC) != wantRCAfterUpdate {
		t.Fatalf("rcDest after update: got %q, want %q (%v)", gotRC, wantRCAfterUpdate, err)
	}

	// 5. Removing operation "rc" from the definition plans delete, which removes only
	// the mm-rc block and its markers and leaves the rest of rcDest byte-identical.
	e.writeSource("shell.yaml", `kind: config
schema_version: 1
id: shell
description: Shell configuration blocks.
targets: [user]
inputs:
  editor: vim
operations:
  - id: env
    provider: managed-block
    destination: env
    block_id: mm-env
    content: |
      export EDITOR={{ inputs.editor }}
requires:
  - command: sh
`)
	e.writeDesired(fmt.Sprintf(`target: user
config:
  shell:
    inputs:
      editor: nvim
    destinations:
      env: %s
`, filepath.ToSlash(envDest)))
	out, errOut, code = e.run("shell", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"operation":"rc","target":"user","action":"delete"`) {
		t.Fatalf("plan orphan block delete: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("shell", "--apply", "--user"); code != 0 {
		t.Fatalf("apply orphan block delete: exit %d %q", code, errOut)
	}
	gotRC, err = os.ReadFile(rcDest)
	wantRCAfterDelete := rcInitial + "// Footer CRLF\r\n"
	if err != nil || string(gotRC) != wantRCAfterDelete {
		t.Fatalf("rcDest after delete: got %q, want %q (%v)", gotRC, wantRCAfterDelete, err)
	}

	// 6. Putting a foreign block with the same block_id (no active owned ledger entry)
	// plans `conflict foreign-block <path>:<block_id>` and --apply refuses with MM_CONFIG_CONFLICT.
	foreignEnv := "# managed-machine:begin mm-foreign\nexport FOO=1\n# managed-machine:end mm-foreign\n"
	foreignDest := filepath.Join(e.homeDir, ".foreignrc")
	if err := os.WriteFile(foreignDest, []byte(foreignEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	e.writeSource("foreign.yaml", `kind: config
schema_version: 1
id: foreign
description: Foreign block test.
targets: [user]
operations:
  - id: f
    provider: managed-block
    destination: f
    block_id: mm-foreign
    content: |
      export FOO=1
`)
	e.writeDesired(fmt.Sprintf(`target: user
config:
  foreign:
    destinations:
      f: %s
`, filepath.ToSlash(foreignDest)))
	out, errOut, code = e.run("foreign", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"conflict foreign-block `) || !strings.Contains(out, `:mm-foreign"`) {
		t.Fatalf("plan foreign block: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("foreign", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") {
		t.Fatalf("apply foreign block: exit %d %q", code, errOut)
	}
	if got, _ := os.ReadFile(foreignDest); string(got) != foreignEnv {
		t.Fatalf("foreign block file modified: %q", got)
	}
}

func readPlan(t *testing.T, stateDir, id string) *plan.Document {
	t.Helper()
	store, err := plan.Open(stateDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	doc, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
