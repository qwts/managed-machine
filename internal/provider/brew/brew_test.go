package brew

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/codesign"
)

// fakeBrew is a Runner that models a Homebrew prefix with a set of
// installed formulae and casks, plus codesign and spctl responses, and
// records every invocation.
type fakeBrew struct {
	t             *testing.T
	prefix        string
	home          string
	appDir        string
	installed     map[string]string
	stable        map[string]string
	revision      map[string]int
	installedCask map[string]string
	caskJSON      map[string][]byte
	caskVersion   map[string]string
	caskAppName   map[string]string
	teamID        string
	spctlTeam     string
	detritus      bool
	badSig        bool
	omitBundle    bool
	failOn        string
	lie           bool // install exits 0 but installs nothing
	calls         []string
	signCalls     []string
	envs          [][]string
}

func (f *fakeBrew) run(_ context.Context, path string, args, env []string) (Result, error) {
	f.envs = append(f.envs, env)
	if path == codesign.CodesignPath || path == codesign.SpctlPath {
		f.signCalls = append(f.signCalls, path+" "+strings.Join(args, " "))
		return f.runCodesign(path, args)
	}
	if path != filepath.Join(f.prefix, "bin", "brew") {
		f.t.Fatalf("the provider started %s, not the prefix's brew", path)
	}
	f.calls = append(f.calls, strings.Join(args, " "))
	switch {
	case len(args) == 1 && args[0] == "--version":
		return Result{Stdout: []byte("Homebrew 4.4.1\n")}, nil
	case len(args) == 4 && args[0] == "info" && args[2] == "--formula":
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
	case len(args) == 4 && args[0] == "info" && args[2] == "--cask":
		token := args[3]
		raw, ok := f.caskJSON[token]
		if !ok {
			return Result{Exit: 1, Stderr: []byte("Error: Cask '" + token + "' is unavailable.\n")}, nil
		}
		return Result{Stdout: raw}, nil
	case len(args) == 3 && args[0] == "install" && args[1] == "--formula":
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
	case len(args) == 4 && args[0] == "install" && args[1] == "--cask":
		token := args[2]
		appdir := strings.TrimPrefix(args[3], "--appdir=")
		if token == f.failOn {
			return Result{Exit: 1, Stderr: []byte("Error: cask download failed\n")}, nil
		}
		if !f.lie {
			v := f.caskVersion[token]
			if v == "" {
				v = "1.4.2"
			}
			f.installedCask[token] = v
			_ = os.MkdirAll(filepath.Join(f.prefix, "Caskroom", token, v), 0o755)
			if !f.omitBundle {
				appName := f.caskAppName[token]
				if appName == "" {
					appName = "Fixture Cask.app"
				}
				_ = os.MkdirAll(filepath.Join(appdir, appName), 0o755)
			}
		}
		return Result{}, nil
	case len(args) == 4 && args[0] == "list" && args[1] == "--versions" && args[2] == "--formula":
		if v, ok := f.installed[args[3]]; ok {
			return Result{Stdout: []byte(args[3] + " " + v + "\n")}, nil
		}
		return Result{Exit: 1}, nil
	case len(args) == 4 && args[0] == "list" && args[1] == "--cask" && args[2] == "--versions":
		if v, ok := f.installedCask[args[3]]; ok {
			return Result{Stdout: []byte(args[3] + " " + v + "\n")}, nil
		}
		return Result{Exit: 1}, nil
	}
	f.t.Fatalf("unexpected brew %v", args)
	return Result{}, nil
}

