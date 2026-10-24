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

// The test binary doubles as the sentinels and as deliberately broken cores,
// chosen by the name it is started under, because the harness passes the
// child no inherited environment.
func TestMain(m *testing.M) {
	sentinelMode()
	if len(os.Args) > 1 && os.Args[1] == "_interrupt-child" {
		if err := interruptChildCmd(os.Args[2:]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	switch strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") {
	case "fake-core-mutates":
		os.WriteFile(filepath.Join(os.Getenv("HOME"), "installed.txt"), []byte("x"), 0o644)
		os.Exit(0)
	case "fake-core-spawns":
		exec.Command("brew", "install", "ruff").Run()
		os.Exit(0)
	case "fake-core-installs":
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeCore(t *testing.T, name string) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), exe(name))
	if err := os.WriteFile(p, b, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func host() string { return runtime.GOOS + "/" + runtime.GOARCH }

func walkthrough(t *testing.T, bin string) Evidence {
	t.Helper()
	out := filepath.Join(t.TempDir(), "evidence.json")
	err := runCmd([]string{"-binary", bin, "-target", host(), "-evidence", out, "-source-revision", "test"})
	b, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatalf("no evidence written (run error %v)", err)
	}
	var ev Evidence
	if jerr := json.Unmarshal(b, &ev); jerr != nil {
		t.Fatal(jerr)
	}
	if (err == nil) != (ev.Result == "pass") {
		t.Fatalf("exit status %v disagrees with result %s", err, ev.Result)
	}
	return ev
}

func failureMentions(ev Evidence, s string) bool {
	for _, f := range ev.Failures {
		if strings.Contains(f, s) {
			return true
		}
	}
	return false
}

func TestMutationGuardCatchesAWrite(t *testing.T) {
	ev := walkthrough(t, fakeCore(t, "fake-core-mutates"))
	if ev.Result != "fail" || len(ev.Guards.FilesystemChanges) == 0 || !failureMentions(ev, "the harness changed") {
		t.Fatalf("a write to HOME was not caught: %+v", ev.Guards)
	}
}

func TestProcessGuardCatchesAProvider(t *testing.T) {
	ev := walkthrough(t, fakeCore(t, "fake-core-spawns"))
	if ev.Result != "fail" || len(ev.Guards.SentinelInvocations) == 0 || !strings.HasPrefix(ev.Guards.SentinelInvocations[0], "brew install ruff") {
		t.Fatalf("starting brew was not caught: %+v", ev.Guards)
	}
}

func TestSuccessfulInstallFails(t *testing.T) {
	ev := walkthrough(t, fakeCore(t, "fake-core-installs"))
	for _, s := range ev.Steps {
		if strings.HasPrefix(s.Name, "install is unavailable") && s.OK {
			t.Fatalf("an install exiting 0 passed: %+v", s)
		}
	}
	if !failureMentions(ev, "install is unavailable (user) (default): exit 0, want 2") {
		t.Fatalf("failures: %v", ev.Failures)
	}
}

// TestRealCorePasses runs the walkthrough against the core built from this
// tree, the same gate the hosted jobs run.
func TestRealCorePasses(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the core")
	}
	bin := filepath.Join(t.TempDir(), exe("managed-machine"))
	build := exec.Command("go", "build", "-o", bin, "../managed-machine")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	ev := walkthrough(t, bin)
	// The F01 harness output is supplied by the hosted macOS job; here only
	// its absence may fail the record.
	for _, f := range ev.Failures {
		if f != "F01 compatibility: missing" {
			t.Fatalf("walkthrough failed: %v", ev.Failures)
		}
	}
	if ev.Guards.HarnessEntries == 0 || !ev.Guards.CustomRootAbsent || ev.Guards.ExtensionExecuted {
		t.Fatalf("guards: %+v", ev.Guards)
	}
}

func TestAggregateRejectsMismatchedRevision(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i, tg := range []string{"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64"} {
		rev := "abc"
		if i == 2 {
			rev = "other"
		}
		p := filepath.Join(dir, strings.ReplaceAll(tg, "/", "-")+".json")
		body := `{"schema_version":1,"story":"F08","source_revision":"` + rev + `","target":"` + tg + `","probe":{"platform":"` + tg + `","emulated":false},"walkthrough_runtime":"` + tg + `","result":"pass","steps":[{"name":"s","mode":"default","args":[],"portable":true,"stdout_sha256":"x"}]}`
		os.WriteFile(p, []byte(body), 0o644)
		files = append(files, p)
	}
	if err := aggregateCmd(append([]string{"-source-revision", "abc"}, files...)); err == nil {
		t.Fatal("a record from another revision was aggregated")
	}
	if err := aggregateCmd(append([]string{"-source-revision", "abc"}, files[:4]...)); err == nil {
		t.Fatal("four records were accepted as five targets")
	}
}
