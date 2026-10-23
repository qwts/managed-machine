package cargo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/source"
)

// taploIndex is a crates.io sparse index document: the newest stable
// version, a yanked one and a pre-release one.
const taploIndex = `{"name":"taplo-cli","vers":"0.9.0","deps":[],"cksum":"a","features":{},"yanked":false,"links":null,"v":2}
{"name":"taplo-cli","vers":"0.10.0","deps":[],"cksum":"b","features":{},"yanked":false,"links":null,"v":2}
{"name":"taplo-cli","vers":"0.11.0-rc.1","deps":[],"cksum":"c","features":{},"yanked":false,"links":null,"v":2}
{"name":"taplo-cli","vers":"0.12.0","deps":[],"cksum":"d","features":{},"yanked":true,"links":null,"v":2}
`

// fakeCargo is a Runner that models a cargo installation with a set of
// installed crates and records every invocation.
type fakeCargo struct {
	t         *testing.T
	tool      string
	version   string
	installed map[string]string
	failOn    string
	lie       bool // install exits 0 but installs nothing
	calls     []string
	envs      [][]string
}

func (f *fakeCargo) run(_ context.Context, path string, args, env []string) (Result, error) {
	if path != f.tool {
		f.t.Fatalf("the provider started %s, not the cargo it located", path)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	switch {
	case len(args) == 1 && args[0] == "--version":
		return Result{Stdout: []byte("cargo " + f.version + " (deac0bcf 2026-01-01)\n")}, nil
	case len(args) == 4 && args[0] == "install" && args[1] == "--list":
		// The listing reads the root; a root that does not exist holds
		// nothing.
		if _, err := os.Stat(args[3]); err != nil {
			return Result{Exit: 1, Stderr: []byte("error: no such root\n")}, nil
		}
		var b strings.Builder
		for _, crate := range sortedCrates(f.installed) {
			fmt.Fprintf(&b, "%s v%s:\n    %s\n", crate, f.installed[crate], crate)
		}
		return Result{Stdout: []byte(b.String())}, nil
	case len(args) > 1 && args[0] == "install":
		crate := args[1]
		if crate == f.failOn {
			return Result{Exit: 1, Stderr: []byte("error: failed to download\n")}, nil
		}
		if !f.lie {
			f.installed[crate] = strings.TrimPrefix(value(args, "--version"), "=")
		}
		return Result{}, nil
	}
	f.t.Fatalf("unexpected cargo %v", args)
	return Result{}, nil
}

func sortedCrates(in map[string]string) []string {
	out := make([]string, 0, len(in))
	for k := range in {
		out = append(out, k)
	}
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// value returns the value of an option in an argv.
func value(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// indexServer serves sparse index documents by path.
func indexServer(t *testing.T, docs map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doc, ok := docs[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(doc))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixture(t *testing.T) (*Provider, *fakeCargo) {
	t.Helper()
	home := t.TempDir()
	tool := filepath.Join(home, "bin", "cargo")
	os.MkdirAll(filepath.Dir(tool), 0o755)
	os.WriteFile(tool, []byte("fake"), 0o755)
	srv := indexServer(t, map[string]string{"/ta/pl/taplo-cli": taploIndex})
	f := &fakeCargo{t: t, tool: tool, version: "1.91.0", installed: map[string]string{}}
	p := &Provider{
		Run: f.run,
		Environ: func() []string {
			return []string{"HOME=" + home, "PATH=" + filepath.Dir(tool), "CARGO_INSTALL_ROOT=/wrong", "CARGO_TARGET_DIR=/wrong"}
		},
		LookPath: func(string) (string, error) {
			return tool, nil
		},
		GOOS:      "darwin",
		IndexBase: srv.URL,
	}
	return p, f
}

func userRoot(t *testing.T, p *Provider) string {
	t.Helper()
	root, err := p.userRoot()
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPlanAndInstall(t *testing.T) {
	p, f := fixture(t)
	root := userRoot(t, p)
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Version != "0.10.0" || res.ProviderVersion != "1.91.0" ||
		res.Package != "taplo-cli" || len(res.Operations) != 1 || len(res.Preconditions) != 2 {
		t.Fatalf("resolution: %+v", res)
	}
	if res.Preconditions[0].Kind != "software" || res.Preconditions[0].Subject != Name ||
		res.Preconditions[0].Requirement != "present at "+f.tool || res.Preconditions[0].Observed != "1.91.0" {
		t.Errorf("tool precondition: %+v", res.Preconditions[0])
	}
	if res.Preconditions[1].Kind != "target" || res.Preconditions[1].Subject != "cargo-root" ||
		res.Preconditions[1].Requirement != "writable at "+root {
		t.Errorf("root precondition: %+v", res.Preconditions[1])
	}
	op := res.Operations[0]
	if op.Action != "create" || op.Provider != Name || op.Target != "user" ||
		op.Params["package"] != "taplo-cli" || op.Params["version"] != "0.10.0" || op.Params["root"] != root {
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
	if err != nil || ev["version"] != "0.10.0" {
		t.Fatalf("verify: %v %v", ev, err)
	}
	want := strings.Join([]string{
		"--version",
		"install --list --root " + root,
		"install taplo-cli --version =0.10.0 --locked --root " + root,
		"install --list --root " + root,
	}, "|")
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("cargo argv:\n%s\nwant\n%s", got, want)
	}
}

func TestPlanCustomRoot(t *testing.T) {
	p, f := fixture(t)
	custom := filepath.Join(t.TempDir(), "souls", "bob")
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "custom:" + custom, Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Operations[0].Params["root"] != custom || res.Operations[0].Target != "custom:"+custom {
		t.Fatalf("operation: %+v", res.Operations[0])
	}
	// Planning wrote nothing: the custom root is still absent.
	if _, err := os.Stat(custom); !os.IsNotExist(err) {
		t.Fatal("planning created the custom root")
	}
	st, err := p.Step(res.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The root is created at apply time, with the cache and the build
	// output inside it.
	if _, err := os.Stat(custom); err != nil {
		t.Fatalf("apply did not create the root: %v", err)
	}
	if _, err := st.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	apply := f.envs[len(f.envs)-2]
	joined := strings.Join(apply, " ")
	if !strings.Contains(joined, "CARGO_HOME="+filepath.Join(custom, "cargo-home")) ||
		!strings.Contains(joined, "CARGO_TARGET_DIR="+filepath.Join(custom, "target")) {
		t.Errorf("the custom root does not keep its own cache: %v", apply)
	}
}

func TestEnvironmentIsExplicit(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	st.Apply(context.Background())
	for _, env := range f.envs {
		joined := strings.Join(env, " ")
		if strings.Contains(joined, "CARGO_INSTALL_ROOT") || strings.Contains(joined, "CARGO_TARGET_DIR") {
			t.Errorf("the provider's own variables reached cargo: %v", env)
		}
		if !strings.Contains(joined, "HOME=") {
			t.Errorf("the environment is not the injected one: %v", env)
		}
	}
}

func TestInstalledPlansNoOperation(t *testing.T) {
	p, f := fixture(t)
	root := userRoot(t, p)
	os.MkdirAll(root, 0o755)
	f.installed["taplo-cli"] = "0.10.0"
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil || !res.Installed || res.Version != "0.10.0" || len(res.Operations) != 0 {
		t.Fatalf("installed crate: %+v %v", res, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "install taplo-cli") {
			t.Error("planning installed something")
		}
	}
}

func TestUpdatePlansAnUpdate(t *testing.T) {
	p, f := fixture(t)
	root := userRoot(t, p)
	os.MkdirAll(root, 0o755)
	f.installed["taplo-cli"] = "0.9.0"
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Version != "0.10.0" || len(res.Operations) != 1 || res.Operations[0].Action != "update" {
		t.Fatalf("update plan: %+v %v", res, err)
	}
	st, err := p.Step(res.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := f.calls[len(f.calls)-1]; !strings.HasSuffix(got, "--force") {
		t.Errorf("an update does not force the install: %s", got)
	}
}

func TestRejections(t *testing.T) {
	cases := map[string]struct {
		req  Request
		mod  func(*Provider)
		want error
	}{
		"global":       {Request{Software: "taplo", Target: "global", Options: map[string]string{"crate": "taplo-cli"}}, nil, ErrUnsupported},
		"relative":     {Request{Software: "taplo", Target: "custom:souls/bob", Options: map[string]string{"crate": "taplo-cli"}}, nil, ErrUnsupported},
		"unknown":      {Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli", "bin": "taplo"}}, nil, ErrUnsupported},
		"no crate":     {Request{Software: "taplo", Target: "user", Options: map[string]string{}}, nil, ErrUnsupported},
		"bad crate":    {Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo; rm -rf /"}}, nil, ErrUnsupported},
		"missing tool": {Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}}, func(p *Provider) { p.LookPath = func(string) (string, error) { return "", errors.New("not found") } }, ErrMissing},
		"not writable": {Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}}, func(p *Provider) { p.Writable = func(string) bool { return false } }, ErrPrivilege},
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
			if strings.HasPrefix(call, "install taplo-cli") {
				t.Errorf("%s: a rejected plan installed", name)
			}
		}
	}
}

func TestCustomRootMustBeADirectory(t *testing.T) {
	p, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "file")
	os.WriteFile(root, []byte("x"), 0o644)
	_, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "custom:" + root, Options: map[string]string{"crate": "taplo-cli"}})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("a root that is not a directory: %v", err)
	}
}

func TestUnwritableAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a directory's mode bits do not deny writes on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("the superuser may write every directory")
	}
	p, _ := fixture(t)
	parent := t.TempDir()
	os.Chmod(parent, 0o555)
	root := filepath.Join(parent, "root")
	_, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "custom:" + root, Options: map[string]string{"crate": "taplo-cli"}})
	if !errors.Is(err, ErrPrivilege) {
		t.Errorf("an unwritable ancestor: %v", err)
	}
}

func TestVerifyCatchesAnInstallThatLied(t *testing.T) {
	p, f := fixture(t)
	root := userRoot(t, p)
	os.MkdirAll(root, 0o755)
	f.lie = true
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted an install that installed nothing")
	}
}

func TestVerifyRequiresThePlannedVersion(t *testing.T) {
	p, f := fixture(t)
	root := userRoot(t, p)
	os.MkdirAll(root, 0o755)
	f.installed["taplo-cli"] = "0.9.0"
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted a different version than planned")
	}
}

func TestInstallFailure(t *testing.T) {
	p, f := fixture(t)
	f.failOn = "taplo-cli"
	res, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "user", Options: map[string]string{"crate": "taplo-cli"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	if _, err := st.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "failed to download") {
		t.Errorf("install failure: %v", err)
	}
}

