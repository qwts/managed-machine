package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"debug/buildinfo"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/provider/brew"
)

// The S2 install gate (issue #273). It drives a built managed-machine binary
// through real installs on a hosted runner, proves that a failed, a retried
// and an interrupted install each end in their own plan, proves that an
// https source is refreshed explicitly before it is used, and proves that no
// guarded program was started and no profile changed. On every other target
// the same gate shows that the install path refuses without touching the
// machine.
//
// The gate changes a machine, so it runs in the brew mode only on a
// GitHub-hosted runner. Everywhere else it refuses before it starts a
// process.

// The modes of the gate.
const (
	// modeBrew runs real installs through the Homebrew prefix of the target.
	modeBrew = "brew"
	// modeRefusal proves that the install path refuses without changing the
	// machine.
	modeRefusal = "refusal"
)

// hostedRunnerMessage is the refusal a gate gives when it would change a
// machine outside a GitHub-hosted runner.
const hostedRunnerMessage = "the install gate changes the machine; it runs only on GitHub-hosted runners"

// macOSRefusalMessage is the failure macOS records when it did not run real
// installs.
const macOSRefusalMessage = "macOS must run real installs"

// brewDefaultPrefix is the canonical Homebrew prefix of a target.
func brewDefaultPrefix(target string) string { return brew.DefaultPrefix(target) }

