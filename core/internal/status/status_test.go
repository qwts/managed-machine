package status_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/compose"
	"github.com/qwts/managed-machine/core/internal/config"
	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/block"
	"github.com/qwts/managed-machine/core/internal/config/provider/file"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/software"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/status"
	"github.com/qwts/managed-machine/core/internal/target"
)

type testEnv struct {
	t        *testing.T
	dir      string
	homeDir  string
	stateDir string
	binDir   string
	sentinel string
	now      time.Time
	flavors  []compose.Flavor
	configs  []config.Entry
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	base := t.TempDir()
	e := &testEnv{
		t:        t,
		dir:      base,
		homeDir:  filepath.Join(base, "home"),
		stateDir: filepath.Join(base, "state"),
		binDir:   filepath.Join(base, "bin"),
		sentinel: filepath.Join(base, "sentinel-invoked"),
		now:      time.Date(2026, 10, 21, 15, 0, 0, 0, time.UTC),
	}
	for _, d := range []string{e.homeDir, e.stateDir, e.binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Install sentinel binaries for every package manager and secret tool so
	// any process execution during status fails the test immediately.
	for _, tool := range []string{"brew", "uv", "npm", "cargo", "go", "winget", "pass", "gpg", "git", "sudo"} {
		writeSentinelTool(t, e.binDir, tool, e.sentinel)
	}
	e.configs = []config.Entry{
		{
			Definition: config.Definition{
				SchemaVersion: 1,
				ID:            "gitconfig",
				Description:   "Git user config",
				Targets:       []string{"user", "global", "custom"},
			},
		},
		{
			Definition: config.Definition{
				SchemaVersion: 1,
				ID:            "shellrc",
				Description:   "Shell block config",
				Targets:       []string{"user"},
			},
		},
		{
			Definition: config.Definition{
				SchemaVersion: 1,
				ID:            "settings",
				Description:   "Structured JSON settings",
				Targets:       []string{"user"},
			},
		},
		{
			Definition: config.Definition{
				SchemaVersion: 1,
				ID:            "starship",
				Description:   "Structured TOML settings",
				Targets:       []string{"user"},
			},
		},
	}
	return e
}

func writeSentinelTool(t *testing.T, binDir, name, markerPath string) {
	t.Helper()
	posixScript := fmt.Sprintf("#!/bin/sh\necho %s >> %q\nexit 99\n", name, markerPath)
	p := filepath.Join(binDir, name)
	if err := os.WriteFile(p, []byte(posixScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		bat := fmt.Sprintf("@echo off\r\necho %s >> \"%s\"\r\nexit /b 99\r\n", name, markerPath)
		if err := os.WriteFile(p+".bat", []byte(bat), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".cmd", []byte(bat), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *testEnv) run(args ...string) (string, string, int) {
	e.t.Helper()
	var stdout, stderr bytes.Buffer
	getenv := func(k string) string {
		switch k {
		case "PATH":
			return e.binDir
		case "PATHEXT":
			return ".COM;.EXE;.BAT;.CMD"
		case "HOME", "USERPROFILE":
			return e.homeDir
		case "MANAGED_MACHINE_STATE_DIR":
			return e.stateDir
		}
		return ""
	}
	app := &cli.App{
		Name: "managed-machine",
		Commands: []cli.Command{
			status.Command(status.Deps{
				SoftwareCatalog: func() ([]source.Entry, *registry.Registry, error) {
					return software.LoadCatalog(func(string) string { return "" })
				},
				ConfigCatalog: func() ([]config.Entry, error) {
					return e.configs, nil
				},
				Flavors:  e.flavors,
				Getenv:   getenv,
				GOOS:     runtime.GOOS,
				StateDir: func() (string, error) { return e.stateDir, nil },
				Now:      func() time.Time { return e.now },
			}),
		},
		Stdout: &stdout,
		Stderr: &stderr,
		Detect: func() (target.Host, error) {
			return target.Host{Target: target.LinuxAMD64}, nil
		},
	}
	full := append([]string{"status"}, args...)
	code := app.Run(full)
	if _, err := os.Stat(e.sentinel); err == nil {
		got, _ := os.ReadFile(e.sentinel)
		e.t.Fatalf("status spawned a sentinel process: %s", string(got))
	}
	return stdout.String(), stderr.String(), code
}

func (e *testEnv) saveSoftwarePlan(planID, swID, tgt, createdAt string, state plan.State, receiptOutcome string, receiptAt string) {
	e.t.Helper()
	st, err := plan.Open(e.stateDir, func() time.Time { return e.now })
	if err != nil {
		e.t.Fatal(err)
	}
	defer st.Close()
	doc := &plan.Document{
		Plan:           planID,
		SchemaVersion:  plan.SchemaVersion,
		Command:        "software --install",
		Software:       swID,
		Target:         tgt,
		Account:        "tester",
		SourceRevision: "sha256:0000",
		CreatedAt:      createdAt,
		State:          plan.Saved,
		Bindings: plan.Bindings{
			Machine:      "local",
			Platform:     "linux/amd64",
			InputsDigest: "sha256:0000",
			Assets:       []plan.Asset{},
			Providers:    []plan.Provider{},
		},
		Preconditions: []plan.Precondition{},
		Operations:    []plan.Operation{},
	}
	if err := st.Save(doc); err != nil {
		e.t.Fatal(err)
	}
	if state == plan.Executing || state == plan.Applied || state == plan.Failed || state == plan.Rejected {
		if state == plan.Rejected {
			if err := st.Move(doc, plan.Rejected); err != nil {
				e.t.Fatal(err)
			}
		} else {
			if err := st.Move(doc, plan.Executing); err != nil {
				e.t.Fatal(err)
			}
			if state == plan.Applied || state == plan.Failed {
				if err := st.Move(doc, state); err != nil {
					e.t.Fatal(err)
				}
			}
		}
	}
	if receiptOutcome != "" {
		if err := st.Append(plan.Record{
			Plan:    planID,
			Action:  plan.ActionReceipt,
			Outcome: receiptOutcome,
			At:      receiptAt,
		}); err != nil {
			e.t.Fatal(err)
		}
	}
}

// snapshotDir captures a deterministic digest of every entry under root so
// tests can prove status performs zero filesystem writes.
func snapshotDir(t *testing.T, root string) string {
	t.Helper()
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return "<absent>"
	}
	var lines []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			lines = append(lines, fmt.Sprintf("D:%s", rel))
			return nil
		}
		if info.Name() == ".lock" {
			// A held plan-store lock cannot be read on Windows, where the
			// lock is mandatory; its size and modification time still show
			// any write.
			lines = append(lines, fmt.Sprintf("L:%s:%d:%d", rel, info.Size(), info.ModTime().UnixNano()))
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("F:%s:%d:%s:%d", rel, info.Size(), file.SHA256Hex(data), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// assertKeyOrder verifies that every JSON object in records has the exact D7
// key order: section, resource, target, state, facts, evidence, at.
func assertKeyOrder(t *testing.T, rawJSONL string) {
	t.Helper()
	wantOrder := []string{"section", "resource", "target", "state", "facts", "evidence", "at"}
	for i, line := range strings.Split(strings.TrimSpace(rawJSONL), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		dec := json.NewDecoder(strings.NewReader(line))
		tok, err := dec.Token()
		if err != nil || tok != json.Delim('{') {
			t.Fatalf("line %d: expected '{', got %v (%v) in %s", i+1, tok, err, line)
		}
		var gotKeys []string
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				t.Fatalf("line %d: key token error: %v", i+1, err)
			}
			ks, ok := k.(string)
			if !ok {
				t.Fatalf("line %d: expected string key, got %T", i+1, k)
			}
			gotKeys = append(gotKeys, ks)
			var val json.RawMessage
			if err := dec.Decode(&val); err != nil {
				t.Fatalf("line %d: decode value for %s: %v", i+1, ks, err)
			}
		}
		if strings.Join(gotKeys, ",") != strings.Join(wantOrder, ",") {
			t.Fatalf("line %d: key order = %v, want %v\nraw: %s", i+1, gotKeys, wantOrder, line)
		}
	}
}

func TestUsageErrors(t *testing.T) {
	e := newTestEnv(t)
	e.flavors = []compose.Flavor{{
		SchemaVersion: compose.SchemaVersion,
		ID:            "workstation",
		Description:   "Developer workstation",
		Config:        []compose.ConfigEntry{{ID: "gitconfig", Target: "user"}},
	}}

	cases := []struct {
		name string
		args []string
	}{
		{name: "name_without_section", args: []string{"ripgrep"}},
		{name: "comma_section_list", args: []string{"--section", "software,config"}},
		{name: "comma_section_equals", args: []string{"--section=software,flavor"}},
		{name: "multiple_section_flags", args: []string{"--section", "software", "--section", "config"}},
		{name: "unknown_section", args: []string{"--section", "bogus"}},
		{name: "legacy_section_account", args: []string{"--section", "account"}},
		{name: "missing_section_value", args: []string{"--section"}},
		{name: "empty_section_equals", args: []string{"--section="}},
		{name: "unknown_software_name", args: []string{"--section", "software", "no-such-software"}},
		{name: "unknown_config_name", args: []string{"--section", "config", "no-such-config"}},
		{name: "unknown_flavor_name", args: []string{"--section", "flavor", "no-such-flavor"}},
		{name: "two_resource_names", args: []string{"--section", "software", "ripgrep", "jq"}},
		{name: "missing_custom_value", args: []string{"--section", "config", "--custom"}},
		{name: "conflicting_targets", args: []string{"--section", "config", "--user", "--global"}},
		{name: "unknown_flag", args: []string{"--section", "software", "--nope"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := e.run(tc.args...)
			if code != diag.ExitUsage {
				t.Fatalf("args %v: exit = %d, want %d; stdout=%q stderr=%q", tc.args, code, diag.ExitUsage, out, errOut)
			}
			if out != "" {
				t.Errorf("args %v: expected empty stdout on usage error, got %q", tc.args, out)
			}
			if !strings.Contains(errOut, `"diagnostic":"MM_USAGE"`) {
				t.Errorf("args %v: expected MM_USAGE on stderr, got %q", tc.args, errOut)
			}
		})
	}
}

func TestSoftwareSectionReceiptsOnly(t *testing.T) {
	e := newTestEnv(t)

	// 1. Applied plan with applied receipt -> reported as state="unknown", evidence="recorded".
	e.saveSoftwarePlan(
		"plan-20261021T120000Z-0001",
		"ripgrep",
		"user",
		"2026-10-21T12:00:00Z",
		plan.Applied,
		plan.OutcomeApplied,
		"2026-10-21T12:00:05Z",
	)
	// 2. Newer applied receipt for ripgrep@user supersedes the older one.
	e.saveSoftwarePlan(
		"plan-20261021T130000Z-0002",
		"ripgrep",
		"user",
		"2026-10-21T13:00:00Z",
		plan.Applied,
		plan.OutcomeApplied,
		"2026-10-21T13:00:09Z",
	)
	// 3. Saved plan without receipt -> ignored.
	e.saveSoftwarePlan(
		"plan-20261021T140000Z-0003",
		"fd",
		"user",
		"2026-10-21T14:00:00Z",
		plan.Saved,
		"",
		"",
	)
	// 4. Failed plan with failed receipt -> ignored.
	e.saveSoftwarePlan(
		"plan-20261021T141000Z-0004",
		"bat",
		"user",
		"2026-10-21T14:10:00Z",
		plan.Failed,
		plan.OutcomeFailed,
		"2026-10-21T14:10:02Z",
	)
	// 5. Rejected plan -> ignored.
	e.saveSoftwarePlan(
		"plan-20261021T142000Z-0005",
		"jq",
		"user",
		"2026-10-21T14:20:00Z",
		plan.Rejected,
		plan.OutcomeRejected,
		"2026-10-21T14:20:02Z",
	)
	// 6. Applied global receipt for jq.
	e.saveSoftwarePlan(
		"plan-20261021T143000Z-0006",
		"jq",
		"global",
		"2026-10-21T14:30:00Z",
		plan.Applied,
		plan.OutcomeApplied,
		"2026-10-21T14:30:04Z",
	)

	// Default JSON Lines mode.
	out, errOut, code := e.run("--section", "software")
	if code != 0 || errOut != "" {
		t.Fatalf("status --section software: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 software records (jq@global and ripgrep@user), got %d:\n%s", len(lines), out)
	}
	wantJQ := `{"section":"software","resource":"jq","target":"global","state":"unknown","facts":{"receipt":"plan-20261021T143000Z-0006"},"evidence":"recorded","at":"2026-10-21T14:30:04Z"}`
	wantRG := `{"section":"software","resource":"ripgrep","target":"user","state":"unknown","facts":{"receipt":"plan-20261021T130000Z-0002"},"evidence":"recorded","at":"2026-10-21T13:00:09Z"}`
	if lines[0] != wantJQ {
		t.Errorf("line 0 = %s\nwant     %s", lines[0], wantJQ)
	}
	if lines[1] != wantRG {
		t.Errorf("line 1 = %s\nwant     %s", lines[1], wantRG)
	}
	if strings.Contains(out, `"state":"present"`) {
		t.Fatal("recorded receipt must never be rendered as present")
	}

	// Filter by name.
	outRG, errOut, code := e.run("--section", "software", "ripgrep")
	if code != 0 || errOut != "" || strings.TrimSpace(outRG) != wantRG {
		t.Fatalf("status --section software ripgrep: exit=%d out=%q err=%q", code, outRG, errOut)
	}

	// Known catalog software without an applied receipt for --user returns 0 records and exit 0.
	outJQUser, errOut, code := e.run("--section", "software", "jq", "--user")
	if code != 0 || errOut != "" || outJQUser != "" {
		t.Fatalf("status --section software jq --user: exit=%d out=%q err=%q", code, outJQUser, errOut)
	}

	// --json mode preserves key order inside the JSON array.
	outJSON, errOut, code := e.run("--section", "software", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("status --section software --json: exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(outJSON, `"records": [`) || !strings.Contains(outJSON, `"section": "software"`) {
		t.Errorf("unexpected --json output:\n%s", outJSON)
	}

	// --human mode renders table headers in exact D7 order.
	outHuman, errOut, code := e.run("--section", "software", "--human")
	if code != 0 || errOut != "" {
		t.Fatalf("status --section software --human: exit=%d err=%q", code, errOut)
	}
	header := strings.Split(outHuman, "\n")[0]
	for _, col := range []string{"SECTION", "RESOURCE", "TARGET", "STATE", "FACTS", "EVIDENCE", "AT"} {
		if !strings.Contains(header, col) {
			t.Errorf("human header %q missing column %s", header, col)
		}
	}
}

func TestConfigSectionLedgerAndFreshInspection(t *testing.T) {
	e := newTestEnv(t)
	ledgerPath := filepath.Join(e.stateDir, ledger.FileName)

	// 1. Whole-file config ("gitconfig"): present when digest matches.
	gitPath := filepath.Join(e.homeDir, ".gitconfig")
	gitContent := []byte("[user]\n\tname = tester\n")
	if err := os.WriteFile(gitPath, gitContent, 0o644); err != nil {
		t.Fatal(err)
	}
	gitSum := file.SHA256Hex(gitContent)

	// 2. Managed-block config ("shellrc"): present when block digest matches.
	rcPath := filepath.Join(e.homeDir, ".zshrc")
	blkBytes := block.FormatBlock("mm-env", "#", "export EDITOR=nvim", "\n")
	rcContent := append([]byte("# header\n"), blkBytes...)
	if err := os.WriteFile(rcPath, rcContent, 0o644); err != nil {
		t.Fatal(err)
	}
	blkSum := file.SHA256Hex(blkBytes)

	// 3. Structured JSON config ("settings"): present when key digest matches.
	jsonPath := filepath.Join(e.homeDir, "settings.json")
	if err := os.WriteFile(jsonPath, []byte("{\"editor\":{\"tabSize\":4}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keySum := file.SHA256Hex([]byte("4"))

	if err := ledger.Append(ledgerPath,
		ledger.Record{
			SchemaVersion: ledger.SchemaVersion,
			Kind:          ledger.KindFile,
			Path:          gitPath,
			SHA256:        gitSum,
			Config:        "gitconfig",
			Operation:     "main",
			Target:        "user",
			Account:       "tester",
			Plan:          "plan-20261021T120000Z-0010",
			At:            "2026-10-21T12:00:00Z",
			State:         ledger.Owned,
		},
		ledger.Record{
			SchemaVersion: ledger.SchemaVersion,
			Kind:          ledger.KindBlock,
			Path:          rcPath,
			ID:            "mm-env",
			SHA256:        blkSum,
			Config:        "shellrc",
			Operation:     "env",
			Target:        "user",
			Account:       "tester",
			Plan:          "plan-20261021T120100Z-0011",
			At:            "2026-10-21T12:01:00Z",
			State:         ledger.Owned,
		},
		ledger.Record{
			SchemaVersion: ledger.SchemaVersion,
			Kind:          ledger.KindKey,
			Path:          jsonPath,
			ID:            "editor.tabSize",
			Format:        "json",
			SHA256:        keySum,
			Config:        "settings",
			Operation:     "tabs",
			Target:        "user",
			Account:       "tester",
			Plan:          "plan-20261021T120200Z-0012",
			At:            "2026-10-21T12:02:00Z",
			State:         ledger.Owned,
		},
	); err != nil {
		t.Fatal(err)
	}

	// All three match their ledger digests -> present / observed.
	out, errOut, code := e.run("--section", "config")
	if code != 0 || errOut != "" {
		t.Fatalf("status --section config: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, out)
	for _, cfgID := range []string{"gitconfig", "settings", "shellrc"} {
		if !strings.Contains(out, fmt.Sprintf(`"section":"config","resource":%q,"target":"user","state":"present"`, cfgID)) {
			t.Errorf("expected %s to be present/observed, got:\n%s", cfgID, out)
		}
	}
	if strings.Contains(out, `"evidence":"recorded"`) {
		t.Errorf("expected observed evidence for matching files, got:\n%s", out)
	}

	// Modify gitconfig on disk, delete block from rcPath, and change key in settings.json -> all become absent / observed.
	if err := os.WriteFile(gitPath, []byte("modified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rcPath, []byte("# header only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsonPath, []byte("{\"editor\":{\"tabSize\":2}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = e.run("--section", "config")
	if code != 0 || errOut != "" {
		t.Fatalf("status after modification: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, out)
	for _, cfgID := range []string{"gitconfig", "settings", "shellrc"} {
		if !strings.Contains(out, fmt.Sprintf(`"section":"config","resource":%q,"target":"user","state":"absent"`, cfgID)) ||
			!strings.Contains(out, `"evidence":"observed"`) {
			t.Errorf("expected %s to be absent/observed after modification, got:\n%s", cfgID, out)
		}
	}

	// Delete gitPath entirely -> still absent / observed.
	if err := os.Remove(gitPath); err != nil {
		t.Fatal(err)
	}
	outGit, errOut, code := e.run("--section", "config", "gitconfig")
	if code != 0 || errOut != "" {
		t.Fatalf("status after delete: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, outGit)
	if !strings.Contains(outGit, `"state":"absent"`) || !strings.Contains(outGit, `"evidence":"observed"`) {
		t.Errorf("expected absent/observed after deletion, got: %s", outGit)
	}

	// Replace gitPath with a directory (portable unreadable file across Darwin, Linux and Windows without chmod)
	// -> state="unknown", evidence="recorded" (unknown is never rendered as absent).
	if err := os.MkdirAll(gitPath, 0o755); err != nil {
		t.Fatal(err)
	}
	outUnreadable, errOut, code := e.run("--section", "config", "gitconfig")
	if code != 0 || errOut != "" {
		t.Fatalf("status on unreadable destination: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, outUnreadable)
	if !strings.Contains(outUnreadable, `"state":"unknown"`) || !strings.Contains(outUnreadable, `"evidence":"recorded"`) {
		t.Errorf("expected unknown/recorded on unreadable destination, got: %s", outUnreadable)
	}
}

func TestAbsentCustomRootCreatesNothing(t *testing.T) {
	e := newTestEnv(t)
	absentRoot := filepath.Join(e.dir, "nonexistent-custom-root")

	out, errOut, code := e.run("--section", "config", "--custom", absentRoot)
	if code != 0 || errOut != "" {
		t.Fatalf("status absent custom root: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, out)
	wantTarget := "custom:" + absentRoot
	var rec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rec); err != nil {
		t.Fatalf("unmarshal record: %v (%s)", err, out)
	}
	if rec["section"] != "config" || rec["resource"] != "root" || rec["target"] != wantTarget || rec["state"] != "absent" || rec["evidence"] != "observed" {
		t.Errorf("unexpected absent custom root record: %+v", rec)
	}
	if _, err := os.Stat(absentRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created absent custom root %s (err=%v)", absentRoot, err)
	}
}

func TestFlavorSectionComposition(t *testing.T) {
	e := newTestEnv(t)
	ledgerPath := filepath.Join(e.stateDir, ledger.FileName)

	gitPath := filepath.Join(e.homeDir, ".gitconfig")
	gitContent := []byte("[user]\n\tname = tester\n")
	if err := os.WriteFile(gitPath, gitContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(ledgerPath, ledger.Record{
		SchemaVersion: ledger.SchemaVersion,
		Kind:          ledger.KindFile,
		Path:          gitPath,
		SHA256:        file.SHA256Hex(gitContent),
		Config:        "gitconfig",
		Operation:     "main",
		Target:        "user",
		Account:       "tester",
		Plan:          "plan-20261021T120000Z-0020",
		At:            "2026-10-21T12:00:00Z",
		State:         ledger.Owned,
	}); err != nil {
		t.Fatal(err)
	}
	e.saveSoftwarePlan(
		"plan-20261021T120500Z-0021",
		"ripgrep",
		"user",
		"2026-10-21T12:05:00Z",
		plan.Applied,
		plan.OutcomeApplied,
		"2026-10-21T12:05:03Z",
	)

	// Flavor 1 ("cfg-only"): only config members, all present on disk -> present/observed.
	// Flavor 2 ("full-dev"): software + config -> unknown/recorded (because software receipt is never present).
	e.flavors = []compose.Flavor{
		{
			SchemaVersion: compose.SchemaVersion,
			ID:            "cfg-only",
			Description:   "Config-only flavor",
			Config:        []compose.ConfigEntry{{ID: "gitconfig", Target: "user"}},
		},
		{
			SchemaVersion: compose.SchemaVersion,
			ID:            "full-dev",
			Description:   "Software and config flavor",
			Software:      []compose.SoftwareEntry{{ID: "ripgrep", Target: "user", Version: "14.1.0"}},
			Config:        []compose.ConfigEntry{{ID: "gitconfig", Target: "user"}},
		},
	}

	out, errOut, code := e.run("--section", "flavor", "cfg-only")
	if code != 0 || errOut != "" {
		t.Fatalf("flavor cfg-only: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, out)
	if !strings.Contains(out, `"section":"flavor","resource":"cfg-only","target":"user","state":"present"`) ||
		!strings.Contains(out, `"evidence":"observed"`) {
		t.Errorf("expected cfg-only to be present/observed, got: %s", out)
	}

	outFull, errOut, code := e.run("--section", "flavor", "full-dev")
	if code != 0 || errOut != "" {
		t.Fatalf("flavor full-dev: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, outFull)
	if !strings.Contains(outFull, `"section":"flavor","resource":"full-dev","target":"user","state":"unknown"`) ||
		!strings.Contains(outFull, `"evidence":"recorded"`) {
		t.Errorf("expected full-dev to be unknown/recorded, got: %s", outFull)
	}

	// Removing gitPath makes cfg-only become absent/observed.
	if err := os.Remove(gitPath); err != nil {
		t.Fatal(err)
	}
	outAbsent, errOut, code := e.run("--section", "flavor", "cfg-only")
	if code != 0 || errOut != "" {
		t.Fatalf("flavor cfg-only absent: exit=%d err=%q", code, errOut)
	}
	if !strings.Contains(outAbsent, `"state":"absent"`) || !strings.Contains(outAbsent, `"evidence":"observed"`) {
		t.Errorf("expected cfg-only to be absent/observed after file removal, got: %s", outAbsent)
	}

	// Loading flavors from config catalog FS via compose.Load when Deps.Flavors is nil.
	e.flavors = nil
	fsys := fstest.MapFS{
		"workstation.yaml": &fstest.MapFile{
			Data: []byte("kind: flavor\nschema_version: 1\nid: workstation\ndescription: Workstation\nconfig:\n  - id: gitconfig\n    target: user\n"),
		},
	}
	e.configs[0].FS = fsys
	outFS, errOut, code := e.run("--section", "flavor", "workstation")
	if code != 0 || errOut != "" {
		t.Fatalf("flavor from FS: exit=%d err=%q", code, errOut)
	}
	assertKeyOrder(t, outFS)
	if !strings.Contains(outFS, `"resource":"workstation"`) {
		t.Errorf("expected workstation flavor record, got: %s", outFS)
	}
}

func TestUnreadableLedgerPartialExit4(t *testing.T) {
	e := newTestEnv(t)
	e.saveSoftwarePlan(
		"plan-20261021T120000Z-0030",
		"ripgrep",
		"user",
		"2026-10-21T12:00:00Z",
		plan.Applied,
		plan.OutcomeApplied,
		"2026-10-21T12:00:02Z",
	)
	e.flavors = []compose.Flavor{{
		SchemaVersion: compose.SchemaVersion,
		ID:            "workstation",
		Description:   "Workstation",
		Config:        []compose.ConfigEntry{{ID: "gitconfig", Target: "user"}},
	}}

	ledgerPath := filepath.Join(e.stateDir, ledger.FileName)

	// Case 1: Corrupt ledger file -> exits 4 with MM_STATUS_PARTIAL while software and flavor sections still print.
	if err := os.WriteFile(ledgerPath, []byte("{corrupt-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := e.run()
	if code != diag.ExitIncomplete {
		t.Fatalf("status with corrupt ledger: exit = %d, want %d; stdout=%q stderr=%q", code, diag.ExitIncomplete, out, errOut)
	}
	if !strings.Contains(errOut, `"diagnostic":"MM_STATUS_PARTIAL"`) {
		t.Errorf("expected MM_STATUS_PARTIAL on stderr, got: %s", errOut)
	}
	assertKeyOrder(t, out)
	if !strings.Contains(out, `"section":"software","resource":"ripgrep"`) {
		t.Errorf("expected software section to still print on partial report, got:\n%s", out)
	}
	if !strings.Contains(out, `"section":"flavor","resource":"workstation"`) {
		t.Errorf("expected flavor section to still print on partial report, got:\n%s", out)
	}

	// Case 2: Ledger path replaced by a directory (portable unreadable file on Darwin, Linux and Windows) -> exits 4 with MM_STATUS_PARTIAL.
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(ledgerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	outCfg, errOutCfg, codeCfg := e.run("--section", "config")
	if codeCfg != diag.ExitIncomplete || !strings.Contains(errOutCfg, `"diagnostic":"MM_STATUS_PARTIAL"`) {
		t.Fatalf("status --section config with directory ledger: exit=%d out=%q err=%q", codeCfg, outCfg, errOutCfg)
	}
}

func TestNoLockNoWriteNoProcessGuards(t *testing.T) {
	e := newTestEnv(t)
	gitPath := filepath.Join(e.homeDir, ".gitconfig")
	gitContent := []byte("[user]\n\tname = tester\n")
	if err := os.WriteFile(gitPath, gitContent, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Append(filepath.Join(e.stateDir, ledger.FileName), ledger.Record{
		SchemaVersion: ledger.SchemaVersion,
		Kind:          ledger.KindFile,
		Path:          gitPath,
		SHA256:        file.SHA256Hex(gitContent),
		Config:        "gitconfig",
		Operation:     "main",
		Target:        "user",
		Account:       "tester",
		Plan:          "plan-20261021T120000Z-0040",
		At:            "2026-10-21T12:00:00Z",
		State:         ledger.Owned,
	}); err != nil {
		t.Fatal(err)
	}
	e.saveSoftwarePlan(
		"plan-20261021T120000Z-0041",
		"ripgrep",
		"user",
		"2026-10-21T12:00:00Z",
		plan.Applied,
		plan.OutcomeApplied,
		"2026-10-21T12:00:05Z",
	)
	e.flavors = []compose.Flavor{{
		SchemaVersion: compose.SchemaVersion,
		ID:            "workstation",
		Description:   "Workstation",
		Software:      []compose.SoftwareEntry{{ID: "ripgrep", Target: "user"}},
		Config:        []compose.ConfigEntry{{ID: "gitconfig", Target: "user"}},
	}}

	// Hold the plan-store lock open while status runs to prove status never acquires .lock.
	heldStore, err := plan.Open(e.stateDir, func() time.Time { return e.now })
	if err != nil {
		t.Fatal(err)
	}
	defer heldStore.Close()

	absentCustom := filepath.Join(e.dir, "absent-soul")
	beforeState := snapshotDir(t, e.stateDir)
	beforeHome := snapshotDir(t, e.homeDir)
	beforeCustom := snapshotDir(t, absentCustom)

	invocations := [][]string{
		{"--section", "software"},
		{"--section", "software", "ripgrep", "--json"},
		{"--section", "config"},
		{"--section", "config", "gitconfig", "--human"},
		{"--section", "config", "--custom", absentCustom},
		{"--section", "flavor"},
		{"--section", "flavor", "workstation"},
		{"-h"},
	}
	for _, args := range invocations {
		out, errOut, code := e.run(args...)
		if code != 0 || errOut != "" || out == "" {
			t.Fatalf("args %v: exit=%d out=%q err=%q", args, code, out, errOut)
		}
	}

	afterState := snapshotDir(t, e.stateDir)
	afterHome := snapshotDir(t, e.homeDir)
	afterCustom := snapshotDir(t, absentCustom)

	if beforeState != afterState {
		t.Errorf("stateDir mutated during status!\nbefore:\n%s\nafter:\n%s", beforeState, afterState)
	}
	if beforeHome != afterHome {
		t.Errorf("homeDir mutated during status!\nbefore:\n%s\nafter:\n%s", beforeHome, afterHome)
	}
	if beforeCustom != afterCustom {
		t.Errorf("absentCustom mutated during status: got %s, want %s", afterCustom, beforeCustom)
	}
}
