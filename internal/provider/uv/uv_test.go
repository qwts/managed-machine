package uv

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
)

// fakeUV is a Runner that models an installed uv and the tools installed
// with it, and records every invocation.
type fakeUV struct {
	t       *testing.T
	path    string
	version string
	user    map[string]string            // the tools of the current user
	roots   map[string]map[string]string // the tools of a custom root, by its tool directory
	failOn  string
	lie     bool // the install exits 0 but installs nothing
	calls   []string
	envs    [][]string
}

// tools returns the tool map the environment selects: the custom root's
// when the environment names one, and the current user's otherwise.
func (f *fakeUV) tools(env []string) map[string]string {
	dir := value(env, "UV_TOOL_DIR")
	if dir == "" {
		return f.user
	}
	if f.roots[dir] == nil {
		f.roots[dir] = map[string]string{}
	}
	return f.roots[dir]
}

func (f *fakeUV) run(_ context.Context, path string, args, env []string) (Result, error) {
	if path != f.path {
		f.t.Fatalf("the provider started %s, not the uv it located", path)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	switch {
	case len(args) == 1 && args[0] == "--version":
		return Result{Stdout: []byte("uv " + f.version + " (0a1b2c3d 2026-01-01)\n")}, nil
	case len(args) == 2 && args[0] == "tool" && args[1] == "list":
		tools := f.tools(env)
		if len(tools) == 0 {
			return Result{Stdout: []byte("No tools installed\n")}, nil
		}
		names := make([]string, 0, len(tools))
		for name := range tools {
			names = append(names, name)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, name := range names {
			fmt.Fprintf(&b, "%s v%s\n- %s\n", name, tools[name], name)
		}
		return Result{Stdout: []byte(b.String())}, nil
	case len(args) >= 3 && args[0] == "tool" && args[1] == "install":
		name, version, ok := strings.Cut(args[len(args)-1], "==")
		if !ok {
			f.t.Fatalf("unexpected uv install %v", args)
		}
		if name == f.failOn {
			return Result{Exit: 1, Stderr: []byte("error: the tool could not be installed\n")}, nil
		}
		if !f.lie {
			f.tools(env)[name] = version
		}
		return Result{}, nil
	}
	f.t.Fatalf("unexpected uv %v", args)
	return Result{}, nil
}

// registry serves the index metadata of the distributions it knows, over
// TLS, and answers 404 for the ones it does not.
func registry(t *testing.T, versions map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/pypi/"), "/json")
		v, ok := versions[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, `{"info":{"name":%q,"version":%q}}`, name, v)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fixture(t *testing.T) (*Provider, *fakeUV) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "uv")
	if err := os.WriteFile(path, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &fakeUV{t: t, path: path, version: "0.9.4", user: map[string]string{}, roots: map[string]map[string]string{}}
	srv := registry(t, map[string]string{"ruff": "0.7.4", "black": "24.10.0"})
	p := &Provider{Run: f.run, Environ: func() []string {
		return []string{"HOME=/h", "PATH=/usr/bin", "UV_TOOL_DIR=/leak", "UV_CACHE_DIR=/leak"}
	}, LookPath: func(string) (string, error) { return path, nil }, IndexBase: srv.URL, Client: srv.Client()}
	return p, f
}

func TestPlanForUserTarget(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Package != "ruff" || res.Version != "0.7.4" || res.ProviderVersion != "0.9.4" {
		t.Fatalf("resolution: %+v", res)
	}
	if len(res.Operations) != 1 || len(res.Preconditions) != 1 {
		t.Fatalf("plan: %+v", res)
	}
	op := res.Operations[0]
	if op.Action != ActionCreate || op.Provider != Name || op.Target != "user" ||
		op.Params["package"] != "ruff" || op.Params["version"] != "0.7.4" {
		t.Fatalf("operation: %+v", op)
	}
	if _, ok := op.Params["root"]; ok {
		t.Errorf("the user target carries a root: %+v", op.Params)
	}
	c := res.Preconditions[0]
	if c.Kind != "software" || c.Subject != Name || c.Observed != "0.9.4" || !strings.HasPrefix(c.Requirement, toolRequirement) {
		t.Fatalf("precondition: %+v", c)
	}
	// Planning reads the version and the installed tools, and installs
	// nothing.
	want := "--version|tool list"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("uv argv:\n%s\nwant\n%s", got, want)
	}
}

func TestPlanForCustomTarget(t *testing.T) {
	p, f := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "custom:" + root, Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Operations) != 1 || len(res.Preconditions) != 2 {
		t.Fatalf("plan: %+v", res)
	}
	op := res.Operations[0]
	if op.Target != "custom:"+root || op.Params["root"] != root || op.Params["package"] != "ruff" || op.Params["version"] != "0.7.4" {
		t.Fatalf("operation: %+v", op)
	}
	c := res.Preconditions[1]
	if c.Kind != "privilege" || c.Subject != "custom-root" || !strings.Contains(c.Requirement, root) {
		t.Fatalf("precondition: %+v", c)
	}
	// Planning wrote nothing: the custom root is still absent.
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("planning created the custom root %s", root)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "tool install") {
			t.Errorf("planning installed: %s", call)
		}
	}
}

