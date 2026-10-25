package main

// The S3/S7 provider gate (issues #285, #351; D11). It drives a built
// managed-machine binary through real installs and tamper refusals per
// provider on a hosted runner: every provider's fixture package is installed
// for the user target and (where supported) into a custom root, the plan, the
// journal, the receipt and the installed binary or bundle are read, a second
// install has to report the software unchanged, unsupported target and
// platform combinations are refused without a plan, an install into a custom
// root is proved to change nothing outside that root and the guarded state
// directory, and tamper refusal fixtures (sha256, team-id, host) are proved to
// fail at the expected operation with a failed plan receipt, an empty stage/
// directory, and zero changes to <home>/Applications or the bin directories.
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
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/appcopy"
	"github.com/qwts/managed-machine/internal/provider/archive"
	"github.com/qwts/managed-machine/internal/provider/brew"
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

// Fixture row expectation, source and tamper constants (S7 D11).
const (
	expectInstall         = "install"
	expectRefuseMismatch  = "refuse:" + diag.SoftwareArtifactMismatch
	expectRefuseSignature = "refuse:" + diag.SoftwareSignatureRejected
	expectRefuseHost      = "refuse:" + diag.SoftwareHostUnlisted

	sourceReal     = "real"
	sourceLoopback = "loopback"

	tamperNone   = "none"
	tamperSHA256 = "sha256"
	tamperTeamID = "team-id"
	tamperHost   = "host"
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
// writes, the package it installs or refuses, the source and tamper mode, and
// where the installed binary or bundle has to be. The binary paths are
// templates: {home} is the isolated home, {root} the custom root, {appdata}
// the redirected APPDATA and {localappdata} the runner's own LOCALAPPDATA on
// Windows, and {bin} the binary's own name.
type providerFixture struct {
	provider   string
	software   string
	packageID  string
	binary     string
	runArgs    []string
	platforms  []string
	targets    []string
	expect     string
	source     string
	tamper     string
	userPath   map[string]string
	customPath map[string]string
	options    map[string]string
}

// providerFixtures are the shared X07 and N07 fixtures of the provider gate
// (D11): the five X07 language/package-manager rows (`uv`, `npm`, `cargo`,
// `go`, `winget`) plus the S7 N07 `brew` cask, `app-copy` (real + `sha256`,
// `team-id`, `host` loopback refusals) and `archive` (real + `sha256`, `host`
// loopback refusals) rows.
var providerFixtures = []providerFixture{
	{
		provider: uv.Name, software: "mm-gate-uv", packageID: "cowsay", binary: "cowsay",
		runArgs:   []string{"--version"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
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
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
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
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
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
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
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
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
		userPath: map[string]string{
			"windows": "{localappdata}/Microsoft/WinGet/Links/jq.exe",
		},
		customPath: map[string]string{
			"windows": "{localappdata}/Microsoft/WinGet/Links/jq.exe",
		},
		options: map[string]string{"package_id": "jqlang.jq"},
	},
	{
		provider: brew.Name, software: "mm-gate-brew-cask", packageID: brewCaskToken, binary: brewCaskApp,
		platforms: []string{"darwin/arm64"},
		targets:   []string{kindUser},
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
		userPath: map[string]string{
			"unix": "{home}/Applications/" + brewCaskApp,
		},
		customPath: map[string]string{
			"unix": "{root}/Applications/" + brewCaskApp,
		},
		options: map[string]string{
			"cask":           brewCaskToken,
			"app_name":       brewCaskApp,
			"team_id":        brewCaskTeamID,
			"url_hosts":      "[" + brewCaskURLHost + "]",
			"homepage_hosts": "[" + brewCaskHomepageHost + "]",
		},
	},
	{
		provider: appcopy.Name, software: "mm-gate-app-copy", packageID: appCopyApp, binary: appCopyApp,
		platforms: []string{"darwin/arm64"},
		targets:   []string{kindUser},
		expect:    expectInstall, source: sourceReal, tamper: tamperNone,
		userPath: map[string]string{
			"unix": "{home}/Applications/" + appCopyApp,
		},
		customPath: map[string]string{
			"unix": "{root}/Applications/" + appCopyApp,
		},
		options: map[string]string{
			"url":       appCopyURL,
			"sha256":    appCopySHA256,
			"app_name":  appCopyApp,
			"version":   appCopyVersion,
			"team_id":   appCopyTeamID,
			"url_hosts": "[" + appCopyURLHost + "]",
			"format":    "dmg",
		},
	},
	{
		provider: appcopy.Name, software: "mm-gate-app-copy-sha256", packageID: gateRefuseApp, binary: gateRefuseApp,
		platforms: []string{"darwin/arm64"},
		targets:   []string{kindUser},
		expect:    expectRefuseMismatch, source: sourceLoopback, tamper: tamperSHA256,
		userPath: map[string]string{
			"unix": "{home}/Applications/" + gateRefuseApp,
		},
		customPath: map[string]string{
			"unix": "{root}/Applications/" + gateRefuseApp,
		},
		options: map[string]string{
			"url":       "https://127.0.0.1/app-copy-sha256.dmg",
			"sha256":    placeholderSHA256,
			"app_name":  gateRefuseApp,
			"version":   "1.0.0",
			"team_id":   brewCaskTeamID,
			"url_hosts": "[127.0.0.1]",
			"format":    "dmg",
		},
	},
	{
		provider: appcopy.Name, software: "mm-gate-app-copy-team-id", packageID: gateRefuseApp, binary: gateRefuseApp,
		platforms: []string{"darwin/arm64"},
		targets:   []string{kindUser},
		expect:    expectRefuseSignature, source: sourceLoopback, tamper: tamperTeamID,
		userPath: map[string]string{
			"unix": "{home}/Applications/" + gateRefuseApp,
		},
		customPath: map[string]string{
			"unix": "{root}/Applications/" + gateRefuseApp,
		},
		options: map[string]string{
			"url":       "https://127.0.0.1/app-copy-team-id.dmg",
			"sha256":    placeholderSHA256,
			"app_name":  gateRefuseApp,
			"version":   "1.0.0",
			"team_id":   brewCaskTeamID,
			"url_hosts": "[127.0.0.1]",
			"format":    "dmg",
		},
	},
	{
		provider: appcopy.Name, software: "mm-gate-app-copy-host", packageID: gateRefuseApp, binary: gateRefuseApp,
		platforms: []string{"darwin/arm64"},
		targets:   []string{kindUser},
		expect:    expectRefuseHost, source: sourceLoopback, tamper: tamperHost,
		userPath: map[string]string{
			"unix": "{home}/Applications/" + gateRefuseApp,
		},
		customPath: map[string]string{
			"unix": "{root}/Applications/" + gateRefuseApp,
		},
		options: map[string]string{
			"url":       "https://127.0.0.1/app-copy-host.dmg",
			"sha256":    placeholderSHA256,
			"app_name":  gateRefuseApp,
			"version":   "1.0.0",
			"team_id":   brewCaskTeamID,
			"url_hosts": "[127.0.0.1]",
			"format":    "dmg",
		},
	},
	{
		provider: archive.Name, software: "mm-gate-archive-sha256", packageID: "managed-machine", binary: "managed-machine",
		runArgs:   []string{"--version"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		expect:    expectRefuseMismatch, source: sourceLoopback, tamper: tamperSHA256,
		userPath: map[string]string{
			"unix": "{home}/.local/bin/managed-machine", "windows": "{home}/.local/bin/managed-machine.exe",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/managed-machine", "windows": "{root}/bin/managed-machine.exe",
		},
		options: map[string]string{
			"url":       "https://127.0.0.1/archive-sha256",
			"sha256":    placeholderSHA256,
			"version":   "1.0.0",
			"url_hosts": "[127.0.0.1]",
			"files":     "[managed-machine]",
			"format":    "tar.gz",
		},
	},
	{
		provider: archive.Name, software: "mm-gate-archive-host", packageID: "managed-machine", binary: "managed-machine",
		runArgs:   []string{"--version"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		expect:    expectRefuseHost, source: sourceLoopback, tamper: tamperHost,
		userPath: map[string]string{
			"unix": "{home}/.local/bin/managed-machine", "windows": "{home}/.local/bin/managed-machine.exe",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/managed-machine", "windows": "{root}/bin/managed-machine.exe",
		},
		options: map[string]string{
			"url":       "https://127.0.0.1/archive-host",
			"sha256":    placeholderSHA256,
			"version":   "1.0.0",
			"url_hosts": "[127.0.0.1]",
			"files":     "[managed-machine]",
			"format":    "tar.gz",
		},
	},
	{
		provider: archive.Name, software: "mm-gate-archive", packageID: "managed-machine", binary: "managed-machine",
		runArgs:   []string{"--version"},
		platforms: firstReleaseTargets,
		targets:   []string{kindUser, kindCustom},
		expect:    expectInstall, source: sourceLoopback, tamper: tamperNone,
		userPath: map[string]string{
			"unix": "{home}/.local/bin/managed-machine", "windows": "{home}/.local/bin/managed-machine.exe",
		},
		customPath: map[string]string{
			"unix": "{root}/bin/managed-machine", "windows": "{root}/bin/managed-machine.exe",
		},
		options: map[string]string{
			"url":       "https://127.0.0.1/archive",
			"sha256":    placeholderSHA256,
			"version":   "1.0.0",
			"url_hosts": "[127.0.0.1]",
			"files":     "[managed-machine]",
			"format":    "tar.gz",
		},
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

// Owner-verified real S7 N07 fixture metadata for the brew cask and app-copy
// rows on darwin/arm64 (D11).
const (
	brewCaskToken        = "telegram"
	brewCaskApp          = "Telegram.app"
	brewCaskTeamID       = "6N38VWS5BX"
	brewCaskURLHost      = "osx.telegram.org"
	brewCaskHomepageHost = "macos.telegram.org"

	appCopyURL     = "https://opencode.ai/files/bin/2.1.4/opencode-desktop-mac-arm64.dmg"
	appCopySHA256  = "1bf4030c8e15bf2dd30ffca4263b8586b052201ecc755d652ec465fcc44a0296"
	appCopyApp     = "OpenCode.app"
	appCopyVersion = "2.1.4"
	appCopyTeamID  = "5NZ4Q7NXJ4"
	appCopyURLHost = "opencode.ai"

	gateRefuseApp     = "GateRefuse.app"
	placeholderSHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
)

func formatOptionYAML(v string) string {
	s := strings.TrimSpace(v)
	if (strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]")) || s == "true" || s == "false" {
		return s
	}
	return strconv.Quote(v)
}

func recipeID(f providerFixture) string {
	if f.provider == brew.Name && f.options["cask"] != "" {
		return "brew-cask"
	}
	return f.provider
}

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
		opts = append(opts, "      "+name+": "+formatOptionYAML(f.options[name]))
	}
	return "schema_version: 1\nid: " + f.software + "\ndescription: Provider gate fixture.\ncategories: [walkthrough]\n" +
		"recipes:\n  - id: " + recipeID(f) + "\n    provider: " + f.provider + "\n    package: " + f.packageID + "\n" +
		"    platforms: [" + strings.Join(f.platforms, ", ") + "]\n" +
		"    targets: [" + strings.Join(f.targets, ", ") + "]\n    requires: []\n" +
		"    options:\n" + strings.Join(opts, "\n") + "\n"
}

// writeProviderDefinitions writes the gate-local fixture definitions: one
// small real or loopback package per row in providerFixtures.
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
	out := []string{"sudo", "osascript", "security", "pip", "pipx"}
	seen := map[string]bool{}
	for _, s := range out {
		seen[s] = true
	}
	for _, f := range providerFixtures {
		if !fixtureOnTarget(f, target) && !seen[f.provider] {
			seen[f.provider] = true
			out = append(out, f.provider)
		}
	}
	return out
}

// providerProfileDirs are the directories inside the harness root that
// providers require to exist before planning: the redirected Windows APPDATA,
// the user Applications directory for brew cask and app-copy, and the user
// ~/.local/bin directory for archive.
var providerProfileDirs = []string{"profile/appdata", "home/Applications", "home/.local/bin"}

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
	// loopbackCA is the certificate of the loopback fixture server. Only
	// loopback rows trust it: SSL_CERT_FILE replaces the system roots, so a
	// harness-wide setting would break every real download.
	loopbackCA string
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

// providersCmd runs the S3/S7 provider gate for one target and writes its
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
// loopback fixture server, the combinations of every provider row and the
// guards.
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
	cleanupLoopback, err := h.startLoopbackFixtures(nil)
	if err != nil {
		h.step("loopback fixtures", "the loopback fixture server could not be started: "+err.Error(), false)
		h.finish()
		return
	}
	defer cleanupLoopback()
	h.goTelemetryOff()
	for _, f := range providerFixtures {
		if !fixtureOnTarget(f, h.target) {
			if f.provider == winget.Name {
				continue
			}
			h.runOffPlatformFixture(f)
			continue
		}
		if f.expect != expectInstall {
			h.runRefusalFixture(f)
			continue
		}
		h.runFixture(f)
	}
	h.guards()
	h.finish()
}

func flipLastByte(b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) == 0 {
		return []byte{0x01}
	}
	out[len(out)-1] ^= 0x01
	return out
}

