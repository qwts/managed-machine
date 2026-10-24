package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func releaseRecords(t *testing.T, dir string, change func(ev *ReleaseEvidence)) []string {
	t.Helper()
	var files []string
	for _, tg := range firstReleaseTargets {
		compat := Compat{Status: "not_applicable"}
		if strings.HasPrefix(tg, "darwin/") {
			compat = Compat{Status: "pass"}
		}
		ev := ReleaseEvidence{
			InstallEvidence: InstallEvidence{
				SchemaVersion: 1, Kind: releaseEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
				SourceRevision: "rev", BinarySHA256: "abc", Mode: releaseMode,
				Setup: []string{}, Steps: []InstallStep{{Name: "probe", OK: true}}, Plans: []PlanRecord{},
				Guards: InstallGuards{SentinelInvocations: []string{}, ProfileChanges: []string{}},
				Result: "pass", Failures: []string{},
			},
			Version:   "0.9.0",
			Artifact:  ReleaseArtifact{Name: targetArchiveName("0.9.0", tg), SHA256: strings.Repeat("a", 64)},
			OSVersion: "fixture-os-" + strings.ReplaceAll(tg, "/", "-"),
			Compat:    compat,
			Cases:     make([]ReleaseCase, 0, len(releaseCaseNames)),
		}
		for _, name := range releaseCaseNames {
			want := "ok"
			detail := "proven"
			if isMacOSOnlyReleaseCase(name) && !strings.HasPrefix(tg, "darwin/") {
				want = "n-a"
				detail = releaseFirstReleaseReason
			}
			if name == caseReleaseGuards {
				want = "intact"
			}
			ev.Cases = append(ev.Cases, ReleaseCase{
				Case:     name,
				Expected: want,
				Observed: want,
				OK:       true,
				Detail:   detail,
			})
		}
		if change != nil {
			change(&ev)
		}
		b, err := json.MarshalIndent(ev, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, "release-gate-"+strings.ReplaceAll(tg, "/", "-")+".json")
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
	}
	return files
}

// TestReleaseFormulaDerivation proves that deriveOldFormula and
// deriveCandidateFormula rewrite only URLs/pins and keep the rest of the
// formula byte-identical.
func TestReleaseFormulaDerivation(t *testing.T) {
	repoRoot, err := findReleaseRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	isolateRealHomeForTest(t)
	h := &releaseHarness{
		installHarness: &installHarness{root: t.TempDir()},
		repoRoot:       repoRoot,
	}

	oldBase, err := h.runGitIsolated(repoRoot, "show", "v0.8.12:Formula/managed-machine.rb")
	if err != nil {
		t.Fatalf("git show v0.8.12:Formula/managed-machine.rb: %v", err)
	}
	commitA := "1111111111111111111111111111111111111111"
	commitC := "3333333333333333333333333333333333333333"
	candSHA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	coreSHA := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	checkoutDir := filepath.Join(h.root, "repos", "managed-machine")
	configDir := filepath.Join(h.root, "repos", "managed-machine-config.git")
	localBinDir := filepath.Join(h.root, "repos", "local-bin.git")
	coreArchive := filepath.Join(h.root, "dist", "managed-machine_0.9.0_darwin_arm64.tar.gz")

	oldDerived, err := deriveOldFormula(oldBase, checkoutDir, configDir, commitA, localBinDir)
	if err != nil {
		t.Fatalf("deriveOldFormula failed: %v", err)
	}
	if err := assertFormulaDiffersOnlyInURLs(oldBase, oldDerived); err != nil {
		t.Fatalf("assertFormulaDiffersOnlyInURLs(old): %v", err)
	}
	if !strings.Contains(oldDerived, `url "file://`+filepath.ToSlash(checkoutDir)+`"`) ||
		!strings.Contains(oldDerived, `revision: "`+commitA+`"`) {
		t.Fatalf("oldDerived missing expected rewrites:\n%s", oldDerived)
	}

	rawCand, err := os.ReadFile(filepath.Join(repoRoot, "Formula", "managed-machine.rb"))
	if err != nil {
		t.Fatal(err)
	}
	candBase, err := ensureCandidateFormulaBase(string(rawCand), "0.9.0")
	if err != nil {
		t.Fatalf("ensureCandidateFormulaBase: %v", err)
	}
	for _, want := range []string{
		"depends_on arch: :arm64",
		"depends_on macos: :sequoia",
		`resource "core" do`,
		`resource("core").stage`,
	} {
		if !strings.Contains(candBase, want) {
			t.Errorf("candBase missing %q", want)
		}
	}

	candDerived, err := deriveCandidateFormula(candBase, checkoutDir, candSHA, configDir, "0.9.0", commitC, localBinDir, coreArchive, coreSHA)
	if err != nil {
		t.Fatalf("deriveCandidateFormula failed: %v", err)
	}
	if err := assertFormulaDiffersOnlyInURLs(candBase, candDerived); err != nil {
		t.Fatalf("assertFormulaDiffersOnlyInURLs(candidate): %v", err)
	}
	for _, want := range []string{
		`url "file://` + filepath.ToSlash(checkoutDir) + `"`,
		`revision: "` + candSHA + `"`,
		`url "file://` + filepath.ToSlash(configDir) + `"`,
		`revision: "` + commitC + `"`,
		`url "file://` + filepath.ToSlash(localBinDir) + `"`,
		`url "file://` + filepath.ToSlash(coreArchive) + `"`,
		`sha256 "` + coreSHA + `"`,
	} {
		if !strings.Contains(candDerived, want) {
			t.Errorf("candDerived missing %q", want)
		}
	}

	// Mutating any non-URL line must be rejected by assertFormulaDiffersOnlyInURLs.
	tampered := strings.Replace(candDerived, `bin.install "bin/managed-machine"`, `bin.install "bin/other"`, 1)
	if err := assertFormulaDiffersOnlyInURLs(candBase, tampered); err == nil {
		t.Fatal("assertFormulaDiffersOnlyInURLs accepted a non-URL edit in def install")
	}
}

