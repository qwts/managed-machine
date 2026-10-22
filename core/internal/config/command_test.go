package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/catalog"
	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/statedir"
)

type harness struct {
	entries []Entry
	err     error
	// reads counts the calls to the catalog: the command reaches the
	// machine only through it.
	reads int
}

func (h *harness) run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	app := &cli.App{Name: "managed-machine", Stdout: &out, Stderr: &errb,
		Commands: []cli.Command{Command(Deps{
			Catalog: func() ([]Entry, error) {
				h.reads++
				if h.err != nil {
					return nil, h.err
				}
				return h.entries, nil
			},
		})}}
	code := app.Run(append([]string{"config"}, args...))
	return out.String(), errb.String(), code
}

func TestCommandGoldens(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"available", []string{"--available"}},
		{"available-human", []string{"--available", "--human"}},
		{"available-json", []string{"--available", "--json"}},
		{"default", []string{}},
		{"help", []string{"-h"}},
		{"help-category", []string{"-h", "--category", "shell"}},
		{"category", []string{"--category", "shell"}},
		{"category-unknown", []string{"--category", "games"}},
		{"target-user", []string{"--user"}},
		{"target-custom", []string{"--custom", "/souls/bob"}},
		{"plan", []string{"shell", "--plan", "--user"}},
		{"check", []string{"shell", "--check", "--user"}},
		{"status", []string{"shell", "--status", "--user"}},
		{"apply", []string{"shell", "--apply", "--user"}},
		{"from-plan", []string{"shell", "--apply", "--user", "--from-plan", "plan-20261021T120000Z-3f9c"}},
		{"plan-without-a-name", []string{"--plan", "--user"}},
		{"plan-without-a-target", []string{"shell", "--plan"}},
		{"conflicting-targets", []string{"shell", "--plan", "--user", "--global"}},
		{"available-and-plan", []string{"--available", "--plan", "--user"}},
		{"conflicting-actions", []string{"shell", "--plan", "--check", "--user"}},
		{"from-plan-without-apply", []string{"shell", "--from-plan", "plan-20261021T120000Z-3f9c", "--user"}},
		{"two-names", []string{"shell", "gitconfig", "--plan", "--user"}},
		{"unknown-option", []string{"--nope"}},
	}
	h := &harness{entries: fixtureEntries(t)}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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

// The actions of the configuration executor are validated first and then
// refused: a wrong invocation is a usage error, and a right one says that
// the action does not exist yet. Neither reads the catalog.
func TestActionsAreValidatedAndRefused(t *testing.T) {
	h := &harness{entries: fixtureEntries(t)}
	for _, args := range [][]string{
		{"shell", "--plan", "--user"},
		{"shell", "--check", "--user"},
		{"shell", "--status", "--user"},
		{"shell", "--apply", "--user"},
	} {
		out, errOut, code := h.run(t, args...)
		if code != 2 || out != "" || !strings.Contains(errOut, "not implemented in this release") {
			t.Errorf("%v: exit %d %q %q", args, code, out, errOut)
		}
	}
	if h.reads != 0 {
		t.Errorf("a refused action read the catalog %d times", h.reads)
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