// installCmd runs the S2 install gate for one target and writes its
// evidence record.
func installCmd(args []string) error {
	fl := flag.NewFlagSet("install", flag.ContinueOnError)
	bin := fl.String("binary", "", "managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *bin == "" || *tgt == "" || *out == "" {
		return errors.New("install needs -binary, -target and -evidence")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	h, err := runInstallGate(absBin, *tgt, *rev)
	if err != nil {
		return err
	}
	defer os.RemoveAll(h.root)
	b, err := json.MarshalIndent(h.ev, "", "  ")
	if err != nil {
		return err
	}
	b = append(h.paths.scrub(b), '\n')
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d steps, mode %s, result %s\n", *tgt, len(h.ev.Steps), h.ev.Mode, h.ev.Result)
	if h.ev.Result != "pass" {
		for _, f := range h.ev.Failures {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		return errors.New("the install gate failed")
	}
	return nil
}

// runInstallGate builds the harness for one target and runs the gate in it.
// The caller owns the harness root and removes it.
func runInstallGate(bin, target, revision string) (*installHarness, error) {
	raw, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	h, err := newInstallHarness(bin, target, revision, raw)
	if err != nil {
		return nil, err
	}
	h.run()
	h.cleanup()
	return h, nil
}

// cleanup removes what the gate created on the machine: the tap with the
// fixture formulae. It runs whatever the gate did, and it is recorded like
// every other Homebrew command of the harness.
func (h *installHarness) cleanup() {
	if !h.tapped {
		return
	}
	h.brew("untap", "mmgate/gate")
	h.tapped = false
}

// run is the gate: the probe, the mode, the steps of the mode and the
// guards.
func (h *installHarness) run() {
	if !h.probe() {
		h.finish()
		return
	}
	h.ev.Mode = h.mode()
	switch h.ev.Mode {
	case modeBrew:
		if !hostedRunner() {
			h.step("brew mode", hostedRunnerMessage, false)
			h.finish()
			return
		}
		h.brewMode()
	default:
		h.refusalMode()
	}
	if h.target == "darwin/arm64" && h.ev.Mode != modeBrew {
		h.fail(macOSRefusalMessage)
	}
	h.guards()
	h.finish()
}

// hostedRunner reports the environment that authorizes a machine change.
func hostedRunner() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" && os.Getenv("RUNNER_ENVIRONMENT") == "github-hosted"
}

// mode is the mode of the gate for this target: the brew mode when the
// target has a usable Homebrew prefix, the refusal mode everywhere else.
func (h *installHarness) mode() string {
	if strings.HasPrefix(h.target, "windows/") || h.prefix == "" {
		return modeRefusal
	}
	if _, err := os.Stat(filepath.Join(h.prefix, "bin", "brew")); err != nil {
		return modeRefusal
	}
	if !accessWritable(h.prefix) {
		return modeRefusal
	}
	return modeBrew
}

// probe checks that the walkthrough and the binary under test both run
// natively as the target.
func (h *installHarness) probe() bool {
	problem := ""
	switch {
	case runtime.GOOS+"/"+runtime.GOARCH != h.target:
		problem = "the gate itself runs as " + runtime.GOOS + "/" + runtime.GOARCH + ", not " + h.target
	}
	buildOS, buildArch, err := binaryBuildTarget(h.bin)
	switch {
	case err != nil:
		if problem == "" {
			problem = "the binary under test carries no build target: " + err.Error()
		}
	case buildOS != h.ev.OS || buildArch != h.ev.Arch:
		if problem == "" {
			problem = "the binary under test is built for " + buildOS + "/" + buildArch + ", not " + h.target
		}
	}
	code, out, errOut := h.core("platform")
	recs, perr := parseJSONL(out)
	switch {
	case code != 0:
		if problem == "" {
			problem = "the platform probe exited " + fmt.Sprint(code) + ": " + firstLineOf(errOut)
		}
	case perr != nil:
		if problem == "" {
			problem = perr.Error()
		}
	case len(recs) != 1:
		if problem == "" {
			problem = "the platform probe returned no record"
		}
	case str(recs[0], "platform") != h.target:
		if problem == "" {
			problem = "the platform probe reports " + str(recs[0], "platform") + ", not " + h.target
		}
	case recs[0]["emulated"] == true:
		if problem == "" {
			problem = "the binary under test runs emulated"
		}
	}
	detail := "the binary under test is a native " + h.target + " binary and reports " + h.target
	if problem != "" {
		detail = problem
	}
	h.step("probe", detail, problem == "")
	return problem == ""
}

// binaryBuildTarget reads the target a Go binary was built for.
func binaryBuildTarget(path string) (string, string, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	os_, arch := "", ""
	for _, s := range bi.Settings {
		switch s.Key {
		case "GOOS":
			os_ = s.Value
		case "GOARCH":
			arch = s.Value
		}
	}
	if os_ == "" || arch == "" {
		return "", "", errors.New("the binary carries no GOOS or GOARCH build setting")
	}
	return os_, arch, nil
}

// gateDefinition is one brew fixture definition. The formula is the id
// itself, so a fixture never names a real formula.
func gateDefinition(id, formula string, platforms []string) string {
	return "schema_version: 1\nid: " + id + "\ndescription: Install gate fixture.\ncategories: [walkthrough]\n" +
		"recipes:\n  - id: homebrew\n    provider: brew\n    package: " + formula + "\n" +
		"    platforms: [" + strings.Join(platforms, ", ") + "]\n" +
		"    targets: [global, user]\n    requires: []\n    options:\n      formula: " + formula + "\n"
}

// downloadDefinition is a Windows fixture whose provider, download, has no
// executor, so its install is refused before anything runs or is written.
func downloadDefinition() string {
	return "schema_version: 1\nid: mm-gate-download\ndescription: Install gate fixture.\ncategories: [walkthrough]\n" +
		"recipes:\n  - id: download\n    provider: download\n    package: mm-gate-download\n" +
		"    platforms: [windows/arm64, windows/amd64]\n    targets: [user]\n    requires: []\n" +
		"    options:\n      url: https://example.invalid/mm-gate-download.zip\n      url_hosts: [example.invalid]\n"
}

// writeGateDefinitions writes the local fixture definitions: one whose
// download never succeeds, one whose install never finishes and one whose
// provider has no executor.
func writeGateDefinitions(dir string) error {
	platforms := []string{"darwin/arm64", "linux/arm64", "linux/amd64"}
	for _, id := range []string{"mm-gate-fail", "mm-gate-slow"} {
		if err := write(filepath.Join(dir, id+".yaml"), gateDefinition(id, id, platforms)); err != nil {
			return err
		}
	}
	return write(filepath.Join(dir, "mm-gate-download.yaml"), downloadDefinition())
}

// helloDefinition is the definition the https fixture server serves.
func helloDefinition() string {
	return gateDefinition("mm-gate-hello", "hello", []string{
		"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64",
	})
}

// brewMode runs the real installs.
func (h *installHarness) brewMode() {
	steps := []struct {
		name string
		run  func() (string, bool)
	}{
		{"brew setup", h.brewSetup},
		{"install ripgrep", h.installRipgrep},
		{"install ripgrep again", h.installRipgrepAgain},
		{"install mm-gate-fail", h.installFailing},
		{"interrupt mm-gate-slow", h.interruptSlow},
		{"https source", h.httpsSource},
	}
	for _, s := range steps {
		detail, ok := s.run()
		h.step(s.name, detail, ok)
		if !ok {
			return
		}
	}
}

// brewSetup prepares the tap the gate installs from: the fixture formulae
// and a real formula that the gate installs, uninstalls and installs again.
func (h *installHarness) brewSetup() (string, bool) {
	if code, _, _ := h.brew("list", "--versions", "--formula", "ripgrep"); code == 0 {
		if code, out, errOut := h.brew("uninstall", "--formula", "--ignore-dependencies", "ripgrep"); code != 0 {
			return "ripgrep could not be uninstalled: " + firstLineOf(out, errOut), false
		}
	}
	// The tap that carries the fixture formulae. When `brew tap-new` fails
	// the tap is created by hand: a tap is a directory with a Formula
	// directory in it.
	tapped, _, _ := h.brew("tap-new", "--no-git", "mmgate/gate")
	code, out, errOut := h.brew("--repository", "mmgate/gate")
	if code != 0 {
		return "the mmgate/gate tap has no repository: " + firstLineOf(out, errOut), false
	}
	tap := strings.TrimSpace(string(out))
	if tap == "" {
		return "the mmgate/gate tap has no repository path", false
	}
	if tapped != 0 {
		dir := filepath.Join(tap, "Formula")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "the mmgate/gate tap could not be created: " + err.Error(), false
		}
		h.ev.Setup = append(h.ev.Setup, "mkdir -p "+dir)
	}
	h.tapped = true
	if err := h.writeFormulas(tap); err != nil {
		return "the fixture formulae could not be written: " + err.Error(), false
	}
	// Homebrew refuses to load a formula from a tap nobody trusted.
	for _, f := range []string{"mmgate/gate/mm-gate-fail", "mmgate/gate/mm-gate-slow"} {
		if code, out, errOut := h.brew("trust", "--formula", f); code != 0 {
			return "brew trust " + f + " exited " + fmt.Sprint(code) + ": " + firstLineOf(errOut, out), false
		}
	}
	return "the mmgate/gate tap carries the mm-gate-fail and mm-gate-slow formulae", true
}

