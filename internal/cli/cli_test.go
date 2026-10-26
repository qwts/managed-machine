package cli

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/target"
)

var update = flag.Bool("update", false, "rewrite the golden files from the current output")

// synthetic exercises exits 1, 3 and 4 and records whether it ran, so the
// tests can prove help precedence causes no action. It claims no real
// check or status support; it exists only in this test file.
type synthetic struct{ ran int }

func (s *synthetic) command() Command {
	return Command{
		Name:    "synthetic",
		Summary: "test-only outcomes",
		Usage:   "Usage: managed-machine synthetic <outcome>\n",
		Run: func(ctx Context, args []string) Result {
			s.ran++
			rec := output.New().Set("id", "zsh").Set("operation", "activation-file").Set("target", "user")
			if len(args) != 1 {
				d := diag.New(diag.Usage, "one outcome is required")
				return Result{Diagnostic: &d}
			}
			switch args[0] {
			case "ok":
				return Result{Records: []*output.Record{rec.Set("state", "applied")}}
			case "failure":
				d := diag.New(diag.ConfigConflict, "replace-file /etc/zshrc owned by another tool")
				return Result{Diagnostic: &d}
			case "findings":
				d := diag.New(diag.ConfigDriftFound, "1 difference")
				return Result{Records: []*output.Record{rec.Set("drift", "theme").Set("detail", "actual=minimal desired=powerline")}, Diagnostic: &d}
			case "incomplete":
				d := diag.New(diag.StatusPartial, "observer brew failed")
				return Result{Records: []*output.Record{rec.Set("state", "unknown")}, Diagnostic: &d}
			}
			d := diag.New(diag.Usage, "unknown outcome "+args[0])
			return Result{Diagnostic: &d}
		},
	}
}

func newApp(detect func() (target.Host, error)) (*App, *synthetic, *bytes.Buffer, *bytes.Buffer) {
	var stdout, stderr bytes.Buffer
	s := &synthetic{}
	app := &App{Name: "managed-machine", Commands: []Command{Platform(), s.command()}, Stdout: &stdout, Stderr: &stderr, Detect: detect}
	return app, s, &stdout, &stderr
}

func fixedHost(t target.Target, emulated bool) func() (target.Host, error) {
	return func() (target.Host, error) { return target.Host{Target: t, Emulated: emulated}, nil }
}

func golden(t *testing.T, name string, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update to record): %v", path, err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden\n--- got ---\n%s--- want ---\n%s", name, got, want)
	}
}

func TestPlatformMapsEveryTargetWithoutChangingSpelling(t *testing.T) {
	for _, tgt := range target.All() {
		app, _, stdout, stderr := newApp(fixedHost(tgt, false))
		if code := app.Run([]string{"platform"}); code != 0 {
			t.Fatalf("%s: exit %d, stderr %q", tgt, code, stderr.String())
		}
		parts := strings.SplitN(string(tgt), "/", 2)
		want := `{"platform":"` + string(tgt) + `","os":"` + parts[0] + `","arch":"` + parts[1] + `","emulated":false}` + "\n"
		if stdout.String() != want {
			t.Errorf("%s: stdout %q, want %q", tgt, stdout.String(), want)
		}
		if stderr.Len() != 0 {
			t.Errorf("%s: stderr not empty: %q", tgt, stderr.String())
		}
	}
}

