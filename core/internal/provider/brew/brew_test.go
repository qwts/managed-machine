package brew

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/plan"
)

// fakeBrew is a Runner that models a Homebrew prefix with a set of
// installed formulae and records every invocation.
type fakeBrew struct {
	t         *testing.T
	prefix    string
	installed map[string]string
	stable    map[string]string
	revision  map[string]int
	failOn    string
	lie       bool // install exits 0 but installs nothing
	calls     []string
	envs      [][]string
}

func (f *fakeBrew) run(_ context.Context, path string, args, env []string) (Result, error) {
	if path != filepath.Join(f.prefix, "bin", "brew") {
		f.t.Fatalf("the provider started %s, not the prefix's brew", path)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	switch {
	case len(args) == 1 && args[0] == "--version":
		return Result{Stdout: []byte("Homebrew 4.4.1\n")}, nil
	case len(args) == 4 && args[0] == "info":
		name := args[3]
		st, ok := f.stable[name]
		if !ok {
			return Result{Exit: 1, Stderr: []byte("Error: No available formula with the name \"" + name + "\".\n")}, nil
		}
		inst := "[]"
		if v, ok := f.installed[name]; ok {
			inst = fmt.Sprintf(`[{"version":%q}]`, v)
		}
		return Result{Stdout: []byte(fmt.Sprintf(`{"formulae":[{"name":%q,"versions":{"stable":%q},"revision":%d,"installed":%s}],"casks":[]}`, name, st, f.revision[name], inst))}, nil
	case len(args) == 3 && args[0] == "install":
		if args[2] == f.failOn {
			return Result{Exit: 1, Stderr: []byte("Error: download failed\n")}, nil
		}
		if !f.lie {
			v := f.stable[args[2]]
			if r := f.revision[args[2]]; r > 0 {
				v = fmt.Sprintf("%s_%d", v, r)
			}
			f.installed[args[2]] = v
		}
		return Result{}, nil
	case len(args) == 4 && args[0] == "list":
		if v, ok := f.installed[args[3]]; ok {
			return Result{Stdout: []byte(args[3] + " " + v + "\n")}, nil
		}
		return Result{Exit: 1}, nil
	}
	f.t.Fatalf("unexpected brew %v", args)
	return Result{}, nil
}

func fixture(t *testing.T) (*Provider, *fakeBrew) {
	prefix := t.TempDir()
	os.MkdirAll(filepath.Join(prefix, "bin"), 0o755)
	os.WriteFile(filepath.Join(prefix, "bin", "brew"), []byte("fake"), 0o755)
	f := &fakeBrew{t: t, prefix: prefix, installed: map[string]string{"git": "2.51.0"}, stable: map[string]string{"git": "2.51.0", "ripgrep": "14.1.1"}}
	p := &Provider{Prefix: prefix, Run: f.run, Environ: func() []string { return []string{"HOME=/h", "HOMEBREW_NO_AUTO_UPDATE=0"} },
		Writable: func(string) bool { return true }, Owned: func(string) bool { return true }}
	return p, f
}

func TestPlanAndInstall(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "ripgrep", Target: "global", Options: map[string]string{"formula": "ripgrep"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Version != "14.1.1" || res.ProviderVersion != "4.4.1" || len(res.Operations) != 1 || len(res.Preconditions) != 2 {
		t.Fatalf("resolution: %+v", res)
	}
	op := res.Operations[0]
	if op.Action != "create" || op.Params["formula"] != "ripgrep" || op.Params["version"] != "14.1.1" {
		t.Fatalf("operation: %+v", op)
	}
	st, err := p.Step(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev, err := st.Verify(context.Background())
	if err != nil || ev["version"] != "14.1.1" {
		t.Fatalf("verify: %v %v", ev, err)
	}
	want := "--version|info --json=v2 --formula ripgrep|install --formula ripgrep|list --versions --formula ripgrep"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("brew argv:\n%s\nwant\n%s", got, want)
	}
	for _, env := range f.envs {
		joined := strings.Join(env, " ")
		if !strings.Contains(joined, "HOMEBREW_NO_AUTO_UPDATE=1") || strings.Contains(joined, "HOMEBREW_NO_AUTO_UPDATE=0") || !strings.Contains(joined, "HOMEBREW_NO_INSTALL_CLEANUP=1") {
			t.Errorf("brew environment is not explicit: %v", env)
		}
	}
}

func TestInstalledPlansNoOperation(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "git", Target: "global", Options: map[string]string{"formula": "git"}})
	if err != nil || !res.Installed || res.Version != "2.51.0" || len(res.Operations) != 0 {
		t.Fatalf("installed formula: %+v %v", res, err)
	}
	for _, c := range f.calls {
		if strings.HasPrefix(c, "install") {
			t.Error("planning installed something")
		}
	}
}