// writeFormulas writes the two fixture formulae into the tap: one whose
// download never succeeds and one whose install never finishes.
func (h *installHarness) writeFormulas(tap string) error {
	tarball := filepath.Join(h.root, "mm-gate-slow-1.0.tar.gz")
	if err := writeTarball(tarball, "mm-gate-slow-1.0/README", "install gate fixture\n"); err != nil {
		return err
	}
	digest, err := fileDigest(tarball)
	if err != nil {
		return err
	}
	dir := filepath.Join(tap, "Formula")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	missing := filepath.ToSlash(filepath.Join(h.root, "missing", "mm-gate-fail-1.0.tar.gz"))
	fail := "class MmGateFail < Formula\n" +
		"  desc \"Install gate fixture whose download never succeeds.\"\n" +
		"  homepage \"https://example.invalid\"\n" +
		"  url \"file://" + missing + "\"\n" +
		"  sha256 \"" + strings.Repeat("0", 64) + "\"\n" +
		"  version \"1.0\"\n" +
		"\n  def install\n    bin.mkpath\n  end\nend\n"
	slow := "class MmGateSlow < Formula\n" +
		"  desc \"Install gate fixture whose install never finishes.\"\n" +
		"  homepage \"https://example.invalid\"\n" +
		"  url \"file://" + filepath.ToSlash(tarball) + "\"\n" +
		"  sha256 \"" + digest + "\"\n" +
		"  version \"1.0\"\n" +
		"\n  def install\n    sleep 900\n    bin.mkpath\n  end\nend\n"
	if err := os.WriteFile(filepath.Join(dir, "mm-gate-fail.rb"), []byte(fail), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "mm-gate-slow.rb"), []byte(slow), 0o644)
}

