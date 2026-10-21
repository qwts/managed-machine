package software

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/install"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/target"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

type harness struct {
	installs int
	env      map[string]string
	reqs     []install.Request
	result   install.Result
	err      error
	// client is the HTTP client --refresh fetches with; nil means the
	// default client.
	client *http.Client
}

func (h *harness) run(t *testing.T, tg target.Target, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	app := &cli.App{Name: "managed-machine", Stdout: &out, Stderr: &errb,
		Detect: func() (target.Host, error) { return target.Host{Target: tg}, nil },
		Commands: []cli.Command{Command(Deps{
			Catalog: func() ([]source.Entry, *registry.Registry, error) {
				return LoadCatalog(func(k string) string { return h.env[k] })
			},
			Install: func(_ context.Context, r install.Request) (install.Result, error) {
				h.installs++
				h.reqs = append(h.reqs, r)
				return h.result, h.err
			},
			Getenv: func(k string) string { return h.env[k] },
			Client: h.client,
		})}}
	code := app.Run(append([]string{"software"}, args...))
	return out.String(), errb.String(), code
}

func TestDiscoveryGoldens(t *testing.T) {
	cases := []struct {
		name string
		tg   target.Target
		args []string
	}{
		{"available-darwin-arm64", target.DarwinARM64, []string{"--available"}},
		{"available-windows-amd64", target.WindowsAMD64, []string{"--available"}},
		{"available-linux-arm64-json", target.LinuxARM64, []string{"--available", "--json"}},
		{"available-darwin-arm64-human", target.DarwinARM64, []string{"--human"}},
		{"help-darwin-arm64", target.DarwinARM64, []string{"-h"}},
		{"help-windows-arm64-provider", target.WindowsARM64, []string{"-h", "--provider", "winget"}},
		{"ruff-user", target.DarwinARM64, []string{"ruff", "--user"}},
		{"category-provider", target.LinuxAMD64, []string{"--category", "python", "--provider", "uv"}},
		{"recipe-filter", target.DarwinARM64, []string{"--recipe", "homebrew", "--global"}},
		{"custom-filter", target.WindowsAMD64, []string{"--custom", "C:/souls/bob"}},
		{"unknown-name", target.DarwinARM64, []string{"nothing-here"}},
		{"unknown-provider", target.DarwinARM64, []string{"--provider", "pip"}},
		{"unknown-category", target.DarwinARM64, []string{"--category", "games"}},
		{"unknown-recipe", target.DarwinARM64, []string{"ruff", "--recipe", "pipx"}},
		{"conflicting-targets", target.DarwinARM64, []string{"--user", "--global"}},
		{"recipe-and-provider", target.DarwinARM64, []string{"--recipe", "uv-tool", "--provider", "uv"}},
		{"install-ambiguous", target.DarwinARM64, []string{"ruff", "--install", "--user"}},
		{"install-ambiguous-human", target.DarwinARM64, []string{"ruff", "--install", "--user", "--human"}},
		{"install-custom-unsupported", target.DarwinARM64, []string{"ripgrep", "--install", "--custom", "/souls/bob"}},
		{"install-without-target", target.DarwinARM64, []string{"ruff", "--install"}},
	}
	h := &harness{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, errOut, code := h.run(t, c.tg, c.args...)
			got := "exit " + itoa(code) + "\n--- stdout\n" + out + "--- stderr\n" + errOut
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				os.WriteFile(path, []byte(got), 0o644)
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
	if h.installs != 0 {
		t.Errorf("installer called %d times", h.installs)
	}
}

func itoa(i int) string { return string(rune('0' + i)) }

// Filters intersect, and discovery lists every recipe: with nothing
// selected, no preference can hide an alternative.
func TestFiltersIntersect(t *testing.T) {
	h := &harness{}
	all, _, _ := h.run(t, target.DarwinARM64, "ruff")
	if strings.Count(all, "\n") != 2 {
		t.Fatalf("ruff should list both recipes:\n%s", all)
	}
	for _, c := range []struct {
		args []string
		want int
	}{
		{[]string{"ruff", "--provider", "uv"}, 1},
		{[]string{"ruff", "--global"}, 1},
		{[]string{"ruff", "--provider", "uv", "--global"}, 0},
		{[]string{"--category", "python"}, 2},
		{[]string{"--category", "python", "--user"}, 2},
		{[]string{"--category", "python", "--custom", "/x"}, 1},
	} {
		out, errOut, code := h.run(t, target.DarwinARM64, c.args...)
		if code != 0 || errOut != "" || strings.Count(out, "\n") != c.want {
			t.Errorf("%v: exit %d, %d records, stderr %q", c.args, code, strings.Count(out, "\n"), errOut)
		}
	}
}

// Discovery, help and a rejected install never create a missing custom
// root and never reach the installer.
func TestNoSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing-soul")
	h := &harness{}
	for _, args := range [][]string{
		{"--custom", root},
		{"--available", "--custom", root},
		{"-h", "--custom", root},
		{"ruff", "--install", "--custom", root, "-h"},
	} {
		h.run(t, target.DarwinARM64, args...)
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("%v created the custom root", args)
		}
	}
	if h.installs != 0 {
		t.Errorf("installer called %d times", h.installs)
	}
	// The uv recipe is the only custom-capable one, so it is selected; the
	// installer refuses it (S3) and the root is still not created.
	h.err = fmt.Errorf("%w: provider uv installs ship in S3", install.ErrUnavailable)
	_, errOut, code := h.run(t, target.DarwinARM64, "ruff", "--install", "--custom", root)
	if code != 2 || !strings.Contains(errOut, "MM_SOFTWARE_INSTALL_UNAVAILABLE") || len(h.reqs) != 1 || h.reqs[0].Recipe.ID != "uv-tool" || h.reqs[0].Target != "custom:"+root {
		t.Fatalf("custom install: exit %d %q %+v", code, errOut, h.reqs)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("a refused install created the custom root")
	}
}

