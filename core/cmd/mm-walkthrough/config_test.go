package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/compose"
	"github.com/qwts/managed-machine/core/internal/config"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/source"
)

// configRecords writes one passing S4 config-gate record per first-release
// target, with every D9 case proven.
func configRecords(t *testing.T, dir string, change func(ev *ConfigEvidence)) []string {
	t.Helper()
	var files []string
	for _, tg := range firstReleaseTargets {
		ev := ConfigEvidence{
			InstallEvidence: InstallEvidence{
				SchemaVersion: 1, Kind: configEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
				SourceRevision: "rev", BinarySHA256: "abc", Mode: configMode,
				Setup: []string{}, Steps: []InstallStep{{Name: "probe", OK: true}}, Plans: []PlanRecord{},
				Guards: InstallGuards{SentinelInvocations: []string{}, ProfileChanges: []string{}},
				Result: "pass", Failures: []string{},
			},
			Cases: make([]ConfigCase, 0, len(configCaseNames)),
		}
		for _, name := range configCaseNames {
			ev.Cases = append(ev.Cases, ConfigCase{
				Case:     name,
				Expected: "ok",
				Observed: "ok",
				OK:       true,
				Detail:   "proven",
			})
		}
		if change != nil {
			change(&ev)
		}
		b, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "config-gate-"+strings.ReplaceAll(tg, "/", "-")+".json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	return files
}

// TestConfigFixturesLoadThroughTheRealCatalogs proves that the gate's own
// configuration and flavor definitions load cleanly through the real
// software, configuration and flavor catalog loaders.
func TestConfigFixturesLoadThroughTheRealCatalogs(t *testing.T) {
	dir := t.TempDir()
	if err := writeConfigDefinitions(dir); err != nil {
		t.Fatal(err)
	}
	id := source.Identity{Kind: "local", Location: "env:MANAGED_MACHINE_SOURCE_DIR"}
	fsys := os.DirFS(dir)

	if _, err := source.Load(id, fsys, ".", registry.Builtin()); err != nil {
		t.Fatalf("software loader rejected config gate definitions: %v", err)
	}
	cfgSnap, err := config.Load(id, fsys, ".")
	if err != nil {
		t.Fatalf("config loader rejected config gate definitions: %v", err)
	}
	wantConfigs := []string{
		"mm-gate-config",
		"mm-gate-conflict-block",
		"mm-gate-conflict-file",
		"mm-gate-conflict-key",
		"mm-gate-flavor-a",
		"mm-gate-flavor-b",
		"mm-gate-secrets",
		"mm-gate-unreadable",
	}
	if len(cfgSnap.Entries) != len(wantConfigs) {
		t.Fatalf("config catalog holds %d entries, want %d", len(cfgSnap.Entries), len(wantConfigs))
	}
	seenCfg := map[string]bool{}
	for _, e := range cfgSnap.Entries {
		seenCfg[e.Definition.ID] = true
	}
	for _, want := range wantConfigs {
		if !seenCfg[want] {
			t.Errorf("config catalog missing %q", want)
		}
	}

	flSnap, err := compose.Load(id, fsys, ".")
	if err != nil {
		t.Fatalf("flavor loader rejected config gate definitions: %v", err)
	}
	wantFlavors := []string{
		"mm-gate-flavor-alpha",
		"mm-gate-flavor-beta",
		"mm-gate-flavor-observed",
	}
	if len(flSnap.Entries) != len(wantFlavors) {
		t.Fatalf("flavor catalog holds %d entries, want %d", len(flSnap.Entries), len(wantFlavors))
	}
	seenFl := map[string]bool{}
	for _, e := range flSnap.Entries {
		seenFl[e.Flavor.ID] = true
	}
	for _, want := range wantFlavors {
		if !seenFl[want] {
			t.Errorf("flavor catalog missing %q", want)
		}
	}
}

// TestConfigSentinelsGuardElevationAndPackageManagers proves that the
// configuration gate guards elevation, keychain, GPG and package-manager
// binaries while leaving pass to the fixture script.
func TestConfigSentinelsGuardElevationAndPackageManagers(t *testing.T) {
	must := []string{"sudo", "osascript", "security", "gpg", "brew", "winget", "npm", "uv", "cargo", "go", "pip", "pipx"}
	for _, name := range must {
		if !contains(configSentinels, name) {
			t.Errorf("configSentinels omits %q", name)
		}
	}
	if contains(configSentinels, "pass") {
		t.Fatal("configSentinels must not overwrite the fixture pass script with a sentinel")
	}
}

// TestConfigEvidenceCarriesTheInstallShapeAndTheCases proves the serialized
// field order of ConfigEvidence and ConfigCase.
func TestConfigEvidenceCarriesTheInstallShapeAndTheCases(t *testing.T) {
	ev := ConfigEvidence{
		InstallEvidence: InstallEvidence{
			SchemaVersion: 1, Kind: configEvidenceKind, Target: "linux/amd64",
			SourceRevision: "rev", Mode: configMode, Result: "pass",
		},
		Cases: []ConfigCase{{
			Case: caseACT04User, Expected: "applied", Observed: "applied", OK: true, Detail: "proven",
		}},
	}
	b := mustMarshal(t, ev)
	keys, err := objectKeys(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "kind", "target", "os", "arch", "source_revision", "binary_sha256", "mode",
		"setup", "steps", "plans", "guards", "result", "failures", "cases",
	}
	if !sameStrings(keys, want) {
		t.Fatalf("the evidence keys are %v, want %v", keys, want)
	}
	c := mustMarshal(t, ev.Cases[0])
	keys, err = objectKeys(c)
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"case", "expected", "observed", "ok", "detail"}
	if !sameStrings(keys, want) {
		t.Fatalf("the case keys are %v, want %v", keys, want)
	}
}

