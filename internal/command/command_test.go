package command_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/target"
)

func TestParseAndUsageErr(t *testing.T) {
	var (
		pos      string
		flagBool bool
		val      string
		optVal   string
		hasVal   bool
		hasOpt   bool
		count    int
	)
	reset := func() {
		pos = ""
		flagBool = false
		val = ""
		optVal = ""
		hasVal = false
		hasOpt = false
		count = 0
	}
	spec := func() command.Spec {
		return command.Spec{
			Positional:     &pos,
			PositionalName: "software name",
			Bools: map[string]*bool{
				"--available": &flagBool,
			},
			Values: map[string]command.Flag{
				"--provider": {Value: &val, Set: &hasVal, Count: &count},
				"--custom":   {Value: &optVal, Set: &hasOpt, Optional: true},
			},
		}
	}

	customRoot := filepath.Join(t.TempDir(), "root")
	reset()
	if dg := command.Parse([]string{"jq", "--available", "--provider", "brew", "--custom", customRoot}, spec()); dg != nil {
		t.Fatalf("unexpected diagnostic: %+v", dg)
	}
	if pos != "jq" || !flagBool || val != "brew" || !hasVal || count != 1 || optVal != customRoot || !hasOpt {
		t.Fatalf("got pos=%q flagBool=%v val=%q hasVal=%v count=%d optVal=%q hasOpt=%v",
			pos, flagBool, val, hasVal, count, optVal, hasOpt)
	}

	reset()
	if dg := command.Parse([]string{"--provider=uv", "--custom="}, spec()); dg != nil {
		t.Fatalf("unexpected diagnostic on equals syntax: %+v", dg)
	}
	if val != "uv" || !hasVal || count != 1 || optVal != "" || !hasOpt {
		t.Fatalf("got val=%q hasVal=%v count=%d optVal=%q hasOpt=%v", val, hasVal, count, optVal, hasOpt)
	}

	reset()
	if dg := command.Parse([]string{"--custom", "--available"}, spec()); dg != nil {
		t.Fatalf("unexpected diagnostic when optional flag precedes bool flag: %+v", dg)
	}
	if !hasOpt || optVal != "" || !flagBool {
		t.Fatalf("got hasOpt=%v optVal=%q flagBool=%v", hasOpt, optVal, flagBool)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--provider"}, "--provider needs a value"},
		{[]string{"--provider", "--available"}, "--provider needs a value"},
		{[]string{"--provider="}, "--provider needs a value"},
		{[]string{"--unknown"}, `unknown option "--unknown"`},
		{[]string{"jq", "ripgrep"}, `at most one software name; got "jq" and "ripgrep"`},
	} {
		reset()
		dg := command.Parse(tc.args, spec())
		if dg == nil || dg.ID != diag.Usage || dg.Message != tc.want {
			t.Errorf("Parse(%v) = %+v, want MM_USAGE %q", tc.args, dg, tc.want)
		}
	}

	dg := command.Parse([]string{"extra"}, command.Spec{})
	if dg == nil || dg.ID != diag.Usage || dg.Message != `unexpected argument "extra"` {
		t.Fatalf("Parse without positional = %+v, want unexpected argument", dg)
	}
}

