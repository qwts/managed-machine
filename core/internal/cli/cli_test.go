package cli

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/target"
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
	var compact, asJSON, human []*output.Record
	for _, mode := range []struct {
		flag string
		sink *[]*output.Record
	}{{"", &compact}, {"--json", &asJSON}, {"--human", &human}} {
		s := &synthetic{}
		cmd := s.command()
		args := []string{"findings"}
		_ = mode.flag
		*mode.sink = cmd.Run(Context{Mode: output.Compact}, args).Records
	}
	for _, other := range [][]*output.Record{asJSON, human} {
		var a, b bytes.Buffer
		_ = output.WriteCompact(&a, compact)
		_ = output.WriteCompact(&b, other)
		if a.String() != b.String() {
			t.Errorf("records differ between presentation modes:\n%s\n%s", a.String(), b.String())
		}
	}
}

func itoa(n int) string { return string(rune('0' + n)) }