// writeTarball writes a gzip-compressed tar archive with one file in one
// directory, the shape Homebrew unpacks.
func writeTarball(path, name, content string) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	dir := filepath.Dir(name)
	if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		return err
	}
	body := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(body); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

// fileDigest returns the hex SHA-256 digest of a file.
func fileDigest(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return sum(b), nil
}

// installRipgrep installs a real formula and checks the plan, the journal
// and the result record.
func (h *installHarness) installRipgrep() (string, bool) {
	code, out, errOut := h.core("software", "ripgrep", "--install", "--global")
	rec, problem := h.installRecord(out, errOut, code, 0, "ripgrep", "global", "ok")
	if problem != "" {
		return problem, false
	}
	planID := str(rec, "plan")
	if !planIDPattern.MatchString(planID) {
		return "the record names no plan: " + planID, false
	}
	_, listed, _ := h.brew("list", "--versions", "--formula", "ripgrep")
	fields := strings.Fields(string(listed))
	if len(fields) < 2 {
		return "brew lists no version for ripgrep", false
	}
	if got := str(rec, "resolved_version"); got != fields[1] {
		return "the resolved version " + got + " is not the installed " + fields[1], false
	}
	doc, err := h.planDoc(planID)
	if err != nil {
		return err.Error(), false
	}
	switch {
	case doc.State != "applied":
		return "the plan is " + doc.State + ", not applied", false
	case doc.Command != "software --install":
		return "the plan runs " + doc.Command, false
	case len(doc.Operations) != 1:
		return fmt.Sprintf("the plan has %d operations, want one", len(doc.Operations)), false
	}
	recs, err := h.journal(planID)
	if err != nil {
		return err.Error(), false
	}
	if problem := h.journalApplied(recs, doc.Operations[0].ID); problem != "" {
		return problem, false
	}
	h.recordPlan(planID)
	h.ripgrepPlan = planID
	return "ripgrep " + fields[1] + " installed; plan " + planID + " applied with one verified operation", true
}

// installRipgrepAgain installs the same formula a second time: the plan
// has no operation and the outcome is unchanged.
func (h *installHarness) installRipgrepAgain() (string, bool) {
	code, out, errOut := h.core("software", "ripgrep", "--install", "--global")
	rec, problem := h.installRecord(out, errOut, code, 0, "ripgrep", "global", "unchanged")
	if problem != "" {
		return problem, false
	}
	planID := str(rec, "plan")
	if planID == h.ripgrepPlan {
		return "the second install reused the plan " + planID, false
	}
	doc, err := h.planDoc(planID)
	if err != nil {
		return err.Error(), false
	}
	switch {
	case doc.State != "applied":
		return "the plan is " + doc.State + ", not applied", false
	case len(doc.Operations) != 0:
		return fmt.Sprintf("the plan has %d operations, want none", len(doc.Operations)), false
	}
	recs, err := h.journal(planID)
	if err != nil {
		return err.Error(), false
	}
	if problem := h.journalApplied(recs, ""); problem != "" {
		return problem, false
	}
	h.recordPlan(planID)
	return "the second install planned no operation; plan " + planID + " applied unchanged", true
}