func (f *fakeBrew) runCodesign(path string, args []string) (Result, error) {
	team := f.teamID
	if team == "" {
		team = "FIXTURETM1"
	}
	spctlTeam := f.spctlTeam
	if spctlTeam == "" {
		spctlTeam = team
	}
	switch {
	case path == codesign.CodesignPath && len(args) >= 1 && args[0] == "-dv":
		return Result{
			Stderr: []byte("Authority=Developer ID Application: Fixture Corp (" + team + ")\nTeamIdentifier=" + team + "\n"),
		}, nil
	case path == codesign.CodesignPath && len(args) >= 1 && args[0] == "--verify":
		if f.detritus {
			return Result{Exit: 1, Stderr: []byte("resource fork, Finder information, or similar detritus not allowed\n")}, nil
		}
		if f.badSig {
			return Result{Exit: 1, Stderr: []byte("invalid signature\n")}, nil
		}
		return Result{}, nil
	case path == codesign.SpctlPath:
		if f.badSig {
			return Result{Exit: 3, Stderr: []byte("rejected\nsource=Unnotarized Developer ID\n")}, nil
		}
		return Result{
			Stderr: []byte("accepted\nsource=Notarized Developer ID\norigin=Developer ID Application: Fixture Corp (" + spctlTeam + ")\n"),
		}, nil
	}
	f.t.Fatalf("unexpected codesign/spctl %s %v", path, args)
	return Result{}, nil
}

func mustReadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixture(t *testing.T) (*Provider, *fakeBrew) {
	t.Helper()
	prefix := t.TempDir()
	home := filepath.Join(t.TempDir(), "home")
	appDir := filepath.Join(t.TempDir(), "Applications")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin", "brew"), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "Applications"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	appFixture := mustReadFixture(t, "cask-app.json")
	pkgFixture := mustReadFixture(t, "cask-pkg.json")
	f := &fakeBrew{
		t:             t,
		prefix:        prefix,
		home:          home,
		appDir:        appDir,
		installed:     map[string]string{"git": "2.51.0"},
		stable:        map[string]string{"git": "2.51.0", "ripgrep": "14.1.1"},
		installedCask: map[string]string{},
		caskJSON: map[string][]byte{
			"fixture-cask-token": appFixture,
			"fixture-pkg-cask":   pkgFixture,
		},
		caskVersion: map[string]string{
			"fixture-cask-token": "1.4.2",
			"fixture-pkg-cask":   "2.0.0",
		},
		caskAppName: map[string]string{
			"fixture-cask-token": "Fixture Cask.app",
		},
		teamID: "FIXTURETM1",
	}
	p := &Provider{
		Prefix:  prefix,
		Run:     f.run,
		AppDir:  appDir,
		AppName: "Fixture Cask.app",
		TeamID:  "FIXTURETM1",
		Environ: func() []string { return []string{"HOME=" + home, "HOMEBREW_NO_AUTO_UPDATE=0"} },
		Writable: func(string) bool {
			return true
		},
		Owned: func(string) bool { return true },
	}
	return p, f
}