// TestReleaseEvidenceCarriesTheInstallShapeAndTheCases proves the serialized
// field order of ReleaseEvidence, ReleaseArtifact and ReleaseCase.
func TestReleaseEvidenceCarriesTheInstallShapeAndTheCases(t *testing.T) {
	ev := ReleaseEvidence{
		InstallEvidence: InstallEvidence{
			SchemaVersion: 1, Kind: releaseEvidenceKind, Target: "linux/amd64",
			SourceRevision: "rev", Mode: releaseMode, Result: "pass",
		},
		Version:   "0.9.0",
		Artifact:  ReleaseArtifact{Name: "managed-machine_0.9.0_linux_amd64.tar.gz", SHA256: strings.Repeat("a", 64)},
		OSVersion: "Ubuntu 24.04 LTS",
		Compat:    Compat{Status: "not_applicable"},
		Cases: []ReleaseCase{{
			Case: caseReleaseUpgrade, Expected: "ok", Observed: "ok", OK: true, Detail: "proven",
		}},
	}
	b := mustMarshal(t, ev)
	keys, err := objectKeys(b)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"schema_version", "kind", "target", "os", "arch", "source_revision", "binary_sha256", "mode",
		"setup", "steps", "plans", "guards", "result", "failures",
		"version", "artifact", "os_version", "f01_compatibility", "cases",
	}
	if !sameStrings(keys, want) {
		t.Fatalf("the evidence keys are %v, want %v", keys, want)
	}
	art := mustMarshal(t, ev.Artifact)
	keys, err = objectKeys(art)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStrings(keys, []string{"name", "sha256"}) {
		t.Fatalf("the artifact keys are %v, want [name sha256]", keys)
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

// TestReleaseAggregateAcceptsFivePassingRecords proves that five passing
// s6-release-gate records of one revision close the gate, and that passing
// -checksums and -out generates support-matrix.md.
func TestReleaseAggregateAcceptsFivePassingRecords(t *testing.T) {
	dir := t.TempDir()
	files := releaseRecords(t, dir, nil)
	if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err != nil {
		t.Fatalf("five passing records were rejected: %v", err)
	}

	// Write SHA256SUMS matching the five records, plus provider and install gate records.
	var sums strings.Builder
	var extraFiles []string
	for _, tg := range firstReleaseTargets {
		arc := targetArchiveName("0.9.0", tg)
		sums.WriteString(strings.Repeat("a", 64) + "  " + arc + "\n")
		slug := strings.ReplaceAll(tg, "/", "-")

		pev := ProviderEvidence{
			InstallEvidence: InstallEvidence{
				SchemaVersion: 1, Kind: providerEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
				SourceRevision: "rev", Mode: providerMode, Result: "pass", Failures: []string{},
			},
			Combinations: []ProviderCombination{{Provider: "npm", Software: "typescript", TargetKind: kindUser, Expected: observedInstalled, Observed: observedInstalled, OK: true}},
		}
		pb, err := json.MarshalIndent(pev, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		pp := filepath.Join(dir, "provider-gate-"+slug+".json")
		if err := os.WriteFile(pp, pb, 0o644); err != nil {
			t.Fatal(err)
		}
		extraFiles = append(extraFiles, pp)

		iev := InstallEvidence{
			SchemaVersion: 1, Kind: installEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
			SourceRevision: "rev", Mode: modeRefusal, Result: "pass", Failures: []string{},
		}
		ib, err := json.MarshalIndent(iev, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		ip := filepath.Join(dir, "install-gate-"+slug+".json")
		if err := os.WriteFile(ip, ib, 0o644); err != nil {
			t.Fatal(err)
		}
		extraFiles = append(extraFiles, ip)
	}
	sumsPath := filepath.Join(dir, "SHA256SUMS")
	if err := os.WriteFile(sumsPath, []byte(sums.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	matrixPath := filepath.Join(dir, "support-matrix.md")
	allArgs := append([]string{
		"-source-revision", "rev",
		"-version", "0.9.0",
		"-checksums", sumsPath,
		"-out", matrixPath,
		"-run-id", "12345",
	}, append(files, extraFiles...)...)
	if err := releaseAggregateCmd(allArgs); err != nil {
		t.Fatalf("release-aggregate with checksums and support-matrix failed: %v", err)
	}
	mdBytes, err := os.ReadFile(matrixPath)
	if err != nil {
		t.Fatalf("read support-matrix.md: %v", err)
	}
	md := string(mdBytes)
	for _, want := range []string{
		"# Support Matrix (`v0.9.0`)",
		"`darwin/arm64`",
		"`linux/amd64`",
		"`linux/arm64`",
		"`windows/amd64`",
		"`windows/arm64`",
		"fixture-os-darwin-arm64",
		"fixture-os-linux-amd64",
		"fixture-os-windows-arm64",
		"managed-machine_0.9.0_darwin_arm64.tar.gz",
		"managed-machine_0.9.0_windows_amd64.zip",
		"## Untested and Unsupported Surfaces",
		"`darwin/amd64` (Intel macOS): **not a target**",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("support-matrix.md missing %q:\n%s", want, md)
		}
	}
}

// TestReleaseAggregateRejectsBadRecords proves that release-aggregate enforces
// the S6 target/case rules including `n-a` handling for macOS-only cases,
// version consistency, and SHA256SUMS digest agreement.
func TestReleaseAggregateRejectsBadRecords(t *testing.T) {
	t.Run("four records", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), nil)
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files[:4]...)); err == nil {
			t.Fatal("four records were accepted as five targets")
		}
	})
	t.Run("another revision", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "linux/amd64" {
				ev.SourceRevision = "other"
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another revision was accepted")
		}
	})
	t.Run("mixed version", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "linux/amd64" {
				ev.Version = "0.9.1"
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("mixed version across records was accepted")
		}
	})
	t.Run("artifact sha256 mismatch against SHA256SUMS", func(t *testing.T) {
		dir := t.TempDir()
		files := releaseRecords(t, dir, func(ev *ReleaseEvidence) {
			if ev.Target == "windows/arm64" {
				ev.Artifact.SHA256 = strings.Repeat("b", 64)
			}
		})
		var sums strings.Builder
		for _, tg := range firstReleaseTargets {
			sums.WriteString(strings.Repeat("a", 64) + "  " + targetArchiveName("0.9.0", tg) + "\n")
		}
		sumsPath := filepath.Join(dir, "SHA256SUMS")
		if err := os.WriteFile(sumsPath, []byte(sums.String()), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev", "-checksums", sumsPath, "-out", filepath.Join(dir, "matrix.md")}, files...)); err == nil {
			t.Fatal("artifact.sha256 mismatch against SHA256SUMS was accepted")
		}
	})
	t.Run("f01_compatibility fail on darwin", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Compat.Status = "fail"
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("f01_compatibility fail on darwin/arm64 was accepted")
		}
	})
	t.Run("macos case n-a on darwin", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "darwin/arm64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseReleasePrivateSeed {
						ev.Cases[i].Observed = "n-a"
					}
				}
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("private-seed n-a on darwin/arm64 was accepted")
		}
	})
	t.Run("macos case ok outside darwin", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "linux/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseReleaseDispatcher {
						ev.Cases[i].Observed = "ok"
					}
				}
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("dispatcher ok on linux/amd64 was accepted")
		}
	})
	t.Run("macos case n-a without reason outside darwin", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "windows/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseReleaseUpgradeFromBash {
						ev.Cases[i].Detail = ""
					}
				}
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("upgrade-from-bash n-a with empty reason was accepted")
		}
	})
	t.Run("portable case n-a", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "linux/amd64" {
				for i := range ev.Cases {
					if ev.Cases[i].Case == caseReleaseRecovery {
						ev.Cases[i].Observed = "n-a"
					}
				}
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("recovery recorded n-a was accepted")
		}
	})
	t.Run("missing case", func(t *testing.T) {
		files := releaseRecords(t, t.TempDir(), func(ev *ReleaseEvidence) {
			if ev.Target == "linux/arm64" {
				ev.Cases = ev.Cases[:len(ev.Cases)-1]
			}
		})
		if err := releaseAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("missing case was accepted")
		}
	})
}

