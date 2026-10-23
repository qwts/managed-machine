package golang

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
)

const (
	module = "github.com/rhysd/actionlint"
	pkg    = "github.com/rhysd/actionlint/cmd/actionlint"
)

// binary is one installed binary: the module it was built from and the
// version it carries.
type binary struct {
	module, version string
}

// fakeGo is a Runner that models a go toolchain and the binaries of one
// target, and records every invocation.
type fakeGo struct {
	t         *testing.T
	goos      string
	version   string            // what `go version` prints
	modules   map[string]string // package path -> module path
	installed map[string]binary // binary path -> what it was built from
	failOn    string            // a package whose install fails
	lie       bool              // install exits 0 but installs nothing
	calls     []string          // every argv, joined
	envs      [][]string        // every environment
}

func (f *fakeGo) run(_ context.Context, path string, args, env []string) (Result, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	switch {
	case len(args) == 1 && args[0] == "version":
		return Result{Stdout: []byte("go version go" + f.version + " " + f.goos + "/arm64\n")}, nil
	case len(args) == 3 && args[0] == "version" && args[1] == "-m":
		b, ok := f.installed[args[2]]
		if !ok {
			return Result{Exit: 1, Stderr: []byte("go: cannot read " + args[2] + "\n")}, nil
		}
		return Result{Stdout: []byte(args[2] + ": go" + f.version + "\n\tpath\t" + args[2] +
			"\n\tmod\t" + b.module + "\t" + b.version + "\n")}, nil
	case len(args) == 2 && args[0] == "install":
		at := strings.LastIndex(args[1], "@")
		if at < 0 {
			f.t.Fatalf("the provider installed %q without a pinned version", args[1])
		}
		path, version := args[1][:at], args[1][at+1:]
		if path == f.failOn {
			return Result{Exit: 1, Stderr: []byte("go: module download failed\n")}, nil
		}
		if f.lie {
			return Result{}, nil
		}
		bin := filepath.Join(envValue(env, "GOBIN"), filepath.Base(path))
		if f.goos == "windows" {
			bin += ".exe"
		}
		if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
			f.t.Fatal(err)
		}
		f.installed[bin] = binary{module: f.modules[path], version: version}
		return Result{}, nil
	}
	f.t.Fatalf("unexpected go %v", args)
	return Result{}, nil
}

// installBinary records an installed binary and creates the file the
// provider looks for.
func (f *fakeGo) installBinary(t *testing.T, bin, mod, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.installed[bin] = binary{module: mod, version: version}
}

// proxy is a module proxy that answers one version and records the paths
// it was asked for.
type proxy struct {
	srv    *httptest.Server
	mu     sync.Mutex
	paths  []string
	latest string
}

func newProxy(t *testing.T, latest string) *proxy {
	p := &proxy{latest: latest}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		p.paths = append(p.paths, r.URL.Path)
		p.mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/@latest") {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"Version": p.latest, "Time": "2026-10-21T00:00:00Z"})
	}))
	t.Cleanup(p.srv.Close)
	return p
}

// asked returns the paths the proxy was asked for.
func (p *proxy) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string{}, p.paths...)
}