func caskOptsMap() map[string]string {
	return map[string]string{
		"cask":           "fixture-cask-token",
		"app_name":       "Fixture Cask.app",
		"team_id":        "FIXTURETM1",
		"url_hosts":      "dl.example.invalid",
		"homepage_hosts": "www.example.invalid",
	}
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
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"cask": "x", "version": "1"}},
		{ID: "1", Provider: Name, Action: "create", Params: map[string]string{"cask": "x", "version": "1", "appdir": "relative/apps"}},
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
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "privilege", Subject: "appdir"}); err != nil {
		t.Errorf("writable appdir: %v", err)
	}
	p.Writable = func(string) bool { return false }
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "privilege", Subject: "homebrew-prefix"}); !errors.Is(err, ErrPrivilege) {
		t.Errorf("privilege check: %v", err)
	}
	if _, err := p.Check(context.Background(), plan.Precondition{Kind: "privilege", Subject: "appdir"}); !errors.Is(err, ErrPrivilege) {
		t.Errorf("appdir privilege check: %v", err)
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

func TestCaskPlanAndInstallGlobalAndUser(t *testing.T) {
	if got, err := (&Provider{}).resolveAppDir("global"); err != nil || got != "/Applications" {
		t.Fatalf("default global appdir: %q %v", got, err)
	}

	for _, tc := range []struct {
		target  string
		wantPre int
		wantDir func(*fakeBrew) string
	}{
		{"global", 3, func(f *fakeBrew) string { return f.appDir }},
		{"user", 4, func(f *fakeBrew) string { return filepath.Join(f.home, "Applications") }},
	} {
		t.Run(tc.target, func(t *testing.T) {
			p, f := fixture(t)
			wantAppDir := tc.wantDir(f)
			res, err := p.Plan(context.Background(), Request{
				Software: "fixture-cask",
				Target:   tc.target,
				Options:  caskOptsMap(),
			})
			if err != nil {
				t.Fatalf("Plan(%s): %v", tc.target, err)
			}
			if res.Installed || res.Package != "fixture-cask-token" || res.Version != "1.4.2" ||
				res.ProviderVersion != "4.4.1" || len(res.Operations) != 1 || len(res.Preconditions) != tc.wantPre {
				t.Fatalf("resolution: %+v", res)
			}
			if res.Preconditions[1].Subject != "homebrew-prefix" || res.Preconditions[2].Subject != "appdir" {
				t.Fatalf("preconditions: %+v", res.Preconditions)
			}
			if tc.target == "user" && (res.Preconditions[3].Kind != "target" || res.Preconditions[3].Subject != "homebrew-prefix") {
				t.Fatalf("user preconditions: %+v", res.Preconditions)
			}
			op := res.Operations[0]
			if op.Action != "create" || len(op.Params) != 3 ||
				op.Params["cask"] != "fixture-cask-token" || op.Params["version"] != "1.4.2" || op.Params["appdir"] != wantAppDir {
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
			if err != nil || ev["version"] != "1.4.2" || ev["team_id"] != "FIXTURETM1" {
				t.Fatalf("verify: %v %v", ev, err)
			}
			wantBrew := "--version|info --json=v2 --cask fixture-cask-token|install --cask fixture-cask-token --appdir=" + wantAppDir + "|list --cask --versions fixture-cask-token"
			if got := strings.Join(f.calls, "|"); got != wantBrew {
				t.Errorf("brew argv:\n%s\nwant\n%s", got, wantBrew)
			}
			bundle := filepath.Join(wantAppDir, "Fixture Cask.app")
			wantSign := codesign.CodesignPath + " -dv --verbose=2 " + bundle + "|" + codesign.CodesignPath + " --verify --deep --strict " + bundle
			if got := strings.Join(f.signCalls, "|"); got != wantSign {
				t.Errorf("codesign argv:\n%s\nwant\n%s", got, wantSign)
			}
			for _, env := range f.envs {
				joined := strings.Join(env, " ")
				if !strings.Contains(joined, "HOMEBREW_NO_AUTO_UPDATE=1") || strings.Contains(joined, "HOMEBREW_NO_AUTO_UPDATE=0") || !strings.Contains(joined, "HOMEBREW_NO_INSTALL_CLEANUP=1") {
					t.Errorf("environment is not explicit: %v", env)
				}
			}
		})
	}
}

func TestCaskRollingURLAndDetritusFallback(t *testing.T) {
	t.Run("rolling_url_requires_spctl", func(t *testing.T) {
		p, f := fixture(t)
		f.caskJSON["fixture-cask-token"] = mutateCaskJSON(t, f.caskJSON["fixture-cask-token"], func(c map[string]any) {
			c["sha256"] = "no_check"
			c["url"] = "https://cdn.sub.example.invalid/FixtureCask.dmg"
			c["homepage"] = "https://example.invalid/"
		})
		opts := caskOptsMap()
		opts["allow_rolling_url"] = "true"
		opts["url_hosts"] = ".example.invalid"
		opts["homepage_hosts"] = "www.example.invalid"
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "user", Options: opts})
		if err != nil {
			t.Fatalf("Plan rolling cask: %v", err)
		}
		st, err := p.Step(res.Operations[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Verify(context.Background()); err != nil {
			t.Fatalf("Verify rolling cask: %v", err)
		}
		bundle := filepath.Join(f.home, "Applications", "Fixture Cask.app")
		wantSign := codesign.CodesignPath + " -dv --verbose=2 " + bundle + "|" + codesign.SpctlPath + " -a -t exec -vv " + bundle
		if got := strings.Join(f.signCalls, "|"); got != wantSign {
			t.Errorf("rolling sign calls:\n%s\nwant\n%s", got, wantSign)
		}
	})

	t.Run("detritus_falls_back_to_spctl", func(t *testing.T) {
		p, f := fixture(t)
		f.detritus = true
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil {
			t.Fatal(err)
		}
		st, err := p.Step(res.Operations[0])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Verify(context.Background()); err != nil {
			t.Fatalf("Verify detritus fallback: %v", err)
		}
		if len(f.signCalls) != 3 {
			t.Fatalf("expected 3 sign calls (inspect, verify, spctl), got %v", f.signCalls)
		}
	})
}

func TestCaskInstalledAndOccupied(t *testing.T) {
	t.Run("installed_with_caskroom_dir", func(t *testing.T) {
		p, f := fixture(t)
		if err := os.MkdirAll(filepath.Join(f.appDir, "Fixture Cask.app"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(f.prefix, "Caskroom", "fixture-cask-token", "1.4.2"), 0o755); err != nil {
			t.Fatal(err)
		}
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil || !res.Installed || res.Version != "1.4.2" || len(res.Operations) != 0 {
			t.Fatalf("installed cask: %+v %v", res, err)
		}
	})

	t.Run("installed_with_json_receipt", func(t *testing.T) {
		p, f := fixture(t)
		if err := os.MkdirAll(filepath.Join(f.appDir, "Fixture Cask.app"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.caskJSON["fixture-cask-token"] = mutateCaskJSON(t, f.caskJSON["fixture-cask-token"], func(c map[string]any) {
			c["installed"] = "1.4.2"
		})
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil || !res.Installed || res.Version != "1.4.2" || len(res.Operations) != 0 {
			t.Fatalf("installed cask via json: %+v %v", res, err)
		}
	})

	t.Run("occupied_without_receipt", func(t *testing.T) {
		p, f := fixture(t)
		if err := os.MkdirAll(filepath.Join(f.appDir, "Fixture Cask.app"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if !errors.Is(err, ErrOccupied) || !errors.Is(err, provider.ErrOccupied) {
			t.Fatalf("occupied bundle without receipt: got %v, want ErrOccupied", err)
		}
		for _, call := range f.calls {
			if strings.HasPrefix(call, "install") {
				t.Error("occupied bundle triggered brew install")
			}
		}
	})

	t.Run("occupied_by_file", func(t *testing.T) {
		p, f := fixture(t)
		if err := os.WriteFile(filepath.Join(f.appDir, "Fixture Cask.app"), []byte("not a dir"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if !errors.Is(err, ErrOccupied) || !errors.Is(err, provider.ErrOccupied) {
			t.Fatalf("occupied by file: got %v, want ErrOccupied", err)
		}
	})
}

func TestCaskPolicyAndArtifactRefusals(t *testing.T) {
	// 1. Recorded pkg cask fixture is refused with MM_SOFTWARE_CASK_REFUSED.
	t.Run("recorded_pkg_cask_fixture", func(t *testing.T) {
		p, f := fixture(t)
		opts := caskOptsMap()
		opts["cask"] = "fixture-pkg-cask"
		opts["app_name"] = "Fixture Installer.app"
		_, err := p.Plan(context.Background(), Request{Software: "fixture-pkg", Target: "global", Options: opts})
		if !errors.Is(err, ErrCaskRefused) || !errors.Is(err, provider.ErrCaskRefused) {
			t.Fatalf("pkg cask fixture: got %v, want ErrCaskRefused", err)
		}
		for _, call := range f.calls {
			if strings.HasPrefix(call, "install") {
				t.Error("refused pkg cask installed")
			}
		}
	})

	// 2. Policy and artifact-shape mutations on the recorded app-only fixture.
	cases := map[string]func(map[string]any){
		"wrong_tap": func(c map[string]any) {
			c["tap"] = "thirdparty/tap"
		},
		"wrong_token": func(c map[string]any) {
			c["token"] = "impostor-cask"
		},
		"no_check_without_rolling": func(c map[string]any) {
			c["sha256"] = "no_check"
		},
		"empty_sha256": func(c map[string]any) {
			c["sha256"] = ""
		},
		"malformed_sha256": func(c map[string]any) {
			c["sha256"] = "xyz"
		},
		"unlisted_url_host": func(c map[string]any) {
			c["url"] = "https://evil.example.invalid/Fixture.dmg"
		},
		"unlisted_homepage_host": func(c map[string]any) {
			c["homepage"] = "https://evil.example.invalid/"
		},
		"installer_artifact": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"Fixture Cask.app"}},
				map[string]any{"installer": []any{map[string]any{"manual": "Install.app"}}},
			}
		},
		"preflight_artifact": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"Fixture Cask.app"}},
				map[string]any{"preflight": nil},
			}
		},
		"postflight_artifact": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"Fixture Cask.app"}},
				map[string]any{"postflight": nil},
			}
		},
		"binary_artifact": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"Fixture Cask.app"}},
				map[string]any{"binary": []any{"fixture"}},
			}
		},
		"no_app_artifact": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"uninstall": []any{map[string]any{"quit": "invalid.example"}}},
				map[string]any{"zap": []any{map[string]any{"trash": []any{"~/Library/Caches/Fixture"}}}},
			}
		},
		"empty_artifacts": func(c map[string]any) {
			c["artifacts"] = []any{}
		},
		"relocated_app_target": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"Fixture Cask.app"}, "target": "/Applications/Utilities/Fixture Cask.app"},
			}
		},
		"target_outside_app_stanza": func(c map[string]any) {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"Fixture Cask.app"}},
				map[string]any{"target": "/Applications/Fixture Cask.app"},
			}
		},
		"missing_version": func(c map[string]any) {
			c["version"] = ""
		},
	}

	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			p, f := fixture(t)
			f.caskJSON["fixture-cask-token"] = mutateCaskJSON(t, f.caskJSON["fixture-cask-token"], mut)
			_, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
			if !errors.Is(err, ErrCaskRefused) || !errors.Is(err, provider.ErrCaskRefused) {
				t.Fatalf("%s: got %v, want ErrCaskRefused", name, err)
			}
			for _, call := range f.calls {
				if strings.HasPrefix(call, "install") {
					t.Errorf("%s: a refused cask installed", name)
				}
			}
		})
	}
}