func TestEnvironmentIsolation(t *testing.T) {
	p, f := fixture(t)
	if _, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}}); err != nil {
		t.Fatal(err)
	}
	for _, env := range f.envs {
		joined := strings.Join(env, " ")
		for _, leak := range []string{"UV_TOOL_DIR=/leak", "UV_CACHE_DIR=/leak"} {
			if strings.Contains(joined, leak) {
				t.Errorf("the environment carries %s: %v", leak, env)
			}
		}
		if !strings.Contains(joined, "UV_NO_MODIFY_PATH=1") {
			t.Errorf("the environment does not keep uv from editing PATH: %v", env)
		}
		if strings.Contains(joined, "UV_TOOL_DIR=") || strings.Contains(joined, "UV_PYTHON_PREFERENCE=") {
			t.Errorf("the user environment carries custom root variables: %v", env)
		}
		if !strings.Contains(joined, "HOME=/h") {
			t.Errorf("the environment dropped the injected base: %v", env)
		}
	}
	p, f = fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	if _, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "custom:" + root, Options: map[string]string{"tool": "ruff"}}); err != nil {
		t.Fatal(err)
	}
	for i, call := range f.calls {
		env := f.envs[i]
		joined := strings.Join(env, " ")
		if strings.Contains(joined, "/leak") {
			t.Errorf("the custom environment carries the base variables: %v", env)
		}
		if call == "--version" {
			// The version of the tool itself does not depend on the
			// target it serves.
			continue
		}
		for _, want := range []string{
			"UV_TOOL_DIR=" + filepath.Join(root, "tools"),
			"UV_TOOL_BIN_DIR=" + filepath.Join(root, "bin"),
			"UV_CACHE_DIR=" + filepath.Join(root, "cache"),
			"UV_PYTHON_INSTALL_DIR=" + filepath.Join(root, "python"),
			"UV_PYTHON_PREFERENCE=only-managed",
		} {
			if !strings.Contains(joined, want) {
				t.Errorf("the custom environment is missing %s: %v", want, env)
			}
		}
	}
}

func TestInstalledPlansNoOperation(t *testing.T) {
	p, f := fixture(t)
	f.user["ruff"] = "0.7.4"
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil || !res.Installed || res.Version != "0.7.4" || len(res.Operations) != 0 {
		t.Fatalf("installed tool: %+v %v", res, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "tool install") {
			t.Errorf("planning installed something: %s", call)
		}
	}
}