func TestPlatformGoldens(t *testing.T) {
	cases := []struct {
		name string
		args []string
		host func() (target.Host, error)
		exit int
	}{
		{"platform-compact", []string{"platform"}, fixedHost(target.WindowsARM64, true), 0},
		{"platform-json", []string{"platform", "--json"}, fixedHost(target.LinuxAMD64, false), 0},
		{"platform-human", []string{"--human", "platform"}, fixedHost(target.DarwinARM64, false), 0},
		{"platform-help", []string{"platform", "-h"}, fixedHost(target.DarwinARM64, false), 0},
		{"platform-unsupported", []string{"platform"}, func() (target.Host, error) {
			return target.FromFacts(target.Facts{OS: "darwin", NativeArch: "x86_64"})
		}, 1},
		{"platform-probe-failed-human", []string{"platform", "--human"}, func() (target.Host, error) {
			return target.Host{}, errors.New("uname -m: executable file not found in $PATH")
		}, 1},
		{"platform-extra-argument", []string{"platform", "extra"}, fixedHost(target.DarwinARM64, false), 2},
		{"help", []string{"-h"}, fixedHost(target.DarwinARM64, false), 0},
		{"conflicting-modes", []string{"platform", "--json", "--human"}, fixedHost(target.DarwinARM64, false), 2},
		{"no-command", nil, fixedHost(target.DarwinARM64, false), 2},
		{"unknown-command", []string{"status"}, fixedHost(target.DarwinARM64, false), 2},
		{"unknown-command-human", []string{"--human", "status"}, fixedHost(target.DarwinARM64, false), 2},
		{"unknown-option", []string{"--verbose", "platform"}, fixedHost(target.DarwinARM64, false), 2},
		{"synthetic-ok", []string{"synthetic", "ok"}, fixedHost(target.DarwinARM64, false), 0},
		{"synthetic-failure", []string{"synthetic", "failure"}, fixedHost(target.DarwinARM64, false), 1},
		{"synthetic-findings", []string{"synthetic", "findings"}, fixedHost(target.DarwinARM64, false), 3},
		{"synthetic-findings-json", []string{"synthetic", "findings", "--json"}, fixedHost(target.DarwinARM64, false), 3},
		{"synthetic-incomplete-human", []string{"synthetic", "incomplete", "--human"}, fixedHost(target.DarwinARM64, false), 4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app, _, stdout, stderr := newApp(c.host)
			code := app.Run(c.args)
			if code != c.exit {
				t.Errorf("exit %d, want %d (stderr %q)", code, c.exit, stderr.String())
			}
			golden(t, c.name, "exit "+itoa(code)+"\n--- stdout ---\n"+stdout.String()+"--- stderr ---\n"+stderr.String())
		})
	}
}

func TestStreamsAreSeparated(t *testing.T) {
	app, _, stdout, stderr := newApp(fixedHost(target.DarwinARM64, false))
	app.Run([]string{"synthetic", "findings"})
	if strings.Contains(stdout.String(), "diagnostic") {
		t.Errorf("diagnostic leaked to stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), `"id"`) {
		t.Errorf("record leaked to stderr: %q", stderr.String())
	}
	for _, line := range strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n") {
		if !strings.HasPrefix(line, `{"diagnostic":"MM_`) {
			t.Errorf("stderr line is not a diagnostic object: %q", line)
		}
	}
}