func TestRejections(t *testing.T) {
	cases := map[string]struct {
		req  Request
		mod  func(*Provider)
		want error
	}{
		"cask":         {Request{Software: "x", Target: "global", Options: map[string]string{"cask": "x"}}, nil, ErrUnsupported},
		"formula+cask": {Request{Software: "x", Target: "global", Options: map[string]string{"formula": "x", "cask": "x"}}, nil, ErrUnsupported},
		"custom":       {Request{Software: "ripgrep", Target: "custom:/souls/bob", Options: map[string]string{"formula": "ripgrep"}}, nil, ErrUnsupported},
		"bad name":     {Request{Software: "x", Target: "global", Options: map[string]string{"formula": "x; rm -rf /"}}, nil, ErrUnsupported},
		"unknown":      {Request{Software: "nope", Target: "global", Options: map[string]string{"formula": "nope"}}, nil, ErrUnsupported},
		"not writable": {Request{Software: "ripgrep", Target: "global", Options: map[string]string{"formula": "ripgrep"}}, func(p *Provider) { p.Writable = func(string) bool { return false } }, ErrPrivilege},
		"user, shared": {Request{Software: "ripgrep", Target: "user", Options: map[string]string{"formula": "ripgrep"}}, func(p *Provider) { p.Owned = func(string) bool { return false } }, ErrUnsupported},
		"missing brew": {Request{Software: "ripgrep", Target: "global", Options: map[string]string{"formula": "ripgrep"}}, func(p *Provider) { p.Prefix = filepath.Join(p.Prefix, "absent") }, ErrMissing},
		"no prefix":    {Request{Software: "ripgrep", Target: "global", Options: map[string]string{"formula": "ripgrep"}}, func(p *Provider) { p.Prefix = "" }, ErrMissing},
	}
	for name, c := range cases {
		p, f := fixture(t)
		if c.mod != nil {
			c.mod(p)
		}
		if _, err := p.Plan(context.Background(), c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		for _, call := range f.calls {
			if strings.HasPrefix(call, "install") {
				t.Errorf("%s: a rejected plan installed", name)
			}
		}
	}
}

func TestFailuresAndVerification(t *testing.T) {
	p, f := fixture(t)
	f.failOn = "ripgrep"
	st, _ := p.Step(plan.Operation{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"formula": "ripgrep", "version": "14.1.1"}})
	if _, err := st.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "download failed") {
		t.Errorf("install failure: %v", err)
	}
	p, f = fixture(t)
	f.lie = true
	st, _ = p.Step(plan.Operation{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"formula": "ripgrep", "version": "14.1.1"}})
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted an install that installed nothing")
	}
	p, f = fixture(t)
	f.installed["ripgrep"] = "13.0.0"
	st, _ = p.Step(plan.Operation{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"formula": "ripgrep", "version": "14.1.1"}})
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted a different version than planned")
	}
	for _, bad := range []plan.Operation{
		{ID: "1", Provider: "uv", Action: "create", Params: map[string]string{"formula": "x", "version": "1"}},
		{ID: "1", Provider: Name, Action: "delete", Params: map[string]string{"formula": "x", "version": "1"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"formula": "x", "version": "1", "args": "--HEAD"}},
	} {
		if _, err := p.Step(bad); err == nil {
			t.Errorf("an unexpected operation was accepted: %+v", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	p, f := fixture(t)
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "software", Subject: Name, Observed: "4.4.1"}); err != nil {
		t.Errorf("unchanged brew: %v", err)
	}
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "software", Subject: Name, Observed: "4.3.0"}); err == nil {
		t.Error("a changed Homebrew version was accepted")
	}
	p.Writable = func(string) bool { return false }
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "privilege", Subject: "homebrew-prefix"}); !errors.Is(err, ErrPrivilege) {
		t.Errorf("privilege check: %v", err)
	}
	_ = f
}

func TestRealAccessChecks(t *testing.T) {
	dir := t.TempDir()
	if !writable(dir) || !owned(dir) {
		t.Skip("the temporary directory is not writable and owned here")
	}
	if writable(filepath.Join(dir, "absent")) {
		t.Error("a missing prefix is writable")
	}
}

func TestRevisionedVersion(t *testing.T) {
	p, f := fixture(t)
	f.revision = map[string]int{"ripgrep": 1}
	res, err := p.Plan(context.Background(), Request{Software: "ripgrep", Target: "global", Options: map[string]string{"formula": "ripgrep"}})
	if err != nil || res.Version != "14.1.1_1" {
		t.Fatalf("revisioned plan: %+v %v", res, err)
	}
	st, _ := p.Step(res.Operations[0])
	st.Apply(context.Background())
	if ev, err := st.Verify(context.Background()); err != nil || ev["version"] != "14.1.1_1" {
		t.Fatalf("revisioned verify: %v %v", ev, err)
	}
}