func TestUpdatePlansAnUpdate(t *testing.T) {
	p, f := fixture(t)
	f.user["ruff"] = "0.7.3"
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil || res.Installed || res.Version != "0.7.4" {
		t.Fatalf("update plan: %+v %v", res, err)
	}
	op := res.Operations[0]
	if op.Action != ActionUpdate {
		t.Fatalf("the operation does not plan an update: %+v", op)
	}
	st, err := p.Step(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.user["ruff"] != "0.7.4" {
		t.Errorf("the update did not install the planned version: %v", f.user)
	}
	if ev, err := st.Verify(context.Background()); err != nil || ev["version"] != "0.7.4" {
		t.Fatalf("update verify: %v %v", ev, err)
	}
	want := "--version|tool list|tool install --force ruff==0.7.4|tool list"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("uv argv:\n%s\nwant\n%s", got, want)
	}
}

func TestApplyAndVerify(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := p.Step(res.Operations[0])
	if err != nil {
		t.Fatal(err)
	}
	ev, err := st.Apply(context.Background())
	if err != nil || ev["command"] != "uv tool install ruff==0.7.4" {
		t.Fatalf("apply: %v %v", ev, err)
	}
	ev, err = st.Verify(context.Background())
	if err != nil || ev["version"] != "0.7.4" {
		t.Fatalf("verify: %v %v", ev, err)
	}
	want := "--version|tool list|tool install ruff==0.7.4|tool list"
	if got := strings.Join(f.calls, "|"); got != want {
		t.Errorf("uv argv:\n%s\nwant\n%s", got, want)
	}
}

func TestVerifyRejectsAnInstallThatLied(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	f.lie = true
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted an install that installed nothing")
	}
}

func TestVerifyRequiresThePlannedVersion(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	f.user["ruff"] = "0.7.3"
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted a different version than planned")
	}
}

func TestApplyCreatesTheCustomRoot(t *testing.T) {
	p, f := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "custom:" + root, Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := p.Step(res.Operations[0])
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(root)
	if err != nil || !fi.IsDir() {
		t.Fatalf("the custom root was not created: %v", err)
	}
	// The install happened inside the root, not for the current user.
	if f.user["ruff"] != "" {
		t.Errorf("the custom install touched the user's tools: %v", f.user)
	}
	if f.roots[filepath.Join(root, "tools")]["ruff"] != "0.7.4" {
		t.Errorf("the tool is not installed in the custom root: %v", f.roots)
	}
}

func TestRejections(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		req  Request
		mod  func(*Provider)
		want error
	}{
		"global":        {Request{Software: "ruff", Target: "global", Options: map[string]string{"tool": "ruff"}}, nil, ErrUnsupported},
		"relative root": {Request{Software: "ruff", Target: "custom:root", Options: map[string]string{"tool": "ruff"}}, nil, ErrUnsupported},
		"empty root":    {Request{Software: "ruff", Target: "custom:", Options: map[string]string{"tool": "ruff"}}, nil, ErrUnsupported},
		"other option":  {Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff", "index": "pypi"}}, nil, ErrUnsupported},
		"no tool":       {Request{Software: "ruff", Target: "user", Options: map[string]string{}}, nil, ErrUnsupported},
		"bad tool name": {Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff; rm -rf /"}}, nil, ErrUnsupported},
		"unknown tool":  {Request{Software: "nope", Target: "user", Options: map[string]string{"tool": "nope"}}, nil, ErrUnsupported},
		"missing uv": {Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}},
			func(p *Provider) { p.LookPath = func(string) (string, error) { return "", errors.New("not found") } }, ErrMissing},
		"unwritable root": {Request{Software: "ruff", Target: "custom:" + root, Options: map[string]string{"tool": "ruff"}},
			func(p *Provider) { p.Writable = func(string) bool { return false } }, ErrPrivilege},
		"root is a file": {Request{Software: "ruff", Target: "custom:" + file, Options: map[string]string{"tool": "ruff"}}, nil, ErrUnsupported},
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
			if strings.HasPrefix(call, "tool install") {
				t.Errorf("%s: a rejected plan installed", name)
			}
		}
	}
}

