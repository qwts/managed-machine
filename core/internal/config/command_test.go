package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/core/internal/catalog"
	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

const goldenPlanID = "plan-20261021T120000Z-3f9c"

type harness struct {
	entries  []Entry
	err      error
	env      map[string]string
	stateDir string
	destPath string
	planID   string
	// reads counts the calls to the catalog: the command reaches the
	// machine only through it.
	reads int
}

func newGoldenHarness(t *testing.T, entries []Entry) *harness {
	t.Helper()
	dir := t.TempDir()
	desiredPath := filepath.Join(dir, "desired.yaml")
	destPath := filepath.Join(dir, "home", ".gitconfig")
	write(t, desiredPath, "target: user\nconfig:\n  gitconfig:\n    destinations:\n      gitconfig: "+filepath.ToSlash(destPath)+"\n")
	return &harness{
		entries:  entries,
		stateDir: filepath.Join(dir, "state"),
		destPath: destPath,
		planID:   goldenPlanID,
		env: map[string]string{
			DesiredEnv: desiredPath,
		},
	}
}

func (h *harness) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	planID := h.planID
	if planID == "" {
		planID = goldenPlanID
	}
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &out,
		Stderr: &errb,
		Detect: func() (target.Host, error) { return target.Host{Target: target.DarwinARM64}, nil },
		Commands: []cli.Command{Command(Deps{
			Catalog: func() ([]Entry, error) {
				h.reads++
				if h.err != nil {
					return nil, h.err
				}
				return h.entries, nil
			},
			Getenv: func(k string) string { return h.env[k] },
			GOOS:   "darwin",
			StateDir: func() (string, error) {
				if h.stateDir != "" {
					return h.stateDir, nil
				}
				return filepath.Join(t.TempDir(), "state"), nil
			},
			Account:   func() (string, error) { return "alice", nil },
			Machine:   func(context.Context) (string, error) { return "machine-1", nil },
			Now:       func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
			NewPlanID: func(time.Time) (string, error) { return planID, nil },
			Writable:  func(string) bool { return true },
		})},
	}
	code := app.Run(append([]string{"config"}, args...))
	return out.String(), errb.String(), code
}

func TestCommandGoldens(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		seedPlan bool
	}{
		{"available", []string{"--available"}, false},
		{"available-human", []string{"--available", "--human"}, false},
		{"available-json", []string{"--available", "--json"}, false},
		{"default", []string{}, false},
		{"help", []string{"-h"}, false},
		{"help-category", []string{"-h", "--category", "shell"}, false},
		{"category", []string{"--category", "shell"}, false},
		{"category-unknown", []string{"--category", "games"}, false},
		{"target-user", []string{"--user"}, false},
		{"target-custom", []string{"--custom", "/souls/bob"}, false},
		{"plan", []string{"gitconfig", "--plan", "--user"}, false},
		{"check", []string{"gitconfig", "--check", "--user"}, false},
		{"status", []string{"gitconfig", "--status", "--user"}, false},
		{"apply", []string{"gitconfig", "--apply", "--user"}, true},
		{"from-plan", []string{"gitconfig", "--apply", "--user", "--from-plan", goldenPlanID}, true},
		{"plan-without-a-name", []string{"--plan", "--user"}, false},
		{"plan-without-a-target", []string{"shell", "--plan"}, false},
		{"conflicting-targets", []string{"shell", "--plan", "--user", "--global"}, false},
		{"available-and-plan", []string{"--available", "--plan", "--user"}, false},
		{"conflicting-actions", []string{"shell", "--plan", "--check", "--user"}, false},
		{"from-plan-without-apply", []string{"shell", "--from-plan", goldenPlanID, "--user"}, false},
		{"two-names", []string{"shell", "gitconfig", "--plan", "--user"}, false},
		{"unknown-option", []string{"--nope"}, false},
	}
	entries := fixtureEntries(t)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newGoldenHarness(t, entries)
			if c.seedPlan {
				if _, errOut, code := h.run(t, "gitconfig", "--plan", "--user"); code != 0 {
					t.Fatalf("seed plan: exit %d %q", code, errOut)
				}
			}
			if c.name == "check" {
				if err := os.MkdirAll(filepath.Dir(h.destPath), 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, h.destPath, "[user]\n\tname = Other\n")
			}
			out, errOut, code := h.run(t, c.args...)
			got := "exit " + itoa(code) + "\n--- stdout\n" + out + "--- stderr\n" + errOut
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("differs from %s:\n%s", path, got)
			}
		})
	}
}

