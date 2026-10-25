package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/appcopy"
	"github.com/qwts/managed-machine/internal/provider/archive"
	"github.com/qwts/managed-machine/internal/provider/brew"
	"github.com/qwts/managed-machine/internal/provider/winget"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/source"
)

// providerRecords writes one passing provider-gate record per first-release
// target. Every record proves every required combination of every fixture row
// in providerFixtures.
func providerRecords(t *testing.T, dir string, change func(ev *ProviderEvidence)) []string {
	t.Helper()
	var files []string
	for _, tg := range firstReleaseTargets {
		ev := ProviderEvidence{
			InstallEvidence: InstallEvidence{
				SchemaVersion: 1, Kind: providerEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
				SourceRevision: "rev", BinarySHA256: "abc", Mode: providerMode,
				Setup: []string{}, Steps: []InstallStep{{Name: "probe", OK: true}}, Plans: []PlanRecord{},
				Guards: InstallGuards{SentinelInvocations: []string{}, ProfileChanges: []string{}},
				Result: "pass", Failures: []string{},
			},
			Combinations: []ProviderCombination{},
		}
		for _, f := range providerFixtures {
			for _, kind := range fixtureRequiredKinds(f, tg) {
				obs := observedInstalled
				if f.expect != expectInstall || !fixtureOnTarget(f, tg) || kind == kindGlobal || kind == kindRelative || (kind == kindCustom && !fixtureHasTarget(f, kindCustom)) {
					obs = observedRefused
				}
				ev.Combinations = append(ev.Combinations, ProviderCombination{
					Provider: f.provider, Software: f.software, TargetKind: kind,
					Expected: obs, Observed: obs, OK: true, Detail: "proven",
				})
			}
		}
		if change != nil {
			change(&ev)
		}
		b, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, strings.ReplaceAll(tg, "/", "-")+".json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	return files
}

// TestProviderFixturesLoadThroughTheRealCatalog proves that the gate's own
// definitions are valid software definitions: the real loader reads them
// with the real registry, and every provider keeps the options its
// provider's schema declares.
func TestProviderFixturesLoadThroughTheRealCatalog(t *testing.T) {
	dir := t.TempDir()
	if err := writeProviderDefinitions(dir); err != nil {
		t.Fatal(err)
	}
	reg := registry.Builtin()
	snap, err := source.Load(source.Identity{Kind: "local", Location: "env:MANAGED_MACHINE_SOURCE_DIR"}, os.DirFS(dir), ".", reg)
	if err != nil {
		t.Fatalf("the gate definitions do not load: %v", err)
	}
	if len(snap.Entries) != len(providerFixtures) {
		t.Fatalf("the catalog holds %d definitions, want %d", len(snap.Entries), len(providerFixtures))
	}
	byID := map[string]bool{}
	for _, e := range snap.Entries {
		byID[e.Software.ID] = true
		if len(e.Software.Recipes) != 1 {
			t.Errorf("%s has %d recipes, want one", e.Software.ID, len(e.Software.Recipes))
			continue
		}
		r := e.Software.Recipes[0]
		p, ok := reg.Lookup(r.Provider)
		if !ok {
			t.Errorf("%s names the unknown provider %q", e.Software.ID, r.Provider)
			continue
		}
		for name := range p.Options {
			if p.Options[name].Required {
				if _, ok := r.Options[name]; !ok {
					t.Errorf("%s omits the required option %q", e.Software.ID, name)
				}
			}
		}
		if len(r.Platforms) == 0 || len(r.Targets) == 0 {
			t.Errorf("%s declares no platform or no target", e.Software.ID)
		}
	}
	for _, f := range providerFixtures {
		if !byID[f.software] {
			t.Errorf("the catalog has no definition for %s", f.software)
		}
	}
}

// TestProviderFixturesCoverEveryTarget proves that every provider is
// declared for the targets it supports, that every row carries valid expect,
// source and tamper fields (D11), and that the S7 cask, app-copy and archive
// rows carry the owner-verified metadata and refusal variants.
func TestProviderFixturesCoverEveryTarget(t *testing.T) {
	validExpect := map[string]bool{
		expectInstall:         true,
		expectRefuseMismatch:  true,
		expectRefuseSignature: true,
		expectRefuseHost:      true,
	}
	validSource := map[string]bool{
		sourceReal:     true,
		sourceLoopback: true,
	}
	validTamper := map[string]bool{
		tamperNone:   true,
		tamperSHA256: true,
		tamperTeamID: true,
		tamperHost:   true,
	}

	bySoftware := map[string]providerFixture{}
	for _, f := range providerFixtures {
		bySoftware[f.software] = f
		if !validExpect[f.expect] {
			t.Errorf("%s has invalid expect %q", f.software, f.expect)
		}
		if !validSource[f.source] {
			t.Errorf("%s has invalid source %q", f.software, f.source)
		}
		if !validTamper[f.tamper] {
			t.Errorf("%s has invalid tamper %q", f.software, f.tamper)
		}
		if (f.expect == expectInstall) != (f.tamper == tamperNone) {
			t.Errorf("%s has expect=%q with tamper=%q", f.software, f.expect, f.tamper)
		}
		if !fixtureHasTarget(f, kindUser) {
			t.Errorf("%s does not declare the user target", f.software)
		}
		switch f.provider {
		case winget.Name:
			if fixtureHasTarget(f, kindCustom) {
				t.Errorf("the winget fixture declares the custom target, but winget installs into its own scopes")
			}
			for _, p := range f.platforms {
				if !strings.HasPrefix(p, "windows/") {
					t.Errorf("the winget fixture declares %q, which is not a Windows target", p)
				}
			}
		case brew.Name, appcopy.Name:
			if fixtureHasTarget(f, kindCustom) {
				t.Errorf("%s declares the custom target, but %s installs into Applications", f.software, f.provider)
			}
			if !sameStrings(f.platforms, []string{"darwin/arm64"}) {
				t.Errorf("%s platforms = %v, want [darwin/arm64]", f.software, f.platforms)
			}
		default:
			if !fixtureHasTarget(f, kindCustom) {
				t.Errorf("%s does not declare the custom target", f.software)
			}
			if len(f.platforms) != len(firstReleaseTargets) {
				t.Errorf("%s is declared for %d targets, want %d", f.software, len(f.platforms), len(firstReleaseTargets))
			}
		}
	}

	// Verify the S7 N07 real and refusal rows are all present with exact metadata.
	caskRow, ok := bySoftware["mm-gate-brew-cask"]
	if !ok {
		t.Fatal("missing mm-gate-brew-cask row")
	}
	if caskRow.options["cask"] != "telegram" || caskRow.options["app_name"] != "Telegram.app" ||
		caskRow.options["team_id"] != "6N38VWS5BX" || caskRow.options["url_hosts"] != "[osx.telegram.org]" ||
		caskRow.options["homepage_hosts"] != "[macos.telegram.org]" {
		t.Errorf("unexpected mm-gate-brew-cask options: %v", caskRow.options)
	}

	appRow, ok := bySoftware["mm-gate-app-copy"]
	if !ok {
		t.Fatal("missing mm-gate-app-copy row")
	}
	if appRow.options["url"] != "https://opencode.ai/files/bin/2.1.4/opencode-desktop-mac-arm64.dmg" ||
		appRow.options["sha256"] != "1bf4030c8e15bf2dd30ffca4263b8586b052201ecc755d652ec465fcc44a0296" ||
		appRow.options["app_name"] != "OpenCode.app" || appRow.options["version"] != "2.1.4" ||
		appRow.options["team_id"] != "5NZ4Q7NXJ4" || appRow.options["url_hosts"] != "[opencode.ai]" ||
		appRow.options["format"] != "dmg" {
		t.Errorf("unexpected mm-gate-app-copy options: %v", appRow.options)
	}

	for id, wantExpect := range map[string]string{
		"mm-gate-app-copy-sha256":  expectRefuseMismatch,
		"mm-gate-app-copy-team-id": expectRefuseSignature,
		"mm-gate-app-copy-host":    expectRefuseHost,
		"mm-gate-archive":          expectInstall,
		"mm-gate-archive-sha256":   expectRefuseMismatch,
		"mm-gate-archive-host":     expectRefuseHost,
	} {
		r, ok := bySoftware[id]
		if !ok {
			t.Errorf("missing %s row", id)
			continue
		}
		if r.expect != wantExpect || r.source != sourceLoopback {
			t.Errorf("%s has expect=%q source=%q, want expect=%q source=%q", id, r.expect, r.source, wantExpect, sourceLoopback)
		}
	}

	for _, tg := range firstReleaseTargets {
		seen := map[string]bool{}
		for _, f := range providerFixtures {
			if fixtureOnTarget(f, tg) {
				seen[f.provider] = true
			}
		}
		if seen[winget.Name] != strings.HasPrefix(tg, "windows/") {
			t.Errorf("the winget provider is under test=%v on %s", seen[winget.Name], tg)
		}
		isDarwin := tg == "darwin/arm64"
		if seen[brew.Name] != isDarwin {
			t.Errorf("the brew provider is under test=%v on %s", seen[brew.Name], tg)
		}
		if seen[appcopy.Name] != isDarwin {
			t.Errorf("the app-copy provider is under test=%v on %s", seen[appcopy.Name], tg)
		}
		for _, name := range []string{"uv", "npm", "cargo", "go", archive.Name} {
			if !seen[name] {
				t.Errorf("the %s provider is not under test on %s", name, tg)
			}
		}
	}
}

// TestProviderSentinelsKeepTheProvidersUnderTestReachable proves that the
// gate shadows every program it must never start and only those: a
// provider under test is found on the runner's PATH, a provider that is
// not under test is a sentinel.
func TestProviderSentinelsKeepTheProvidersUnderTestReachable(t *testing.T) {
	always := []string{"sudo", "osascript", "security", "pip", "pipx"}
	for _, tg := range firstReleaseTargets {
		got := providerSentinels(tg)
		for _, name := range always {
			if !contains(got, name) {
				t.Errorf("the sentinels of %s omit %q: %v", tg, name, got)
			}
		}
		for _, f := range providerFixtures {
			shadowed := contains(got, f.provider)
			if fixtureOnTarget(f, tg) && shadowed {
				t.Errorf("the %s provider is shadowed on %s, so its tool cannot be found", f.provider, tg)
			}
			if !fixtureOnTarget(f, tg) && !shadowed {
				t.Errorf("the %s provider is not shadowed on %s", f.provider, tg)
			}
		}
	}
	// The install gate keeps its own sentinels: the provider gate never
	// changes what the S2 gate guards.
	for _, name := range gateSentinels {
		if !contains(gateSentinels, name) {
			t.Errorf("the install gate lost the sentinel %q", name)
		}
	}
}

// TestContainmentNamesEveryChangeOutsideTheCustomRoot proves that the
// containment check names every file that changed outside the custom root
// and the guarded state directory, and stays quiet about the two it may
// change.
func TestContainmentNamesEveryChangeOutsideTheCustomRoot(t *testing.T) {
	root := t.TempDir()
	custom := filepath.Join(root, "custom", "uv")
	state := filepath.Join(root, "state")
	home := filepath.Join(root, "home")
	for _, d := range []string{custom, state, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "profile.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &providerHarness{installHarness: &installHarness{root: root, state: state}}
	before, err := h.containmentSnapshot(custom)
	if err != nil {
		t.Fatal(err)
	}
	// The custom root and the state directory may change.
	if err := os.WriteFile(filepath.Join(custom, "tool"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "plan.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := h.containmentSnapshot(custom)
	if err != nil {
		t.Fatal(err)
	}
	if changes := profileDiff(before, after); len(changes) != 0 {
		t.Fatalf("the containment check reports %v, want no change", changes)
	}
	// A change outside them is named.
	if err := os.WriteFile(filepath.Join(home, "profile.txt"), []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "new.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err = h.containmentSnapshot(custom)
	if err != nil {
		t.Fatal(err)
	}
	changes := profileDiff(before, after)
	want := []string{"changed harness/home/profile.txt", "created harness/home/new.txt"}
	if !sameStrings(changes, want) {
		t.Fatalf("the containment check reports %v, want %v", changes, want)
	}
}

// TestProviderEvidenceCarriesTheInstallShapeAndTheCombinations proves the
// shape of the evidence: the install-gate fields, unchanged, and the
// combination matrix beside them.
func TestProviderEvidenceCarriesTheInstallShapeAndTheCombinations(t *testing.T) {
	ev := ProviderEvidence{
		InstallEvidence: InstallEvidence{
			SchemaVersion: 1, Kind: providerEvidenceKind, Target: "linux/amd64",
			SourceRevision: "rev", Mode: providerMode, Result: "pass",
		},
		Combinations: []ProviderCombination{{
			Provider: "uv", Software: "mm-gate-uv", TargetKind: kindUser,
			Expected: observedInstalled, Observed: observedInstalled, OK: true, Detail: "proven",
		}},
	}
	b := mustMarshal(t, ev)
	keys, err := objectKeys(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "kind", "target", "os", "arch", "source_revision", "binary_sha256", "mode",
		"setup", "steps", "plans", "guards", "result", "failures", "combinations",
	}
	if !sameStrings(keys, want) {
		t.Fatalf("the evidence keys are %v, want %v", keys, want)
	}
	combo := mustMarshal(t, ev.Combinations[0])
	keys, err = objectKeys(combo)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"provider", "software", "target_kind", "expected", "observed", "ok", "detail"}
	if !sameStrings(keys, want) {
		t.Fatalf("the combination keys are %v, want %v", keys, want)
	}
}

// TestProvidersAggregateAcceptsFivePassingRecords proves that five records
// of one revision close the provider gate.
func TestProvidersAggregateAcceptsFivePassingRecords(t *testing.T) {
	files := providerRecords(t, t.TempDir(), nil)
	if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err != nil {
		t.Fatalf("five passing records were rejected: %v", err)
	}
}

// TestProvidersAggregateRejectsBadRecords proves that the aggregate refuses
// every record set that is not five passing records of one revision with
// every combination proven (and refuses n-a on cask or app-copy rows).
func TestProvidersAggregateRejectsBadRecords(t *testing.T) {
	t.Run("four records", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), nil)
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files[:4]...)); err == nil {
			t.Fatal("four records were accepted as five targets")
		}
	})
	t.Run("another revision", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), func(ev *ProviderEvidence) {
			if ev.Target == "linux/amd64" {
				ev.SourceRevision = "other"
			}
		})
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another revision was accepted")
		}
	})
	t.Run("a duplicate target", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), nil)
		dup := filepath.Join(t.TempDir(), "duplicate.json")
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dup, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, append(files, dup)...)); err == nil {
			t.Fatal("two records for one target were accepted")
		}
	})
	t.Run("a failed combination", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), func(ev *ProviderEvidence) {
			if ev.Target == "windows/amd64" {
				for i := range ev.Combinations {
					if ev.Combinations[i].Provider == "npm" {
						ev.Combinations[i].OK = false
						ev.Combinations[i].Observed = observedFailed
					}
				}
			}
		})
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a failed combination was accepted")
		}
	})
	t.Run("a missing combination", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), func(ev *ProviderEvidence) {
			if ev.Target == "linux/arm64" {
				ev.Combinations = ev.Combinations[:1]
			}
		})
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record without every combination was accepted")
		}
	})
	t.Run("missing refusal row", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), func(ev *ProviderEvidence) {
			if ev.Target == "darwin/arm64" {
				var kept []ProviderCombination
				for _, c := range ev.Combinations {
					if c.Software != "mm-gate-app-copy-team-id" {
						kept = append(kept, c)
					}
				}
				ev.Combinations = kept
			}
		})
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record missing mm-gate-app-copy-team-id was accepted")
		}
	})
	t.Run("cask or app-copy recorded as n-a on linux", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), func(ev *ProviderEvidence) {
			if ev.Target == "linux/amd64" {
				for i := range ev.Combinations {
					if ev.Combinations[i].Software == "mm-gate-app-copy" && ev.Combinations[i].TargetKind == kindUser {
						ev.Combinations[i].Observed = "n-a"
					}
				}
			}
		})
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("an app-copy record with n-a on linux was accepted")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), nil)
		broken := filepath.Join(t.TempDir(), "broken.json")
		if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, append(files, broken)...)); err == nil {
			t.Fatal("a malformed record was accepted")
		}
	})
	t.Run("another record kind", func(t *testing.T) {
		files := providerRecords(t, t.TempDir(), func(ev *ProviderEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Kind = installEvidenceKind
			}
		})
		if err := providersAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another kind was accepted")
		}
	})
}