// buildAdHocDMG builds a small UDZO DMG containing an ad-hoc-signed bundle
// inside the harness temp directory on the hosted macOS runner. It is guarded
// so that it never runs outside a darwin GitHub-hosted runner (and never in
// go test).
func buildAdHocDMG(root, appName, version string) ([]byte, error) {
	if runtime.GOOS != "darwin" || !hostedRunner() {
		return nil, errors.New("ad-hoc DMG fixtures run only on a darwin GitHub-hosted runner")
	}
	workDir := filepath.Join(root, "dmg-work")
	srcDir := filepath.Join(workDir, "src")
	bundleDir := filepath.Join(srcDir, appName)
	macosDir := filepath.Join(bundleDir, "Contents", "MacOS")
	if err := os.MkdirAll(macosDir, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(workDir)

	execName := strings.TrimSuffix(appName, ".app")
	execPath := filepath.Join(macosDir, execName)
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		return nil, err
	}
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleExecutable</key>
  <string>%s</string>
  <key>CFBundleIdentifier</key>
  <string>invalid.managed-machine.gate-refuse</string>
  <key>CFBundleName</key>
  <string>%s</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>%s</string>
  <key>CFBundleVersion</key>
  <string>%s</string>
</dict>
</plist>
`, execName, execName, version, version)
	if err := os.WriteFile(filepath.Join(bundleDir, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
		return nil, err
	}

	signCmd := exec.Command("/usr/bin/codesign", "--force", "--deep", "-s", "-", bundleDir)
	if out, err := signCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("codesign -s -: %v: %s", err, firstLineOf(out))
	}

	dmgPath := filepath.Join(workDir, "fixture.dmg")
	hdiCmd := exec.Command("/usr/bin/hdiutil", "create", "-volname", execName, "-srcfolder", srcDir, "-ov", "-format", "UDZO", dmgPath)
	if out, err := hdiCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("hdiutil create: %v: %s", err, firstLineOf(out))
	}
	return os.ReadFile(dmgPath)
}

// startLoopbackFixtures starts the loopback HTTPS server for the archive and
// app-copy loopback fixtures, trusts its per-fixture certificate, and rewrites
// the loopback fixture definitions in h.defs with the live port and SHA-256
// digests. When dmgOverride is non-nil (in unit tests), it is used instead of
// calling buildAdHocDMG.
func (h *providerHarness) startLoopbackFixtures(dmgOverride []byte) (func(), error) {
	binBytes, err := os.ReadFile(h.bin)
	if err != nil {
		return nil, err
	}
	licBytes := []byte("MIT License\n")
	isWin := strings.HasPrefix(h.target, "windows/")
	binName := "managed-machine"
	archiveFormat := "tar.gz"
	var archiveBytes []byte
	if isWin {
		binName = "managed-machine.exe"
		archiveFormat = "zip"
		archiveBytes, err = packReleaseZip(binName, binBytes, licBytes)
	} else {
		archiveBytes, err = packReleaseTarGz(binName, binBytes, licBytes)
	}
	if err != nil {
		return nil, err
	}
	archiveSHA := sum(archiveBytes)
	tamperedArchive := flipLastByte(archiveBytes)

	dmgBytes := dmgOverride
	if dmgBytes == nil {
		if h.target == "darwin/arm64" && runtime.GOOS == "darwin" && hostedRunner() {
			dmgBytes, err = buildAdHocDMG(h.root, gateRefuseApp, "1.0.0")
			if err != nil {
				return nil, err
			}
		} else {
			dmgBytes = []byte("synthetic-dmg-placeholder")
		}
	}
	dmgSHA := sum(dmgBytes)
	tamperedDMG := flipLastByte(dmgBytes)

	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(archiveBytes)
	})
	mux.HandleFunc("/archive-sha256", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tamperedArchive)
	})
	mux.HandleFunc("/archive-host", func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(srvURL)
		port := u.Port()
		http.Redirect(w, r, "https://localhost:"+port+"/archive", http.StatusFound)
	})
	mux.HandleFunc("/app-copy-sha256.dmg", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tamperedDMG)
	})
	mux.HandleFunc("/app-copy-team-id.dmg", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(dmgBytes)
	})
	mux.HandleFunc("/app-copy-host.dmg", func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(srvURL)
		port := u.Port()
		http.Redirect(w, r, "https://localhost:"+port+"/app-copy-team-id.dmg", http.StatusFound)
	})

	srv := newFixtureTLSServer(mux)
	srvURL = srv.URL

	caPath := filepath.Join(h.root, "fixture-ca.pem")
	if err := os.WriteFile(caPath, pemCertificate(srv.Certificate().Raw), 0o644); err != nil {
		srv.Close()
		return nil, err
	}
	release, err := trustFixtureCertificate(caPath)
	if err != nil {
		srv.Close()
		return nil, err
	}
	h.loopbackCA = caPath

	for _, f := range providerFixtures {
		if f.source != sourceLoopback {
			continue
		}
		live := h.liveLoopbackFixture(f, srv.URL, archiveSHA, archiveFormat, binName, dmgSHA)
		if err := write(filepath.Join(h.defs, live.software+".yaml"), providerDefinition(live)); err != nil {
			release()
			srv.Close()
			return nil, err
		}
	}

	return func() {
		release()
		srv.Close()
	}, nil
}

func (h *providerHarness) liveLoopbackFixture(f providerFixture, baseURL, archiveSHA, archiveFormat, binName, dmgSHA string) providerFixture {
	opts := f.optionValues()
	switch f.provider {
	case archive.Name:
		opts["sha256"] = archiveSHA
		opts["format"] = archiveFormat
		opts["files"] = "[" + binName + "]"
		switch f.tamper {
		case tamperNone:
			opts["url"] = baseURL + "/archive"
		case tamperSHA256:
			opts["url"] = baseURL + "/archive-sha256"
		case tamperHost:
			opts["url"] = baseURL + "/archive-host"
		}
	case appcopy.Name:
		opts["sha256"] = dmgSHA
		switch f.tamper {
		case tamperSHA256:
			opts["url"] = baseURL + "/app-copy-sha256.dmg"
		case tamperTeamID:
			opts["url"] = baseURL + "/app-copy-team-id.dmg"
		case tamperHost:
			opts["url"] = baseURL + "/app-copy-host.dmg"
		}
	}
	f.options = opts
	return f
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

// providerFixtureOf is the first install fixture of one provider or software id.
func providerFixtureOf(name string) providerFixture {
	for _, f := range providerFixtures {
		if f.provider == name || f.software == name {
			return f
		}
	}
	return providerFixture{}
}

// fixtureEnv is the extra environment one fixture's installs run with.
// Loopback rows trust the loopback fixture certificate (on Unix through
// SSL_CERT_FILE; Windows reads the machine root store the gate adds it to).
// WinGet runs with the runner's real profile: it is a packaged app whose
// state and package links live there, not under the isolated profile. It
// installs for the user target only, so there is no custom root to contain.
func (h *providerHarness) fixtureEnv(f providerFixture) []string {
	if f.source == sourceLoopback && h.loopbackCA != "" {
		return []string{"SSL_CERT_FILE=" + h.loopbackCA}
	}
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

// runFixture proves every combination of one install fixture on this target. A
// failing step ends the fixture; the next provider is still proven.
func (h *providerHarness) runFixture(f providerFixture) {
	if f.provider == brew.Name && f.options["cask"] != "" && h.target == "darwin/arm64" && hostedRunner() {
		tok := f.options["cask"]
		if code, _, _ := h.brew("list", "--versions", "--cask", tok); code == 0 {
			_, _, _ = h.brew("uninstall", "--cask", "--force", tok)
		}
		defer func() {
			_, _, _ = h.brew("uninstall", "--cask", "--force", tok)
		}()
	}
	steps := []providerStep{
		{"install " + f.software + " for the user target", func() (string, bool) { return h.installUser(f) }},
		{"install " + f.software + " again for the user target", func() (string, bool) { return h.installUserAgain(f) }},
	}
	if fixtureHasTarget(f, kindCustom) {
		steps = append(steps,
			providerStep{"install " + f.software + " into a custom root", func() (string, bool) { return h.installCustom(f) }},
			providerStep{"install " + f.software + " again into a custom root", func() (string, bool) { return h.installCustomAgain(f) }},
		)
	} else {
		steps = append(steps, providerStep{"refuse a custom root for " + f.software,
			func() (string, bool) { return h.refuseCustom(f) }})
	}
	steps = append(steps,
		providerStep{"refuse the global target for " + f.software, func() (string, bool) { return h.refuseGlobal(f) }},
		providerStep{"refuse a relative custom root for " + f.software, func() (string, bool) { return h.refuseRelative(f) }},
	)
	for _, s := range steps {
		detail, ok := s.run()
		h.step(s.name, detail, ok)
		if !ok {
			return
		}
	}
}

// runOffPlatformFixture runs a darwin/arm64-only cask or app-copy fixture on
// Linux or Windows and records the observed MM_SOFTWARE_UNSUPPORTED_COMBINATION
// refusal as refused (never n-a by assumption; D11).
func (h *providerHarness) runOffPlatformFixture(f providerFixture) {
	if f.expect != expectInstall {
		detail, ok := h.refuse(f, kindUser, []string{"--user"}, f.software+" on "+h.target)
		h.step("refuse "+f.software+" on "+h.target, detail, ok)
		return
	}
	steps := []providerStep{
		{"refuse " + f.software + " for the user target on " + h.target,
			func() (string, bool) {
				return h.refuse(f, kindUser, []string{"--user"}, "the user target on "+h.target)
			}},
		{"refuse a custom root for " + f.software + " on " + h.target,
			func() (string, bool) { return h.refuseCustom(f) }},
		{"refuse the global target for " + f.software + " on " + h.target,
			func() (string, bool) { return h.refuseGlobal(f) }},
		{"refuse a relative custom root for " + f.software + " on " + h.target,
			func() (string, bool) { return h.refuseRelative(f) }},
	}
	for _, s := range steps {
		detail, ok := s.run()
		h.step(s.name, detail, ok)
		if !ok {
			return
		}
	}
}

// runRefusalFixture runs one tamper refusal row (sha256, team-id or host) and
// verifies that <home>/Applications, the bin directories and <state>/stage are
// unchanged and <state>/stage is empty, and that the plan store holds one new
// failed plan whose receipt names the failed operation (D11).
func (h *providerHarness) runRefusalFixture(f providerFixture) {
	detail, ok := h.refuseTamperedFixture(f)
	h.step("refuse "+f.software+" ("+f.tamper+")", detail, ok)
}

func expectedFailedOperation(f providerFixture) string {
	if f.tamper == tamperTeamID {
		return "stage"
	}
	return "download"
}

// refusalSnapshot hashes <home>/Applications, the user and custom bin
// directories, and <state>/stage so a refusal row can prove that nothing was
// written to any destination or left in stage/.
func (h *providerHarness) refusalSnapshot() (map[string]string, error) {
	out := map[string]string{}
	dirs := []struct {
		prefix string
		path   string
	}{
		{"applications", filepath.Join(h.home, "Applications")},
		{"bin/user-local", filepath.Join(h.home, ".local", "bin")},
		{"bin/user-cargo", filepath.Join(h.home, ".cargo", "bin")},
		{"bin/user-go", filepath.Join(h.home, "go", "bin")},
		{"bin/custom", filepath.Join(h.root, "custom")},
		{"stage", filepath.Join(h.state, "stage")},
	}
	for _, d := range dirs {
		if _, err := os.Lstat(d.path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		err := filepath.WalkDir(d.path, func(p string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if p == d.path {
				return nil
			}
			rel, err := filepath.Rel(d.path, p)
			if err != nil {
				return err
			}
			key := d.prefix + "/" + filepath.ToSlash(rel)
			if entry.IsDir() {
				out[key] = "dir"
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				target, err := os.Readlink(p)
				if err != nil {
					return err
				}
				out[key] = "symlink:" + target
				return nil
			}
			if entry.Type().IsRegular() {
				b, err := os.ReadFile(p)
				if err != nil {
					return err
				}
				out[key] = sum(b)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func stageDirEmpty(stageDir string) bool {
	entries, err := os.ReadDir(stageDir)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && len(entries) == 0
}

func readPlanJournal(path string) ([]plan.Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []plan.Record
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r plan.Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (h *providerHarness) refuseTamperedFixture(f providerFixture) (string, bool) {
	wantDiag := strings.TrimPrefix(f.expect, "refuse:")
	wantOp := expectedFailedOperation(f)

	beforeSnap, err := h.refusalSnapshot()
	if err != nil {
		h.combine(f, kindUser, observedRefused, observedFailed, false, err.Error())
		return err.Error(), false
	}
	beforePlans := h.planIDs()

	code, _, errOut := h.coreEnv(h.fixtureEnv(f), "software", f.software, "--install", "--user")

	afterSnap, err := h.refusalSnapshot()
	if err != nil {
		h.combine(f, kindUser, observedRefused, observedFailed, false, err.Error())
		return err.Error(), false
	}

	if code != 1 {
		problem := fmt.Sprintf("the install exited %d, want 1: %s", code, firstLineOf(errOut))
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}
	if !strings.Contains(string(errOut), wantDiag) {
		problem := "the diagnostic is not " + wantDiag + ": " + firstLineOf(errOut)
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}
	if changes := profileDiff(beforeSnap, afterSnap); len(changes) > 0 {
		problem := "the refused install changed destination or stage paths: " + strings.Join(changes, "; ")
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}
	if !stageDirEmpty(filepath.Join(h.state, "stage")) {
		problem := "the state directory's stage/ is not empty after the refusal"
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}

	afterPlans := h.planIDs()
	var newPlans []string
	for id := range afterPlans {
		if !beforePlans[id] {
			newPlans = append(newPlans, id)
		}
	}
	if len(newPlans) != 1 {
		problem := fmt.Sprintf("the refusal wrote %d new plans, want one", len(newPlans))
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}
	planID := newPlans[0]
	if named := planInText(string(errOut)); named != "" && named != planID {
		problem := fmt.Sprintf("the diagnostic names %s, but the new plan in the store is %s", named, planID)
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}

	doc, err := h.planDoc(planID)
	if err != nil {
		h.combine(f, kindUser, observedRefused, observedFailed, false, err.Error())
		return err.Error(), false
	}
	if doc.State != "failed" {
		problem := "the plan is " + doc.State + ", not failed"
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}

	jRecs, err := readPlanJournal(filepath.Join(h.state, "journal", planID+".jsonl"))
	if err != nil {
		h.combine(f, kindUser, observedRefused, observedFailed, false, err.Error())
		return err.Error(), false
	}
	if len(jRecs) == 0 {
		problem := "the failed plan's journal is empty"
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}
	last := jRecs[len(jRecs)-1]
	failedOp, _ := last.Evidence["failed_operation"].(string)
	if last.Action != plan.ActionReceipt || last.Outcome != "failed" || last.State != plan.Failed {
		problem := fmt.Sprintf("the receipt is action=%q outcome=%q state=%q, want failed receipt", last.Action, last.Outcome, last.State)
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}
	if last.Operation != wantOp || failedOp != wantOp {
		problem := fmt.Sprintf("the receipt names operation=%q failed_operation=%q, want %q", last.Operation, failedOp, wantOp)
		h.combine(f, kindUser, observedRefused, observedFailed, false, problem)
		return problem, false
	}

	h.recordPlan(planID)
	detail := fmt.Sprintf("%s refused with %s; plan %s failed at %s with an empty stage/ and no destination write", f.software, wantDiag, planID, wantOp)
	h.combine(f, kindUser, observedRefused, observedRefused, true, detail)
	return detail, true
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
	if f.provider == archive.Name {
		if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
			h.combine(f, kindCustom, observedInstalled, observedFailed, false, err.Error())
			return err.Error(), false
		}
	}
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

func expectedOperationCount(providerName string) int {
	switch providerName {
	case appcopy.Name:
		return 3
	case archive.Name:
		return 2
	default:
		return 1
	}
}

func providerJournalApplied(recs []journalRecord, ops []struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}) string {
	if len(recs) == 0 {
		return "the journal is empty"
	}
	last := recs[len(recs)-1]
	if last.Action != "receipt" || last.Outcome != "applied" || last.State != "applied" {
		return "the receipt is " + last.Outcome + ", not applied"
	}
	wantLen := 2*len(ops) + 1
	if len(recs) != wantLen {
		return fmt.Sprintf("the journal has %d records, want %d", len(recs), wantLen)
	}
	for i, op := range ops {
		if recs[2*i].Outcome != "started" || recs[2*i].Operation != op.ID {
			return fmt.Sprintf("operation %s was not started", op.ID)
		}
		if recs[2*i+1].Outcome != "ok" || recs[2*i+1].Operation != op.ID {
			return fmt.Sprintf("operation %s was not verified", op.ID)
		}
	}
	return ""
}

// installFixture installs the fixture for one target and checks the
// record, the plan, the journal, the receipt and the installed binary or
// bundle. It records the combination when the install fails; on success the
// caller records it with the detail it knows best.
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
	wantOps := expectedOperationCount(f.provider)
	switch {
	case doc.State != "applied":
		problem := "the plan is " + doc.State + ", not applied"
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	case doc.Command != "software --install":
		problem := "the plan runs " + doc.Command
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	case len(doc.Operations) != wantOps:
		problem := fmt.Sprintf("the plan has %d operations, want %d", len(doc.Operations), wantOps)
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	for _, op := range doc.Operations {
		if op.Provider != f.provider {
			problem := "the plan runs the provider " + op.Provider + ", not " + f.provider
			h.combine(f, kind, observedInstalled, observedFailed, false, problem)
			return problem, false
		}
	}
	recs, err := h.journal(planID)
	if err != nil {
		h.combine(f, kind, observedInstalled, observedFailed, false, err.Error())
		return err.Error(), false
	}
	if problem := providerJournalApplied(recs, doc.Operations); problem != "" {
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	if problem := h.binaryRuns(binPath, f.runArgs); problem != "" {
		h.combine(f, kind, observedInstalled, observedFailed, false, problem)
		return problem, false
	}
	h.recordPlan(planID)
	h.plans[f.software+"/"+kind] = planID
	detail := f.packageID + " " + str(rec, "resolved_version") + " installed; plan " + planID +
		" applied; " + f.binary + " verified"
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
	if first := h.plans[f.software+"/"+kind]; first != "" && planID == first {
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
	case brew.Name:
		return &brew.Provider{Prefix: gatePrefix(runtime.GOOS + "/" + runtime.GOARCH), Run: noRun, Environ: base}
	case appcopy.Name:
		return &appcopy.Provider{Run: noRun, Environ: base}
	case archive.Name:
		return &archive.Provider{Environ: base, GOOS: runtime.GOOS}
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
	return filepath.Join(h.root, "custom", f.software)
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

// binaryRuns verifies the installed artifact at path: for macOS .app bundles
// it checks that the bundle directory and Contents/Info.plist exist; for CLI
// executables it starts the binary with args and checks that it exits 0 and
// prints output.
func (h *providerHarness) binaryRuns(path string, args []string) string {
	if path == "" {
		return "the fixture declares no path for the installed binary"
	}
	if _, err := os.Stat(path); err != nil {
		return "the installed binary is missing: " + err.Error()
	}
	if strings.HasSuffix(path, ".app") {
		plist := filepath.Join(path, "Contents", "Info.plist")
		if _, err := os.Stat(plist); err != nil {
			return "the installed application bundle has no Info.plist: " + err.Error()
		}
		return ""
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

// fixtureRequiredKinds returns the target kinds that must be proven in
// ProviderEvidence for fixture f on target. WinGet is only on Windows targets;
// brew cask and app-copy rows must be proven on all five targets (recorded as
// refused on Linux and Windows, never n-a by assumption; D11).
func fixtureRequiredKinds(f providerFixture, target string) []string {
	if f.provider == winget.Name && !fixtureOnTarget(f, target) {
		return nil
	}
	if f.expect != expectInstall {
		return []string{kindUser}
	}
	return []string{kindUser, kindCustom, kindGlobal, kindRelative}
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
			if c.Observed == "n-a" {
				problems = append(problems, fmt.Sprintf("%s: %s (%s) recorded n-a; refusals must be observed", ev.Target, c.Provider, c.Software))
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
			for _, kind := range fixtureRequiredKinds(f, t) {
				if !combinationProven(ev, f, kind) {
					problems = append(problems, fmt.Sprintf("%s: no combination for %s (%s) %s", t, f.provider, f.software, kind))
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

// combinationProven reports whether a record holds a proven combination for
// fixture f and target kind, enforcing that off-platform cask and app-copy
// rows and tamper refusal rows record observedRefused.
func combinationProven(ev ProviderEvidence, f providerFixture, kind string) bool {
	for _, c := range ev.Combinations {
		if c.Provider != f.provider || c.Software != f.software || c.TargetKind != kind {
			continue
		}
		if (f.expect != expectInstall || !fixtureOnTarget(f, ev.Target)) && c.Observed != observedRefused {
			return false
		}
		return true
	}
	return false
}

// printProviderMatrix prints the combination matrix: one row per provider
// fixture and target kind, one column per first-release target. A cell is ok
// when every combination of that cell passed, FAIL when one did not and n-a
// only when the provider is WinGet on a non-Windows target.
func printProviderMatrix(byTarget map[string]ProviderEvidence) {
	fmt.Printf("| provider | target | %s |\n", strings.Join(firstReleaseTargets, " | "))
	fmt.Printf("|---|---|%s\n", strings.Repeat("---|", len(firstReleaseTargets)))
	for _, f := range providerFixtures {
		kinds := []string{kindUser, kindCustom, kindGlobal, kindRelative}
		if f.expect != expectInstall {
			kinds = []string{kindUser}
		}
		label := f.provider
		if f.tamper != tamperNone {
			label = f.provider + " (" + f.tamper + ")"
		}
		for _, kind := range kinds {
			fmt.Printf("| %s | %s |", label, kind)
			for _, t := range firstReleaseTargets {
				fmt.Printf(" %s |", providerCell(byTarget, f, kind, t))
			}
			fmt.Println()
		}
	}
}

// providerCell is one cell of the combination matrix.
func providerCell(byTarget map[string]ProviderEvidence, f providerFixture, kind, target string) string {
	ev, ok := byTarget[target]
	if !ok {
		return "FAIL"
	}
	seen, failed := false, false
	for _, c := range ev.Combinations {
		if c.Provider != f.provider || c.Software != f.software || c.TargetKind != kind {
			continue
		}
		seen = true
		if !c.OK || c.Observed == "n-a" {
			failed = true
		}
		if (f.expect != expectInstall || !fixtureOnTarget(f, target)) && c.Observed != observedRefused {
			failed = true
		}
	}
	switch {
	case !seen && f.provider == winget.Name && !fixtureOnTarget(f, target):
		return "n-a"
	case !seen || failed:
		return "FAIL"
	default:
		return "ok"
	}
}