func TestHelpTakesPrecedenceAndCausesNoAction(t *testing.T) {
	for _, args := range [][]string{
		{"synthetic", "failure", "-h"},
		{"-h", "synthetic", "failure"},
		{"synthetic", "--help", "incomplete", "--json", "--human"},
		{"--json", "--human", "-h"},
		{"--verbose", "-h"},
	} {
		app, s, stdout, stderr := newApp(fixedHost(target.DarwinARM64, false))
		code := app.Run(args)
		if code != 0 {
			t.Errorf("%v: exit %d", args, code)
		}
		if s.ran != 0 {
			t.Errorf("%v: command ran %d time(s) under -h", args, s.ran)
		}
		if stderr.Len() != 0 {
			t.Errorf("%v: stderr %q", args, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "Usage: managed-machine") {
			t.Errorf("%v: stdout %q", args, stdout.String())
		}
	}
}

func TestPresentationFlagsDoNotChangeWhatIsComputed(t *testing.T) {
	// Drive the real App with each presentation flag and compare what the
	// command computed, not how it was rendered.
	var computed []string
	for _, flags := range [][]string{nil, {"--json"}, {"--human"}} {
		app, s, stdout, stderr := newApp(fixedHost(target.DarwinARM64, false))
		app.Commands[1].Run = func(ctx Context, args []string) Result {
			res := s.command().Run(ctx, args)
			var b bytes.Buffer
			_ = output.WriteCompact(&b, res.Records)
			computed = append(computed, b.String()+"|"+strings.Join(args, " "))
			return res
		}
		code := app.Run(append([]string{"synthetic", "findings"}, flags...))
		if code != 3 || stdout.Len() == 0 || stderr.Len() == 0 {
			t.Errorf("%v: exit %d, stdout %d bytes, stderr %d bytes", flags, code, stdout.Len(), stderr.Len())
		}
	}
	if len(computed) != 3 || computed[0] != computed[1] || computed[1] != computed[2] {
		t.Errorf("records or arguments differ between presentation modes: %q", computed)
	}
}

func TestUnknownOptionIsReportedInTheRequestedMode(t *testing.T) {
	app, _, _, stderr := newApp(fixedHost(target.DarwinARM64, false))
	if code := app.Run([]string{"--verbose", "--human", "platform"}); code != 2 {
		t.Errorf("exit %d", code)
	}
	if stderr.String() != "MM_USAGE: unknown option \"--verbose\"\n" {
		t.Errorf("stderr %q", stderr.String())
	}
	app, _, _, stderr = newApp(fixedHost(target.DarwinARM64, false))
	app.Run([]string{"--verbose", "--json", "--human", "platform"})
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("conflicting modes not reported first: %q", stderr.String())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestOutputFailureIsAnOutputDiagnostic(t *testing.T) {
	var stderr bytes.Buffer
	s := &synthetic{}
	app := &App{Name: "managed-machine", Commands: []Command{s.command()}, Stdout: failingWriter{}, Stderr: &stderr, Detect: fixedHost(target.DarwinARM64, false)}
	if code := app.Run([]string{"synthetic", "ok"}); code != 1 {
		t.Errorf("exit %d", code)
	}
	if !strings.HasPrefix(stderr.String(), `{"diagnostic":"MM_OUTPUT_WRITE_FAILED"`) {
		t.Errorf("stderr %q", stderr.String())
	}
}

func TestVersionFlagPrecedenceAndMisuse(t *testing.T) {
	probed := 0
	failingProbe := func() (target.Host, error) {
		probed++
		return target.Host{}, errors.New("probe must not run for --version")
	}

	// Unflagged build reports 0.0.0-dev without probing.
	app, s, stdout, stderr := newApp(failingProbe)
	if code := app.Run([]string{"--version"}); code != 0 {
		t.Fatalf("unflagged --version: exit %d, stderr %q", code, stderr.String())
	}
	if stdout.String() != "0.0.0-dev\n" {
		t.Errorf("unflagged --version: stdout %q, want %q", stdout.String(), "0.0.0-dev\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("unflagged --version: stderr %q", stderr.String())
	}
	if probed != 0 || s.ran != 0 {
		t.Errorf("unflagged --version: probed=%d, ran=%d", probed, s.ran)
	}

	// Embedded version prints <version>\n and runs no probe.
	app, s, stdout, stderr = newApp(failingProbe)
	app.Version = "0.9.0"
	if code := app.Run([]string{"--version"}); code != 0 {
		t.Fatalf("flagged --version: exit %d, stderr %q", code, stderr.String())
	}
	if stdout.String() != "0.9.0\n" {
		t.Errorf("flagged --version: stdout %q, want %q", stdout.String(), "0.9.0\n")
	}
	if stderr.Len() != 0 {
		t.Errorf("flagged --version: stderr %q", stderr.String())
	}
	if probed != 0 || s.ran != 0 {
		t.Errorf("flagged --version: probed=%d, ran=%d", probed, s.ran)
	}

	// Any further argument with --version is MM_USAGE (exit 2) before option parsing or probing.
	for _, args := range [][]string{
		{"--version", "extra"},
		{"--version", "--json"},
		{"--version", "--human"},
		{"--version", "-h"},
		{"-h", "--version"},
		{"--json", "--version"},
		{"--human", "--version"},
		{"platform", "--version"},
	} {
		app, s, stdout, stderr = newApp(failingProbe)
		app.Version = "0.9.0"
		code := app.Run(args)
		if code != 2 {
			t.Errorf("%v: exit %d, want 2 (stderr %q)", args, code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("%v: stdout not empty: %q", args, stdout.String())
		}
		if !strings.HasPrefix(stderr.String(), `{"diagnostic":"MM_USAGE"`) {
			t.Errorf("%v: stderr %q does not start with MM_USAGE diagnostic", args, stderr.String())
		}
		if probed != 0 || s.ran != 0 {
			t.Errorf("%v: probed=%d, ran=%d", args, probed, s.ran)
		}
	}
}

func TestVersionCommandRecordInAllModes(t *testing.T) {
	tc := runtime.Version()
	for _, tgt := range target.All() {
		app, _, stdout, stderr := newApp(fixedHost(tgt, false))
		app.Version = "0.9.0"
		app.Commands = append(app.Commands, Version("0.9.0"))

		// Compact JSON Lines mode (default)
		if code := app.Run([]string{"version"}); code != 0 {
			t.Fatalf("%s compact: exit %d, stderr %q", tgt, code, stderr.String())
		}
		wantCompact := `{"version":"0.9.0","target":"` + string(tgt) + `","toolchain":"` + tc + `"}` + "\n"
		if stdout.String() != wantCompact {
			t.Errorf("%s compact: got %q, want %q", tgt, stdout.String(), wantCompact)
		}
		if stderr.Len() != 0 {
			t.Errorf("%s compact: stderr %q", tgt, stderr.String())
		}

		// --json mode
		stdout.Reset()
		stderr.Reset()
		if code := app.Run([]string{"version", "--json"}); code != 0 {
			t.Fatalf("%s --json: exit %d, stderr %q", tgt, code, stderr.String())
		}
		wantJSON := "{\n" +
			`  "version": "0.9.0",` + "\n" +
			`  "target": "` + string(tgt) + `",` + "\n" +
			`  "toolchain": "` + tc + `"` + "\n" +
			"}\n"
		if stdout.String() != wantJSON {
			t.Errorf("%s --json: got %q, want %q", tgt, stdout.String(), wantJSON)
		}

		// --human mode
		stdout.Reset()
		stderr.Reset()
		if code := app.Run([]string{"version", "--human"}); code != 0 {
			t.Fatalf("%s --human: exit %d, stderr %q", tgt, code, stderr.String())
		}
		lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
		if len(lines) != 2 {
			t.Fatalf("%s --human: got %d lines: %q", tgt, len(lines), stdout.String())
		}
		if got := strings.Fields(lines[0]); len(got) != 3 || got[0] != "VERSION" || got[1] != "TARGET" || got[2] != "TOOLCHAIN" {
			t.Errorf("%s --human header: %q", tgt, lines[0])
		}
		if got := strings.Fields(lines[1]); len(got) != 3 || got[0] != "0.9.0" || got[1] != string(tgt) || got[2] != tc {
			t.Errorf("%s --human row: %q", tgt, lines[1])
		}
	}

	// Unflagged version command defaults to 0.0.0-dev; extra argument is MM_USAGE; -h runs no probe.
	app, _, stdout, stderr := newApp(fixedHost(target.DarwinARM64, false))
	if code := app.Run([]string{"version"}); code != 0 {
		t.Fatalf("unflagged version: exit %d", code)
	}
	if !strings.HasPrefix(stdout.String(), `{"version":"0.0.0-dev","target":"darwin/arm64","toolchain":`) {
		t.Errorf("unflagged version: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := app.Run([]string{"version", "extra"}); code != 2 {
		t.Errorf("version extra: exit %d, stderr %q", code, stderr.String())
	}
	if !strings.HasPrefix(stderr.String(), `{"diagnostic":"MM_USAGE"`) {
		t.Errorf("version extra: stderr %q", stderr.String())
	}

	probed := 0
	app, _, stdout, stderr = newApp(func() (target.Host, error) {
		probed++
		return target.Host{}, errors.New("probe must not run under -h")
	})
	if code := app.Run([]string{"version", "-h"}); code != 0 || probed != 0 || stderr.Len() != 0 {
		t.Errorf("version -h: exit %d, probed %d, stderr %q", code, probed, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "Usage: managed-machine version") {
		t.Errorf("version -h: stdout %q", stdout.String())
	}
}

func TestSetupHelpServedByCore(t *testing.T) {
	legacyRoot := t.TempDir()
	legacyBin := filepath.Join(legacyRoot, "bin", "managed-machine")
	if err := os.MkdirAll(filepath.Dir(legacyBin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"setup", "--help"},
		{"setup", "-h"},
		{"--help", "setup"},
		{"-h", "setup"},
	} {
		probed := 0
		execCalls := 0
		app, s, stdout, stderr := newApp(func() (target.Host, error) {
			probed++
			return target.Host{Target: target.DarwinARM64}, nil
		})
		app.Getenv = func(k string) string {
			if k == LegacyRootEnv {
				return legacyRoot
			}
			return ""
		}
		app.Exec = func(argv0 string, argv, envv []string) error {
			execCalls++
			return nil
		}
		code := app.Run(args)
		if code != 0 {
			t.Errorf("%v: exit=%d, want 0 (stderr=%q)", args, code, stderr.String())
		}
		if probed != 0 || s.ran != 0 || execCalls != 0 {
			t.Errorf("%v: probed=%d ran=%d execCalls=%d, want 0", args, probed, s.ran, execCalls)
		}
		if stderr.Len() != 0 {
			t.Errorf("%v: stderr=%q, want empty", args, stderr.String())
		}
		if !strings.HasPrefix(stdout.String(), "Usage: managed-machine <command> [options]") {
			t.Errorf("%v: stdout=%q, want core help", args, stdout.String())
		}
	}
}

func TestDarwinLegacyExecSeamAndTreeMissing(t *testing.T) {
	base := t.TempDir()
	legacyRoot := filepath.Join(base, "legacy")
	legacyEntry := filepath.Join(legacyRoot, "bin", "managed-machine")
	if err := os.MkdirAll(filepath.Dir(legacyEntry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyEntry, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	survivingArgs := [][]string{
		{"setup", "--list"},
		{"setup", "alpha"},
		{"catalog", "list", "--no-refresh"},
		{"adopt", "vscode"},
		{"adopt", "--help"},
		{"add-agent", "claude"},
		{"add-agent", "--help"},
		{"account", "doctor", "qwts-claude-agent", "--json"},
		{"ssh", "status", "--json"},
		{"--bootstrap", "--non-interactive"},
		{"--update"},
		{"fleet", "remove", "sha256-x", "--yes"},
		{"status", "--section", "bootstrap"},
		{"status", "--section", "update", "--json"},
	}

	// Populate a darwin checkout on disk under homeWithCheckout so the core's
	// own BridgeDarwinEnv is active, and verify that Exec still receives the
	// exact original environment slice without MANAGED_MACHINE_POLICY_DIR or
	// MANAGED_MACHINE_SOURCE_DIR injected.
	homeWithCheckout := filepath.Join(base, "home-with-checkout")
	if err := os.MkdirAll(filepath.Join(homeWithCheckout, ".local", "share", "managed-machine", "managed-machine-config", "software"), 0o755); err != nil {
		t.Fatal(err)
	}
	wantEnv := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + homeWithCheckout,
		"MANAGED_MACHINE_LEGACY_ROOT=" + legacyRoot,
		"CUSTOM_VAR=kept-verbatim",
	}

	// 1. On darwin/arm64 with MANAGED_MACHINE_LEGACY_ROOT set to a valid tree,
	// every surviving legacy route invokes Exec with the original argv and
	// original environment, without running Detect.
	for _, args := range survivingArgs {
		var gotArgv0 string
		var gotArgv, gotEnvv []string
		execCalls := 0
		probed := 0
		app, _, stdout, stderr := newApp(func() (target.Host, error) {
			probed++
			return target.Host{}, errors.New("probe must not run on legacy route")
		})
		app.GOOS = "darwin"
		app.Getenv = func(k string) string {
			switch k {
			case LegacyRootEnv:
				return legacyRoot
			case "HOME":
				return homeWithCheckout
			}
			return ""
		}
		app.Environ = func() []string {
			return append([]string(nil), wantEnv...)
		}
		app.Exec = func(argv0 string, argv, envv []string) error {
			execCalls++
			gotArgv0 = argv0
			gotArgv = append([]string(nil), argv...)
			gotEnvv = append([]string(nil), envv...)
			return nil
		}
		code := app.Run(args)
		if code != 0 || stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("darwin exec %v: code=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
		if probed != 0 {
			t.Errorf("darwin exec %v: Detect called %d time(s), want 0", args, probed)
		}
		if execCalls != 1 {
			t.Fatalf("darwin exec %v: execCalls=%d, want 1", args, execCalls)
		}
		if gotArgv0 != legacyEntry {
			t.Errorf("darwin exec %v: argv0=%q, want %q", args, gotArgv0, legacyEntry)
		}
		wantArgv := append([]string{legacyEntry}, args...)
		if strings.Join(gotArgv, "\x00") != strings.Join(wantArgv, "\x00") {
			t.Errorf("darwin exec %v: argv=%v, want %v", args, gotArgv, wantArgv)
		}
		if strings.Join(gotEnvv, "\x00") != strings.Join(wantEnv, "\x00") {
			t.Errorf("darwin exec %v: envv=%v, want exact original %v", args, gotEnvv, wantEnv)
		}
	}

	// 2. Missing or non-executable <legacy root>/bin/managed-machine fails with
	// MM_LEGACY_TREE_MISSING (exit 1) naming the entry and brew reinstall managed-machine.
	missingRoot := filepath.Join(base, "no-such-legacy")
	nonExecRoot := filepath.Join(base, "nonexec-legacy")
	nonExecEntry := filepath.Join(nonExecRoot, "bin", "managed-machine")
	if err := os.MkdirAll(filepath.Dir(nonExecEntry), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nonExecEntry, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	badRoots := []string{missingRoot}
	if runtime.GOOS != "windows" {
		badRoots = append(badRoots, nonExecRoot)
	}
	for _, badRoot := range badRoots {
		wantEntry := filepath.Join(badRoot, "bin", "managed-machine")
		execCalls := 0
		probed := 0
		app, _, stdout, stderr := newApp(func() (target.Host, error) {
			probed++
			return target.Host{}, errors.New("probe must not run on legacy route")
		})
		app.GOOS = "darwin"
		app.Getenv = func(k string) string {
			if k == LegacyRootEnv {
				return badRoot
			}
			return ""
		}
		app.Exec = func(string, []string, []string) error {
			execCalls++
			return nil
		}
		code := app.Run([]string{"status", "--section", "bootstrap"})
		if code != 1 || stdout.Len() != 0 || execCalls != 0 || probed != 0 {
			t.Fatalf("bad legacy root %s: code=%d stdout=%q execCalls=%d probed=%d", badRoot, code, stdout.String(), execCalls, probed)
		}
		if !strings.Contains(stderr.String(), `"diagnostic":"MM_LEGACY_TREE_MISSING"`) ||
			!strings.Contains(stderr.String(), wantEntry) ||
			!strings.Contains(stderr.String(), "brew reinstall managed-machine") {
			t.Errorf("bad legacy root %s: stderr=%q", badRoot, stderr.String())
		}
	}

	// 3. On non-darwin targets, surviving routes refuse with MM_LEGACY_ROUTE_UNSUPPORTED
	// even when MANAGED_MACHINE_LEGACY_ROOT is set, without calling Detect.
	for _, tgt := range []target.Target{target.LinuxAMD64, target.LinuxARM64, target.WindowsAMD64, target.WindowsARM64} {
		execCalls := 0
		probed := 0
		app, _, stdout, stderr := newApp(func() (target.Host, error) {
			probed++
			return target.Host{}, errors.New("probe must not run on legacy route")
		})
		app.GOOS = string(tgt)
		app.Getenv = func(k string) string {
			if k == LegacyRootEnv {
				return legacyRoot
			}
			return ""
		}
		app.Exec = func(string, []string, []string) error {
			execCalls++
			return nil
		}
		code := app.Run([]string{"status", "--section", "bootstrap"})
		if code != 2 || stdout.Len() != 0 || execCalls != 0 || probed != 0 || !strings.Contains(stderr.String(), `"diagnostic":"MM_LEGACY_ROUTE_UNSUPPORTED"`) {
			t.Errorf("%s: code=%d execCalls=%d probed=%d stderr=%q", tgt, code, execCalls, probed, stderr.String())
		}
	}

	// 4. Removed routes emit MM_LEGACY_ROUTE_REMOVED (exit 2) on darwin even when
	// MANAGED_MACHINE_LEGACY_ROOT is set and executable, without calling Detect.
	for _, r := range legacy.Routes {
		if r.RemovedIn == "" {
			continue
		}
		var args []string
		switch {
		case r.Section != "":
			args = []string{"status", "--section", r.Section}
		case r.Subcommand != "":
			args = []string{r.Command, r.Subcommand}
		default:
			args = []string{r.Command}
		}
		execCalls := 0
		probed := 0
		app, _, stdout, stderr := newApp(func() (target.Host, error) {
			probed++
			return target.Host{}, errors.New("probe must not run on removed legacy route")
		})
		app.GOOS = "darwin"
		app.Getenv = func(k string) string {
			if k == LegacyRootEnv {
				return legacyRoot
			}
			return ""
		}
		app.Exec = func(string, []string, []string) error {
			execCalls++
			return nil
		}
		code := app.Run(args)
		if code != 2 || stdout.Len() != 0 || execCalls != 0 || probed != 0 || !strings.Contains(stderr.String(), `"diagnostic":"MM_LEGACY_ROUTE_REMOVED"`) {
			t.Errorf("removed route on darwin %v: code=%d execCalls=%d probed=%d stderr=%q", args, code, execCalls, probed, stderr.String())
		}
	}

	// 5. ResolveLegacyRoot precedence: MANAGED_MACHINE_LEGACY_ROOT > ../libexec/legacy > ../legacy.
	rawRoot := t.TempDir()
	resRoot, err := filepath.EvalSymlinks(rawRoot)
	if err != nil {
		resRoot = rawRoot
	}
	exeFile := filepath.Join(resRoot, "Cellar", "bin", "managed-machine")
	if err := os.MkdirAll(filepath.Dir(exeFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exeFile, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	exeFn := func() (string, error) { return exeFile, nil }
	if got, want := ResolveLegacyRoot(nil, exeFn), filepath.Join(resRoot, "Cellar", "legacy"); got != want {
		t.Errorf("ResolveLegacyRoot fallback = %q, want %q", got, want)
	}
	libexecLegacyDir := filepath.Join(resRoot, "Cellar", "libexec", "legacy")
	if err := os.MkdirAll(libexecLegacyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ResolveLegacyRoot(nil, exeFn); got != libexecLegacyDir {
		t.Errorf("ResolveLegacyRoot libexec/legacy = %q, want %q", got, libexecLegacyDir)
	}
	if got := ResolveLegacyRoot(func(k string) string {
		if k == LegacyRootEnv {
			return legacyRoot
		}
		return ""
	}, exeFn); got != legacyRoot {
		t.Errorf("ResolveLegacyRoot env override = %q, want %q", got, legacyRoot)
	}
}

func TestDarwinPolicyAndSourceDirectoryBridge(t *testing.T) {
	base := t.TempDir()
	emptyHome := filepath.Join(base, "empty-home")
	if err := os.MkdirAll(emptyHome, 0o755); err != nil {
		t.Fatal(err)
	}

	// 1. Unset and checkout directory absent -> both resolve to "" and nothing is created.
	envAbsent := BridgeDarwinEnv("darwin", func(k string) string {
		if k == "HOME" {
			return emptyHome
		}
		return ""
	})
	if got := envAbsent(PolicyDirEnv); got != "" {
		t.Errorf("absent checkout policy = %q, want empty", got)
	}
	if got := envAbsent(SourceDirEnv); got != "" {
		t.Errorf("absent checkout source = %q, want empty", got)
	}
	defaultCheckout := filepath.Join(emptyHome, ".local", "share", "managed-machine", "managed-machine-config")
	if _, err := os.Stat(defaultCheckout); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("bridge created checkout directory: %v", err)
	}

	// 2. Default $HOME/.local/share/managed-machine/managed-machine-config exists without software/.
	if err := os.MkdirAll(defaultCheckout, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := envAbsent(PolicyDirEnv); got != defaultCheckout {
		t.Errorf("home checkout policy = %q, want %q", got, defaultCheckout)
	}
	if got := envAbsent(SourceDirEnv); got != "" {
		t.Errorf("home checkout source without software/ = %q, want empty", got)
	}

	// 3. Default checkout software/ exists -> both resolve; setup cutover uses bridged MANAGED_MACHINE_SOURCE_DIR.
	defaultSoft := filepath.Join(defaultCheckout, "software")
	if err := os.MkdirAll(defaultSoft, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultSoft, "warp.yaml"), []byte("schema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := envAbsent(SourceDirEnv); got != defaultSoft {
		t.Errorf("home checkout source with software/ = %q, want %q", got, defaultSoft)
	}

	var gotSwArgs []string
	var stdout, stderr bytes.Buffer
	app := &App{
		Name:   "managed-machine",
		GOOS:   "darwin",
		Stdout: &stdout,
		Stderr: &stderr,
		Detect: func() (target.Host, error) {
			t.Fatal("Detect must not be called by the darwin policy/source directory bridge")
			return target.Host{}, nil
		},
		Getenv: func(k string) string {
			if k == "HOME" {
				return emptyHome
			}
			return ""
		},
		Commands: []Command{{
			Name: "software",
			Run: func(ctx Context, args []string) Result {
				gotSwArgs = append([]string(nil), args...)
				return Result{}
			},
		}},
	}
	if code := app.Run([]string{"setup", "warp"}); code != 0 {
		t.Fatalf("setup warp on darwin with bridged source dir: exit=%d stderr=%q", code, stderr.String())
	}
	if strings.Join(gotSwArgs, " ") != "warp --install --global" {
		t.Errorf("setup warp routed to software %v, want [warp --install --global]", gotSwArgs)
	}

	// 4. XDG_DATA_HOME precedence over HOME.
	xdgDir := filepath.Join(base, "xdg")
	xdgCheckout := filepath.Join(xdgDir, "managed-machine", "managed-machine-config")
	xdgSoft := filepath.Join(xdgCheckout, "software")
	if err := os.MkdirAll(xdgSoft, 0o755); err != nil {
		t.Fatal(err)
	}
	envXDG := BridgeDarwinEnv("darwin", func(k string) string {
		switch k {
		case "HOME":
			return emptyHome
		case XDGDataHomeEnv:
			return xdgDir
		}
		return ""
	})
	if got := envXDG(PolicyDirEnv); got != xdgCheckout {
		t.Errorf("XDG policy = %q, want %q", got, xdgCheckout)
	}
	if got := envXDG(SourceDirEnv); got != xdgSoft {
		t.Errorf("XDG source = %q, want %q", got, xdgSoft)
	}

	// 5. CONFIG_REPO_ROOT precedence over XDG_DATA_HOME and HOME.
	cfgOverride := filepath.Join(base, "cfg-override")
	cfgSoft := filepath.Join(cfgOverride, "software")
	if err := os.MkdirAll(cfgSoft, 0o755); err != nil {
		t.Fatal(err)
	}
	envCfg := BridgeDarwinEnv("darwin", func(k string) string {
		switch k {
		case "HOME":
			return emptyHome
		case XDGDataHomeEnv:
			return xdgDir
		case ConfigRepoRootEnv:
			return cfgOverride
		}
		return ""
	})
	if got := envCfg(PolicyDirEnv); got != cfgOverride {
		t.Errorf("CONFIG_REPO_ROOT policy = %q, want %q", got, cfgOverride)
	}
	if got := envCfg(SourceDirEnv); got != cfgSoft {
		t.Errorf("CONFIG_REPO_ROOT source = %q, want %q", got, cfgSoft)
	}

	// 6. Explicit MANAGED_MACHINE_POLICY_DIR and MANAGED_MACHINE_SOURCE_DIR win.
	envExplicit := BridgeDarwinEnv("darwin", func(k string) string {
		switch k {
		case "HOME":
			return emptyHome
		case ConfigRepoRootEnv:
			return cfgOverride
		case PolicyDirEnv:
			return "/explicit/policy"
		case SourceDirEnv:
			return "/explicit/source"
		}
		return ""
	})
	if got := envExplicit(PolicyDirEnv); got != "/explicit/policy" {
		t.Errorf("explicit policy = %q, want /explicit/policy", got)
	}
	if got := envExplicit(SourceDirEnv); got != "/explicit/source" {
		t.Errorf("explicit source = %q, want /explicit/source", got)
	}

	// 7. Linux and Windows never bridge CONFIG_REPO_ROOT, XDG_DATA_HOME, or HOME/.local/share.
	for _, nonDarwin := range []string{"linux", "windows", "linux/amd64", "windows/arm64"} {
		envOther := BridgeDarwinEnv(nonDarwin, func(k string) string {
			switch k {
			case "HOME":
				return emptyHome
			case XDGDataHomeEnv:
				return xdgDir
			case ConfigRepoRootEnv:
				return cfgOverride
			}
			return ""
		})
		if got := envOther(PolicyDirEnv); got != "" {
			t.Errorf("%s policy = %q, want empty", nonDarwin, got)
		}
		if got := envOther(SourceDirEnv); got != "" {
			t.Errorf("%s source = %q, want empty", nonDarwin, got)
		}
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