// fixture is one provider with a fake toolchain, a fake environment and a
// registry that serves one version.
func fixture(t *testing.T) (*Provider, *fakeGo, *proxy, string) {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// The tool is created under both spellings, so the fixture works on
	// every host whatever the target's operating system is.
	for _, name := range []string{"go", "go.exe"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("fake"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeGo{t: t, goos: "darwin", version: "1.27.3", modules: map[string]string{pkg: module}, installed: map[string]binary{}}
	p := newProxy(t, "v1.7.7")
	env := []string{"HOME=" + home, "PATH=" + bin, "GOBIN=/inherited/gobin", "GOFLAGS=-mod=readonly", "GOPATH=/inherited/gopath", "GOTOOLCHAIN=auto"}
	prov := &Provider{GOOS: "darwin", ProxyBase: p.srv.URL,
		Environ: func() []string { return env },
		LookPath: func(file string, e []string) (string, error) {
			return LookPath(file, e)
		},
		Run: f.run}
	return prov, f, p, home
}

// userBin is where the user target installs a package.
func userBin(home, name string) string {
	return filepath.Join(home, "go", "bin", name)
}

func TestPlanUserTarget(t *testing.T) {
	prov, f, p, home := fixture(t)
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Version != "v1.7.7" || res.ProviderVersion != "1.27.3" || res.Package != pkg {
		t.Fatalf("resolution: %+v", res)
	}
	if len(res.Operations) != 1 || len(res.Preconditions) != 2 {
		t.Fatalf("resolution: %+v", res)
	}
	op := res.Operations[0]
	if op.Action != "create" || op.Provider != Name || op.Target != "user" ||
		op.Params["module"] != module || op.Params["package"] != pkg || op.Params["version"] != "v1.7.7" {
		t.Fatalf("operation: %+v", op)
	}
	pre := res.Preconditions[0]
	if pre.Kind != "software" || pre.Subject != Name || pre.Observed != "1.27.3" ||
		!strings.HasPrefix(pre.Requirement, "present at "+filepath.Join(home, "bin", "go")) {
		t.Fatalf("precondition: %+v", pre)
	}
	if got := strings.Join(p.asked(), "|"); got != "/github.com/rhysd/actionlint/@latest" {
		t.Errorf("the proxy was asked for %q", got)
	}
	if got := strings.Join(f.calls, "|"); got != "version" {
		t.Errorf("go argv: %q", got)
	}
	// The version probe runs with the explicit base environment: the
	// variables this provider owns are gone, whatever the shell exported.
	for _, kv := range f.envs[0] {
		if strings.HasPrefix(kv, "GOBIN=") || strings.HasPrefix(kv, "GOFLAGS=") ||
			strings.HasPrefix(kv, "GOPATH=") || strings.HasPrefix(kv, "GOTOOLCHAIN=") {
			t.Errorf("the inherited environment reached the go tool: %q", kv)
		}
	}
	// Planning writes nothing.
	if _, err := os.Stat(filepath.Join(home, "go")); !os.IsNotExist(err) {
		t.Error("planning created the user's go directory")
	}
}

func TestPlanCustomTarget(t *testing.T) {
	prov, _, p, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "soul")
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "custom:" + root,
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != "v1.7.7" || len(res.Operations) != 1 || res.Operations[0].Target != "custom:"+root {
		t.Fatalf("resolution: %+v", res)
	}
	if res.Preconditions[1].Subject != "go-root:"+root {
		t.Fatalf("precondition: %+v", res.Preconditions[1])
	}
	if len(p.asked()) != 1 {
		t.Errorf("the proxy was asked %v times", len(p.paths))
	}
	// Planning creates neither the custom root nor its caches.
	for _, dir := range []string{root, filepath.Join(root, "bin"), filepath.Join(root, "gopath"), filepath.Join(root, "cache")} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("planning created %s", dir)
		}
	}
}

func TestInstalledPlansNoOperation(t *testing.T) {
	prov, f, _, home := fixture(t)
	f.installBinary(t, userBin(home, "actionlint"), module, "v1.7.7")
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil || !res.Installed || res.Version != "v1.7.7" || len(res.Operations) != 0 {
		t.Fatalf("installed package: %+v %v", res, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "install") {
			t.Error("planning installed something")
		}
	}
}

func TestOlderVersionPlansUpdate(t *testing.T) {
	prov, f, _, home := fixture(t)
	f.installBinary(t, userBin(home, "actionlint"), module, "v1.7.6")
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || len(res.Operations) != 1 || res.Operations[0].Action != "update" || res.Version != "v1.7.7" {
		t.Fatalf("update plan: %+v", res)
	}
}

func TestApplyAndVerifyUser(t *testing.T) {
	prov, f, _, home := fixture(t)
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := prov.Step(res.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	ev, err := st.Verify(context.Background())
	if err != nil || ev["version"] != "v1.7.7" || ev["module"] != module {
		t.Fatalf("verify: %v %v", ev, err)
	}
	want := "version|install " + pkg + "@v1.7.7|version -m " + userBin(home, "actionlint")
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("go argv:\n%s\nwant\n%s", got, want)
	}
	// The install runs with the target's own environment and nothing else.
	env := f.envs[len(f.envs)-1]
	joined := strings.Join(env, " ")
	for _, want := range []string{"GOBIN=" + filepath.Join(home, "go", "bin"), "GOTOOLCHAIN=local", "GOFLAGS=-mod=mod", "CGO_ENABLED=0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the go environment lacks %s: %v", want, env)
		}
	}
	for _, unwanted := range []string{"GOBIN=/inherited/gobin", "GOFLAGS=-mod=readonly", "GOPATH=/inherited/gopath", "GOTOOLCHAIN=auto", "GOENV=", "GOMODCACHE=", "GOCACHE="} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("the go environment carries %s: %v", unwanted, env)
		}
	}
	// The binary the install produced is where the target expects it.
	if _, err := os.Stat(userBin(home, "actionlint")); err != nil {
		t.Errorf("the binary is not in the target: %v", err)
	}
}