// TestCaskPolicyAcceptsDefaultAppTarget covers the shape brew info
// --json=v2 records for a plain app cask (telegram, discord): the app stanza
// carries its default "target" path beside the app name.
func TestCaskPolicyAcceptsDefaultAppTarget(t *testing.T) {
	p, f := fixture(t)
	f.caskJSON["fixture-cask-token"] = mutateCaskJSON(t, f.caskJSON["fixture-cask-token"], func(c map[string]any) {
		c["artifacts"] = []any{
			map[string]any{"uninstall": []any{map[string]any{"quit": "invalid.example"}}},
			map[string]any{"app": []any{"Fixture Cask.app"}, "target": "/Applications/Fixture Cask.app"},
			map[string]any{"zap": []any{map[string]any{"trash": []any{"~/Library/Caches/Fixture"}}}},
		}
	})
	if _, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()}); err != nil {
		t.Fatalf("app stanza with its default target: %v", err)
	}
}

func TestCaskOptionAndPreconditionRejections(t *testing.T) {
	cases := map[string]struct {
		target string
		modOpt func(map[string]string)
		modP   func(*Provider, *fakeBrew)
		want   error
	}{
		"missing_app_name": {
			"global",
			func(m map[string]string) { delete(m, "app_name") },
			nil,
			ErrUnsupported,
		},
		"bad_app_name_no_suffix": {
			"global",
			func(m map[string]string) { m["app_name"] = "Fixture Cask" },
			nil,
			ErrUnsupported,
		},
		"bad_app_name_slash": {
			"global",
			func(m map[string]string) { m["app_name"] = "../Fixture Cask.app" },
			nil,
			ErrUnsupported,
		},
		"missing_team_id": {
			"global",
			func(m map[string]string) { delete(m, "team_id") },
			nil,
			ErrUnsupported,
		},
		"bad_team_id_short": {
			"global",
			func(m map[string]string) { m["team_id"] = "SHORT123" },
			nil,
			ErrUnsupported,
		},
		"bad_team_id_lowercase": {
			"global",
			func(m map[string]string) { m["team_id"] = "fixturetm1" },
			nil,
			ErrUnsupported,
		},
		"missing_url_hosts": {
			"global",
			func(m map[string]string) { delete(m, "url_hosts") },
			nil,
			ErrUnsupported,
		},
		"missing_homepage_hosts": {
			"global",
			func(m map[string]string) { delete(m, "homepage_hosts") },
			nil,
			ErrUnsupported,
		},
		"bad_allow_rolling_url": {
			"global",
			func(m map[string]string) { m["allow_rolling_url"] = "yes" },
			nil,
			ErrUnsupported,
		},
		"unknown_option": {
			"global",
			func(m map[string]string) { m["extra"] = "1" },
			nil,
			ErrUnsupported,
		},
		"custom_target": {
			"custom:/souls/bob",
			nil,
			nil,
			ErrUnsupported,
		},
		"user_no_home": {
			"user",
			nil,
			func(p *Provider, _ *fakeBrew) { p.Environ = func() []string { return nil } },
			ErrUnsupported,
		},
		"user_shared_prefix": {
			"user",
			nil,
			func(p *Provider, _ *fakeBrew) { p.Owned = func(string) bool { return false } },
			ErrUnsupported,
		},
		"absent_appdir": {
			"global",
			nil,
			func(p *Provider, f *fakeBrew) { p.AppDir = filepath.Join(f.appDir, "absent") },
			ErrPrivilege,
		},
		"unwritable_appdir": {
			"global",
			nil,
			func(p *Provider, f *fakeBrew) {
				p.Writable = func(path string) bool { return path == p.Prefix }
			},
			ErrPrivilege,
		},
		"unwritable_prefix": {
			"global",
			nil,
			func(p *Provider, _ *fakeBrew) {
				p.Writable = func(string) bool { return false }
			},
			ErrPrivilege,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p, f := fixture(t)
			opts := caskOptsMap()
			if tc.modOpt != nil {
				tc.modOpt(opts)
			}
			if tc.modP != nil {
				tc.modP(p, f)
			}
			if _, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: tc.target, Options: opts}); !errors.Is(err, tc.want) {
				t.Fatalf("%s: got %v, want %v", name, err, tc.want)
			}
		})
	}
}