func TestCheck(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "user", Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Preconditions[0]
	if _, err := p.Check(context.Background(), c); err != nil {
		t.Errorf("unchanged uv: %v", err)
	}
	f.version = "0.9.5"
	if _, err := p.Check(context.Background(), c); err == nil {
		t.Error("a changed uv version was accepted")
	}
	f.version = "0.9.4"
	moved := filepath.Join(t.TempDir(), "uv")
	p.LookPath = func(string) (string, error) { return moved, nil }
	if _, err := p.Check(context.Background(), c); err == nil {
		t.Error("a moved uv was accepted")
	}
}

func TestCheckOfTheCustomRoot(t *testing.T) {
	p, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	res, err := p.Plan(context.Background(), Request{Software: "ruff", Target: "custom:" + root, Options: map[string]string{"tool": "ruff"}})
	if err != nil {
		t.Fatal(err)
	}
	c := res.Preconditions[1]
	if _, err := p.Check(context.Background(), c); err != nil {
		t.Errorf("unchanged root: %v", err)
	}
	p.Writable = func(string) bool { return false }
	if _, err := p.Check(context.Background(), c); !errors.Is(err, ErrPrivilege) {
		t.Errorf("privilege check: %v", err)
	}
}

func TestStepRejectsUnexpectedOperations(t *testing.T) {
	p, _ := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	for _, bad := range []plan.Operation{
		{ID: "1", Provider: "brew", Action: ActionCreate, Target: "user", Params: map[string]string{"package": "ruff", "version": "0.7.4"}},
		{ID: "1", Provider: Name, Action: "delete", Target: "user", Params: map[string]string{"package": "ruff", "version": "0.7.4"}},
		{ID: "1", Provider: Name, Action: ActionCreate, Target: "user", Params: map[string]string{"package": "ruff", "version": "0.7.4", "args": "--HEAD"}},
		{ID: "1", Provider: Name, Action: ActionCreate, Target: "user", Params: map[string]string{"package": "ruff"}},
		{ID: "1", Provider: Name, Action: ActionCreate, Target: "global", Params: map[string]string{"package": "ruff", "version": "0.7.4"}},
		{ID: "1", Provider: Name, Action: ActionCreate, Target: "custom:root", Params: map[string]string{"package": "ruff", "version": "0.7.4", "root": "root"}},
		{ID: "1", Provider: Name, Action: ActionCreate, Target: "custom:" + root, Params: map[string]string{"package": "ruff", "version": "0.7.4"}},
	} {
		if _, err := p.Step(bad); err == nil {
			t.Errorf("an unexpected operation was accepted: %+v", bad)
		}
	}
}

func TestVersionIsReadFromTheTool(t *testing.T) {
	p, f := fixture(t)
	v, err := p.Version(context.Background())
	if err != nil || v != "0.9.4" {
		t.Fatalf("version: %q %v", v, err)
	}
	f.version = ""
	if _, err := p.Version(context.Background()); err == nil {
		t.Error("an unreadable version was accepted")
	}
}

func TestTheToolIsFoundOnThePathOfTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	name := "uv"
	if runtime.GOOS == "windows" {
		// Windows finds executables by their PATHEXT extension.
		name = "uv.exe"
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + dir, "HOME=/h", "PATHEXT=.COM;.EXE;.BAT;.CMD"}
	got, err := lookPath("uv", env)
	if err != nil || got != path {
		t.Fatalf("the lookup found %q, want %q (%v)", got, path, err)
	}
	if _, err := lookPath("absent", env); err == nil {
		t.Error("an absent tool was found")
	}
}

func TestRealAccessChecks(t *testing.T) {
	dir := t.TempDir()
	if !writable(dir) {
		t.Skip("the temporary directory is not writable here")
	}
	if !writable(filepath.Join(dir, "absent")) {
		t.Error("the nearest existing ancestor of a missing directory is not writable")
	}
}