func TestApplyAndVerifyCustom(t *testing.T) {
	prov, f, _, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "soul")
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "custom:" + root,
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := prov.Step(res.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A custom root keeps the module and build caches inside itself.
	env := strings.Join(f.envs[len(f.envs)-1], " ")
	for _, want := range []string{
		"GOBIN=" + filepath.Join(root, "bin"),
		"GOPATH=" + filepath.Join(root, "gopath"),
		"GOMODCACHE=" + filepath.Join(root, "gopath", "pkg", "mod"),
		"GOCACHE=" + filepath.Join(root, "cache"),
		"GOENV=off",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("the go environment lacks %s: %s", want, env)
		}
	}
	// The custom root is created at apply time, and only inside itself:
	// the module and build caches are the go tool's to create.
	for _, dir := range []string{root, filepath.Join(root, "bin")} {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("the apply did not create %s: %v", dir, err)
		}
	}
}

func TestVerifyCatchesALiedApply(t *testing.T) {
	prov, f, _, _ := fixture(t)
	f.lie = true
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := prov.Step(res.Operations[0])
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted an install that installed nothing")
	}
}

func TestVerifyRejectsAnotherVersion(t *testing.T) {
	prov, f, _, home := fixture(t)
	f.installBinary(t, userBin(home, "actionlint"), module, "v1.7.6")
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := prov.Step(res.Operations[0])
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted a different version than planned")
	}
}

func TestRejections(t *testing.T) {
	// An absolute root on every host, Windows included.
	bob := filepath.Join(t.TempDir(), "souls", "bob")
	cases := map[string]struct {
		req  Request
		mod  func(*Provider, *fakeGo, string)
		want error
	}{
		"global":            {Request{Software: "x", Target: "global", Options: map[string]string{"module": module, "package": pkg}}, nil, ErrUnsupported},
		"relative custom":   {Request{Software: "x", Target: "custom:souls/bob", Options: map[string]string{"module": module, "package": pkg}}, nil, ErrUnsupported},
		"unknown option":    {Request{Software: "x", Target: "user", Options: map[string]string{"module": module, "package": pkg, "flags": "-x"}}, nil, ErrUnsupported},
		"no module":         {Request{Software: "x", Target: "user", Options: map[string]string{"package": pkg}}, nil, ErrUnsupported},
		"no package":        {Request{Software: "x", Target: "user", Options: map[string]string{"module": module}}, nil, ErrUnsupported},
		"bad module":        {Request{Software: "x", Target: "user", Options: map[string]string{"module": "github.com/x; rm -rf /", "package": pkg}}, nil, ErrUnsupported},
		"package elsewhere": {Request{Software: "x", Target: "user", Options: map[string]string{"module": "github.com/other", "package": pkg}}, nil, ErrUnsupported},
		"missing tool": {Request{Software: "x", Target: "user", Options: map[string]string{"module": module, "package": pkg}},
			func(p *Provider, f *fakeGo, home string) {
				p.LookPath = func(string, []string) (string, error) { return "", errors.New("not found") }
			}, ErrMissing},
		"unwritable root": {Request{Software: "x", Target: "custom:" + bob, Options: map[string]string{"module": module, "package": pkg}},
			func(p *Provider, f *fakeGo, home string) { p.Writable = func(string) bool { return false } }, ErrPrivilege},
	}
	for name, c := range cases {
		prov, f, _, home := fixture(t)
		if c.mod != nil {
			c.mod(prov, f, home)
		}
		if _, err := prov.Plan(context.Background(), c.req); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
		for _, call := range f.calls {
			if strings.HasPrefix(call, "install") {
				t.Errorf("%s: a rejected plan installed", name)
			}
		}
	}
}

func TestRootThatIsNotADirectory(t *testing.T) {
	prov, _, _, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	if err := os.WriteFile(root, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "custom:" + root,
		Options: map[string]string{"module": module, "package": pkg}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a root that is not a directory: %v", err)
	}
}

func TestBinaryOfAnotherModule(t *testing.T) {
	prov, f, _, home := fixture(t)
	f.installBinary(t, userBin(home, "actionlint"), "github.com/other/tool", "v1.7.7")
	_, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a binary of another module: %v", err)
	}
}

