package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/core/internal/definition"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/provider"
	"github.com/qwts/managed-machine/core/internal/provider/brew"
	"github.com/qwts/managed-machine/core/internal/source"
)

// fixture is a fake Homebrew prefix with one formula and an isolated
// state directory, so no test ever starts a real brew.
type fixture struct {
	t         *testing.T
	state     string
	prefix    string
	installed string // installed version of rg, "" for none
	failApply bool
	writable  bool
	owned     bool
	machines  []string // returned in turn by Machine; the last repeats
	brewVer   []string // returned in turn by --version; the last repeats
	calls     []string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{t: t, state: filepath.Join(t.TempDir(), "state"), prefix: t.TempDir(), writable: true, owned: true,
		machines: []string{"machine-a"}, brewVer: []string{"4.4.1"}}
	if err := os.MkdirAll(filepath.Join(f.prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.prefix, "bin", "brew"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func next(xs *[]string) string {
	v := (*xs)[0]
	if len(*xs) > 1 {
		*xs = (*xs)[1:]
	}
	return v
}

func (f *fixture) run(_ context.Context, _ string, args, _ []string) (brew.Result, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case "--version":
		return brew.Result{Stdout: []byte("Homebrew " + next(&f.brewVer) + "\n")}, nil
	case "info":
		inst := "[]"
		if f.installed != "" {
			inst = fmt.Sprintf(`[{"version":%q}]`, f.installed)
		}
		return brew.Result{Stdout: []byte(`{"formulae":[{"name":"ripgrep","versions":{"stable":"14.1.1"},"revision":0,"installed":` + inst + `}],"casks":[]}`)}, nil
	case "install":
		if f.failApply {
			return brew.Result{Exit: 1, Stderr: []byte("Error: download failed\n")}, nil
		}
		f.installed = "14.1.1"
		return brew.Result{}, nil
	case "list":
		if f.installed == "" {
			return brew.Result{Exit: 1}, nil
		}
		return brew.Result{Stdout: []byte("ripgrep " + f.installed + "\n")}, nil
	}
	f.t.Fatalf("unexpected brew %v", args)
	return brew.Result{}, nil
}

func (f *fixture) env() Env {
	return Env{
		StateDir: func() (string, error) { return f.state, nil },
		Account:  func() (string, error) { return "tester", nil },
		Machine:  func(context.Context) (string, error) { return next(&f.machines), nil },
		Now:      func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) },
		Provider: func(name, _ string) provider.Provider {
			if name != brew.Name {
				return nil
			}
			return &brew.Provider{Prefix: f.prefix, Run: f.run,
				Writable: func(string) bool { return f.writable }, Owned: func(string) bool { return f.owned }}
		},
	}
}

func request(provider, target string) Request {
	sw := definition.Software{SchemaVersion: 1, ID: "ripgrep"}
	return Request{
		Entry:    source.Entry{Software: sw, Source: source.Identity{Kind: "builtin", Location: "builtin"}, Revision: "sha256:aa", File: "ripgrep.yaml", Digest: "sha256:bb"},
		Recipe:   definition.Recipe{ID: "homebrew", Provider: provider, Package: "ripgrep", Options: map[string]any{"formula": "ripgrep"}},
		Target:   target,
		Platform: "darwin/arm64",
	}
}

func (f *fixture) store() *plan.Store {
	st, err := plan.Open(f.state, time.Now)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { st.Close() })
	return st
}

func (f *fixture) noState() {
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		f.t.Fatalf("state was written: %v", err)
	}
}

func TestInstallAppliesAndJournals(t *testing.T) {
	f := newFixture(t)
	res, err := Run(context.Background(), f.env(), request("brew", "global"))
	if err != nil || res.Outcome != "ok" || res.ResolvedVersion != "14.1.1" || !strings.HasPrefix(res.Plan, "plan-20261021T120000Z-") {
		t.Fatalf("install: %+v %v", res, err)
	}
	st := f.store()
	d, err := st.Load(res.Plan)
	if err != nil || d.State != plan.Applied || d.Command != Command || d.Account != "tester" || d.Bindings.Machine != "machine-a" ||
		d.SourceRevision != "builtin@sha256:aa" || len(d.Operations) != 1 || d.Operations[0].Params["version"] != "14.1.1" {
		t.Fatalf("saved plan: %+v %v", d, err)
	}
	rc, err := st.Receipt(res.Plan)
	if err != nil || rc == nil || rc.Outcome != plan.OutcomeApplied {
		t.Fatalf("receipt: %+v %v", rc, err)
	}
	if want := "install --formula ripgrep"; !contains(f.calls, want) {
		t.Errorf("brew calls %v lack %q", f.calls, want)
	}
}

func TestInstalledFormulaIsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.installed = "14.1.1"
	res, err := Run(context.Background(), f.env(), request("brew", "user"))
	if err != nil || res.Outcome != "unchanged" || res.ResolvedVersion != "14.1.1" {
		t.Fatalf("unchanged: %+v %v", res, err)
	}
	if contains(f.calls, "install --formula ripgrep") {
		t.Error("an installed formula was installed again")
	}
}

func TestFailedApplyIsReported(t *testing.T) {
	f := newFixture(t)
	f.failApply = true
	_, err := Run(context.Background(), f.env(), request("brew", "global"))
	var fl *Failed
	if !errors.As(err, &fl) || !strings.Contains(err.Error(), "brew install exited 1") {
		t.Fatalf("failure: %v", err)
	}
	if d, _ := f.store().Load(fl.Plan); d == nil || d.State != plan.Failed {
		t.Fatalf("failed plan state: %+v", d)
	}
}

func TestChangedMachineIsRejected(t *testing.T) {
	for name, mut := range map[string]func(*fixture){
		"machine": func(f *fixture) { f.machines = []string{"machine-a", "machine-b"} },
		"brew":    func(f *fixture) { f.brewVer = []string{"4.4.1", "4.5.0"} },
	} {
		f := newFixture(t)
		mut(f)
		_, err := Run(context.Background(), f.env(), request("brew", "global"))
		var rej *Rejected
		if !errors.As(err, &rej) || len(rej.Changes) == 0 {
			t.Fatalf("%s: %v", name, err)
		}
		if contains(f.calls, "install --formula ripgrep") {
			t.Errorf("%s: a rejected plan ran", name)
		}
	}
}

func TestRefusalsWriteNoState(t *testing.T) {
	for name, c := range map[string]struct {
		mut  func(*fixture)
		req  Request
		want error
	}{
		"other provider": {func(*fixture) {}, request("npm", "user"), ErrUnavailable},
		"custom root":    {func(*fixture) {}, request("brew", "custom:/x"), ErrUnsupported},
		"not writable":   {func(f *fixture) { f.writable = false }, request("brew", "global"), ErrPrivilege},
		"shared user":    {func(f *fixture) { f.owned = false }, request("brew", "user"), ErrUnsupported},
		"no brew":        {func(f *fixture) { os.Remove(filepath.Join(f.prefix, "bin", "brew")) }, request("brew", "global"), ErrMissing},
	} {
		f := newFixture(t)
		c.mut(f)
		if _, err := Run(context.Background(), f.env(), c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		f.noState()
	}
}

func TestInputsDigestCoversTheRequest(t *testing.T) {
	a := request("brew", "global")
	b := request("brew", "user")
	c := request("brew", "global")
	c.Recipe.Options = map[string]any{"formula": "rg"}
	if InputsDigest(a) == InputsDigest(b) || InputsDigest(a) == InputsDigest(c) || InputsDigest(a) != InputsDigest(request("brew", "global")) {
		t.Fatal("the inputs digest does not track the request")
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
