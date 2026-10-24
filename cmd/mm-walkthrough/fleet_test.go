package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
)

// isolateRealHomeForTest redirects HOME, USERPROFILE and platform state/config
// environment variables into a temporary directory seeded with a synthetic
// .ssh/authorized_keys, shell profile and .config/managed-machine tree so
// tests never read or touch the developer machine's real home directory while
// still exercising the real-home byte-identical hash guards.
func isolateRealHomeForTest(t *testing.T) string {
	t.Helper()
	fakeHome := filepath.Join(t.TempDir(), "runner-home")
	for _, rel := range []string{
		".ssh/authorized_keys",
		".zshrc",
		".config/managed-machine/settings.json",
	} {
		p := filepath.Join(fakeHome, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("seed-"+rel+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(fakeHome, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(fakeHome, ".local", "share"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(fakeHome, ".local", "state"))
	t.Setenv("LOCALAPPDATA", filepath.Join(fakeHome, "AppData", "Local"))
	t.Setenv("APPDATA", filepath.Join(fakeHome, "AppData", "Roaming"))
	return fakeHome
}

// fleetRecords writes one passing S5 fleet-gate record per first-release
// target, with every D9 case proven (`legacy-cutover-refusal` recorded `ok` on
// darwin/arm64 and `n-a` on the other four targets).
func fleetRecords(t *testing.T, dir string, change func(ev *FleetEvidence)) []string {
	t.Helper()
	var files []string
	for _, tg := range firstReleaseTargets {
		ev := FleetEvidence{
			InstallEvidence: InstallEvidence{
				SchemaVersion: 1, Kind: fleetEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
				SourceRevision: "rev", BinarySHA256: "abc", Mode: fleetMode,
				Setup: []string{}, Steps: []InstallStep{{Name: "probe", OK: true}}, Plans: []PlanRecord{},
				Guards: InstallGuards{SentinelInvocations: []string{}, ProfileChanges: []string{}},
				Result: "pass", Failures: []string{},
			},
			Cases: make([]FleetCase, 0, len(fleetCaseNames)),
		}
		for _, name := range fleetCaseNames {
			want := "ok"
			if name == caseFleetLegacyCutoverRefusal && !strings.HasPrefix(tg, "darwin/") {
				want = "n-a"
			}
			ev.Cases = append(ev.Cases, FleetCase{
				Case:     name,
				Expected: want,
				Observed: want,
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
		p := filepath.Join(dir, "fleet-gate-"+strings.ReplaceAll(tg, "/", "-")+".json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	return files
}

// TestFleetFixturesLoadThroughTheRealLoaders proves that the fleet gate's
// generated Ed25519 keys, key-directory YAML, policy tree and desired.yaml
// validate cleanly through the real L01/L03 loaders.
func TestFleetFixturesLoadThroughTheRealLoaders(t *testing.T) {
	isolateRealHomeForTest(t)
	h, err := newFleetHarness("managed-machine", host(), "rev", "", "", []byte("bin"))
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(h.root)

	if err := h.cleanKeyDirectory(); err != nil {
		t.Fatal(err)
	}
	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		t.Fatal(err)
	}

	dir, err := directory.LoadPath(h.keyDir)
	if err != nil {
		t.Fatalf("directory.LoadPath rejected fleet gate key directory: %v", err)
	}
	if dir.OwnerCount() != 2 || dir.KeyCount() != 3 {
		t.Fatalf("directory owners/keys = %d/%d, want 2/3", dir.OwnerCount(), dir.KeyCount())
	}

	pol, err := policy.Load(h.policyDir)
	if err != nil {
		t.Fatalf("policy.Load rejected fleet gate policy tree: %v", err)
	}
	if err := pol.RequireCutover(); err != nil {
		t.Fatalf("policy.RequireCutover failed: %v", err)
	}
	if len(pol.Groups) != 1 || pol.Groups[0].ID != "ops" {
		t.Fatalf("unexpected policy groups: %+v", pol.Groups)
	}

	des, err := config.LoadDesired(h.desired)
	if err != nil {
		t.Fatalf("config.LoadDesired rejected fleet gate desired.yaml: %v", err)
	}
	if filepath.Clean(des.Fleet.Destinations["authorized_keys"]) != filepath.Clean(h.authKeys) {
		t.Fatalf("desired authorized_keys = %q, want %q", des.Fleet.Destinations["authorized_keys"], h.authKeys)
	}
}

// TestFleetSentinelsGuardSSHGitGHAndElevation proves that the fleet gate
// shadows ssh-keygen, ssh-add, ssh, gh, git, osascript, sudo, security, dscl
// and package-manager binaries on PATH.
func TestFleetSentinelsGuardSSHGitGHAndElevation(t *testing.T) {
	must := []string{
		"ssh-keygen", "ssh-add", "ssh", "gh", "git",
		"osascript", "sudo", "security", "dscl",
		"gpg", "brew", "winget", "npm", "uv", "cargo", "go", "pip", "pipx",
	}
	for _, name := range must {
		if !contains(fleetSentinels, name) {
			t.Errorf("fleetSentinels omits %q", name)
		}
	}
}

// TestFleetEvidenceCarriesTheInstallShapeAndTheCases proves the serialized
// field order of FleetEvidence and FleetCase.
func TestFleetEvidenceCarriesTheInstallShapeAndTheCases(t *testing.T) {
	ev := FleetEvidence{
		InstallEvidence: InstallEvidence{
			SchemaVersion: 1, Kind: fleetEvidenceKind, Target: "linux/amd64",
			SourceRevision: "rev", Mode: fleetMode, Result: "pass",
		},
		Cases: []FleetCase{{
			Case: caseFleetDirectoryPath, Expected: "ok", Observed: "ok", OK: true, Detail: "proven",
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

// TestFleetAggregateAcceptsFivePassingRecords proves that five passing
// s5-fleet-gate records of one revision close the gate.
func TestFleetAggregateAcceptsFivePassingRecords(t *testing.T) {
	files := fleetRecords(t, t.TempDir(), nil)
	if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err != nil {
		t.Fatalf("five passing records were rejected: %v", err)
	}
}

// TestFleetAggregateRejectsBadRecords proves that fleet-aggregate refuses
// every record set that is not five passing s5-fleet-gate records of one
// revision with every D9 case proven and `legacy-cutover-refusal` recorded
// `ok` on darwin and `n-a` elsewhere.
func TestFleetAggregateRejectsBadRecords(t *testing.T) {
	t.Run("four records", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), nil)
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files[:4]...)); err == nil {
			t.Fatal("four records were accepted as five targets")
		}
	})
	t.Run("another revision", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "linux/amd64" {
				ev.SourceRevision = "other"
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another revision was accepted")
		}
	})
	t.Run("a duplicate target", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), nil)
		dup := filepath.Join(t.TempDir(), "duplicate.json")
		b, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dup, b, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, append(files, dup)...)); err == nil {
			t.Fatal("two records for one target were accepted")
		}
	})
	t.Run("a failed record", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "windows/arm64" {
				ev.Result = "fail"
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a failed record was accepted")
		}
	})
	t.Run("a failed case", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "windows/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseFleetPublicationGrantsNothing {
						ev.Cases[i].OK = false
						ev.Cases[i].Observed = observedFailed
					}
				}
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a failed case was accepted")
		}
	})
	t.Run("a missing case", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "linux/arm64" {
				ev.Cases = ev.Cases[:len(ev.Cases)-1]
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record missing a D9 case was accepted")
		}
	})
	t.Run("legacy-cutover-refusal n-a on darwin", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "darwin/arm64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseFleetLegacyCutoverRefusal {
						ev.Cases[i].Observed = "n-a"
					}
				}
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("legacy-cutover-refusal n-a on darwin/arm64 was accepted")
		}
	})
	t.Run("legacy-cutover-refusal ok outside darwin", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "linux/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseFleetLegacyCutoverRefusal {
						ev.Cases[i].Observed = "ok"
					}
				}
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("legacy-cutover-refusal ok on linux/amd64 was accepted")
		}
	})
	t.Run("non-legacy case n-a", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "linux/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseFleetPublishStage {
						ev.Cases[i].Observed = "n-a"
					}
				}
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a non-legacy case recorded n-a was accepted")
		}
	})
	t.Run("another mode", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Mode = modeRefusal
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record in another mode was accepted")
		}
	})
	t.Run("malformed json", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), nil)
		broken := filepath.Join(t.TempDir(), "broken.json")
		if err := os.WriteFile(broken, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, append(files, broken)...)); err == nil {
			t.Fatal("a malformed record was accepted")
		}
	})
	t.Run("another record kind", func(t *testing.T) {
		files := fleetRecords(t, t.TempDir(), func(ev *FleetEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Kind = configEvidenceKind
			}
		})
		if err := fleetAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another kind was accepted")
		}
	})
}