// TestReleaseUpgradeFromBashRefusesOutsideHostedRunner proves that
// upgrade-from-bash never invokes brew when not on a GitHub-hosted runner and
// without a test brew prefix override, and records n-a with the first-release
// reason on non-darwin targets.
func TestReleaseUpgradeFromBashRefusesOutsideHostedRunner(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_ENVIRONMENT", "")
	origOverride := releaseBrewPrefixOverride
	releaseBrewPrefixOverride = ""
	t.Cleanup(func() { releaseBrewPrefixOverride = origOverride })

	h := &releaseHarness{
		installHarness: &installHarness{
			root:   t.TempDir(),
			target: "darwin/arm64",
			ev:     &InstallEvidence{},
		},
	}
	h.runUpgradeFromBash()
	if len(h.cases) != 1 || h.cases[0].OK || h.cases[0].Detail != releaseHostedBrewRefusal {
		t.Fatalf("unexpected upgrade-from-bash case outside hosted runner: %+v", h.cases)
	}

	nonDarwin := &releaseHarness{
		installHarness: &installHarness{
			root:   t.TempDir(),
			target: "linux/amd64",
			ev:     &InstallEvidence{},
		},
	}
	nonDarwin.runUpgradeFromBash()
	nonDarwin.runPrivateSeed()
	nonDarwin.runDispatcher()
	if len(nonDarwin.cases) != 3 {
		t.Fatalf("expected 3 non-darwin cases, got %d", len(nonDarwin.cases))
	}
	for _, c := range nonDarwin.cases {
		if !c.OK || c.Observed != "n-a" || c.Detail != releaseFirstReleaseReason {
			t.Errorf("non-darwin case %s = %+v, want OK n-a with %q", c.Case, c, releaseFirstReleaseReason)
		}
	}
}

// TestReleaseZeroHitGrep proves that grepEvidenceBytes and grepStateAndCaptures
// catch runner paths, the invoking account name, synthetic seed markers and
// key bytes.
func TestReleaseZeroHitGrep(t *testing.T) {
	alice, err := generateFleetKey("alice@test")
	if err != nil {
		t.Fatal(err)
	}
	seedKey, err := generateFleetKey("seed@test")
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "harness-root")
	realHome := filepath.Join(t.TempDir(), "real-home")
	stateDir := filepath.Join(root, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &releaseHarness{
		installHarness: &installHarness{
			root:     root,
			realHome: realHome,
			state:    stateDir,
			paths:    gatePaths{root: root, home: realHome, tmp: filepath.Join(root, "tmp")},
		},
		account:        "testowneracct",
		aliceKey:       alice,
		seedFixtureKey: seedKey,
		stateForbidden: []string{
			fleetPrivateKeyHeader,
			releaseSeedMarkerZSH,
			releaseSeedMarkerAuthKeys,
			alice.seedHex,
			seedKey.base64Blob,
		},
		evidenceForbidden: []string{
			fleetPrivateKeyHeader,
			releaseSeedMarkerZSH,
			releaseSeedMarkerAuthKeys,
			alice.base64Blob,
			seedKey.base64Blob,
			root, filepath.ToSlash(root),
			realHome, filepath.ToSlash(realHome),
		},
	}

	cleanEv := []byte(`{"result":"pass","detail":"<harness>/ok by <account>"}`)
	if hit := h.grepEvidenceBytes(cleanEv); hit != "" {
		t.Fatalf("clean evidence reported leak: %s", hit)
	}
	for _, sample := range []string{
		`{"detail":"leaked ` + releaseSeedMarkerZSH + `"}`,
		`{"detail":"leaked ` + releaseSeedMarkerAuthKeys + `"}`,
		`{"detail":"leaked ` + alice.base64Blob + `"}`,
		`{"detail":"leaked ` + filepath.ToSlash(root) + `"}`,
		`{"detail":"leaked user testowneracct in text"}`,
	} {
		if hit := h.grepEvidenceBytes([]byte(sample)); hit == "" {
			t.Errorf("grepEvidenceBytes missed leak in %s", sample)
		}
	}

	scrubbed := h.scrubReleaseEvidence([]byte(`{"detail":"path ` + filepath.ToSlash(root) + ` user testowneracct"}`))
	if hit := h.grepEvidenceBytes(scrubbed); hit != "" {
		t.Fatalf("scrubbed evidence still triggered grep: %s (%s)", hit, string(scrubbed))
	}

	if hit := h.grepStateAndCaptures(); hit != "" {
		t.Fatalf("empty state reported leak: %s", hit)
	}
	h.captures = [][]byte{[]byte("captured " + releaseSeedMarkerZSH)}
	if hit := h.grepStateAndCaptures(); hit == "" {
		t.Fatal("grepStateAndCaptures missed seed marker in capture")
	}
}

// TestReleasePrivateSeedScenarios exercises the D7 synthetic seed scenarios
// (a)-(f) directly against a staged v0.8.12 + candidate tree and verifies both
// the passing path and that a git sentinel invocation in scenario (f) fails the
// case.
func TestReleasePrivateSeedScenarios(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("private-seed is a macOS Bash/git case")
	}
	repoRoot, err := findReleaseRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	isolateRealHomeForTest(t)

	alice, err := generateFleetKey("alice@managed-machine-release-gate")
	if err != nil {
		t.Fatal(err)
	}
	seedKey, err := generateFleetKey("seed-fixture@managed-machine-release-gate")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := generateFleetKey("host-state@managed-machine-release-gate")
	if err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	sentinelsDir := filepath.Join(root, "sentinels")
	logPath := filepath.Join(root, "sentinel.log")
	if err := installGateSentinelsFor(sentinelsDir, logPath, releaseSentinels); err != nil {
		t.Fatal(err)
	}
	goodCore := `#!/bin/bash
set -euo pipefail
[[ -n "${MANAGED_MACHINE_POLICY_DIR:-}" && -f "${MANAGED_MACHINE_POLICY_DIR}/fleet/key-directory.yaml" ]] || exit 1
if [[ "${1:-}" == "fleet" ]]; then
  printf '{"directory":"path","owners":1,"keys":1}\n'
  printf '{"owner":"alice","fingerprint":"` + alice.fingerprint + `"}\n'
  exit 0
fi
if [[ "${1:-}" == "status" ]]; then
  printf '{"section":"fleet","resource":"policy","state":"present"}\n'
  exit 0
fi
exit 1
`
	distDir := filepath.Join(root, "dist")
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tarBytes, err := packReleaseTarGz("managed-machine", []byte(goodCore), []byte("LICENSE\n"))
	if err != nil {
		t.Fatal(err)
	}
	tarPath := filepath.Join(distDir, targetArchiveName("0.9.0", "darwin/arm64"))
	if err := os.WriteFile(tarPath, tarBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	h := &releaseHarness{
		installHarness: &installHarness{
			root:   root,
			target: "darwin/arm64",
			sents:  sentinelsDir,
			log:    logPath,
			ev:     &InstallEvidence{},
		},
		repoRoot:       repoRoot,
		keyDir:         filepath.Join(root, "keydir"),
		upgradeDistDir: distDir,
		upgradeVer:     "0.9.0",
		stagedOld:      filepath.Join(root, "staged", "old"),
		stagedNew:      filepath.Join(root, "staged", "new"),
		bashStubBin:    filepath.Join(root, "bash-stubs"),
		aliceKey:       alice,
		seedFixtureKey: seedKey,
		hostStateKey:   hostKey,
	}

	h.runPrivateSeed()
	if len(h.cases) != 1 || !h.cases[0].OK || h.cases[0].Observed != "ok" {
		t.Fatalf("expected private-seed scenarios (a)-(f) to pass, got %+v", h.cases)
	}

	// Verify that if the core starts `git` during scenario (f), the sentinel is
	// caught and the case fails.
	badCore := strings.Replace(goodCore, "if [[ \"${1:-}\" == \"fleet\" ]]; then", "git status >/dev/null 2>&1 || true\nif [[ \"${1:-}\" == \"fleet\" ]]; then", 1)
	stagedCore := filepath.Join(h.stagedNew, "opt", "managed-machine", "libexec", "core", "managed-machine")
	if err := writeExecutableAtomically(stagedCore, []byte(badCore)); err != nil {
		t.Fatal(err)
	}
	_ = os.RemoveAll(filepath.Join(root, "private-seed-home"))
	_ = os.Remove(logPath)
	h.cases = nil
	h.runPrivateSeed()
	if len(h.cases) != 1 || h.cases[0].OK || !strings.Contains(h.cases[0].Detail, "sentinel was started by the core") {
		t.Fatalf("expected git sentinel invocation in scenario (f) to fail private-seed, got %+v", h.cases)
	}
}