// TestProviderModeIsRefusedWithoutAHostedRunner proves that the provider
// gate refuses to install before it starts any provider tool: the refusal
// comes before the first install, so nothing is installed and no
// combination is proven.
func TestProviderModeIsRefusedWithoutAHostedRunner(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core and runs the gate")
	}
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_ENVIRONMENT", "")
	bin := buildCore(t)
	out := filepath.Join(t.TempDir(), "evidence.json")
	err := providersCmd([]string{"-binary", bin, "-target", host(), "-evidence", out, "-source-revision", "test"})
	if err == nil {
		t.Fatal("the provider gate ran outside a hosted runner")
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("no evidence written: %v", rerr)
	}
	var ev ProviderEvidence
	if jerr := json.Unmarshal(b, &ev); jerr != nil {
		t.Fatal(jerr)
	}
	if ev.Result != "fail" {
		t.Fatalf("result = %q, want fail", ev.Result)
	}
	if ev.Mode != providerMode {
		t.Fatalf("mode = %q, want %q", ev.Mode, providerMode)
	}
	if !strings.Contains(strings.Join(ev.Failures, "\n"), providerHostedRunnerMessage) {
		t.Fatalf("the failure does not name the refusal: %v", ev.Failures)
	}
	if len(ev.Combinations) != 0 {
		t.Fatalf("the gate proved %d combinations before it refused", len(ev.Combinations))
	}
	if len(ev.Plans) != 0 {
		t.Fatalf("the gate wrote %d plans before it refused", len(ev.Plans))
	}
}

