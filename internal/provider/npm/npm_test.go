package npm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
)

// fakeNpm is a Runner that models an npm prefix with a set of installed
// packages and records every invocation.
type fakeNpm struct {
	t         *testing.T
	tool      string
	installed map[string]string
	badJSON   bool // npm ls answers something that is not JSON
	lie       bool // install exits 0 but installs nothing
	calls     []string
	envs      [][]string
}

func (f *fakeNpm) run(_ context.Context, path string, args, env []string) (Result, error) {
	if path != f.tool {
		f.t.Fatalf("the provider started %s, not the npm it located", path)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	switch {
	case len(args) == 1 && args[0] == "--version":
		return Result{Stdout: []byte("10.9.2\n")}, nil
	case len(args) == 7 && args[0] == "ls":
		pkg := args[6]
		if f.badJSON {
			return Result{Stdout: []byte("not json")}, nil
		}
		if v, ok := f.installed[pkg]; ok {
			return Result{Stdout: []byte(fmt.Sprintf(`{"dependencies":{%q:{"version":%q}}}`, pkg, v))}, nil
		}
		// npm exits 1 when the package is not there.
		return Result{Exit: 1, Stdout: []byte(`{"dependencies":{}}`)}, nil
	case len(args) == 9 && args[0] == "install":
		spec := args[8]
		i := strings.LastIndex(spec, "@")
		if i < 0 {
			f.t.Fatalf("the provider installed %q without a version", spec)
		}
		if !f.lie {
			f.installed[spec[:i]] = spec[i+1:]
		}
		return Result{}, nil
	}
	f.t.Fatalf("unexpected npm %v", args)
	return Result{}, nil
}

// registry is an httptest registry that answers the latest version of
// the packages it knows.
func registry(t *testing.T, versions map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), "/latest")
		v, ok := versions[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"name":%q,"version":%q}`, name, v)
	}))
	t.Cleanup(srv.Close)
	return srv
}

type fixture struct {
	provider *Provider
	fake     *fakeNpm
	home     string
	registry *httptest.Server
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	srv := registry(t, map[string]string{"typescript": "5.4.5", "@scope/pkg": "1.2.3"})
	f := &fakeNpm{t: t, tool: filepath.Join(home, "bin", "npm"), installed: map[string]string{}}
	p := &Provider{GOOS: "darwin", Run: f.run, Environ: func() []string {
		return []string{"HOME=" + home, "PATH=/usr/bin", "npm_config_registry=https://private.example/", "NPM_CONFIG_CACHE=/private/cache"}
	}, LookPath: func(string) (string, error) { return f.tool, nil },
		Registry: srv.URL, Writable: func(string) bool { return true }}
	return &fixture{provider: p, fake: f, home: home, registry: srv}
}

func (f *fixture) plan(t *testing.T, target, pkg string) (*Resolution, error) {
	t.Helper()
	return f.provider.Plan(context.Background(), Request{Software: "typescript", Target: target, Options: map[string]string{"package": pkg}})
}

func TestPlanAndInstall(t *testing.T) {
	f := newFixture(t)
	res, err := f.plan(t, "user", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Version != "5.4.5" || res.ProviderVersion != "10.9.2" ||
		len(res.Operations) != 1 || len(res.Preconditions) != 2 || res.Package != "typescript" {
		t.Fatalf("resolution: %+v", res)
	}
	op := res.Operations[0]
	prefix := filepath.Join(f.home, ".local")
	if op.Action != "create" || op.Provider != Name || op.Target != "user" ||
		op.Params["package"] != "typescript" || op.Params["version"] != "5.4.5" || op.Params["prefix"] != prefix {
		t.Fatalf("operation: %+v", op)
	}
	software := res.Preconditions[0]
	if software.Kind != "software" || software.Subject != Name ||
		software.Requirement != "present at "+f.fake.tool || software.Observed != "10.9.2" {
		t.Fatalf("software precondition: %+v", software)
	}
	root := res.Preconditions[1]
	if root.Kind != "target" || root.Subject != "npm-prefix" || root.Observed != prefix {
		t.Fatalf("root precondition: %+v", root)
	}
	st, err := f.provider.Step(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev, err := st.Verify(context.Background())
	if err != nil || ev["version"] != "5.4.5" {
		t.Fatalf("verify: %v %v", ev, err)
	}
	want := "--version|ls --global --prefix " + prefix + " --json --depth=0 typescript|" +
		"install --global --prefix " + prefix + " --ignore-scripts --no-fund --no-audit --registry=" +
		f.registry.URL + "/ typescript@5.4.5|ls --global --prefix " + prefix + " --json --depth=0 typescript"
	if got := strings.Join(f.fake.calls, "|"); got != want {
		t.Errorf("npm argv:\n%s\nwant\n%s", got, want)
	}
	for _, env := range f.fake.envs {
		joined := strings.Join(env, " ")
		if strings.Contains(joined, "private.example") || strings.Contains(joined, "NPM_CONFIG_CACHE") {
			t.Errorf("the machine's npm configuration reached the process: %v", env)
		}
		for _, want := range []string{"npm_config_update_notifier=false", "npm_config_fund=false", "npm_config_audit=false"} {
			if !strings.Contains(joined, want) {
				t.Errorf("the npm environment is not explicit: %v", env)
			}
		}
	}
}

func TestPlanCustomRoot(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(t.TempDir(), "custom root")
	res, err := f.plan(t, "custom:"+root, "typescript")
	if err != nil {
		t.Fatal(err)
	}
	op := res.Operations[0]
	if op.Action != "create" || op.Target != "custom:"+root || op.Params["prefix"] != root {
		t.Fatalf("operation: %+v", op)
	}
	// Planning wrote nothing: the root and its cache do not exist.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("planning created the custom root")
	}
	if _, err := os.Stat(filepath.Join(root, ".npm-cache")); !os.IsNotExist(err) {
		t.Errorf("planning created the cache")
	}
	// The cache of a custom root lives in that root.
	if env := strings.Join(f.fake.envs[1], " "); !strings.Contains(env, "npm_config_cache="+filepath.Join(root, ".npm-cache")) {
		t.Errorf("the cache is not inside the custom root: %v", f.fake.envs[1])
	}
	st, err := f.provider.Step(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("apply did not create the custom root: %v", err)
	}
	if ev, err := st.Verify(context.Background()); err != nil || ev["version"] != "5.4.5" {
		t.Fatalf("verify: %v %v", ev, err)
	}
}

func TestUserPrefixPerPlatform(t *testing.T) {
	cases := map[string]struct {
		goos, home, appdata, want string
	}{
		"darwin":  {"darwin", "/home/u", "", filepath.Join("/home/u", ".local")},
		"linux":   {"linux", "/home/u", "", filepath.Join("/home/u", ".local")},
		"windows": {"windows", "C:\\Users\\u", "C:\\Users\\u\\AppData\\Roaming", filepath.Join("C:\\Users\\u\\AppData\\Roaming", "npm")},
	}
	for name, c := range cases {
		prefix, _, err := (&Provider{GOOS: c.goos, Environ: func() []string {
			return []string{"HOME=" + c.home, "APPDATA=" + c.appdata}
		}}).prefix("user")
		if err != nil || prefix != c.want {
			t.Errorf("%s: prefix %q, want %q (%v)", name, prefix, c.want, err)
		}
	}
}

func TestUserTargetPlansOnEveryPlatform(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		f := newFixture(t)
		home := t.TempDir()
		app := filepath.Join(home, "AppData", "Roaming")
		os.MkdirAll(app, 0o755)
		f.provider.GOOS = goos
		f.provider.Environ = func() []string { return []string{"HOME=" + home, "APPDATA=" + app} }
		res, err := f.plan(t, "user", "typescript")
		if err != nil {
			t.Fatalf("%s: %v", goos, err)
		}
		want := filepath.Join(home, ".local")
		if goos == "windows" {
			want = filepath.Join(app, "npm")
		}
		if got := res.Operations[0].Params["prefix"]; got != want {
			t.Errorf("%s: prefix %q, want %q", goos, got, want)
		}
	}
}

func TestInstalledPlansNoOperation(t *testing.T) {
	f := newFixture(t)
	f.fake.installed["typescript"] = "5.4.5"
	res, err := f.plan(t, "user", "typescript")
	if err != nil || !res.Installed || res.Version != "5.4.5" || len(res.Operations) != 0 {
		t.Fatalf("installed package: %+v %v", res, err)
	}
	for _, call := range f.fake.calls {
		if strings.HasPrefix(call, "install") {
			t.Error("planning installed something")
		}
	}
}

func TestUpdatePlansAnUpdate(t *testing.T) {
	f := newFixture(t)
	f.fake.installed["typescript"] = "5.3.0"
	res, err := f.plan(t, "user", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Version != "5.4.5" || len(res.Operations) != 1 {
		t.Fatalf("resolution: %+v", res)
	}
	if op := res.Operations[0]; op.Action != "update" {
		t.Fatalf("operation: %+v", op)
	}
}

func TestRejections(t *testing.T) {
	cases := map[string]struct {
		req  Request
		mod  func(*fixture)
		want error
	}{
		"global":          {Request{Software: "x", Target: "global", Options: map[string]string{"package": "typescript"}}, nil, ErrUnsupported},
		"relative custom": {Request{Software: "x", Target: "custom:relative/root", Options: map[string]string{"package": "typescript"}}, nil, ErrUnsupported},
		"no package":      {Request{Software: "x", Target: "user", Options: map[string]string{}}, nil, ErrUnsupported},
		"bad name":        {Request{Software: "x", Target: "user", Options: map[string]string{"package": "types; rm -rf /"}}, nil, ErrUnsupported},
		"bad option":      {Request{Software: "x", Target: "user", Options: map[string]string{"package": "typescript", "version": "1"}}, nil, ErrUnsupported},
		"missing tool": {Request{Software: "x", Target: "user", Options: map[string]string{"package": "typescript"}}, func(f *fixture) {
			f.provider.LookPath = func(string) (string, error) { return "", errors.New("not found") }
		}, ErrMissing},
		"unwritable root": {Request{Software: "x", Target: "user", Options: map[string]string{"package": "typescript"}}, func(f *fixture) {
			f.provider.Writable = func(string) bool { return false }
		}, ErrPrivilege},
		"no home": {Request{Software: "x", Target: "user", Options: map[string]string{"package": "typescript"}}, func(f *fixture) {
			f.provider.Environ = func() []string { return []string{"PATH=/usr/bin"} }
		}, ErrUnsupported},
	}
	for name, c := range cases {
		f := newFixture(t)
		if c.mod != nil {
			c.mod(f)
		}
		if _, err := f.provider.Plan(context.Background(), c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		for _, call := range f.fake.calls {
			if strings.HasPrefix(call, "install") {
				t.Errorf("%s: a rejected plan installed", name)
			}
		}
	}
}

func TestRootThatIsNotADirectory(t *testing.T) {
	f := newFixture(t)
	root := filepath.Join(t.TempDir(), "root")
	os.WriteFile(root, []byte("x"), 0o644)
	if _, err := f.plan(t, "custom:"+root, "typescript"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a root that is not a directory: %v", err)
	}
}

func TestRegistryAnswers(t *testing.T) {
	f := newFixture(t)
	res, err := f.plan(t, "user", "@scope/pkg")
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "1.2.3" {
		t.Fatalf("resolved version: %+v", res)
	}
	// The registry is asked over HTTPS for the escaped package name.
	if _, err := f.plan(t, "user", "no-such-package"); err == nil {
		t.Error("a package the registry does not know was resolved")
	}
}

func TestMalformedRegistryAnswer(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()
	f.provider.Registry = srv.URL
	if _, err := f.plan(t, "user", "typescript"); err == nil {
		t.Error("a malformed registry answer was accepted")
	}
}

func TestMalformedInstalledState(t *testing.T) {
	f := newFixture(t)
	f.fake.badJSON = true
	if _, err := f.plan(t, "user", "typescript"); err == nil {
		t.Error("a malformed npm ls answer was accepted")
	}
}

func TestFailuresAndVerification(t *testing.T) {
	f := newFixture(t)
	f.fake.lie = true
	st, err := f.provider.Step(plan.Operation{ID: "1", Provider: Name, Action: "create",
		Params: map[string]string{"package": "typescript", "version": "5.4.5", "prefix": filepath.Join(f.home, ".local")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted an install that installed nothing")
	}
	f = newFixture(t)
	f.fake.installed["typescript"] = "5.3.0"
	st, _ = f.provider.Step(plan.Operation{ID: "1", Provider: Name, Action: "create",
		Params: map[string]string{"package": "typescript", "version": "5.4.5", "prefix": filepath.Join(f.home, ".local")}})
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted a different version than planned")
	}
	for _, bad := range []plan.Operation{
		{ID: "1", Provider: "uv", Action: "create", Params: map[string]string{"package": "typescript", "version": "5.4.5", "prefix": "/p"}},
		{ID: "1", Provider: Name, Action: "delete", Params: map[string]string{"package": "typescript", "version": "5.4.5", "prefix": "/p"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "typescript", "version": "5.4.5"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "typescript", "version": "5.4.5", "prefix": "/p", "args": "--HEAD"}},
	} {
		if _, err := f.provider.Step(bad); err == nil {
			t.Errorf("an unexpected operation was accepted: %+v", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	f := newFixture(t)
	res, err := f.plan(t, "user", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.provider.Check(context.Background(), res.Preconditions[0]); err != nil {
		t.Errorf("unchanged npm: %v", err)
	}
	// A tool that moved rejects the plan.
	moved := res.Preconditions[0]
	f.provider.LookPath = func(string) (string, error) { return "/elsewhere/bin/npm", nil }
	if _, err := f.provider.Check(context.Background(), moved); err == nil {
		t.Error("a moved npm was accepted")
	}
	f.provider.LookPath = func(string) (string, error) { return f.fake.tool, nil }
	// A tool that changed rejects the plan.
	changed := res.Preconditions[0]
	changed.Observed = "10.9.1"
	if _, err := f.provider.Check(context.Background(), changed); err == nil {
		t.Error("a changed npm version was accepted")
	}
	// The root is checked again.
	f.provider.Writable = func(string) bool { return false }
	if _, err := f.provider.Check(context.Background(), res.Preconditions[1]); !errors.Is(err, ErrPrivilege) {
		t.Errorf("root check: %v", err)
	}
	if _, err := f.provider.Check(context.Background(), plan.Precondition{Kind: "software", Subject: "other"}); err == nil {
		t.Error("an unknown precondition was accepted")
	}
}

func TestRealAccessChecks(t *testing.T) {
	dir := t.TempDir()
	if !writable(dir) {
		t.Skip("the temporary directory is not writable here")
	}
	if writable(filepath.Join(dir, "absent")) {
		t.Error("a missing directory is writable")
	}
}

func TestPinnedVersionIsPlannedAndInstalled(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/typescript/latest":
			fmt.Fprint(w, `{"name":"typescript","version":"5.4.5"}`)
		case "/typescript/5.3.3":
			fmt.Fprint(w, `{"name":"typescript","version":"5.3.3"}`)
		case "/typescript/5.0.0":
			fmt.Fprint(w, `{"name":"typescript","version":"5.4.5"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	f.provider.Registry = srv.URL
	plan := func(version string) (*Resolution, error) {
		return f.provider.Plan(context.Background(), Request{Software: "typescript", Target: "user",
			Options: map[string]string{"package": "typescript", "version": version}})
	}
	res, err := plan("5.3.3")
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "5.3.3" || res.Operations[0].Params["version"] != "5.3.3" {
		t.Fatalf("plan: %+v", res)
	}
	st, err := f.provider.Step(res.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.fake.installed["typescript"] != "5.3.3" {
		t.Fatalf("installed %v", f.fake.installed)
	}
	for _, v := range []string{"4.9.9", "5.0.0"} {
		if _, err := plan(v); err == nil {
			t.Errorf("pin %s was planned", v)
		}
	}
	for _, v := range []string{"", "latest", "^5.3.3", "5.3", "5.3.3 --force"} {
		if _, err := plan(v); !errors.Is(err, ErrUnsupported) {
			t.Errorf("version %q: %v", v, err)
		}
	}
}