func TestLocalSource(t *testing.T) {
	h := &harness{env: map[string]string{SourceDirEnv: "testdata/local"}}
	out, errOut, code := h.run(t, target.DarwinARM64, "jq")
	if code != 0 || out != `{"software":"jq","recipe":"homebrew","provider":"brew","package":"jq","targets":["global","user"]}`+"\n" {
		t.Errorf("local definition: exit %d %q %q", code, out, errOut)
	}
	h.env[SourceDirEnv] = "testdata/shadow"
	_, errOut, code = h.run(t, target.DarwinARM64, "--available")
	if code != 2 || !strings.Contains(errOut, "MM_SOFTWARE_SOURCE_INVALID") || !strings.Contains(errOut, `\"git\" is already defined by builtin/git.yaml`) {
		t.Errorf("shadowing local definition: exit %d %q", code, errOut)
	}
	if strings.Contains(errOut, "testdata") {
		t.Errorf("diagnostic carries the host path: %q", errOut)
	}
}

func TestInstallRecordAndErrors(t *testing.T) {
	h := &harness{result: install.Result{ResolvedVersion: "14.1.1", Outcome: "ok", Plan: "plan-20261021T120000Z-3f9c"}}
	out, errOut, code := h.run(t, target.DarwinARM64, "ripgrep", "--install", "--global")
	want := `{"software":"ripgrep","recipe":"homebrew","provider":"brew","target":"global","resolved_version":"14.1.1","outcome":"ok","plan":"plan-20261021T120000Z-3f9c"}` + "\n"
	if code != 0 || out != want || errOut != "" {
		t.Fatalf("install record: exit %d %q %q", code, out, errOut)
	}
	if r := h.reqs[0]; r.Recipe.ID != "homebrew" || r.Platform != "darwin/arm64" || r.Entry.Software.ID != "ripgrep" || r.Entry.Digest == "" {
		t.Errorf("installer request: %+v", r)
	}
	for err, id := range map[error]string{
		fmt.Errorf("%w: npm", install.ErrUnavailable):          "MM_SOFTWARE_INSTALL_UNAVAILABLE",
		fmt.Errorf("%w (/opt/homebrew)", install.ErrPrivilege): "MM_SOFTWARE_PRIVILEGE_REQUIRED",
		fmt.Errorf("%w", install.ErrMissing):                   "MM_SOFTWARE_PROVIDER_MISSING",
		fmt.Errorf("%w: cask", install.ErrUnsupported):         "MM_SOFTWARE_UNSUPPORTED_COMBINATION",
		install.ErrBusy: "MM_SOFTWARE_BUSY",
		&install.Rejected{Plan: "p", Changes: []string{"provider brew changed"}}: "MM_SOFTWARE_PLAN_REJECTED",
		&install.Failed{Plan: "p", Err: errors.New("brew install exited 1")}:     "MM_SOFTWARE_INSTALL_FAILED",
	} {
		h := &harness{err: err}
		out, errOut, code := h.run(t, target.DarwinARM64, "ripgrep", "--install", "--global")
		if out != "" || !strings.HasPrefix(errOut, `{"diagnostic":"`+id+`"`) || code == 0 {
			t.Errorf("%v: exit %d %q", err, code, errOut)
		}
	}
	// Selection failures never reach the installer.
	h = &harness{}
	for _, args := range [][]string{{"ruff", "--install", "--user"}, {"ripgrep", "--install", "--custom", "/x"}, {"nothing", "--install", "--user"}, {"ripgrep", "--install", "--global", "--category", "python"}} {
		h.run(t, target.DarwinARM64, args...)
	}
	if h.installs != 0 {
		t.Errorf("selection failures reached the installer %d times", h.installs)
	}
}