// TestConfigAggregateAcceptsFivePassingRecords proves that five passing
// s4-config-gate records of one revision close the gate.
func TestConfigAggregateAcceptsFivePassingRecords(t *testing.T) {
	files := configRecords(t, t.TempDir(), nil)
	if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err != nil {
		t.Fatalf("five passing records were rejected: %v", err)
	}
}

// TestConfigAggregateRejectsBadRecords proves that config-aggregate refuses
// every record set that is not five passing s4-config-gate records of one
// revision with every D9 case proven.
func TestConfigAggregateRejectsBadRecords(t *testing.T) {
	t.Run("four records", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), nil)
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files[:4]...)); err == nil {
			t.Fatal("four records were accepted as five targets")
		}
	})
	t.Run("another revision", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), func(ev *ConfigEvidence) {
			if ev.Target == "linux/amd64" {
				ev.SourceRevision = "other"
			}
		})
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another revision was accepted")
		}
	})
	t.Run("a duplicate target", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), nil)
		dup := filepath.Join(t.TempDir(), "duplicate.json")
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dup, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, append(files, dup)...)); err == nil {
			t.Fatal("two records for one target were accepted")
		}
	})
	t.Run("a failed record", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), func(ev *ConfigEvidence) {
			if ev.Target == "windows/arm64" {
				ev.Result = "fail"
			}
		})
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a failed record was accepted")
		}
	})
	t.Run("a failed case", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), func(ev *ConfigEvidence) {
			if ev.Target == "windows/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseSecretMatrix {
						ev.Cases[i].OK = false
						ev.Cases[i].Observed = observedFailed
					}
				}
			}
		})
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a failed case was accepted")
		}
	})
	t.Run("a missing case", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), func(ev *ConfigEvidence) {
			if ev.Target == "linux/arm64" {
				ev.Cases = ev.Cases[:len(ev.Cases)-1]
			}
		})
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record missing a D9 case was accepted")
		}
	})
	t.Run("another mode", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), func(ev *ConfigEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Mode = modeRefusal
			}
		})
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record in another mode was accepted")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), nil)
		broken := filepath.Join(t.TempDir(), "broken.json")
		if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, append(files, broken)...)); err == nil {
			t.Fatal("a malformed record was accepted")
		}
	})
	t.Run("another record kind", func(t *testing.T) {
		files := configRecords(t, t.TempDir(), func(ev *ConfigEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Kind = providerEvidenceKind
			}
		})
		if err := configAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another kind was accepted")
		}
	})
}

// TestConfigZeroHitsSecretGrepCatchesLeaks proves that grepTree and grepBytes
// report any occurrence of a fixture secret value in a directory tree or byte
// slice.
func TestConfigZeroHitsSecretGrepCatchesLeaks(t *testing.T) {
	dir := t.TempDir()
	cleanFile := filepath.Join(dir, "plan.json")
	if err := os.WriteFile(cleanFile, []byte(`{"plan":"plan-20261021T120000Z-0001"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if hit := grepTree(dir, "state", secretSentinels); hit != "" {
		t.Fatalf("clean tree reported leak: %s", hit)
	}
	leakFile := filepath.Join(dir, "nested", "leak.json")
	if err := os.MkdirAll(filepath.Dir(leakFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leakFile, []byte("value="+secretPassValue), 0o644); err != nil {
		t.Fatal(err)
	}
	if hit := grepTree(dir, "state", secretSentinels); hit == "" {
		t.Fatal("leaked secret in tree was not caught")
	}
	if hit := grepBytes([]byte("stdout "+secretEnvValue), "capture[0]", secretSentinels); hit == "" {
		t.Fatal("leaked secret in capture was not caught")
	}
}

// TestConfigGateEndToEnd builds the real managed-machine binary and runs
// `mm-walkthrough config` against it on the current host, proving every D9
// case, zero sentinel invocations, zero profile changes, no host paths and no
// secret values in the evidence.
func TestConfigGateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core and runs the S4 config gate")
	}
	bin := buildCore(t)
	out := filepath.Join(t.TempDir(), "config-gate.json")
	if err := configCmd([]string{"-binary", bin, "-target", host(), "-evidence", out, "-source-revision", "test-rev"}); err != nil {
		t.Fatalf("config gate failed: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var ev ConfigEvidence
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.SchemaVersion != 1 || ev.Kind != configEvidenceKind || ev.Mode != configMode || ev.Result != "pass" {
		t.Fatalf("unexpected evidence header: %+v", ev)
	}
	if len(ev.Failures) != 0 {
		t.Fatalf("evidence carries failures: %v", ev.Failures)
	}
	if len(ev.Guards.SentinelInvocations) != 0 {
		t.Fatalf("sentinel invoked: %v", ev.Guards.SentinelInvocations)
	}
	if len(ev.Guards.ProfileChanges) != 0 {
		t.Fatalf("profiles changed: %v", ev.Guards.ProfileChanges)
	}
	for _, want := range configCaseNames {
		if !configCaseProven(ev, want) {
			t.Errorf("missing D9 case %q", want)
		}
	}
	for _, c := range ev.Cases {
		if !c.OK {
			t.Errorf("case %s failed: %s", c.Case, c.Detail)
		}
	}
	for _, sec := range secretSentinels {
		if strings.Contains(string(b), sec) {
			t.Fatalf("evidence leaked secret sentinel %q", sec)
		}
	}
	if realHome, err := os.UserHomeDir(); err == nil && realHome != "" {
		if strings.Contains(string(b), realHome) || strings.Contains(string(b), filepath.ToSlash(realHome)) {
			t.Fatalf("evidence leaked real home path %q", realHome)
		}
	}
}