// installFailing installs a formula whose download never succeeds, twice:
// the plan fails and the retry uses a new plan.
func (h *installHarness) installFailing() (string, bool) {
	code, _, errOut := h.core("software", "mm-gate-fail", "--install", "--global")
	planID := planInText(string(errOut))
	if code != 1 {
		return "the install exited " + fmt.Sprint(code) + ", want 1: " + firstLineOf(errOut), false
	}
	if !strings.Contains(string(errOut), "MM_SOFTWARE_INSTALL_FAILED") {
		return "the diagnostic is not MM_SOFTWARE_INSTALL_FAILED: " + firstLineOf(errOut), false
	}
	if planID == "" {
		return "the diagnostic names no plan: " + firstLineOf(errOut), false
	}
	doc, err := h.planDoc(planID)
	if err != nil {
		return err.Error(), false
	}
	if doc.State != "failed" {
		return "the plan is " + doc.State + ", not failed", false
	}
	recs, err := h.journal(planID)
	if err != nil {
		return err.Error(), false
	}
	receipt := journalReceipt(recs)
	if receipt != "failed" {
		return "the receipt is " + receipt + ", not failed", false
	}
	h.recordPlan(planID)

	// The retry is a new plan: a failed plan never runs again.
	code, _, errOut = h.core("software", "mm-gate-fail", "--install", "--global")
	retry := planInText(string(errOut))
	if code != 1 {
		return "the retry exited " + fmt.Sprint(code) + ", want 1", false
	}
	if retry == "" || retry == planID {
		return "the retry reused the plan " + planID, false
	}
	doc, err = h.planDoc(retry)
	if err != nil {
		return err.Error(), false
	}
	if doc.State != "failed" {
		return "the retry plan is " + doc.State + ", not failed", false
	}
	h.recordPlan(retry)
	return "the install failed with MM_SOFTWARE_INSTALL_FAILED; plan " + planID + " failed and the retry used the new plan " + retry, true
}