func TestStepRejectsOtherOperations(t *testing.T) {
	p, _ := fixture(t)
	root := userRoot(t, p)
	good := map[string]string{"package": "taplo-cli", "version": "0.10.0", "root": root}
	for _, bad := range []plan.Operation{
		{ID: "1", Provider: "uv", Action: "create", Params: good},
		{ID: "1", Provider: Name, Action: "delete", Params: good},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "taplo-cli", "version": "0.10.0", "root": root, "args": "--HEAD"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "taplo-cli", "version": "0.10.0"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "taplo-cli", "version": "0.10.0", "root": "relative/root"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "taplo cli", "version": "0.10.0", "root": root}},
	} {
		if _, err := p.Step(bad); err == nil {
			t.Errorf("an unexpected operation was accepted: %+v", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	p, f := fixture(t)
	pre := plan.Precondition{Kind: "software", Subject: Name, Requirement: "present at " + f.tool, Observed: "1.91.0"}
	if _, err := p.Check(context.Background(), pre); err != nil {
		t.Errorf("unchanged cargo: %v", err)
	}
	f.version = "1.92.0"
	if _, err := p.Check(context.Background(), pre); err == nil {
		t.Error("a changed cargo version was accepted")
	}
	f.version = "1.91.0"
	moved := filepath.Join(t.TempDir(), "cargo")
	os.WriteFile(moved, []byte("fake"), 0o755)
	p.LookPath = func(string) (string, error) { return moved, nil }
	if _, err := p.Check(context.Background(), pre); err == nil {
		t.Error("a moved cargo was accepted")
	}
	p.LookPath = func(string) (string, error) { return "", errors.New("not found") }
	if _, err := p.Check(context.Background(), pre); !errors.Is(err, ErrMissing) {
		t.Errorf("an absent cargo: %v", err)
	}
	p.LookPath = func(string) (string, error) { return f.tool, nil }
	p.Writable = func(string) bool { return false }
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "target", Subject: "cargo-root", Requirement: "writable at " + userRoot(t, p)}); !errors.Is(err, ErrPrivilege) {
		t.Errorf("root check: %v", err)
	}
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "software", Subject: "other"}); err == nil {
		t.Error("an unknown precondition was accepted")
	}
}

