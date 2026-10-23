package main

// The S3 provider gate (issue #285). It drives a built managed-machine
// binary through one real install per provider on a hosted runner: every
// provider's small fixture package is installed for the user target and
// into a custom root, the plan, the journal, the receipt and the installed
// binary are read, a second install has to report the software unchanged,
// the unsupported combinations are refused without a plan, and an install
// into a custom root is proved to change nothing outside that root and
// the guarded state directory.
//
// The gate installs software, so it refuses to start outside a
// GitHub-hosted runner, exactly as the S2 install gate's brew mode does.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/cargo"
	"github.com/qwts/managed-machine/internal/provider/golang"
	"github.com/qwts/managed-machine/internal/provider/npm"
	"github.com/qwts/managed-machine/internal/provider/uv"
	"github.com/qwts/managed-machine/internal/provider/winget"
)

// providerEvidenceKind names the evidence record of the provider gate.
const providerEvidenceKind = "s3-provider-gate"

// providerMode is the mode the provider gate runs in. The gate has one
// mode only: it installs software or it refuses to start.
const providerMode = "providers"

// providerHostedRunnerMessage is the refusal the provider gate gives when
// it would install software outside a GitHub-hosted runner.
const providerHostedRunnerMessage = "the provider gate installs software; it runs only on GitHub-hosted runners"

// The outcomes a combination can be observed with. The expected outcome of
// an install is that the software is installed, that a second install
// reports it unchanged, or that the combination is refused.
const (
	observedInstalled = "installed"
	observedUnchanged = "unchanged"
	observedRefused   = "refused"
	observedFailed    = "failed"
)

// The installation target kinds the gate proves.
const (
	kindUser     = "user"
	kindCustom   = "custom"
	kindGlobal   = "global"
	kindRelative = "relative"
)

// firstReleaseTargets are the five targets of ADR-0014. Every gate needs
// one record per target.
var firstReleaseTargets = []string{"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64"}

// ProviderEvidence is the per-target record of the provider gate: the
// install-gate record with the combination matrix added. The field order
// is the serialized key order.
type ProviderEvidence struct {
	InstallEvidence
	Combinations []ProviderCombination `json:"combinations"`
}

// ProviderCombination is one proven combination of a provider, a package
// and an installation target.
type ProviderCombination struct {
	Provider   string `json:"provider"`
	Software   string `json:"software"`
	TargetKind string `json:"target_kind"`
	Expected   string `json:"expected"`
	Observed   string `json:"observed"`
	OK         bool   `json:"ok"`
	Detail     string `json:"detail"`
}

// providerFixture is one provider's gate fixture: the definition the gate
// writes, the package it installs and where the installed binary has to
// be. The binary paths are templates: {home} is the isolated home, {root}
// the custom root, {appdata} the redirected APPDATA and {localappdata} the
// runner's own LOCALAPPDATA on Windows, and {bin} the binary's own name.
type providerFixture struct {
	provider   string
	software   string
	packageID  string
	binary     string
	runArgs    []string
	platforms  []string
	targets    []string
	userPath   map[string]string
	customPath map[string]string
	options    map[string]string
}