// interruptSlow kills an install while it runs and checks that the next
// install recovers the killed plan as interrupted.
func (h *installHarness) interruptSlow() (string, bool) {
	before := h.planIDs()
	cmd := exec.Command(h.bin, "software", "mm-gate-slow", "--install", "--global")
	cmd.Env, cmd.Dir = h.env, h.home
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	isolateProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return "the install could not be started: " + err.Error(), false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	slow := ""
	exited := false
	deadline := time.Now().Add(180 * time.Second)
	for time.Now().Before(deadline) && slow == "" {
		select {
		case <-done:
			exited = true
		default:
		}
		if exited {
			break
		}
		for id := range h.planIDs() {
			if before[id] {
				continue
			}
			recs, err := h.journal(id)
			if err != nil {
				continue
			}
			if journalHas(recs, "started") {
				slow = id
				break
			}
		}
		if slow != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if slow == "" {
		killProcessGroup(cmd)
		<-done
		return "the interrupted install never reached its first operation", false
	}
	// The operation is running: the whole group is killed, not the core
	// alone, so no child of the install survives it.
	time.Sleep(3 * time.Second)
	if err := killProcessGroup(cmd); err != nil {
		<-done
		return "the install could not be killed: " + err.Error(), false
	}
	<-done

	// The next install opens the store, which recovers the killed plan.
	code, out, errOut := h.core("software", "ripgrep", "--install", "--global")
	rec, problem := h.installRecord(out, errOut, code, 0, "ripgrep", "global", "unchanged")
	if problem != "" {
		return "the install after the interruption failed: " + problem, false
	}
	next := str(rec, "plan")
	if next == slow {
		return "the install after the interruption reused the plan " + slow, false
	}
	doc, err := h.planDoc(slow)
	if err != nil {
		return err.Error(), false
	}
	if doc.State != "failed" {
		return "the interrupted plan is " + doc.State + ", not failed", false
	}
	recs, err := h.journal(slow)
	if err != nil {
		return err.Error(), false
	}
	if receipt := journalReceipt(recs); receipt != "interrupted" {
		return "the interrupted receipt is " + receipt + ", not interrupted", false
	}
	h.recordPlan(slow)
	h.recordPlan(next)
	return "the killed plan " + slow + " was recovered as failed with an interrupted receipt; the next install used the plan " + next, true
}

// sourceFixture serves one source over https from a local fixture server and
// refreshes it into the snapshot cache. It returns the source URL. The
// server is closed again: an install reads the cache and never fetches.
func (h *installHarness) sourceFixture() (string, string, string) {
	srv, ca, err := h.startSourceServer()
	if err != nil {
		return "", "", "the fixture server could not be started: " + err.Error()
	}
	defer srv.Close()
	sourceURL := srv.URL + "/index.json"
	// The fixture certificate has to be trusted by the platform for the
	// refresh: Go reads SSL_CERT_FILE on Unix, and the Windows store
	// everywhere else.
	release, err := trustFixtureCertificate(ca)
	if err != nil {
		return "", "", "the fixture certificate could not be trusted: " + err.Error()
	}
	defer release()
	code, out, errOut := h.coreEnv([]string{
		"MANAGED_MACHINE_SOURCE_URL=" + sourceURL,
		"SSL_CERT_FILE=" + ca,
	}, "software", "--refresh")
	recs, perr := parseJSONL(out)
	switch {
	case code != 0:
		return "", "", "the refresh exited " + fmt.Sprint(code) + ": " + firstLineOf(errOut)
	case perr != nil:
		return "", "", perr.Error()
	case len(recs) != 1:
		return "", "", fmt.Sprintf("the refresh returned %d records, want one", len(recs))
	case str(recs[0], "outcome") != "refreshed":
		return "", "", "the refresh outcome is " + str(recs[0], "outcome") + ", not refreshed"
	}
	return sourceURL, ca, ""
}

// httpsSource refreshes a source served by a local https fixture server and
// installs from it.
func (h *installHarness) httpsSource() (string, bool) {
	sourceURL, _, problem := h.sourceFixture()
	if problem != "" {
		return problem, false
	}
	// The install reads the cached snapshot: it never fetches, so it needs
	// no trust in the fixture certificate.
	code, out, errOut := h.coreEnv([]string{"MANAGED_MACHINE_SOURCE_URL=" + sourceURL},
		"software", "mm-gate-hello", "--install", "--global")
	rec, problem := h.installRecord(out, errOut, code, 0, "mm-gate-hello", "global", "ok")
	if problem != "" {
		return problem, false
	}
	planID := str(rec, "plan")
	doc, err := h.planDoc(planID)
	if err != nil {
		return err.Error(), false
	}
	if !strings.HasPrefix(doc.SourceRevision, "https://127.0.0.1") {
		return "the plan names the source " + doc.SourceRevision, false
	}
	if !strings.Contains(doc.SourceRevision, "@sha256:") {
		return "the plan names no snapshot revision: " + doc.SourceRevision, false
	}
	h.recordPlan(planID)
	// The fixture formula is removed again: the gate leaves no formula of
	// its own behind.
	if code, out, errOut := h.brew("uninstall", "--formula", "hello"); code != 0 {
		return "hello could not be uninstalled: " + firstLineOf(out, errOut), false
	}
	return "the https source refreshed into the snapshot cache and the install planned from it; plan " + planID + " names " + doc.SourceRevision, true
}

// startSourceServer serves the index document and the definition of one
// https source and writes the server's certificate to the harness.
func (h *installHarness) startSourceServer() (*httptest.Server, string, error) {
	definition := helloDefinition()
	index := `{"schema_version":1,"files":[{"name":"mm-gate-hello.yaml","sha256":"` + sum([]byte(definition)) + `"}]}`
	mux := http.NewServeMux()
	mux.HandleFunc("/index.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(index))
	})
	mux.HandleFunc("/mm-gate-hello.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write([]byte(definition))
	})
	srv := httptest.NewTLSServer(mux)
	ca := filepath.Join(h.root, "ca.pem")
	if err := os.WriteFile(ca, pemCertificate(srv.Certificate().Raw), 0o644); err != nil {
		srv.Close()
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(h.root, "ca.cer"), srv.Certificate().Raw, 0o644); err != nil {
		srv.Close()
		return nil, "", err
	}
	return srv, ca, nil
}