// The listing never reaches the machine: it reads the definitions and
// nothing else. The desired configuration, the plan store and the state
// directory are not seams the command has.
func TestListingChangesNothing(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	local := t.TempDir()
	write(t, filepath.Join(local, "editor.yaml"), "kind: config\nschema_version: 1\nid: editor\ndescription: An editor.\ntargets: [user]\noperations:\n  - id: rc\n    provider: managed-block\n    destination: rc\n    block_id: managed-machine\n    content: text\n")
	env := map[string]string{
		catalog.SourceDirEnv: local,
		statedir.Override:    state,
	}
	before := fixtureFiles(t, local)
	var out, errOut bytes.Buffer
	app := &cli.App{Name: "managed-machine", Stdout: &out, Stderr: &errOut,
		Commands: []cli.Command{Command(Deps{
			Catalog: func() ([]Entry, error) { return LoadCatalog(func(k string) string { return env[k] }) },
		})}}
	code := app.Run([]string{"config", "--available"})
	if code != 0 || errOut.String() != "" || out.String() == "" {
		t.Fatalf("listing: exit %d %q %q", code, out.String(), errOut.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("the listing created the state directory")
	}
	after := fixtureFiles(t, local)
	if len(before) != len(after) {
		t.Errorf("the source changed: %d files, then %d", len(before), len(after))
	}
	for name := range before {
		if _, ok := after[name]; !ok {
			t.Errorf("the listing removed %q", name)
		}
	}
}

// fixtureFiles names the files of a source directory.
func fixtureFiles(t *testing.T, dir string) map[string]bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, e := range ents {
		out[e.Name()] = true
	}
	return out
}

// Usage errors are rejected before reading the catalog, and a provider
// that has no executor yet (such as managed-block in C04) is refused
// before anything is written.
func TestActionsValidationAndProviderRefusal(t *testing.T) {
	h := newGoldenHarness(t, fixtureEntries(t))
	for _, args := range [][]string{
		{"--plan", "--user"},
		{"shell", "--plan"},
		{"shell", "--plan", "--check", "--user"},
		{"shell", "--from-plan", goldenPlanID, "--user"},
	} {
		out, errOut, code := h.run(t, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "MM_USAGE") {
			t.Errorf("%v: exit %d %q %q", args, code, out, errOut)
		}
	}
	if h.reads != 0 {
		t.Errorf("a usage error read the catalog %d times", h.reads)
	}

	dir := t.TempDir()
	desiredPath := filepath.Join(dir, "desired.yaml")
	write(t, desiredPath, "target: user\nconfig:\n  shell:\n    destinations:\n      shell-rc: "+filepath.ToSlash(filepath.Join(dir, ".bashrc"))+"\n")
	h.env = map[string]string{DesiredEnv: desiredPath}
	out, errOut, code := h.run(t, "shell", "--plan", "--user")
	if code != 2 || out != "" || !strings.Contains(errOut, "MM_CONFIG_UNSUPPORTED_COMBINATION") || !strings.Contains(errOut, "has no executor in this release") {
		t.Errorf("unimplemented provider: exit %d %q %q", code, out, errOut)
	}
}

// A source that cannot be read is a source error, and the diagnostic
// names it.
func TestSourceError(t *testing.T) {
	h := &harness{err: os.ErrPermission}
	_, errOut, code := h.run(t, "--available")
	if code != 2 || !strings.Contains(errOut, "MM_CONFIG_SOURCE_INVALID") {
		t.Errorf("exit %d %q", code, errOut)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }
