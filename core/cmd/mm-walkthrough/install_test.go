package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildCore builds the core from this tree. The prefix override is a
// link-time setting of an unexported variable: it is never a flag and never
// an environment variable, so no test can reach a real Homebrew.
func buildCore(t *testing.T, extra ...string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), exe("managed-machine"))
	args := append([]string{"build", "-o", bin}, extra...)
	args = append(args, "../managed-machine")
	out, err := exec.Command("go", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("build the core: %v\n%s", err, out)
	}
	return bin
}

// fakeBrewPrefix builds a prefix that looks like a Homebrew prefix: the
// brew program is there and the prefix is writable. The program records
// every invocation, so a test can prove that the gate started none.
func fakeBrewPrefix(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	prefix := filepath.Join(dir, "prefix")
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "brew-invocations.log")
	if runtime.GOOS == "windows" {
		body := "@echo off\r\necho %* >> \"" + marker + "\"\r\n"
		if err := os.WriteFile(filepath.Join(bin, "brew.cmd"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + marker + "\"\nexit 0\n"
		if err := os.WriteFile(filepath.Join(bin, "brew"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return prefix, marker
}

// stepByName returns one step of the evidence.
func stepByName(ev *InstallEvidence, name string) (InstallStep, bool) {
	for _, s := range ev.Steps {
		if s.Name == name {
			return s, true
		}
	}
	return InstallStep{}, false
}

// TestBrewModeIsRefusedWithoutAHostedRunner proves that the gate refuses
// to change a machine before it starts any process: the refusal comes
// before the mode setup, so no Homebrew command ever runs.
func TestBrewModeIsRefusedWithoutAHostedRunner(t *testing.T) {
	prefix, marker := fakeBrewPrefix(t)
	restore := brewPrefixOverride
	brewPrefixOverride = prefix
	defer func() { brewPrefixOverride = restore }()
	t.Setenv("GITHUB_ACTIONS", "")
	t.Setenv("RUNNER_ENVIRONMENT", "")
	bin := buildCore(t, "-ldflags", "-X main.prefixOverride="+prefix)

	out := filepath.Join(t.TempDir(), "evidence.json")
	err := installCmd([]string{"-binary", bin, "-target", host(), "-evidence", out, "-source-revision", "test"})
	if err == nil {
		t.Fatal("the gate ran the brew mode outside a hosted runner")
	}
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("no evidence written: %v", rerr)
	}
	var ev InstallEvidence
	if jerr := json.Unmarshal(b, &ev); jerr != nil {
		t.Fatal(jerr)
	}
	if ev.Mode != modeBrew {
		t.Fatalf("mode = %q, want %q", ev.Mode, modeBrew)
	}
	if len(ev.Setup) != 0 {
		t.Fatalf("the gate ran Homebrew commands before it refused: %v", ev.Setup)
	}
	if !strings.Contains(strings.Join(ev.Failures, "\n"), hostedRunnerMessage) {
		t.Fatalf("the failure does not name the refusal: %v", ev.Failures)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a Homebrew command ran before the gate refused")
	}
}

// TestGateFixturesAreWellFormed proves that the archive the slow formula
// points at is a real gzip tar and that both formulae carry what Homebrew
// needs: a version, a digest and an install method.
func TestGateFixturesAreWellFormed(t *testing.T) {
	dir := t.TempDir()
	tarball := filepath.Join(dir, "mm-gate-slow-1.0.tar.gz")
	if err := writeTarball(tarball, "mm-gate-slow-1.0/README", "install gate fixture\n"); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(tarball)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	tr := tar.NewReader(gz)
	var files []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("the archive is not a tar: %v", err)
		}
		if hdr.Typeflag == tar.TypeReg {
			files = append(files, hdr.Name)
		}
	}
	if len(files) != 1 || files[0] != "mm-gate-slow-1.0/README" {
		t.Fatalf("the archive holds %v, want one file", files)
	}
	digest, err := fileDigest(tarball)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 {
		t.Fatalf("the digest is %q, want 64 hex characters", digest)
	}

	h := &installHarness{root: dir, paths: gatePaths{root: dir}}
	tap := filepath.Join(dir, "tap")
	if err := h.writeFormulas(tap); err != nil {
		t.Fatal(err)
	}
	fail, err := os.ReadFile(filepath.Join(tap, "Formula", "mm-gate-fail.rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"class MmGateFail < Formula",
		`homepage "https://example.invalid"`,
		`url "file://`,
		`sha256 "` + strings.Repeat("0", 64) + `"`,
		`version "1.0"`,
		"def install",
	} {
		if !strings.Contains(string(fail), want) {
			t.Errorf("the failing formula omits %q:\n%s", want, fail)
		}
	}
	if !strings.Contains(string(fail), "file://") {
		t.Errorf("the failing formula names no file URL:\n%s", fail)
	}
	slow, err := os.ReadFile(filepath.Join(tap, "Formula", "mm-gate-slow.rb"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"class MmGateSlow < Formula",
		`url "file://`,
		`sha256 "` + digest + `"`,
		`version "1.0"`,
		"sleep 900",
	} {
		if !strings.Contains(string(slow), want) {
			t.Errorf("the slow formula omits %q:\n%s", want, slow)
		}
	}
}

// TestEvidenceSanitizationRemovesTheHarnessAndTheHome proves that the
// evidence carries placeholders instead of the runner's paths.
func TestEvidenceSanitizationRemovesTheHarnessAndTheHome(t *testing.T) {
	p := gatePaths{root: "/tmp/mm-install-gate-42", home: "/Users/runner", tmp: "/tmp"}
	got := p.clean("the harness /tmp/mm-install-gate-42/home and the home /Users/runner/.zshrc and /tmp/x")
	want := "the harness <harness>/home and the home <home>/.zshrc and <tmp>/x"
	if got != want {
		t.Errorf("clean = %q, want %q", got, want)
	}
	scrubbed := string(p.scrub(mustMarshal(t, map[string]string{
		"detail": "the harness /tmp/mm-install-gate-42/home and the home /Users/runner/.zshrc",
	})))
	if strings.Contains(scrubbed, "mm-install-gate-42") || strings.Contains(scrubbed, "/Users/runner") {
		t.Errorf("the encoded evidence still carries a path: %s", scrubbed)
	}
}

// mustMarshal encodes a value or fails the test.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// gateRecords writes one passing evidence record per first-release target.
func gateRecords(t *testing.T, dir string, change func(ev *InstallEvidence)) []string {
	t.Helper()
	var files []string
	for _, tg := range []string{"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64"} {
		ev := InstallEvidence{
			SchemaVersion: 1, Kind: installEvidenceKind, Target: tg, OS: targetOS(tg), Arch: targetArch(tg),
			SourceRevision: "rev", BinarySHA256: "abc", Mode: modeRefusal,
			Setup: []string{}, Steps: []InstallStep{{Name: "probe", OK: true}}, Plans: []PlanRecord{},
			Guards: InstallGuards{SentinelInvocations: []string{}, ProfileChanges: []string{}},
			Result: "pass", Failures: []string{},
		}
		if tg == "darwin/arm64" {
			ev.Mode = modeBrew
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

// TestInstallAggregateAcceptsFivePassingRecords proves that five records of
// one revision close the gate.
func TestInstallAggregateAcceptsFivePassingRecords(t *testing.T) {
	files := gateRecords(t, t.TempDir(), nil)
	if err := installAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err != nil {
		t.Fatalf("five passing records were rejected: %v", err)
	}
}

// TestInstallAggregateRejectsBadRecords proves that the aggregate refuses
// every record set that is not five passing records of one revision with
// macOS in the brew mode.
func TestInstallAggregateRejectsBadRecords(t *testing.T) {
	t.Run("four records", func(t *testing.T) {
		files := gateRecords(t, t.TempDir(), nil)
		if err := installAggregateCmd(append([]string{"-source-revision", "rev"}, files[:4]...)); err == nil {
			t.Fatal("four records were accepted as five targets")
		}
	})
	t.Run("another revision", func(t *testing.T) {
		files := gateRecords(t, t.TempDir(), func(ev *InstallEvidence) {
			if ev.Target == "linux/amd64" {
				ev.SourceRevision = "other"
			}
		})
		if err := installAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another revision was accepted")
		}
	})
	t.Run("a failed record", func(t *testing.T) {
		files := gateRecords(t, t.TempDir(), func(ev *InstallEvidence) {
			if ev.Target == "windows/amd64" {
				ev.Result = "fail"
			}
		})
		if err := installAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a failed record was accepted")
		}
	})
	t.Run("macOS in the refusal mode", func(t *testing.T) {
		files := gateRecords(t, t.TempDir(), func(ev *InstallEvidence) {
			if ev.Target == "darwin/arm64" {
				ev.Mode = modeRefusal
			}
		})
		if err := installAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("macOS in the refusal mode was accepted")
		}
	})
	t.Run("another record kind", func(t *testing.T) {
		files := gateRecords(t, t.TempDir(), func(ev *InstallEvidence) {
			if ev.Target == "linux/arm64" {
				ev.Kind = "f08-walkthrough"
			}
		})
		if err := installAggregateCmd(append([]string{"-source-revision", "rev"}, files...)); err == nil {
			t.Fatal("a record of another kind was accepted")
		}
	})
}

// TestRefusalModeEndToEnd runs the gate against the core built from this
// tree with a prefix that does not exist: the install path refuses, the
// https source is refreshed, no plan is written and nothing on the machine
// changed.
func TestRefusalModeEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core and runs the gate")
	}
	prefix := filepath.Join(t.TempDir(), "no-such-prefix")
	restore := brewPrefixOverride
	brewPrefixOverride = prefix
	defer func() { brewPrefixOverride = restore }()
	bin := buildCore(t, "-ldflags", "-X main.prefixOverride="+prefix)

	h, err := runInstallGate(bin, host(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(h.root)
	ev := h.ev
	if ev.Mode != modeRefusal {
		t.Fatalf("mode = %q, want %q", ev.Mode, modeRefusal)
	}
	want := "MM_SOFTWARE_PROVIDER_MISSING"
	if runtime.GOOS == "windows" {
		want = "MM_SOFTWARE_INSTALL_UNAVAILABLE"
	}
	refusal, ok := stepByName(ev, "refuse the install")
	if !ok {
		t.Fatalf("the gate did not refuse the install: %+v", ev.Steps)
	}
	if !refusal.OK || !strings.Contains(refusal.Detail, want) {
		t.Fatalf("the refusal step = %+v, want %s", refusal, want)
	}
	refresh, ok := stepByName(ev, "https source")
	if !ok {
		t.Fatalf("the gate did not refresh the https source: %+v", ev.Steps)
	}
	if !refresh.OK {
		t.Fatalf("the refresh step failed: %+v", refresh)
	}
	plans, ok := stepByName(ev, "no plan written")
	if !ok {
		t.Fatalf("the gate did not check the plans: %+v", ev.Steps)
	}
	if !plans.OK {
		t.Fatalf("the refusal wrote a plan: %+v", plans)
	}
	if entries, err := os.ReadDir(filepath.Join(h.state, "plans")); err == nil && len(entries) > 0 {
		t.Fatalf("the refusal left %d plans behind", len(entries))
	}
	if len(ev.Guards.SentinelInvocations) != 0 {
		t.Fatalf("a guarded program was started: %v", ev.Guards.SentinelInvocations)
	}
	if len(ev.Guards.ProfileChanges) != 0 {
		t.Fatalf("the profiles changed: %v", ev.Guards.ProfileChanges)
	}
	// The evidence carries no path of the runner.
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = h.paths.scrub(b)
	for _, p := range []string{h.root, h.realHome} {
		if strings.Contains(string(b), p) {
			t.Fatalf("the evidence carries the path %s", p)
		}
	}
	if runtime.GOOS == "darwin" {
		// macOS must run real installs: a refusal there is a failure of the
		// gate, not of the refusal itself.
		if ev.Result != "fail" {
			t.Fatalf("result = %q, want fail on macOS", ev.Result)
		}
		if !strings.Contains(strings.Join(ev.Failures, "\n"), macOSRefusalMessage) {
			t.Fatalf("the failures do not name macOS: %v", ev.Failures)
		}
	}
}