// refusalMode proves that the install path refuses without changing the
// machine.
func (h *installHarness) refusalMode() {
	want := "MM_SOFTWARE_PROVIDER_MISSING"
	args := []string{"software", "ripgrep", "--install", "--global"}
	wantCode := 1
	if strings.HasPrefix(h.target, "windows/") {
		// Windows has no Homebrew prefix: the fixture's provider, download,
		// has no executor in this release.
		want = "MM_SOFTWARE_INSTALL_UNAVAILABLE"
		args = []string{"software", "mm-gate-download", "--install", "--user"}
		wantCode = 2
	} else if h.prefix != "" {
		if _, err := os.Stat(filepath.Join(h.prefix, "bin", "brew")); err == nil {
			// Homebrew is there but its prefix is not writable: the plan
			// fails its privilege precondition.
			want = "MM_SOFTWARE_PRIVILEGE_REQUIRED"
		}
	}
	code, _, errOut := h.core(args...)
	switch {
	case code != wantCode:
		h.step("refuse the install", "the install exited "+fmt.Sprint(code)+", want "+fmt.Sprint(wantCode)+": "+firstLineOf(errOut), false)
	case !strings.Contains(string(errOut), want):
		h.step("refuse the install", "the diagnostic is not "+want+": "+firstLineOf(errOut), false)
	default:
		h.step("refuse the install", "the install was refused with "+want+" before anything was written", true)
	}
	sourceURL, _, problem := h.sourceFixture()
	if problem != "" {
		h.step("https source", problem, false)
		return
	}
	h.step("https source", "the https source refreshed into the snapshot cache", true)
	// The cached source's recipe is a brew recipe, so even where the
	// selected provider was unavailable it is refused as missing brew.
	if want == "MM_SOFTWARE_INSTALL_UNAVAILABLE" {
		want = "MM_SOFTWARE_PROVIDER_MISSING"
	}
	code, _, errOut = h.coreEnv([]string{"MANAGED_MACHINE_SOURCE_URL=" + sourceURL},
		"software", "mm-gate-hello", "--install", "--global")
	switch {
	case code != 1:
		h.step("refuse the install from the https source", "the install exited "+fmt.Sprint(code)+", want 1: "+firstLineOf(errOut), false)
	case !strings.Contains(string(errOut), want):
		h.step("refuse the install from the https source", "the diagnostic is not "+want+": "+firstLineOf(errOut), false)
	default:
		h.step("refuse the install from the https source", "the install from the cached https source was refused with "+want, true)
	}
	// The refusal never writes a plan.
	if entries, err := os.ReadDir(filepath.Join(h.state, "plans")); err == nil && len(entries) > 0 {
		h.step("no plan written", fmt.Sprintf("%d plans were written", len(entries)), false)
		return
	}
	h.step("no plan written", "the refusal wrote no plan", true)
}

// installRecord checks the result record of one install.
func (h *installHarness) installRecord(out, errOut []byte, code, wantCode int, software, target, outcome string) (record, string) {
	if code != wantCode {
		return nil, "exit " + fmt.Sprint(code) + ", want " + fmt.Sprint(wantCode) + ": " + firstLineOf(errOut)
	}
	recs, err := parseJSONL(out)
	if err != nil {
		return nil, err.Error()
	}
	if len(recs) != 1 {
		return nil, fmt.Sprintf("%d records, want one", len(recs))
	}
	keys, err := objectKeys(firstLineBytes(out))
	if err != nil {
		return nil, err.Error()
	}
	want := []string{"software", "recipe", "provider", "target", "resolved_version", "outcome", "plan"}
	if !sameStrings(keys, want) {
		return nil, "the record keys are " + strings.Join(keys, ",") + ", want " + strings.Join(want, ",")
	}
	r := recs[0]
	switch {
	case str(r, "software") != software:
		return nil, "the record names " + str(r, "software") + ", not " + software
	case str(r, "target") != target:
		return nil, "the record names the target " + str(r, "target") + ", not " + target
	case str(r, "outcome") != outcome:
		return nil, "the outcome is " + str(r, "outcome") + ", not " + outcome
	}
	return r, ""
}

// sameStrings reports whether two lists hold the same strings in the same
// order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// firstLineBytes returns the first line of a stream.
func firstLineBytes(b []byte) []byte {
	line := strings.SplitN(strings.TrimRight(string(b), "\n"), "\n", 2)[0]
	return []byte(line)
}