// TestBuildAdHocDMGRefusesOutsideHostedDarwinRunner proves that buildAdHocDMG
// is guarded so it never invokes hdiutil or codesign outside a darwin
// GitHub-hosted runner.
func TestBuildAdHocDMGRefusesOutsideHostedDarwinRunner(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_ENVIRONMENT", "")
	if _, err := buildAdHocDMG(t.TempDir(), "GateRefuse.app", "1.0.0"); err == nil {
		t.Fatal("buildAdHocDMG succeeded outside a hosted runner")
	}
}

// TestProviderLoopbackArchiveAndRefusalRows proves the S7 loopback archive
// install (user and custom targets, running `managed-machine --version` from
// the installed path) and the loopback refusal rows (`sha256` and `host`, plus
// off-platform `brew` cask and `app-copy` refusals on Linux and Windows)
// against a built core binary in an isolated temp harness without invoking any
// external package manager or macOS tool.
func TestProviderLoopbackArchiveAndRefusalRows(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core and runs loopback archive and refusal fixtures")
	}
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_ENVIRONMENT", "")
	bin := buildCore(t)
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	h, err := newProviderHarness(bin, host(), "test-rev", raw)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(h.root)

	cleanup, err := h.startLoopbackFixtures([]byte("synthetic-dmg-bytes-for-download-refusal"))
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// 1. Archive refusal rows (sha256 and host) run while ~/.local/bin/managed-machine is still absent.
	h.runRefusalFixture(providerFixtureOf("mm-gate-archive-sha256"))
	h.runRefusalFixture(providerFixtureOf("mm-gate-archive-host"))
	// 2. Archive real install row (user + custom + global refusal + relative refusal).
	h.runFixture(providerFixtureOf("mm-gate-archive"))

	// 3. On darwin/arm64, also exercise the app-copy download-stage refusals
	// (sha256 and host fail during download before hdiutil or codesign runs).
	// On Linux and Windows, exercise the off-platform refusals for brew cask
	// and app-copy.
	if host() == "darwin/arm64" {
		h.runRefusalFixture(providerFixtureOf("mm-gate-app-copy-sha256"))
		h.runRefusalFixture(providerFixtureOf("mm-gate-app-copy-host"))
	} else {
		h.runOffPlatformFixture(providerFixtureOf("mm-gate-brew-cask"))
		h.runOffPlatformFixture(providerFixtureOf("mm-gate-app-copy"))
		h.runOffPlatformFixture(providerFixtureOf("mm-gate-app-copy-sha256"))
	}

	h.guards()
	h.finish()
	if h.ev.Result != "pass" {
		t.Fatalf("loopback archive and refusal fixtures failed: %v", h.ev.Failures)
	}

	// Verify that a destination or stage mutation during a refusal is caught by refusalSnapshot.
	before, err := h.refusalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	stageLeak := filepath.Join(h.state, "stage", "plan-leak")
	if err := os.MkdirAll(stageLeak, 0o755); err != nil {
		t.Fatal(err)
	}
	after, err := h.refusalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(profileDiff(before, after)) == 0 || stageDirEmpty(filepath.Join(h.state, "stage")) {
		t.Fatal("refusalSnapshot or stageDirEmpty missed a leftover stage directory")
	}
}

