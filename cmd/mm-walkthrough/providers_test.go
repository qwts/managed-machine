package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/source"
)

// providerRecords writes one passing provider-gate record per first-release
// target. Every record proves every combination of every provider that is
// declared for its target.
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
			if !fixtureOnTarget(f, tg) {
				continue
			}
			for _, kind := range []string{kindUser, kindCustom, kindGlobal, kindRelative} {
				ev.Combinations = append(ev.Combinations, ProviderCombination{
					Provider: f.provider, Software: f.software, TargetKind: kind,
					Expected: observedInstalled, Observed: observedInstalled, OK: true, Detail: "proven",
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
// declared for the targets it supports and that every fixture proves the
// user target, and the custom target wherever the provider supports one.
func TestProviderFixturesCoverEveryTarget(t *testing.T) {
	for _, f := range providerFixtures {
		if !fixtureHasTarget(f, kindUser) {
			t.Errorf("%s does not declare the user target", f.provider)
		}
		if f.provider == "winget" {
			if fixtureHasTarget(f, kindCustom) {
				t.Errorf("the winget fixture declares the custom target, but winget installs into its own scopes")
			}
			for _, p := range f.platforms {
				if !strings.HasPrefix(p, "windows/") {
					t.Errorf("the winget fixture declares %q, which is not a Windows target", p)
				}
			}
			continue
		}
		if !fixtureHasTarget(f, kindCustom) {
			t.Errorf("%s does not declare the custom target", f.provider)
		}
		if len(f.platforms) != len(firstReleaseTargets) {
			t.Errorf("%s is declared for %d targets, want %d", f.provider, len(f.platforms), len(firstReleaseTargets))
		}
	}
	// Every provider of the first release is under test somewhere, and the
	// WinGet provider only on the Windows targets.
	for _, tg := range firstReleaseTargets {
		seen := map[string]bool{}
		for _, f := range providerFixtures {
			if fixtureOnTarget(f, tg) {
				seen[f.provider] = true
			}
		}
		if seen["winget"] != strings.HasPrefix(tg, "windows/") {
			t.Errorf("the winget provider is under test=%v on %s", seen["winget"], tg)
		}
		for _, f := range providerFixtures {
			if f.provider == "winget" {
				continue
			}
			if !seen[f.provider] {
				t.Errorf("the %s provider is not under test on %s", f.provider, tg)
			}
		}
	}
}

// TestProviderSentinelsKeepTheProvidersUnderTestReachable proves that the
// gate shadows every program it must never start and only those: a
// provider under test is found on the runner's PATH, a provider that is
// not under test is a sentinel.
func TestProviderSentinelsKeepTheProvidersUnderTestReachable(t *testing.T) {
	always := []string{"sudo", "osascript", "security", "brew", "pip", "pipx"}
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
// every combination proven.
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
			t.Errorf("the %s provider planned an install into a relative custom root", f.provider)
			continue
		}
		if !errors.Is(err, provider.ErrUnsupported) {
			t.Errorf("the %s provider refused the relative root with %v", f.provider, err)
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
				p := h.fixturePath(f, kind, filepath.Join(root, "custom", f.provider))
				if p == "" {
					t.Errorf("%s declares no path for the %s target on %s", f.provider, kind, tg)
					continue
				}
				if !strings.HasPrefix(p, root) {
					t.Errorf("the %s %s path %q is outside the harness", f.provider, kind, p)
				}
				if strings.Contains(p, ":") && runtime.GOOS == "windows" {
					t.Errorf("the %s %s path %q holds a colon", f.provider, kind, p)
				}
			}
		}
	}
}