func TestStepRejectsOtherOperations(t *testing.T) {
	prov, _, _, _ := fixture(t)
	for _, bad := range []plan.Operation{
		{ID: "1", Provider: "uv", Action: "create", Target: "user", Params: map[string]string{"module": module, "package": pkg, "version": "v1.7.7"}},
		{ID: "1", Provider: Name, Action: "delete", Target: "user", Params: map[string]string{"module": module, "package": pkg, "version": "v1.7.7"}},
		{ID: "1", Provider: Name, Action: "create", Target: "user", Params: map[string]string{"module": module, "package": pkg, "version": "v1.7.7", "flags": "-x"}},
		{ID: "1", Provider: Name, Action: "create", Target: "user", Params: map[string]string{"module": module, "package": pkg}},
		{ID: "1", Provider: Name, Action: "create", Target: "user", Params: map[string]string{"module": module, "package": pkg, "version": "latest"}},
		{ID: "1", Provider: Name, Action: "create", Target: "global", Params: map[string]string{"module": module, "package": pkg, "version": "v1.7.7"}},
	} {
		if _, err := prov.Step(bad); !errors.Is(err, ErrUnsupported) {
			t.Errorf("an unexpected operation was accepted: %+v (%v)", bad, err)
		}
	}
}

func TestCheck(t *testing.T) {
	prov, f, _, home := fixture(t)
	res, err := prov.Plan(context.Background(), Request{Software: "actionlint", Target: "user",
		Options: map[string]string{"module": module, "package": pkg}})
	if err != nil {
		t.Fatal(err)
	}
	software := res.Preconditions[0]
	if _, err := prov.Check(context.Background(), software); err != nil {
		t.Errorf("an unchanged go tool: %v", err)
	}
	if _, err := prov.Check(context.Background(), res.Preconditions[1]); err != nil {
		t.Errorf("an unchanged root: %v", err)
	}
	// A changed tool version rejects the plan.
	f.version = "1.28.0"
	if _, err := prov.Check(context.Background(), software); err == nil {
		t.Error("a changed go tool version was accepted")
	}
	f.version = "1.27.3"
	// A moved tool rejects the plan.
	moved := filepath.Join(home, "elsewhere")
	if err := os.MkdirAll(moved, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(moved, "go"), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	prov.Environ = func() []string { return []string{"HOME=" + home, "PATH=" + moved} }
	if _, err := prov.Check(context.Background(), software); err == nil {
		t.Error("a moved go tool was accepted")
	}
	// A root that is no longer writable rejects the plan.
	prov.Environ = func() []string { return []string{"HOME=" + home, "PATH=" + filepath.Join(home, "bin")} }
	prov.Writable = func(string) bool { return false }
	if _, err := prov.Check(context.Background(), res.Preconditions[1]); !errors.Is(err, ErrPrivilege) {
		t.Errorf("an unwritable root: %v", err)
	}
	if _, err := prov.Check(context.Background(), plan.Precondition{Kind: "software", Subject: "brew"}); err == nil {
		t.Error("an unknown precondition was accepted")
	}
}

func TestProxyEscapesUppercase(t *testing.T) {
	prov, _, p, _ := fixture(t)
	p.latest = "v1.2.3"
	version, err := prov.latest(context.Background(), "github.com/Masterminds/semver")
	if err != nil {
		t.Fatal(err)
	}
	if version != "v1.2.3" {
		t.Fatalf("version: %s", version)
	}
	if got := strings.Join(p.asked(), "|"); got != "/github.com/!masterminds/semver/@latest" {
		t.Errorf("the proxy was asked for %q", got)
	}
}

func TestProxyRefusesPlaintext(t *testing.T) {
	prov, _, _, _ := fixture(t)
	prov.ProxyBase = "http://proxy.example.com"
	if _, err := prov.latest(context.Background(), module); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a plaintext proxy: %v", err)
	}
}

func TestWindowsLayout(t *testing.T) {
	home := t.TempDir()
	prov := &Provider{GOOS: "windows", Environ: func() []string { return []string{"USERPROFILE=" + home, "PATH=" + home} },
		LookPath: func(string, []string) (string, error) { return "", errors.New("not found") }}
	l, err := prov.layoutFor("user")
	if err != nil {
		t.Fatal(err)
	}
	if l.bin != filepath.Join(home, "go", "bin") {
		t.Errorf("the windows bin directory is %s", l.bin)
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