func TestIndexChoosesTheHighestStableVersion(t *testing.T) {
	doc := `{"name":"tool","vers":"1.0.9","yanked":false}
{"name":"tool","vers":"1.0.10","yanked":false}
{"name":"tool","vers":"1.0.2","yanked":false}
{"name":"tool","vers":"2.0.0-rc.1","yanked":false}
{"name":"tool","vers":"3.0.0","yanked":true}
`
	srv := indexServer(t, map[string]string{"/to/ol/tool": doc})
	p := &Provider{GOOS: "darwin", IndexBase: srv.URL}
	v, err := p.resolve(context.Background(), "tool")
	if err != nil || v != "1.0.10" {
		t.Fatalf("resolved version: %q %v", v, err)
	}
	// The sparse index layout follows the crate name.
	for crate, want := range map[string]string{
		"a": "1/a", "ab": "2/ab", "abc": "3/a/abc", "taplo-cli": "ta/pl/taplo-cli",
	} {
		if got := indexPath(crate); got != want {
			t.Errorf("index path of %s: %q, want %q", crate, got, want)
		}
	}
}

func TestUnknownCrateIsRefused(t *testing.T) {
	srv := indexServer(t, map[string]string{})
	p := &Provider{GOOS: "darwin", IndexBase: srv.URL}
	if _, err := p.resolve(context.Background(), "nothing"); !errors.Is(err, ErrUnsupported) {
		t.Errorf("an unknown crate: %v", err)
	}
}

func TestPlanningWritesNothing(t *testing.T) {
	p, _ := fixture(t)
	custom := filepath.Join(t.TempDir(), "absent")
	if _, err := p.Plan(context.Background(), Request{Software: "taplo", Target: "custom:" + custom, Options: map[string]string{"crate": "taplo-cli"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(custom); !os.IsNotExist(err) {
		t.Error("planning created the custom root")
	}
	if _, err := os.Stat(filepath.Join(custom, "cargo-home")); !os.IsNotExist(err) {
		t.Error("planning created a cache")
	}
}

// The plan round trip: the plan is written to the state directory as
// JSON and read back before it runs, so the pinned operation and the
// preconditions must survive it.
func TestInstallThroughTheExecutor(t *testing.T) {
	p, f := fixture(t)
	root := userRoot(t, p)
	os.MkdirAll(root, 0o755)
	dir := t.TempDir()
	env := install.Env{
		StateDir: func() (string, error) { return dir, nil },
		Account:  func() (string, error) { return "tester", nil },
		Machine:  func(context.Context) (string, error) { return "machine-1", nil },
		Now:      func() time.Time { return time.Unix(0, 0).UTC() },
		Provider: func(name, _ string) provider.Provider {
			if name != Name {
				return nil
			}
			return p
		},
	}
	req := install.Request{
		Entry:    source.Entry{Software: definition.Software{ID: "taplo"}, Digest: "sha256:abc"},
		Recipe:   definition.Recipe{ID: "cargo", Provider: Name, Package: "taplo-cli", Options: map[string]any{"crate": "taplo-cli"}},
		Target:   "user",
		Platform: "darwin/arm64",
	}
	res, err := install.Run(context.Background(), env, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "ok" || res.ResolvedVersion != "0.10.0" || res.Plan == "" {
		t.Fatalf("install result: %+v", res)
	}
	if f.installed["taplo-cli"] != "0.10.0" {
		t.Errorf("the crate was not installed: %v", f.installed)
	}
	// The second run finds the planned version installed and plans no
	// operation.
	res, err = install.Run(context.Background(), env, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != "unchanged" || res.Plan == "" {
		t.Fatalf("second install result: %+v", res)
	}
}