func TestTable(t *testing.T) {
	if got := command.Table(nil); got != "" {
		t.Fatalf("Table(nil) = %q, want empty", got)
	}
	rows := [][]string{
		{"ID", "CATEGORIES", "TARGET"},
		{"jq", "cli,json", "-"},
		{"ripgrep", "cli", "user"},
	}
	got := command.Table(rows)
	want := strings.Join([]string{
		"ID       CATEGORIES  TARGET",
		"jq       cli,json    -",
		"ripgrep  cli         user",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("Table =\n%q\nwant\n%q", got, want)
	}
}

func TestDetectHost(t *testing.T) {
	host, dg := command.DetectHost(cli.Context{
		Detect: func() (target.Host, error) {
			return target.Host{Target: target.LinuxARM64}, nil
		},
	})
	if dg != nil || host.Target != target.LinuxARM64 {
		t.Fatalf("DetectHost success = (%+v, %+v)", host, dg)
	}

	_, dg = command.DetectHost(cli.Context{
		Detect: func() (target.Host, error) {
			return target.Host{}, &target.UnsupportedError{OS: "freebsd", Arch: "amd64"}
		},
	})
	if dg == nil || dg.ID != diag.PlatformUnsupported {
		t.Fatalf("DetectHost unsupported = %+v, want MM_PLATFORM_UNSUPPORTED", dg)
	}

	_, dg = command.DetectHost(cli.Context{
		Detect: func() (target.Host, error) {
			return target.Host{}, errors.New("probe boom")
		},
	})
	if dg == nil || dg.ID != diag.PlatformProbeFailed {
		t.Fatalf("DetectHost probe error = %+v, want MM_PLATFORM_PROBE_FAILED", dg)
	}
}

func TestDepsGettersAndWritability(t *testing.T) {
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state")
	homePath := filepath.Join(tmp, "home")
	if err := os.MkdirAll(homePath, 0o755); err != nil {
		t.Fatal(err)
	}
	fixedNow := time.Date(2026, 10, 24, 15, 0, 0, 500000000, time.UTC)

	env := map[string]string{
		"PATH":                      filepath.Join(tmp, "bin"),
		"PATHEXT":                   ".EXE;.CMD",
		"HOME":                      homePath,
		"USERPROFILE":               filepath.Join(tmp, "winhome"),
		"MANAGED_MACHINE_STATE_DIR": statePath,
	}
	d := command.Deps{
		Getenv:    func(k string) string { return env[k] },
		GOOS:      "darwin",
		Account:   func() (string, error) { return "alice", nil },
		Username:  func() (string, error) { return `CORP\bob`, nil },
		Machine:   func(context.Context) (string, error) { return "machine-42", nil },
		Now:       func() time.Time { return fixedNow },
		NewPlanID: func(time.Time) (string, error) { return "plan-20261024T150000Z-0001", nil },
	}

	if got := d.Env("HOME"); got != homePath {
		t.Fatalf("Env(HOME) = %q, want %q", got, homePath)
	}
	if got := d.EnvironVars(); !reflect.DeepEqual(got, []string{"PATH=" + env["PATH"], "PATHEXT=.EXE;.CMD"}) {
		t.Fatalf("EnvironVars() = %v", got)
	}
	if got := d.OS("linux/arm64"); got != "darwin" {
		t.Fatalf("OS with explicit GOOS = %q, want darwin", got)
	}
	if got := (command.Deps{}).OS("windows/amd64"); got != "windows" {
		t.Fatalf("OS from detected target = %q, want windows", got)
	}
	if got, err := d.ResolveStateDir("darwin"); err != nil || got != statePath {
		t.Fatalf("ResolveStateDir = (%q, %v), want %q", got, err, statePath)
	}
	if got, err := d.ResolveAccount(); err != nil || got != "alice" {
		t.Fatalf("ResolveAccount = (%q, %v), want alice", got, err)
	}
	if got, err := d.ResolveUsername("windows"); err != nil || got != "bob" {
		t.Fatalf("ResolveUsername(windows) = (%q, %v), want bob", got, err)
	}
	if got, err := d.ResolveUsername("darwin"); err != nil || got != `CORP\bob` {
		t.Fatalf("ResolveUsername(darwin) = (%q, %v), want CORP\\bob", got, err)
	}
	if got, err := d.ResolveHome("darwin"); err != nil || got != homePath {
		t.Fatalf("ResolveHome(darwin) = (%q, %v), want %q", got, err, homePath)
	}
	if got, err := d.ResolveHome("windows"); err != nil || got != filepath.Clean(env["USERPROFILE"]) {
		t.Fatalf("ResolveHome(windows) = (%q, %v), want %q", got, err, filepath.Clean(env["USERPROFILE"]))
	}
	if got, err := d.ResolveMachine(context.Background()); err != nil || got != "machine-42" {
		t.Fatalf("ResolveMachine = (%q, %v), want machine-42", got, err)
	}
	if got, err := (command.Deps{}).ResolveMachine(context.Background()); err != nil || got != "local" {
		t.Fatalf("default ResolveMachine = (%q, %v), want local", got, err)
	}
	if got := d.NowRFC3339(); got != "2026-10-24T15:00:00Z" {
		t.Fatalf("NowRFC3339 = %q, want 2026-10-24T15:00:00Z", got)
	}
	if got, err := d.PlanID(fixedNow); err != nil || got != "plan-20261024T150000Z-0001" {
		t.Fatalf("PlanID = (%q, %v)", got, err)
	}
	if got, err := command.NewPlanID(fixedNow); err != nil || !strings.HasPrefix(got, "plan-20261024T150000Z-") {
		t.Fatalf("NewPlanID = (%q, %v)", got, err)
	}

	destInTmp := filepath.Join(tmp, "nested", "dir", "file.txt")
	if !d.IsWritable(destInTmp) {
		t.Fatalf("expected %s to be writable via existing ancestor %s", destInTmp, tmp)
	}
	if !d.IsWritableDir(filepath.Join(tmp, "nested")) {
		t.Fatalf("expected %s to be a writable directory via existing ancestor %s", filepath.Join(tmp, "nested"), tmp)
	}
	blocker := filepath.Join(tmp, "regular.txt")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d.IsWritable(filepath.Join(blocker, "child.txt")) {
		t.Fatalf("expected child under regular file %s not to be writable", blocker)
	}
	if d.IsWritableDir(blocker) {
		t.Fatalf("expected regular file %s not to be a writable directory", blocker)
	}
}

func TestBindingsAndMachineAdapter(t *testing.T) {
	b := command.Bindings(
		"machine-1",
		"darwin/arm64",
		"sha256:1111",
		[]plan.Asset{
			{Name: "b", SHA256: "2"},
			{Name: "a", SHA256: "1"},
			{Name: "b", SHA256: "2-updated"},
		},
		[]plan.Provider{
			{Name: "template", Version: "1"},
			{Name: "file", Version: "1"},
			{Name: "template", Version: "1"},
		},
	)
	wantAssets := []plan.Asset{
		{Name: "a", SHA256: "1"},
		{Name: "b", SHA256: "2-updated"},
	}
	wantProviders := []plan.Provider{
		{Name: "file", Version: "1"},
		{Name: "template", Version: "1"},
	}
	if !reflect.DeepEqual(b.Assets, wantAssets) {
		t.Fatalf("Bindings.Assets = %+v, want %+v", b.Assets, wantAssets)
	}
	if !reflect.DeepEqual(b.Providers, wantProviders) {
		t.Fatalf("Bindings.Providers = %+v, want %+v", b.Providers, wantProviders)
	}

	var empty command.Machine
	if got, err := empty.Bindings(context.Background(), nil); err != nil || !reflect.DeepEqual(got, plan.Bindings{}) {
		t.Fatalf("empty Machine.Bindings = (%+v, %v)", got, err)
	}
	if got, err := empty.Check(context.Background(), plan.Precondition{Observed: "ok"}); err != nil || got != "ok" {
		t.Fatalf("empty Machine.Check = (%q, %v)", got, err)
	}
	if _, err := empty.Step(plan.Operation{Provider: "file"}); err == nil {
		t.Fatalf("expected error from empty Machine.Step")
	}

	called := false
	m := &command.Machine{
		BindingsFunc: func(context.Context, *plan.Document) (plan.Bindings, error) {
			return b, nil
		},
		CheckFunc: func(_ context.Context, c plan.Precondition) (string, error) {
			return c.Requirement, nil
		},
		StepFunc: func(op plan.Operation) (execute.Step, error) {
			called = true
			return nil, nil
		},
	}
	if got, err := m.Bindings(context.Background(), nil); err != nil || got.Machine != "machine-1" {
		t.Fatalf("Machine.Bindings = (%+v, %v)", got, err)
	}
	if got, err := m.Check(context.Background(), plan.Precondition{Requirement: "darwin/arm64"}); err != nil || got != "darwin/arm64" {
		t.Fatalf("Machine.Check = (%q, %v)", got, err)
	}
	if _, err := m.Step(plan.Operation{}); err != nil || !called {
		t.Fatalf("Machine.Step = %v, called=%v", err, called)
	}
}

// TestInputsDigestUnchanged verifies that extracting internal/command.Digest
// preserves the exact NUL-separated SHA-256 inputs_digest values across
// install, config and fleet (S7 N04 acceptance).
func TestInputsDigestUnchanged(t *testing.T) {
	legacyDigest := func(parts ...string) string {
		h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
		return "sha256:" + hex.EncodeToString(h[:])
	}

	// 1. Software install inputs_digest.
	installReq := install.Request{
		Entry: source.Entry{
			Software: definition.Software{ID: "jq"},
			Digest:   "aaaa1111",
		},
		Recipe: definition.Recipe{
			ID:       "brew",
			Provider: "brew",
			Package:  "jq",
			Options: map[string]any{
				"formula": "jq",
				"tap":     "homebrew/core",
			},
		},
		Target:   "global",
		Platform: "darwin/arm64",
	}
	gotInstall := install.InputsDigest(installReq)
	wantInstall := legacyDigest(
		"software=jq",
		"recipe=brew",
		"provider=brew",
		"package=jq",
		"target=global",
		"platform=darwin/arm64",
		"definition=aaaa1111",
		"option.formula=jq",
		"option.tap=homebrew/core",
	)
	const wantInstallPinned = "sha256:8e286080d53a916d66f3d19f87ce481f6d462acab75eb42c377ab12725ae0510"
	if wantInstall != wantInstallPinned {
		t.Fatalf("precomputed install digest = %s, want %s", wantInstall, wantInstallPinned)
	}
	if gotInstall != wantInstallPinned {
		t.Fatalf("install.InputsDigest = %s, want %s", gotInstall, wantInstallPinned)
	}

	// 2. Configuration inputs_digest.
	gotConfig := config.InputsDigest(
		"gitconfig",
		"user",
		"darwin/arm64",
		config.Resolved{
			Inputs: map[string]string{
				"email": "alice@example.com",
				"name":  "Alice",
			},
			Destinations: map[string]string{
				"gitconfig": ".gitconfig",
			},
		},
		[]string{"workstation"},
	)
	wantConfig := legacyDigest(
		"config=gitconfig",
		"target=user",
		"platform=darwin/arm64",
		"input.email=alice@example.com",
		"input.name=Alice",
		"destination.gitconfig=.gitconfig",
		"flavor=workstation",
	)
	const wantConfigPinned = "sha256:4697706102775946265e9d15dba17349b20184bebcce121d9da240bac15bb62a"
	if wantConfig != wantConfigPinned {
		t.Fatalf("precomputed config digest = %s, want %s", wantConfig, wantConfigPinned)
	}
	if gotConfig != wantConfigPinned {
		t.Fatalf("config.InputsDigest = %s, want %s", gotConfig, wantConfigPinned)
	}

	// 3. Fleet inputs_digest.
	dest := filepath.Join(t.TempDir(), ".ssh", "authorized_keys")
	gotFleet := fleet.InputsDigest("alice", "user", "darwin/arm64", dest)
	wantFleet := legacyDigest(
		"fleet=alice",
		"target=user",
		"platform=darwin/arm64",
		"destination.authorized_keys="+filepath.Clean(dest),
	)
	if gotFleet != wantFleet {
		t.Fatalf("fleet.InputsDigest = %s, want %s", gotFleet, wantFleet)
	}
	gotFleetFixed := fleet.InputsDigest("alice", "user", "darwin/arm64", "authorized_keys")
	const wantFleetFixed = "sha256:06df3437d95bb62f0b43a021284da1ef66bef6cb5f9f92b67aafcbf1a587bca9"
	if gotFleetFixed != wantFleetFixed {
		t.Fatalf("fleet.InputsDigest (fixed) = %s, want %s", gotFleetFixed, wantFleetFixed)
	}
}