// installFakeBrewForTest writes a self-contained fake `brew` script under a
// temporary Homebrew prefix inside t.TempDir() so `upgrade-from-bash`,
// `private-seed`, and `dispatcher` can be unit-tested end-to-end on macOS
// without ever running the machine's real `brew` or touching `/opt/homebrew`.
func installFakeBrewForTest(t *testing.T) string {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "fake-homebrew")
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := `#!/usr/bin/env bash
set -euo pipefail
PREFIX="$(cd "$(dirname "$0")/.." && pwd)"
CELLAR="$PREFIX/Cellar/managed-machine"
OPT="$PREFIX/opt/managed-machine"

cmd="${1:-}"
shift || true

case "$cmd" in
  --prefix)
    printf '%s\n' "$PREFIX"
    exit 0
    ;;
  tap-new)
    tap=""
    for arg in "$@"; do
      case "$arg" in
        --*) ;;
        *) tap="$arg" ;;
      esac
    done
    mkdir -p "$PREFIX/Library/Taps/${tap%%/*}/homebrew-${tap#*/}/Formula"
    exit 0
    ;;
  --repository)
    tap="${1:-}"
    printf '%s\n' "$PREFIX/Library/Taps/${tap%%/*}/homebrew-${tap#*/}"
    exit 0
    ;;
  untap)
    for arg in "$@"; do
      case "$arg" in
        --*) ;;
        *) rm -rf "$PREFIX/Library/Taps/${arg%%/*}/homebrew-${arg#*/}" ;;
      esac
    done
    exit 0
    ;;
  list)
    cask=0
    versions=0
    pkg=""
    for arg in "$@"; do
      case "$arg" in
        --cask) cask=1 ;;
        --versions) versions=1 ;;
        --formula) ;;
        *) pkg="$arg" ;;
      esac
    done
    if [[ "$cask" == 1 ]]; then
      exit 0
    fi
    if [[ -n "$pkg" && "$pkg" != "managed-machine" ]]; then
      exit 1
    fi
    if [[ -z "$pkg" ]]; then
      printf 'git 2.47.0\n'
    fi
    if [[ -L "$OPT" && -f "$OPT/libexec/VERSION" ]]; then
      ver="$(tr -d '[:space:]' <"$OPT/libexec/VERSION")"
      printf 'managed-machine %s\n' "$ver"
    elif [[ "$pkg" == "managed-machine" ]]; then
      exit 1
    fi
    exit 0
    ;;
  uninstall)
    rm -rf "$CELLAR" "$OPT"
    exit 0
    ;;
  install|upgrade)
    formula=""
    for arg in "$@"; do
      case "$arg" in
        --formula) ;;
        *) formula="$arg" ;;
      esac
    done
    if [[ ! -f "$formula" && "$formula" == */*/* ]]; then
      t="${formula%/*}"
      formula="$PREFIX/Library/Taps/${t%%/*}/homebrew-${t#*/}/Formula/${formula##*/}.rb"
    fi
    [[ -f "$formula" ]] || { echo "missing formula $formula" >&2; exit 1; }
    eval "$(python3 - "$formula" <<'PY'
import re, shlex, sys
text = open(sys.argv[1], encoding="utf-8").read()
ver = re.search(r'^\s*version\s+"([^"]+)"', text, re.M).group(1)
main_m = re.search(r'^\s*url\s+"file://([^"]+)",\s*\n\s*using:\s*:git,\s*\n\s*(tag|revision):\s*"([^"]+)"', text, re.M)
main_repo, main_ref = main_m.group(1), main_m.group(3)
cfg_m = re.search(r'resource "managed-machine-config" do\s*\n\s*url\s+"file://([^"]+)",\s*\n\s*using:\s*:git,\s*\n\s*tag:\s*"[^"]+",\s*\n\s*revision:\s*"([^"]+)"', text, re.M)
cfg_repo, cfg_rev = cfg_m.group(1), cfg_m.group(2)
lb_m = re.search(r'resource "local-bin" do\s*\n\s*url\s+"file://([^"]+)"', text, re.M)
lb_repo = lb_m.group(1)
core_m = re.search(r'resource "core" do\s*\n\s*url\s+"file://([^"]+)"\s*\n\s*sha256\s+"([0-9a-f]{64})"', text, re.M)
core_url = core_m.group(1) if core_m else ""
core_sha = core_m.group(2) if core_m else ""
for k, v in [
    ("F_VER", ver),
    ("F_MAIN_REPO", main_repo),
    ("F_MAIN_REF", main_ref),
    ("F_CFG_REPO", cfg_repo),
    ("F_CFG_REV", cfg_rev),
    ("F_LB_REPO", lb_repo),
    ("F_CORE_URL", core_url),
    ("F_CORE_SHA", core_sha),
]:
    print(f"{k}={shlex.quote(v)}")
PY
)"
    keg="$CELLAR/$F_VER"
    libexec="$keg/libexec"
    bin_dir="$keg/bin"
    rm -rf "$keg"
    mkdir -p "$libexec" "$bin_dir" "$PREFIX/opt"

    work="$(mktemp -d)"
    trap 'rm -rf "$work"' EXIT
    git -c protocol.file.allow=always clone --quiet --no-hardlinks "$F_MAIN_REPO" "$work/src"
    git -C "$work/src" checkout --quiet "$F_MAIN_REF"
    commit_sha="$(git -C "$work/src" rev-parse HEAD)"

    for f in "$work/src"/setup-*; do
      [[ -f "$f" ]] && cp "$f" "$libexec/" && chmod 0755 "$libexec/$(basename "$f")"
    done
    cp -R "$work/src/lib" "$libexec/lib"
    mkdir -p "$libexec/scripts" "$libexec/git-hooks"
    cp -R "$work/src/scripts/." "$libexec/scripts/"
    cp -R "$work/src/git-hooks/." "$libexec/git-hooks/"

    git -c protocol.file.allow=always clone --quiet --no-hardlinks "$F_CFG_REPO" "$libexec/managed-machine-config"
    git -C "$libexec/managed-machine-config" checkout --quiet --detach "$F_CFG_REV"

    git -c protocol.file.allow=always clone --quiet --no-hardlinks "$F_LB_REPO" "$libexec/local-bin"

    if [[ -f "$work/src/VERSION" ]]; then
      cp "$work/src/VERSION" "$libexec/VERSION"
      mkdir -p "$libexec/skills"
      cp -R "$work/src/skills/managed-machine" "$libexec/skills/managed-machine"
      printf '%s\n' "$commit_sha" >"$libexec/RELEASE_COMMIT"
    fi

    if [[ -n "$F_CORE_URL" ]]; then
      actual_sha="$(shasum -a 256 "$F_CORE_URL" | awk '{print $1}')"
      if [[ "$actual_sha" != "$F_CORE_SHA" ]]; then
        echo "sha256 mismatch for core resource" >&2
        exit 1
      fi
      mkdir -p "$libexec/core"
      tar -xzf "$F_CORE_URL" -C "$libexec/core" managed-machine
      chmod 0755 "$libexec/core/managed-machine"
    fi

    cp "$work/src/bin/managed-machine" "$bin_dir/managed-machine"
    chmod 0755 "$bin_dir/managed-machine"

    rm -f "$OPT"
    ln -s "../Cellar/managed-machine/$F_VER" "$OPT"
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`
	brewPath := filepath.Join(binDir, "brew")
	if err := os.WriteFile(brewPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return prefix
}

// TestReleaseGateEndToEnd builds the five release archives and runs
// `mm-walkthrough release` on the current host (with a fake Homebrew prefix in
// t.TempDir() on darwin), proving all 15 D10 release cases, zero sentinel
// invocations, zero real-home changes, and zero hits for runner paths, account
// names, fixture seed markers, or key bytes in the evidence.
func TestReleaseGateEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core and runs the S6 release gate")
	}
	repoRoot, err := findReleaseRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	vRaw, err := os.ReadFile(filepath.Join(repoRoot, "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	ver := strings.TrimSpace(string(vRaw))
	goModCache, _ := exec.Command("go", "env", "GOMODCACHE").Output()
	goCache, _ := exec.Command("go", "env", "GOCACHE").Output()
	fakeHome := isolateRealHomeForTest(t)
	if m := strings.TrimSpace(string(goModCache)); m != "" {
		t.Setenv("GOMODCACHE", m)
	}
	if c := strings.TrimSpace(string(goCache)); c != "" {
		t.Setenv("GOCACHE", c)
	}

	if runtime.GOOS == "darwin" {
		fakePrefix := installFakeBrewForTest(t)
		origOverride := releaseBrewPrefixOverride
		releaseBrewPrefixOverride = fakePrefix
		t.Cleanup(func() { releaseBrewPrefixOverride = origOverride })
	}

	distDir := filepath.Join(t.TempDir(), "dist")
	if err := buildFullDist(repoRoot, ver, distDir); err != nil {
		t.Fatalf("buildFullDist: %v", err)
	}

	out := filepath.Join(t.TempDir(), "release-gate.json")
	if err := releaseCmd([]string{
		"-dist", distDir,
		"-version", ver,
		"-target", host(),
		"-evidence", out,
		"-source-revision", "test-rev",
	}); err != nil {
		t.Fatalf("release gate failed: %v", err)
	}

	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var ev ReleaseEvidence
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.SchemaVersion != 1 || ev.Kind != releaseEvidenceKind || ev.Mode != releaseMode || ev.Result != "pass" {
		t.Fatalf("unexpected evidence header: %+v", ev)
	}
	if ev.Version != ver {
		t.Fatalf("ev.Version = %q, want %q", ev.Version, ver)
	}
	wantArtifact := targetArchiveName(ver, host())
	if ev.Artifact.Name != wantArtifact || len(ev.Artifact.SHA256) != 64 {
		t.Fatalf("ev.Artifact = %+v, want %s with 64-hex sha256", ev.Artifact, wantArtifact)
	}
	if strings.HasPrefix(host(), "darwin/") {
		if ev.Compat.Status != "pass" {
			t.Fatalf("ev.Compat = %+v, want status pass on darwin", ev.Compat)
		}
	} else if ev.Compat.Status != "not_applicable" {
		t.Fatalf("ev.Compat = %+v, want status not_applicable outside darwin", ev.Compat)
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
	if len(ev.Cases) != len(releaseCaseNames) {
		t.Fatalf("len(ev.Cases) = %d, want %d", len(ev.Cases), len(releaseCaseNames))
	}
	for i, want := range releaseCaseNames {
		if ev.Cases[i].Case != want {
			t.Errorf("case[%d] = %q, want %q", i, ev.Cases[i].Case, want)
		}
	}
	for _, c := range ev.Cases {
		if !c.OK {
			t.Errorf("case %s failed: %s", c.Case, c.Detail)
		}
		if isMacOSOnlyReleaseCase(c.Case) {
			if strings.HasPrefix(ev.Target, "darwin/") && c.Observed != "ok" {
				t.Errorf("darwin case %s observed = %q, want \"ok\"", c.Case, c.Observed)
			}
			if !strings.HasPrefix(ev.Target, "darwin/") && (c.Observed != "n-a" || c.Detail != releaseFirstReleaseReason) {
				t.Errorf("non-darwin case %s observed=%q detail=%q, want n-a with %q",
					c.Case, c.Observed, c.Detail, releaseFirstReleaseReason)
			}
		}
	}
	for _, s := range []string{
		fleetPrivateKeyHeader, fleetPrivateKeyBody, fleetPrivateKeyFooter,
		releaseSeedMarkerZSH, releaseSeedMarkerAuthKeys,
		fakeHome, filepath.ToSlash(fakeHome),
	} {
		if strings.Contains(string(b), s) {
			t.Fatalf("evidence leaked forbidden string %q", s)
		}
	}
}