func TestCaskVerifyFailures(t *testing.T) {
	t.Run("apply_failure", func(t *testing.T) {
		p, f := fixture(t)
		f.failOn = "fixture-cask-token"
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := p.Step(res.Operations[0])
		if _, err := st.Apply(context.Background()); err == nil || !strings.Contains(err.Error(), "cask download failed") {
			t.Fatalf("expected apply failure, got %v", err)
		}
	})

	t.Run("verify_missing_bundle", func(t *testing.T) {
		p, f := fixture(t)
		f.omitBundle = true
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := p.Step(res.Operations[0])
		if _, err := st.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Verify(context.Background()); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("expected missing bundle failure, got %v", err)
		}
	})

	t.Run("verify_wrong_team_id", func(t *testing.T) {
		p, f := fixture(t)
		f.teamID = "WRONGTEAM9"
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := p.Step(res.Operations[0])
		if _, err := st.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err = st.Verify(context.Background())
		if !errors.Is(err, ErrSignatureRejected) || !errors.Is(err, provider.ErrSignatureRejected) {
			t.Fatalf("wrong team_id: got %v, want ErrSignatureRejected", err)
		}
	})

	t.Run("verify_bad_signature", func(t *testing.T) {
		p, f := fixture(t)
		f.badSig = true
		res, err := p.Plan(context.Background(), Request{Software: "fixture-cask", Target: "global", Options: caskOptsMap()})
		if err != nil {
			t.Fatal(err)
		}
		st, _ := p.Step(res.Operations[0])
		if _, err := st.Apply(context.Background()); err != nil {
			t.Fatal(err)
		}
		_, err = st.Verify(context.Background())
		if !errors.Is(err, ErrSignatureRejected) || !errors.Is(err, provider.ErrSignatureRejected) {
			t.Fatalf("bad signature: got %v, want ErrSignatureRejected", err)
		}
	})
}

func mutateCaskJSON(t *testing.T, raw []byte, mut func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	casks := doc["casks"].([]any)
	mut(casks[0].(map[string]any))
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