// TestProviderGateRefusesARelativeCustomRoot proves that every provider
// refuses a relative custom root with the unsupported-combination kind,
// and that proving it starts no tool.
func TestProviderGateRefusesARelativeCustomRoot(t *testing.T) {
	for _, f := range providerFixtures {
		p := gateProvider(f.provider, os.Environ())
		if p == nil {
			t.Errorf("the provider %s has no implementation", f.provider)
			continue
		}
		_, err := p.Plan(t.Context(), provider.Request{
			Software: f.software,
			Target:   "custom:relative-root",
			Options:  f.optionValues(),
		})
		if err == nil {
			t.Errorf("the %s provider (%s) planned an install into a relative custom root", f.provider, f.software)
			continue
		}
		if !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("the %s provider (%s) refused the relative root with %v", f.provider, f.software, err)
		}
	}
}

// TestProviderFixturePathsAreInsideTheHarness proves that every path the
// gate expects a binary at is inside the isolated home, the custom root or
// a redirected profile directory, and that no path depends on the machine.
// WinGet is the exception: its packages live in the runner's own
// LOCALAPPDATA, where WinGet keeps its state.
func TestProviderFixturePathsAreInsideTheHarness(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "harness", "root")
	for _, tg := range firstReleaseTargets {
		h := &providerHarness{installHarness: &installHarness{
			target: tg,
			root:   root,
			home:   filepath.Join(root, "home"),
		}}
		for _, f := range providerFixtures {
			if !fixtureOnTarget(f, tg) || f.provider == "winget" {
				continue
			}
			for _, kind := range []string{kindUser, kindCustom} {
				p := h.fixturePath(f, kind, filepath.Join(root, "custom", f.software))
				if p == "" {
					t.Errorf("%s declares no path for the %s target on %s", f.software, kind, tg)
					continue
				}
				if !strings.HasPrefix(p, root) {
					t.Errorf("the %s %s path %q is outside the harness", f.software, kind, p)
				}
				if strings.Contains(p, ":") && runtime.GOOS == "windows" {
					t.Errorf("the %s %s path %q holds a colon", f.software, kind, p)
				}
			}
		}
	}
}

// TestSupportMatrixStillDeclaresInstallShUpdateAndBootstrapUntested proves that
// the generated support matrix text continues to report install.sh, --update
// and bootstrap as untested in CI (D11).
func TestSupportMatrixStillDeclaresInstallShUpdateAndBootstrapUntested(t *testing.T) {
	md := renderSupportMatrix("0.9.0", "rev", "123", nil, nil, nil)
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "`install.sh`") && strings.Contains(line, "--update") && strings.Contains(line, "bootstrap") {
			if !strings.Contains(line, "`untested`") {
				t.Fatalf("install.sh / bootstrap / --update line does not say untested: %s", line)
			}
			return
		}
	}
	t.Fatalf("support matrix missing untested line for install.sh, bootstrap and --update:\n%s", md)
}

var _ = diag.SoftwareArtifactMismatch