// providerFixtures are the five fixtures of the gate, one per provider of
// the first release. Every fixture installs one small real package: a
// pure Python distribution with a console script, an npm package without
// dependencies, a Go module command, a Rust crate without dependencies
// and a WinGet package for the Windows targets.
//
// The WinGet provider installs into its own scopes, so its fixture
// declares the user target only; every other provider declares the user
// and the custom target.
var providerFixtures = []providerFixture{
	{
		provider: uv.Name, software: "mm-gate-uv", packageID: "cowsay", binary: "cowsay",
		runArgs:   []string{"--version"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		userPath: map[string]string{
			"unix": "{home}/.local/bin/cowsay", "windows": "{home}/.local/bin/cowsay.exe",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/cowsay", "windows": "{root}/bin/cowsay.exe",
		},
		options: map[string]string{"tool": "cowsay"},
	},
	{
		provider: npm.Name, software: "mm-gate-npm", packageID: "semver", binary: "semver",
		runArgs:   []string{"--help"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		userPath: map[string]string{
			"unix": "{home}/.local/bin/semver", "windows": "{appdata}/npm/semver.cmd",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/semver", "windows": "{root}/semver.cmd",
		},
		options: map[string]string{"package": "semver"},
	},
	{
		provider: cargo.Name, software: "mm-gate-cargo", packageID: cargoCrate, binary: cargoCrate,
		runArgs:   []string{"levenshtein", "kitten", "sitting"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		userPath: map[string]string{
			"unix": "{home}/.cargo/bin/" + cargoCrate, "windows": "{home}/.cargo/bin/" + cargoCrate + ".exe",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/" + cargoCrate, "windows": "{root}/bin/" + cargoCrate + ".exe",
		},
		options: map[string]string{"crate": cargoCrate},
	},
	{
		provider: golang.Name, software: "mm-gate-go", packageID: goPackage, binary: "stringer",
		runArgs:   []string{"-help"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		userPath: map[string]string{
			"unix": "{home}/go/bin/stringer", "windows": "{home}/go/bin/stringer.exe",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/stringer", "windows": "{root}/bin/stringer.exe",
		},
		options: map[string]string{"module": goModule, "package": goPackage},
	},
	{
		provider: winget.Name, software: "mm-gate-winget", packageID: "jqlang.jq", binary: "jq",
		runArgs:   []string{"--version"},
		platforms: []string{"windows/arm64", "windows/amd64"},
		targets:   []string{kindUser},
		userPath: map[string]string{
			"windows": "{localappdata}/Microsoft/WinGet/Links/jq.exe",
		},
		customPath: map[string]string{
			"windows": "{localappdata}/Microsoft/WinGet/Links/jq.exe",
		},
		options: map[string]string{"package_id": "jqlang.jq"},
	},
}

// cargoCrate is the Rust crate the gate installs: textdistance, a crate
// with no runtime dependency, so the build is fast and the lock file the
// crate publishes is the one the install pins.
const cargoCrate = "textdistance"

// goModule and goPackage are the Go module and the command inside it the
// gate installs.
const (
	goModule  = "golang.org/x/tools"
	goPackage = "golang.org/x/tools/cmd/stringer"
)

// providerDefinition is one fixture definition. The recipe declares the
// platforms the provider is declared for and the targets the gate proves.
func providerDefinition(f providerFixture) string {
	names := make([]string, 0, len(f.options))
	for name := range f.options {
		names = append(names, name)
	}
	sort.Strings(names)
	opts := make([]string, 0, len(names))
	for _, name := range names {
		opts = append(opts, "      "+name+": "+f.options[name])
	}
	return "schema_version: 1\nid: " + f.software + "\ndescription: Provider gate fixture.\ncategories: [walkthrough]\n" +
		"recipes:\n  - id: " + f.provider + "\n    provider: " + f.provider + "\n    package: " + f.packageID + "\n" +
		"    platforms: [" + strings.Join(f.platforms, ", ") + "]\n" +
		"    targets: [" + strings.Join(f.targets, ", ") + "]\n    requires: []\n" +
		"    options:\n" + strings.Join(opts, "\n") + "\n"
}

// writeProviderDefinitions writes the gate-local fixture definitions: one
// small real package per provider.
func writeProviderDefinitions(dir string) error {
	for _, f := range providerFixtures {
		if err := write(filepath.Join(dir, f.software+".yaml"), providerDefinition(f)); err != nil {
			return err
		}
	}
	return nil
}

// fixtureOnTarget reports whether a provider is declared for the target.
func fixtureOnTarget(f providerFixture, target string) bool {
	for _, p := range f.platforms {
		if p == target {
			return true
		}
	}
	return false
}

// fixtureHasTarget reports whether a fixture declares an installation
// target.
func fixtureHasTarget(f providerFixture, kind string) bool {
	for _, t := range f.targets {
		if t == kind {
			return true
		}
	}
	return false
}

// providerSentinels are the programs the provider gate must never start:
// the authorization programs, the package managers and every provider that
// is not under test on this target. The providers under test are found on
// the runner's PATH, so they are never shadowed.
func providerSentinels(target string) []string {
	out := []string{"sudo", "osascript", "security", "brew", "pip", "pipx"}
	for _, f := range providerFixtures {
		if !fixtureOnTarget(f, target) {
			out = append(out, f.provider)
		}
	}
	return out
}

// providerProfileDirs are the profile directories the Windows providers
// write to that the harness redirects: npm's per-user prefix lives in
// APPDATA. LOCALAPPDATA is left alone: WinGet keeps its own state there and
// fails without it.
var providerProfileDirs = []string{"profile/appdata"}

// providerEnv redirects the directories the providers write to into the
// harness: the Windows APPDATA and the cargo home, so the registry cache of
// the user target stays inside the isolated home too. rustup keeps its
// toolchains under the real home, which the isolated HOME would hide, so
// RUSTUP_HOME names it explicitly.
func providerEnv(root string) []string {
	env := []string{"CARGO_HOME=" + filepath.Join(root, "home", ".cargo")}
	if rustup := rustupHome(); rustup != "" {
		env = append(env, "RUSTUP_HOME="+rustup)
	}
	if runtime.GOOS != "windows" {
		return env
	}
	return append(env, "APPDATA="+filepath.Join(root, "profile", "appdata"))
}

// rustupHome is the runner's rustup home: RUSTUP_HOME when set, else
// .rustup in the real home directory, or "" when neither exists.
func rustupHome() string {
	if v := os.Getenv("RUSTUP_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(home, ".rustup")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return ""
	}
	return dir
}

// providerHarness is the install harness with the provider gate's own
// choices: the fixtures, the guarded programs and the redirected profile
// directories.
type providerHarness struct {
	*installHarness
	combinations []ProviderCombination
	machine      map[string]string
	// plans holds the plan of the first install of every provider and
	// target kind: the second install has to use a plan of its own.
	plans map[string]string
}

// newProviderHarness builds the harness of the provider gate.
func newProviderHarness(bin, target, revision string, raw []byte) (*providerHarness, error) {
	h, err := newInstallHarnessWith(bin, target, revision, raw, installHarnessOptions{
		sentinels:   providerSentinels(target),
		definitions: writeProviderDefinitions,
		extraEnv:    providerEnv,
		extraDirs:   providerProfileDirs,
	})
	if err != nil {
		return nil, err
	}
	h.ev.Kind = providerEvidenceKind
	h.ev.Mode = providerMode
	machine, err := h.machineSnapshot()
	if err != nil {
		return nil, err
	}
	return &providerHarness{
		installHarness: h, machine: machine,
		combinations: []ProviderCombination{}, plans: map[string]string{},
	}, nil
}

// machineSnapshot is the profile snapshot without the isolated home: the
// gate's own installs land in the isolated home, but the machine's own
// profiles and configuration must stay byte-identical.
func (h *installHarness) machineSnapshot() (map[string]string, error) {
	all, err := h.profileSnapshot()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for path, digest := range all {
		if strings.HasPrefix(path, "home/") {
			continue
		}
		out[path] = digest
	}
	return out, nil
}

// providersCmd runs the S3 provider gate for one target and writes its
// evidence record.
func providersCmd(args []string) error {
	fl := flag.NewFlagSet("providers", flag.ContinueOnError)
	bin := fl.String("binary", "", "managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *bin == "" || *tgt == "" || *out == "" {
		return errors.New("providers needs -binary, -target and -evidence")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	h, err := runProviderGate(absBin, *tgt, *rev)
	if err != nil {
		return err
	}
	defer os.RemoveAll(h.root)
	ev := ProviderEvidence{InstallEvidence: *h.ev, Combinations: h.combinations}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return err
	}
	b = append(h.paths.scrub(b), '\n')
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d steps, %d combinations, result %s\n", *tgt, len(h.ev.Steps), len(h.combinations), h.ev.Result)
	if h.ev.Result != "pass" {
		for _, f := range h.ev.Failures {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		return errors.New("the provider gate failed")
	}
	return nil
}

// runProviderGate builds the harness for one target and runs the gate in
// it. The caller owns the harness root and removes it.
func runProviderGate(bin, target, revision string) (*providerHarness, error) {
	raw, err := os.ReadFile(bin)
	if err != nil {
		return nil, err
	}
	h, err := newProviderHarness(bin, target, revision, raw)
	if err != nil {
		return nil, err
	}
	h.run()
	return h, nil
}

// run is the provider gate: the probe, the hosted-runner check, the
// combinations of every provider of this target and the guards.
func (h *providerHarness) run() {
	if !h.probe() {
		h.finish()
		return
	}
	if !hostedRunner() {
		h.step("provider mode", providerHostedRunnerMessage, false)
		h.finish()
		return
	}
	h.goTelemetryOff()
	for _, f := range providerFixtures {
		if !fixtureOnTarget(f, h.target) {
			continue
		}
		h.runFixture(f)
	}
	h.guards()
	h.finish()
}

// goTelemetryOff turns the Go toolchain's telemetry off in the harness
// config directory before anything is snapshotted: in its default local
// mode every go command bumps counters in the user config directory, which
// would show up as a change outside a custom root that no install made.
func (h *providerHarness) goTelemetryOff() {
	if !fixtureOnTarget(providerFixtureOf("go"), h.target) {
		return
	}
	c := exec.Command("go", "telemetry", "off")
	c.Env = h.env
	if out, err := c.CombinedOutput(); err != nil {
		h.step("go telemetry off", "go telemetry off failed: "+firstLineOf(out), false)
		return
	}
	h.step("go telemetry off", "the Go toolchain's telemetry is off in the harness", true)
}

// providerFixtureOf is the fixture of one provider.
func providerFixtureOf(name string) providerFixture {
	for _, f := range providerFixtures {
		if f.provider == name {
			return f
		}
	}
	return providerFixture{}
}

// fixtureEnv is the extra environment one fixture's installs run with.
// WinGet runs with the runner's real profile: it is a packaged app whose
// state and package links live there, not under the isolated profile. It
// installs for the user target only, so there is no custom root to contain.
func (h *providerHarness) fixtureEnv(f providerFixture) []string {
	if f.provider != "winget" {
		return nil
	}
	var env []string
	for _, k := range []string{"USERPROFILE", "APPDATA", "LOCALAPPDATA", "HOMEDRIVE", "HOMEPATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// providerStep is one checked step of a provider's fixture.
type providerStep struct {
	name string
	run  func() (string, bool)
}

// runFixture proves every combination of one provider on this target. A
// failing step ends the fixture; the next provider is still proven.
func (h *providerHarness) runFixture(f providerFixture) {
	steps := []providerStep{
		{"install " + f.provider + " for the user target", func() (string, bool) { return h.installUser(f) }},
		{"install " + f.provider + " again for the user target", func() (string, bool) { return h.installUserAgain(f) }},
	}
	if fixtureHasTarget(f, kindCustom) {
		steps = append(steps,
			providerStep{"install " + f.provider + " into a custom root", func() (string, bool) { return h.installCustom(f) }},
			providerStep{"install " + f.provider + " again into a custom root", func() (string, bool) { return h.installCustomAgain(f) }},
		)
	} else {
		steps = append(steps, providerStep{"refuse a custom root for " + f.provider,
			func() (string, bool) { return h.refuseCustom(f) }})
	}
	steps = append(steps,
		providerStep{"refuse the global target for " + f.provider, func() (string, bool) { return h.refuseGlobal(f) }},
		providerStep{"refuse a relative custom root for " + f.provider, func() (string, bool) { return h.refuseRelative(f) }},
	)
	for _, s := range steps {
		detail, ok := s.run()
		h.step(s.name, detail, ok)
		if !ok {
			return
		}
	}
}

// installUser installs the fixture for the current user.
func (h *providerHarness) installUser(f providerFixture) (string, bool) {
	detail, ok := h.installFixture(f, kindUser, []string{"--user"}, kindUser, h.fixturePath(f, kindUser, ""))
	if !ok {
		return detail, false
	}
	h.combine(f, kindUser, observedInstalled, observedInstalled, true, detail)
	return detail, true
}

// installUserAgain installs the fixture for the current user a second
// time: the outcome is unchanged and the plan has no operation.
func (h *providerHarness) installUserAgain(f providerFixture) (string, bool) {
	return h.installFixtureAgain(f, kindUser, []string{"--user"}, kindUser)
}

// installCustom installs the fixture into a custom root and proves that
// the install changed nothing outside that root and the guarded state
// directory.
func (h *providerHarness) installCustom(f providerFixture) (string, bool) {
	root := h.customRoot(f)
	before, err := h.containmentSnapshot(root)
	if err != nil {
		h.combine(f, kindCustom, observedInstalled, observedFailed, false, err.Error())
		return err.Error(), false
	}
	detail, ok := h.installFixture(f, kindCustom, []string{"--custom", root}, "custom:"+root, h.fixturePath(f, kindCustom, root))
	if !ok {
		return detail, false
	}
	after, err := h.containmentSnapshot(root)
	if err != nil {
		h.combine(f, kindCustom, observedInstalled, observedFailed, false, err.Error())
		return err.Error(), false
	}
	changes := profileDiff(before, after)
	if len(changes) > 0 {
		problem := "the install changed " + strings.Join(changes, "; ")
		h.combine(f, kindCustom, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	detail = detail + "; nothing outside the custom root and the state directory changed"
	h.combine(f, kindCustom, observedInstalled, observedInstalled, true, detail)
	return detail, true
}

// installCustomAgain installs the fixture into the same custom root a
// second time: the outcome is unchanged and the plan has no operation.
func (h *providerHarness) installCustomAgain(f providerFixture) (string, bool) {
	root := h.customRoot(f)
	before, err := h.containmentSnapshot(root)
	if err != nil {
		h.combine(f, kindCustom, observedUnchanged, observedFailed, false, err.Error())
		return err.Error(), false
	}
	detail, ok := h.installFixtureAgain(f, kindCustom, []string{"--custom", root}, "custom:"+root)
	if !ok {
		return detail, false
	}
	after, err := h.containmentSnapshot(root)
	if err != nil {
		h.combine(f, kindCustom, observedUnchanged, observedFailed, false, err.Error())
		return err.Error(), false
	}
	changes := profileDiff(before, after)
	if len(changes) > 0 {
		problem := "the second install changed " + strings.Join(changes, "; ")
		h.combine(f, kindCustom, observedUnchanged, observedFailed, false, problem)
		return problem, false
	}
	h.combine(f, kindCustom, observedUnchanged, observedUnchanged, true, detail)
	return detail, true
}

// installFixture installs the fixture for one target and checks the
// record, the plan, the journal, the receipt and the installed binary. It
// records the combination when the install fails; on success the caller
// records it with the detail it knows best.
func (h *providerHarness) installFixture(f providerFixture, kind string, args []string, wantTarget, binPath string) (string, bool) {
	code, out, errOut := h.coreEnv(h.fixtureEnv(f), append([]string{"software", f.software, "--install"}, args...)...)
	rec, problem := h.installRecord(out, errOut, code, 0, f.software, wantTarget, "ok")
	if problem != "" {
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	planID := str(rec, "plan")
	if !planIDPattern.MatchString(planID) {
		problem := "the record names no plan: " + planID
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	doc, err := h.planDoc(planID)
	if err != nil {
		h.combine(f, kind, observedInstalled, observedFailed, false, err.Error())
		return err.Error(), false
	}
	switch {
	case doc.State != "applied":
		problem := "the plan is " + doc.State + ", not applied"
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	case doc.Command != "software --install":
		problem := "the plan runs " + doc.Command
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	case len(doc.Operations) != 1:
		problem := fmt.Sprintf("the plan has %d operations, want one", len(doc.Operations))
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	case doc.Operations[0].Provider != f.provider:
		problem := "the plan runs the provider " + doc.Operations[0].Provider + ", not " + f.provider
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	recs, err := h.journal(planID)
	if err != nil {
		h.combine(f, kind, observedInstalled, observedFailed, false, err.Error())
		return err.Error(), false
	}
	if problem := h.journalApplied(recs, doc.Operations[0].ID); problem != "" {
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	if problem := h.binaryRuns(binPath, f.runArgs); problem != "" {
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	h.recordPlan(planID)
	h.plans[f.provider+"/"+kind] = planID
	detail := f.packageID + " " + str(rec, "resolved_version") + " installed; plan " + planID +
		" applied with one verified operation; " + f.binary + " runs"
	return detail, true
}

// installFixtureAgain installs the fixture a second time for one target:
// the outcome is unchanged and the plan has no operation.
func (h *providerHarness) installFixtureAgain(f providerFixture, kind string, args []string, wantTarget string) (string, bool) {
	code, out, errOut := h.coreEnv(h.fixtureEnv(f), append([]string{"software", f.software, "--install"}, args...)...)
	rec, problem := h.installRecord(out, errOut, code, 0, f.software, wantTarget, observedUnchanged)
	if problem != "" {
		h.combine(f, kind, observedUnchanged, observedFailed, false, problem)
		return problem, false
	}
	planID := str(rec, "plan")
	if first := h.plans[f.provider+"/"+kind]; first != "" && planID == first {
		problem := "the second install reused the plan " + planID
		h.combine(f, kind, observedUnchanged, observedFailed, false, problem)
		return problem, false
	}
	doc, err := h.planDoc(planID)
	if err != nil {
		h.combine(f, kind, observedUnchanged, observedFailed, false, err.Error())
		return err.Error(), false
	}
	switch {
	case doc.State != "applied":
		problem := "the plan is " + doc.State + ", not applied"
		h.combine(f, kind, observedUnchanged, observedFailed, false, problem)
		return problem, false
	case len(doc.Operations) != 0:
		problem := fmt.Sprintf("the second plan has %d operations, want none", len(doc.Operations))
		h.combine(f, kind, observedUnchanged, observedFailed, false, problem)
		return problem, false
	}
	recs, err := h.journal(planID)
	if err != nil {
		h.combine(f, kind, observedUnchanged, observedFailed, false, err.Error())
		return err.Error(), false
	}
	if problem := h.journalApplied(recs, ""); problem != "" {
		h.combine(f, kind, observedUnchanged, observedFailed, false, problem)
		return problem, false
	}
	h.recordPlan(planID)
	detail := "the second install planned no operation; plan " + planID + " applied unchanged"
	return detail, true
}

// refuseGlobal proves that the whole-machine target is refused without a
// plan.
func (h *providerHarness) refuseGlobal(f providerFixture) (string, bool) {
	return h.refuse(f, kindGlobal, []string{"--global"}, "the global target")
}

// refuseCustom proves that a custom root is refused without a plan, for a
// provider whose fixture declares no custom target.
func (h *providerHarness) refuseCustom(f providerFixture) (string, bool) {
	root := h.customRoot(f)
	return h.refuse(f, kindCustom, []string{"--custom", root}, "the custom root")
}

// refuse proves that a combination is refused and that the refusal writes
// no plan: the diagnostic is the unsupported-combination one and the
// guarded state directory gained no plan.
func (h *providerHarness) refuse(f providerFixture, kind string, args []string, what string) (string, bool) {
	before := len(h.planIDs())
	code, _, errOut := h.coreEnv(h.fixtureEnv(f), append([]string{"software", f.software, "--install"}, args...)...)
	detail := ""
	ok := true
	switch {
	case code == 0:
		ok = false
		detail = "the install of " + what + " succeeded; it must be refused"
	case !strings.Contains(string(errOut), diag.SoftwareUnsupportedCombo):
		ok = false
		detail = "the diagnostic is not " + diag.SoftwareUnsupportedCombo + ": " + firstLineOf(errOut)
	case len(h.planIDs()) != before:
		ok = false
		detail = "the refusal of " + what + " wrote a plan"
	default:
		detail = what + " refused with " + diag.SoftwareUnsupportedCombo + " and no plan was written"
	}
	if !ok {
		h.combine(f, kind, observedRefused, observedFailed, false, detail)
		return detail, false
	}
	h.combine(f, kind, observedRefused, observedRefused, true, detail)
	return detail, true
}

// refuseRelative proves that a relative custom root is refused. The
// software command resolves a relative --custom against the working
// directory, so the rule that a custom root is an absolute path lives in
// the provider: this is where it is proven. Planning is read-only, so no
// tool is started and nothing is written.
func (h *providerHarness) refuseRelative(f providerFixture) (string, bool) {
	p := gateProvider(f.provider, h.env)
	if p == nil {
		detail := "the provider " + f.provider + " has no implementation"
		h.combine(f, kindRelative, observedRefused, observedFailed, false, detail)
		return detail, false
	}
	_, err := p.Plan(context.Background(), provider.Request{
		Software: f.software,
		Target:   "custom:relative-root",
		Options:  f.optionValues(),
	})
	switch {
	case err == nil:
		detail := "the provider planned an install into a relative custom root"
		h.combine(f, kindRelative, observedRefused, observedFailed, false, detail)
		return detail, false
	case !errors.Is(err, provider.ErrUnsupported):
		detail := "the refusal is not the unsupported-combination kind: " + err.Error()
		h.combine(f, kindRelative, observedRefused, observedFailed, false, detail)
		return detail, false
	}
	detail := "the provider refused the relative custom root; the software command maps that refusal to " + diag.SoftwareUnsupportedCombo
	h.combine(f, kindRelative, observedRefused, observedRefused, true, detail)
	return detail, true
}

// gateProvider is the provider of the gate, wired like the core wires it
// and reading the environment of the harness. Its runner starts nothing:
// the gate plans a refusal, so a tool that would be started is a failure
// of the gate.
func gateProvider(name string, env []string) provider.Provider {
	noRun := func(context.Context, string, []string, []string) (provider.Result, error) {
		return provider.Result{}, errors.New("the provider gate starts no tool while it plans a refusal")
	}
	base := func() []string { return env }
	switch name {
	case uv.Name:
		return &uv.Provider{Run: noRun, Environ: base, LookPath: exec.LookPath}
	case npm.Name:
		return &npm.Provider{GOOS: runtime.GOOS, Run: noRun, Environ: base, LookPath: exec.LookPath}
	case cargo.Name:
		return &cargo.Provider{Run: noRun, Environ: base, LookPath: exec.LookPath, GOOS: runtime.GOOS}
	case golang.Name:
		return &golang.Provider{GOOS: runtime.GOOS, Run: noRun, Environ: base, LookPath: golang.LookPath}
	case winget.Name:
		return &winget.Provider{Run: noRun, Environ: base, LookPath: exec.LookPath}
	}
	return nil
}

// optionValues are the fixture's options as the provider receives them.
func (f providerFixture) optionValues() map[string]string {
	out := map[string]string{}
	for name, value := range f.options {
		out[name] = value
	}
	return out
}

// customRoot is the custom root of one provider's fixture: a directory of
// the harness root, so nothing it holds is ever part of the machine.
func (h *providerHarness) customRoot(f providerFixture) string {
	return filepath.Join(h.root, "custom", f.provider)
}

// fixturePath is where the provider installs the fixture's binary: the
// user's own location under the isolated home, or the custom root.
func (h *providerHarness) fixturePath(f providerFixture, kind, root string) string {
	tmpl := f.userPath[h.osKey()]
	if kind == kindCustom {
		tmpl = f.customPath[h.osKey()]
	}
	if tmpl == "" {
		return ""
	}
	r := strings.NewReplacer(
		"{home}", h.home,
		"{root}", root,
		"{appdata}", h.profileDir("appdata"),
		"{localappdata}", os.Getenv("LOCALAPPDATA"),
		"{bin}", f.binary,
	)
	return r.Replace(tmpl)
}

// osKey is the key of the target's operating system in the fixture paths.
func (h *providerHarness) osKey() string {
	if strings.HasPrefix(h.target, "windows/") {
		return "windows"
	}
	return "unix"
}

// profileDir is one of the redirected profile directories of the harness.
func (h *providerHarness) profileDir(name string) string {
	return filepath.Join(h.root, "profile", name)
}

// binaryRuns starts the installed binary and checks that it reports
// itself. The path is the one the provider installs into.
func (h *providerHarness) binaryRuns(path string, args []string) string {
	if path == "" {
		return "the fixture declares no path for the installed binary"
	}
	if _, err := os.Stat(path); err != nil {
		return "the installed binary is missing: " + err.Error()
	}
	code, out, errOut := runWithTimeout(path, args, 120*time.Second)
	if code != 0 {
		return "the installed binary exited " + fmt.Sprint(code) + ": " + firstLineOf(out, errOut)
	}
	if firstLineOf(out, errOut) == "" {
		return "the installed binary printed nothing"
	}
	return ""
}

// runWithTimeout starts one program and waits for it, with a timeout. The
// program runs in a process group of its own, so a timeout kills the whole
// group and no child of it survives.
func runWithTimeout(path string, args []string, timeout time.Duration) (int, []byte, []byte) {
	name, argv := path, args
	if runtime.GOOS == "windows" && strings.HasSuffix(strings.ToLower(path), ".cmd") {
		// Windows starts a batch file through the command interpreter.
		name, argv = "cmd", append([]string{"/c", path}, args...)
	}
	c := exec.Command(name, argv...)
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	isolateProcessGroup(c)
	if err := c.Start(); err != nil {
		return -1, nil, []byte(err.Error())
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		code := 0
		if x, ok := err.(*exec.ExitError); ok {
			code = x.ExitCode()
		} else if err != nil {
			code = -1
			e.WriteString(err.Error())
		}
		return code, o.Bytes(), e.Bytes()
	case <-time.After(timeout):
		_ = killProcessGroup(c)
		<-done
		return -1, o.Bytes(), append(e.Bytes(), []byte("the program did not finish in time")...)
	}
}

// containmentSnapshot hashes every regular file under the harness root
// except the custom root and the guarded state directory, which an
// install into a custom root is allowed to change.
func (h *providerHarness) containmentSnapshot(root string) (map[string]string, error) {
	out := map[string]string{}
	// Windows' own WinINet cache under the isolated profile changes on its
	// own whenever the machine touches the network; it is not the install.
	inet := filepath.Join(h.home, "AppData", "Local", "Microsoft", "Windows", "INetCache")
	if err := hashTreeExcept(h.root, "harness", []string{root, h.state, inet}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// hashTreeExcept records the digest of every regular file under root,
// named by its path relative to root and the given prefix. The named
// directories are skipped.
func hashTreeExcept(root, prefix string, skip []string, out map[string]string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		for _, s := range skip {
			if p != s && !strings.HasPrefix(p, s+string(os.PathSeparator)) {
				continue
			}
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[prefix+"/"+filepath.ToSlash(rel)] = sum(b)
		return nil
	})
}

// combine records one proven combination.
func (h *providerHarness) combine(f providerFixture, kind, expected, observed string, ok bool, detail string) {
	h.combinations = append(h.combinations, ProviderCombination{
		Provider: f.provider, Software: f.software, TargetKind: kind,
		Expected: expected, Observed: observed, OK: ok, Detail: h.paths.clean(detail),
	})
}

// guards reads the sentinel log and compares the machine's own profiles
// with the snapshot taken before the first core invocation. The isolated
// home is not part of it: the gate's own installs land there.
func (h *providerHarness) guards() {
	h.ev.Guards.SentinelInvocations = []string{}
	if b, err := os.ReadFile(h.log); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			h.ev.Guards.SentinelInvocations = append(h.ev.Guards.SentinelInvocations, h.paths.clean(line))
		}
	}
	after, err := h.machineSnapshot()
	if err != nil {
		h.ev.Guards.ProfileChanges = []string{"the profiles could not be read: " + err.Error()}
		h.step("guards", "the profiles could not be read", false)
		return
	}
	changes := profileDiff(h.machine, after)
	if changes == nil {
		changes = []string{}
	}
	h.ev.Guards.ProfileChanges = changes
	problem := ""
	switch {
	case len(h.ev.Guards.SentinelInvocations) > 0:
		problem = "a guarded program was started: " + strings.Join(h.ev.Guards.SentinelInvocations, "; ")
	case len(changes) > 0:
		problem = "the profiles changed: " + strings.Join(changes, "; ")
	}
	detail := "no guarded program was started and the profiles are byte-identical"
	if problem != "" {
		detail = problem
	}
	h.step("guards", detail, problem == "")
}

// providersAggregateCmd checks that five provider-gate records belong
// together: one per first-release target, the same source revision, every
// gate passed and every combination proven. It prints the combination
// matrix.
func providersAggregateCmd(args []string) error {
	fl := flag.NewFlagSet("providers-aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("providers-aggregate needs -source-revision")
	}
	files := fl.Args()
	var problems []string
	byTarget := map[string]ProviderEvidence{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var ev ProviderEvidence
		if err := json.Unmarshal(b, &ev); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		if ev.Kind != providerEvidenceKind {
			problems = append(problems, fmt.Sprintf("%s is a %q record, not %q", ev.Target, ev.Kind, providerEvidenceKind))
		}
		if _, dup := byTarget[ev.Target]; dup {
			problems = append(problems, "two records for "+ev.Target)
		}
		byTarget[ev.Target] = ev
		if ev.SourceRevision != *rev {
			problems = append(problems, fmt.Sprintf("%s ran %q, not %s", ev.Target, ev.SourceRevision, *rev))
		}
		if ev.Result != "pass" {
			problems = append(problems, ev.Target+" did not pass")
		}
		for _, c := range ev.Combinations {
			if !c.OK {
				problems = append(problems, fmt.Sprintf("%s: %s %s is %s", ev.Target, c.Provider, c.TargetKind, c.Observed))
			}
		}
	}
	for _, t := range firstReleaseTargets {
		ev, ok := byTarget[t]
		if !ok {
			problems = append(problems, "no evidence for "+t)
			continue
		}
		for _, f := range providerFixtures {
			if !fixtureOnTarget(f, t) {
				continue
			}
			for _, kind := range []string{kindUser, kindCustom, kindGlobal, kindRelative} {
				if !combinationProven(ev, f.provider, kind) {
					problems = append(problems, fmt.Sprintf("%s: no combination for %s %s", t, f.provider, kind))
				}
			}
		}
	}
	if len(files) != len(firstReleaseTargets) {
		problems = append(problems, fmt.Sprintf("%d records, want %d", len(files), len(firstReleaseTargets)))
	}
	sort.Strings(problems)
	printProviderMatrix(byTarget)
	fmt.Printf("\nsource revision `%s`\n", *rev)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("FAIL:", p)
		}
		return errors.New("providers-aggregate failed")
	}
	return nil
}

// combinationProven reports whether a record holds a combination of a
// provider and a target kind.
func combinationProven(ev ProviderEvidence, name, kind string) bool {
	for _, c := range ev.Combinations {
		if c.Provider == name && c.TargetKind == kind {
			return true
		}
	}
	return false
}

// printProviderMatrix prints the combination matrix: one row per provider
// and target kind, one column per first-release target. A cell is ok when
// every combination of that cell passed, FAIL when one did not and n-a
// when the provider is not declared for that target.
func printProviderMatrix(byTarget map[string]ProviderEvidence) {
	fmt.Printf("| provider | target | %s |\n", strings.Join(firstReleaseTargets, " | "))
	fmt.Printf("|---|---|%s\n", strings.Repeat("---|", len(firstReleaseTargets)))
	for _, f := range providerFixtures {
		for _, kind := range []string{kindUser, kindCustom, kindGlobal, kindRelative} {
			fmt.Printf("| %s | %s |", f.provider, kind)
			for _, t := range firstReleaseTargets {
				fmt.Printf(" %s |", providerCell(byTarget, f.provider, kind, t))
			}
			fmt.Println()
		}
	}
}

// providerCell is one cell of the combination matrix.
func providerCell(byTarget map[string]ProviderEvidence, name, kind, target string) string {
	ev, ok := byTarget[target]
	if !ok {
		return "FAIL"
	}
	seen, failed := false, false
	for _, c := range ev.Combinations {
		if c.Provider != name || c.TargetKind != kind {
			continue
		}
		seen = true
		if !c.OK {
			failed = true
		}
	}
	switch {
	case !seen:
		return "n-a"
	case failed:
		return "FAIL"
	default:
		return "ok"
	}
}