// TestFleetZeroHitsGrepCatchesUnauthorizedBlobsAndPrivateKeys proves that
// grepFleetTree and grepFleetBytes report any occurrence of an unauthorized
// public-key blob or private-key fixture fragment.
func TestFleetZeroHitsGrepCatchesUnauthorizedBlobsAndPrivateKeys(t *testing.T) {
	dir := t.TempDir()
	kp, err := generateFleetKey("mallory@unapproved")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{kp.base64Blob, fleetPrivateKeyHeader, fleetPrivateKeyBody, fleetPrivateKeyFooter}

	cleanFile := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(cleanFile, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI0000000000000000000000000000000000000000000 alice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hit := grepFleetTree(dir, "home", forbidden); hit != "" {
		t.Fatalf("clean tree reported leak: %s", hit)
	}

	leakFile := filepath.Join(dir, "nested", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(leakFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(leakFile, []byte(kp.pubLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if hit := grepFleetTree(dir, "home", forbidden); hit == "" {
		t.Fatal("leaked unauthorized key blob in tree was not caught")
	}
	if hit := grepFleetBytes([]byte(fleetPrivateKeyHeader), "capture[0]", forbidden); hit == "" {
		t.Fatal("leaked private-key header in capture was not caught")
	}
}

func buildFleetImport(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), exe("mm-fleet-import"))
	build := exec.Command("go", "build", "-o", bin, "../mm-fleet-import")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build mm-fleet-import: %v\n%s", err, out)
	}
	return bin
}

// TestFleetGateEndToEnd builds the real managed-machine and mm-fleet-import
// binaries and runs `mm-walkthrough fleet` against them on the current host,
// proving every D9 case, zero sentinel invocations, zero real-home profile or
// ~/.ssh changes, no host paths and zero unauthorized or private-key bytes in
// the evidence.
func TestFleetGateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core and runs the S5 fleet gate")
	}
	bin := buildCore(t)
	importBin := buildFleetImport(t)
	fakeHome := isolateRealHomeForTest(t)
	out := filepath.Join(t.TempDir(), "fleet-gate.json")
	if err := fleetCmd([]string{
		"-binary", bin,
		"-import-binary", importBin,
		"-target", host(),
		"-evidence", out,
		"-source-revision", "test-rev",
	}); err != nil {
		t.Fatalf("fleet gate failed: %v", err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var ev FleetEvidence
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.SchemaVersion != 1 || ev.Kind != fleetEvidenceKind || ev.Mode != fleetMode || ev.Result != "pass" {
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
	for _, want := range fleetCaseNames {
		if !fleetCasePresent(ev, want) {
			t.Errorf("missing D9 case %q", want)
		}
	}
	for _, c := range ev.Cases {
		if !c.OK {
			t.Errorf("case %s failed: %s", c.Case, c.Detail)
		}
	}
	for _, s := range []string{fleetPrivateKeyHeader, fleetPrivateKeyBody, fleetPrivateKeyFooter} {
		if strings.Contains(string(b), s) {
			t.Fatalf("evidence leaked private-key fixture %q", s)
		}
	}
	if strings.Contains(string(b), fakeHome) || strings.Contains(string(b), filepath.ToSlash(fakeHome)) {
		t.Fatalf("evidence leaked real home path %q", fakeHome)
	}
}

// TestReadCompatAcceptsFleetCutoverLine proves that the macOS F01 compatibility
// reader accepts the 4-line f01-compat.txt of all four characterization
// suites (read-only, host-state, catalog and fleet cutover), and fails when
// any one of them is missing.
func TestReadCompatAcceptsFleetCutoverLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f01-compat.txt")
	lines := []string{
		"read-only characterization: 17 cases match goldens",
		"host-state characterization: 20 cases match goldens",
		"catalog characterization: 33 cases match goldens",
		fleetCutoverCompatLine,
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := readCompat(p, "darwin/arm64")
	if c.Status != "pass" || len(c.Results) != 4 {
		t.Fatalf("readCompat = %+v, want pass with 4 results", c)
	}
	for drop := range lines {
		partial := append(append([]string{}, lines[:drop]...), lines[drop+1:]...)
		if err := os.WriteFile(p, []byte(strings.Join(partial, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if c := readCompat(p, "darwin/arm64"); c.Status != "fail" {
			t.Errorf("readCompat without %q = %q, want fail", lines[drop], c.Status)
		}
	}
}
