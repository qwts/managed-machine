package winget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/provider"
)

// fakeWinget is a Runner that models the packages winget knows and the ones
// installed, and records every invocation.
type fakeWinget struct {
	t         *testing.T
	path      string
	latest    map[string]string
	installed map[string]string
	failOn    string
	lie       bool // install exits 0 but installs nothing
	calls     []string
	envs      [][]string
}

func (f *fakeWinget) run(_ context.Context, path string, args, env []string) (Result, error) {
	if path != f.path {
		f.t.Fatalf("the provider started %s, not the resolved winget", path)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	f.envs = append(f.envs, env)
	id := argValue(args, "--id")
	switch args[0] {
	case "--version":
		return Result{Stdout: []byte("v1.9.25200\r\n")}, nil
	case "show":
		v, ok := f.latest[id]
		if !ok {
			return Result{Exit: 0x8A150014, Stdout: []byte("No package found matching input criteria.\r\n")}, nil
		}
		return Result{Stdout: []byte(fmt.Sprintf("Progress: 10%%\rProgress: 100%%\rFound %s [%s]\r\nVersion: %s\r\nPublisher: Test\r\n", id, id, v))}, nil
	case "list":
		v, ok := f.installed[id]
		if !ok {
			return Result{Exit: 0x8A150014, Stdout: []byte("No installed package found matching input criteria.\r\n")}, nil
		}
		return Result{Stdout: []byte(listTable(id, v))}, nil
	case "install", "upgrade":
		if id == f.failOn {
			return Result{Exit: 0x8A150014, Stdout: []byte("Installer failed with exit code: 0x8A150014\r\n")}, nil
		}
		if !f.lie {
			f.installed[id] = f.latest[id]
		}
		return Result{}, nil
	}
	f.t.Fatalf("unexpected winget %v", args)
	return Result{}, nil
}

// argValue is the value of an option in an argument vector.
func argValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// listTable is the table `winget list` prints: a header, a line of dashes and
// the rows.
func listTable(id, version string) string {
	name := id
	if i := strings.LastIndex(id, "."); i >= 0 {
		name = id[i+1:]
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("%-24s %-24s %-12s %-12s %s\n", "Name", "Id", "Version", "Available", "Source"))
	b.WriteString(strings.Repeat("-", 90) + "\n")
	b.WriteString(fmt.Sprintf("%-24s %-24s %-12s %-12s %s\n", name, id, version, version, "winget"))
	return b.String()
}

// want is the argument vector of an invocation, with the common arguments.
func want(args ...string) string {
	return strings.Join(argv(args...), " ")
}

func fixture(t *testing.T) (*Provider, *fakeWinget) {
	dir := t.TempDir()
	for _, name := range []string{"winget", "winget.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("fake"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeWinget{t: t, path: filepath.Join(dir, toolNames()[0]),
		latest:    map[string]string{"Microsoft.PowerShell": "7.4.6", "Git.Git": "2.51.0"},
		installed: map[string]string{}}
	p := &Provider{
		Run:     f.run,
		Environ: func() []string { return []string{"HOME=/h", "PATH=" + dir} },
		LookPath: func(file string) (string, error) {
			if _, err := os.Stat(file); err != nil {
				return "", err
			}
			return file, nil
		},
		Elevated: func() bool { return false },
	}
	return p, f
}

func TestPlanAndInstall(t *testing.T) {
	p, f := fixture(t)
	res, err := p.Plan(context.Background(), Request{Software: "powershell", Target: "user", Options: map[string]string{"package_id": "Microsoft.PowerShell"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || res.Package != "Microsoft.PowerShell" || res.Version != "7.4.6" || res.ProviderVersion != "1.9.25200" ||
		len(res.Operations) != 1 || len(res.Preconditions) != 1 {
		t.Fatalf("resolution: %+v", res)
	}
	pre := res.Preconditions[0]
	if pre.Kind != "software" || pre.Subject != Name || pre.Requirement != "present at "+f.path || pre.Observed != "1.9.25200" {
		t.Fatalf("precondition: %+v", pre)
	}
	op := res.Operations[0]
	if op.Action != "create" || op.Provider != Name || op.Target != "user" || op.Operation != "software:powershell" ||
		op.Params["package"] != "Microsoft.PowerShell" || op.Params["version"] != "7.4.6" || op.Params["scope"] != "user" {
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
	if err != nil || ev["version"] != "7.4.6" {
		t.Fatalf("verify: %v %v", ev, err)
	}
	wants := []string{
		want("--version"),
		want("show", "--id", "Microsoft.PowerShell", "--exact"),
		want("list", "--id", "Microsoft.PowerShell", "--exact"),
		want("install", "--id", "Microsoft.PowerShell", "--exact", "--version", "7.4.6", "--scope", "user", "--silent", "--accept-package-agreements"),
		want("list", "--id", "Microsoft.PowerShell", "--exact"),
	}
	if got := strings.Join(f.calls, "|"); got != strings.Join(wants, "|") {
		t.Errorf("winget argv:\n%s\nwant\n%s", got, strings.Join(wants, "|"))
	}
}

func TestPlanGlobalNeedsAnElevatedProcess(t *testing.T) {
	p, f := fixture(t)
	_, err := p.Plan(context.Background(), Request{Software: "git", Target: "global", Options: map[string]string{"package_id": "Git.Git"}})
	if !errors.Is(err, provider.ErrPrivilege) {
		t.Fatalf("global target in a plain process: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a refused plan started %v", f.calls)
	}
	p.Elevated = func() bool { return true }
	res, err := p.Plan(context.Background(), Request{Software: "git", Target: "global", Options: map[string]string{"package_id": "Git.Git"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Operations[0].Params["scope"] != "machine" || len(res.Preconditions) != 2 {
		t.Fatalf("global resolution: %+v", res)
	}
	if res.Preconditions[1].Kind != "privilege" || res.Preconditions[1].Subject != "winget-scope" {
		t.Fatalf("global precondition: %+v", res.Preconditions[1])
	}
}

func TestInstalledPlansNoOperation(t *testing.T) {
	p, f := fixture(t)
	f.installed["Git.Git"] = "2.51.0"
	res, err := p.Plan(context.Background(), Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git"}})
	if err != nil || !res.Installed || res.Version != "2.51.0" || len(res.Operations) != 0 {
		t.Fatalf("installed package: %+v %v", res, err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "install") || strings.HasPrefix(call, "upgrade") {
			t.Error("planning installed something")
		}
	}
}

func TestAnotherInstalledVersionPlansAnUpdate(t *testing.T) {
	p, f := fixture(t)
	f.installed["Git.Git"] = "2.50.0"
	res, err := p.Plan(context.Background(), Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Installed || len(res.Operations) != 1 {
		t.Fatalf("update resolution: %+v", res)
	}
	op := res.Operations[0]
	if op.Action != "update" || op.Params["package"] != "Git.Git" || op.Params["version"] != "2.51.0" || len(op.Params) != 2 {
		t.Fatalf("update operation: %+v", op)
	}
	st, err := p.Step(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ev, err := st.Verify(context.Background()); err != nil || ev["version"] != "2.51.0" {
		t.Fatalf("update verify: %v %v", ev, err)
	}
	wantCall := want("upgrade", "--id", "Git.Git", "--exact", "--version", "2.51.0", "--silent", "--accept-package-agreements")
	if f.calls[len(f.calls)-2] != wantCall {
		t.Errorf("upgrade argv:\n%s\nwant\n%s", f.calls[len(f.calls)-2], wantCall)
	}
}

func TestEnvironmentIsExplicitAndUnchanged(t *testing.T) {
	for _, target := range []string{"user", "global"} {
		p, f := fixture(t)
		p.Elevated = func() bool { return true }
		base := []string{"HOME=/h", "PATH=" + filepath.Dir(f.path)}
		p.Environ = func() []string { return base }
		if _, err := p.Plan(context.Background(), Request{Software: "git", Target: target, Options: map[string]string{"package_id": "Git.Git"}}); err != nil {
			t.Fatal(err)
		}
		if len(f.envs) == 0 {
			t.Fatal("no process was started")
		}
		for _, env := range f.envs {
			if len(env) != len(base) {
				t.Fatalf("%s: the environment is not the injected one: %v", target, env)
			}
			for i, kv := range base {
				if env[i] != kv {
					t.Fatalf("%s: the environment is not the injected one: %v", target, env)
				}
			}
		}
		if len(base) != 2 || base[0] != "HOME=/h" {
			t.Fatalf("%s: the injected environment was changed: %v", target, base)
		}
	}
}

func TestRejections(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	readonly := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(readonly, 0o555); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		req  Request
		mod  func(*Provider)
		want error
	}{
		"custom root":     {Request{Software: "git", Target: "custom:" + root, Options: map[string]string{"package_id": "Git.Git"}}, nil, ErrUnsupported},
		"unwritable root": {Request{Software: "git", Target: "custom:" + readonly, Options: map[string]string{"package_id": "Git.Git"}}, nil, ErrUnsupported},
		"relative root":   {Request{Software: "git", Target: "custom:relative/root", Options: map[string]string{"package_id": "Git.Git"}}, nil, ErrUnsupported},
		"unknown target":  {Request{Software: "git", Target: "somewhere", Options: map[string]string{"package_id": "Git.Git"}}, nil, ErrUnsupported},
		"other option":    {Request{Software: "git", Target: "user", Options: map[string]string{"package": "Git.Git"}}, nil, ErrUnsupported},
		"extra option":    {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git", "version": "2.51.0"}}, nil, ErrUnsupported},
		"no package":      {Request{Software: "git", Target: "user", Options: map[string]string{}}, nil, ErrUnsupported},
		"bad package id":  {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git"}}, nil, ErrUnsupported},
		"unsafe id":       {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git; rm -rf /"}}, nil, ErrUnsupported},
		"unknown":         {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "No.Such.Package"}}, nil, ErrUnsupported},
		"not elevated":    {Request{Software: "git", Target: "global", Options: map[string]string{"package_id": "Git.Git"}}, nil, ErrPrivilege},
		"missing tool": {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git"}}, func(p *Provider) {
			p.Environ = func() []string { return []string{"HOME=/h", "PATH=" + filepath.Join(root, "absent")} }
		}, ErrMissing},
		"no PATH": {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git"}}, func(p *Provider) { p.Environ = func() []string { return []string{"HOME=/h"} } }, ErrMissing},
		"no tool": {Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git"}}, func(p *Provider) {
			p.LookPath = func(string) (string, error) { return "", os.ErrNotExist }
		}, ErrMissing},
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
			if strings.HasPrefix(call, "install") || strings.HasPrefix(call, "upgrade") {
				t.Errorf("%s: a rejected plan installed", name)
			}
		}
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("a refused plan created %s", root)
	}
}

func TestPlanningWritesNothing(t *testing.T) {
	p, f := fixture(t)
	root := filepath.Join(t.TempDir(), "root")
	if _, err := p.Plan(context.Background(), Request{Software: "git", Target: "custom:" + root, Options: map[string]string{"package_id": "Git.Git"}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("custom root: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("planning created %s", root)
	}
	if _, err := p.Plan(context.Background(), Request{Software: "git", Target: "user", Options: map[string]string{"package_id": "Git.Git"}}); err != nil {
		t.Fatal(err)
	}
	for _, call := range f.calls {
		if strings.HasPrefix(call, "install") || strings.HasPrefix(call, "upgrade") {
			t.Errorf("planning installed something: %s", call)
		}
	}
}

func TestFailuresAndVerification(t *testing.T) {
	p, f := fixture(t)
	f.failOn = "Git.Git"
	st, err := p.Step(plan.Operation{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "0x8A150014") {
		t.Errorf("install failure: %v", err)
	}
	p, f = fixture(t)
	f.lie = true
	st, _ = p.Step(plan.Operation{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user"}})
	if _, err := st.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted an install that installed nothing")
	}
	p, f = fixture(t)
	f.installed["Git.Git"] = "2.50.0"
	st, _ = p.Step(plan.Operation{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user"}})
	if _, err := st.Verify(context.Background()); err == nil {
		t.Error("verification accepted a different version than planned")
	}
	for _, bad := range []plan.Operation{
		{ID: "1", Provider: "uv", Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user"}},
		{ID: "1", Provider: Name, Action: "delete", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user", "args": "--HEAD"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "elsewhere"}},
		{ID: "1", Provider: Name, Action: "update", Params: map[string]string{"package": "Git.Git", "version": "2.51.0", "scope": "user"}},
		{ID: "1", Provider: Name, Action: "update", Params: map[string]string{"package": "Git.Git"}},
	} {
		if _, err := p.Step(bad); err == nil {
			t.Errorf("an unexpected operation was accepted: %+v", bad)
		}
	}
}

func TestCheck(t *testing.T) {
	p, f := fixture(t)
	pre := plan.Precondition{Kind: "software", Subject: Name, Requirement: "present at " + f.path, Observed: "1.9.25200"}
	if _, err := p.Check(context.Background(), pre); err != nil {
		t.Errorf("unchanged winget: %v", err)
	}
	changed := pre
	changed.Observed = "1.9.25000"
	if _, err := p.Check(context.Background(), changed); err == nil {
		t.Error("a changed winget version was accepted")
	}
	moved := pre
	moved.Requirement = "present at " + filepath.Join(filepath.Dir(f.path), "elsewhere")
	if _, err := p.Check(context.Background(), moved); err == nil {
		t.Error("a moved winget was accepted")
	}
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "privilege", Subject: "winget-scope"}); !errors.Is(err, ErrPrivilege) {
		t.Errorf("privilege check: %v", err)
	}
	p.Elevated = func() bool { return true }
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "privilege", Subject: "winget-scope"}); err != nil {
		t.Errorf("elevated privilege check: %v", err)
	}
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "target", Subject: "winget"}); err == nil {
		t.Error("an unknown precondition was accepted")
	}
}

func TestTableAndProgressParsing(t *testing.T) {
	lines := cleanLines([]byte("\rProgress: 10%\rProgress: 100%\rName Id Version Available Source\r\n" +
		"----------------------------------------------------------------------------------------------\r\n" +
		"Git Git.Git 2.50.0 2.51.0 winget\r\n" +
		"PowerShell Microsoft.PowerShell 7.4.6 7.4.6 winget\r\n"))
	if got := listVersion(lines, "Git.Git"); got != "2.50.0" {
		t.Errorf("the version of another package was read: %q", got)
	}
	if got := listVersion(lines, "Microsoft.PowerShell"); got != "7.4.6" {
		t.Errorf("the version of the package was not read: %q", got)
	}
	if got := listVersion(lines, "No.Such.Package"); got != "" {
		t.Errorf("an absent package was read as installed: %q", got)
	}
	if got := listVersion(cleanLines([]byte("No installed package found matching input criteria.\n")), "Git.Git"); got != "" {
		t.Errorf("the empty result was read as installed: %q", got)
	}
}