// planDoc is the part of a saved plan the gate reads.
type planDoc struct {
	Plan           string `json:"plan"`
	Command        string `json:"command"`
	Software       string `json:"software"`
	State          string `json:"state"`
	SourceRevision string `json:"source_revision"`
	Operations     []struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
	} `json:"operations"`
}

// planDoc reads one saved plan.
func (h *installHarness) planDoc(id string) (planDoc, error) {
	var d planDoc
	b, err := os.ReadFile(filepath.Join(h.state, "plans", id+".json"))
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	return d, nil
}

// journalRecord is one results-journal line.
type journalRecord struct {
	Plan      string `json:"plan"`
	Operation string `json:"operation"`
	Action    string `json:"action"`
	Outcome   string `json:"outcome"`
	State     string `json:"state"`
}

// journal reads the journal of one plan.
func (h *installHarness) journal(id string) ([]journalRecord, error) {
	b, err := os.ReadFile(filepath.Join(h.state, "journal", id+".jsonl"))
	if err != nil {
		return nil, err
	}
	var out []journalRecord
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r journalRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// journalApplied checks the journal of an applied plan: the operation was
// started and finished, and the receipt says applied.
func (h *installHarness) journalApplied(recs []journalRecord, operation string) string {
	if len(recs) == 0 {
		return "the journal is empty"
	}
	last := recs[len(recs)-1]
	if last.Action != "receipt" || last.Outcome != "applied" || last.State != "applied" {
		return "the receipt is " + last.Outcome + ", not applied"
	}
	if operation == "" {
		if len(recs) != 1 {
			return fmt.Sprintf("the journal has %d records, want one receipt", len(recs))
		}
		return ""
	}
	if len(recs) != 3 {
		return fmt.Sprintf("the journal has %d records, want three", len(recs))
	}
	if recs[0].Outcome != "started" || recs[0].Operation != operation {
		return "the operation was not started"
	}
	if recs[1].Outcome != "ok" || recs[1].Operation != operation {
		return "the operation was not verified"
	}
	return ""
}

// journalHas reports whether the journal holds a record with the outcome.
func journalHas(recs []journalRecord, outcome string) bool {
	for _, r := range recs {
		if r.Outcome == outcome {
			return true
		}
	}
	return false
}

// journalReceipt returns the outcome of the plan's receipt.
func journalReceipt(recs []journalRecord) string {
	if len(recs) == 0 {
		return ""
	}
	last := recs[len(recs)-1]
	if last.Action != "receipt" {
		return ""
	}
	return last.Outcome
}

// planIDs returns the ids of every plan in the state directory.
func (h *installHarness) planIDs() map[string]bool {
	out := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(h.state, "plans"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if name := e.Name(); strings.HasPrefix(name, "plan-") && strings.HasSuffix(name, ".json") {
			out[strings.TrimSuffix(name, ".json")] = true
		}
	}
	return out
}

// lastPlan returns the id of the newest plan of a software package.
func (h *installHarness) lastPlan(software string) string {
	ids := []string{}
	for id := range h.planIDs() {
		d, err := h.planDoc(id)
		if err != nil {
			continue
		}
		if d.Software == software {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

// recordPlan adds one observed plan to the evidence.
func (h *installHarness) recordPlan(id string) {
	doc, err := h.planDoc(id)
	if err != nil {
		return
	}
	receipt := ""
	if recs, err := h.journal(id); err == nil {
		receipt = journalReceipt(recs)
	}
	h.ev.Plans = append(h.ev.Plans, PlanRecord{Plan: id, State: doc.State, Receipt: receipt})
}

// planIDPattern is the shape of a plan id.
var planIDPattern = regexp.MustCompile(`^plan-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}$`)

// planInText returns the plan id a diagnostic names.
var planIDInText = regexp.MustCompile(`plan-[0-9]{8}T[0-9]{6}Z-[0-9a-f]{4}`)

func planInText(s string) string {
	if m := planIDInText.FindString(s); m != "" {
		return m
	}
	return ""
}
