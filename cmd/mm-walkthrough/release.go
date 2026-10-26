package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/plan"
)

// The S6 release gate (issues #332, #335; decisions D3, D4, D5, D6, D7, D8, D9,
// D10) drives a release archive and its extracted managed-machine binary
// through the full 15-case D10 release matrix on all five first-release targets:
//   - checksums          (all five targets)
//   - archive-layout     (all five targets)
//   - version            (all five targets)
//   - reproducible-build (all five targets)
//   - walkthrough        (all five targets)
//   - config-cycle       (all five targets)
//   - fleet-read         (all five targets)
//   - legacy-refusal     (all five targets)
//   - installer          (all five targets)
//   - upgrade            (all five targets)
//   - recovery           (all five targets)
//   - upgrade-from-bash  (darwin/arm64; n-a on Linux and Windows)
//   - private-seed       (darwin/arm64; n-a on Linux and Windows)
//   - dispatcher         (darwin/arm64; n-a on Linux and Windows)
//   - guards             (all five targets)

const (
	releaseEvidenceKind = "s6-release-gate"
	releaseMode         = "release"
)

// ReleaseArtifact names the release archive and SHA-256 digest tested by the
// gate on one target. The field order is the serialized key order.
type ReleaseArtifact struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// ReleaseEvidence is the per-target record of the S6 release gate: the shared
// InstallEvidence base plus version, artifact, observed OS version, F01
// compatibility and the 15-case D10 matrix. The field order is the serialized
// key order.
type ReleaseEvidence struct {
	InstallEvidence
	Version   string          `json:"version"`
	Artifact  ReleaseArtifact `json:"artifact"`
	OSVersion string          `json:"os_version"`
	Toolchain string          `json:"toolchain"`
	Emulated  *bool           `json:"emulated"`
	Compat    Compat          `json:"f01_compatibility"`
	Cases     []ReleaseCase   `json:"cases"`
}

// ReleaseCase is one S6 release gate case on one target. The field order is
// the serialized key order.
type ReleaseCase struct {
	Case     string `json:"case"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
}

const (
	caseReleaseChecksums         = "checksums"
	caseReleaseArchiveLayout     = "archive-layout"
	caseReleaseVersion           = "version"
	caseReleaseReproducibleBuild = "reproducible-build"
	caseReleaseWalkthrough       = "walkthrough"
	caseReleaseConfigCycle       = "config-cycle"
	caseReleaseFleetRead         = "fleet-read"
	caseReleaseLegacyRefusal     = "legacy-refusal"
	caseReleaseInstaller         = "installer"
	caseReleaseUpgrade           = "upgrade"
	caseReleaseRecovery          = "recovery"
	caseReleaseUpgradeFromBash   = "upgrade-from-bash"
	caseReleasePrivateSeed       = "private-seed"
	caseReleaseDispatcher        = "dispatcher"
	caseReleaseGuards            = "guards"
)

var releaseCaseNames = []string{
	caseReleaseChecksums,
	caseReleaseArchiveLayout,
	caseReleaseVersion,
	caseReleaseReproducibleBuild,
	caseReleaseWalkthrough,
	caseReleaseConfigCycle,
	caseReleaseFleetRead,
	caseReleaseLegacyRefusal,
	caseReleaseInstaller,
	caseReleaseUpgrade,
	caseReleaseRecovery,
	caseReleaseUpgradeFromBash,
	caseReleasePrivateSeed,
	caseReleaseDispatcher,
	caseReleaseGuards,
}

// releaseFirstReleaseReason is the D6 reason recorded for the three macOS
// compatibility cases on Linux and Windows targets.
const releaseFirstReleaseReason = "first release; no upgrade path from an earlier release exists"

// releaseHostedBrewRefusal is the refusal message when upgrade-from-bash is
// invoked outside a GitHub-hosted runner without a test prefix override.
const releaseHostedBrewRefusal = "upgrade-from-bash changes Homebrew; it runs only on GitHub-hosted runners"

// Synthetic marker strings placed inside the synthetic private-seed fixture
// files (D7). The gate's zero-hit grep proves that neither the core state
// directory nor the encoded evidence record ever carries any seed marker.
const (
	releaseSeedMarkerZSH      = "mm-release-gate-seed-marker-zshrc-718281"
	releaseSeedMarkerAuthKeys = "mm-release-gate-seed-marker-authkeys-271828"
)

// releaseSentinels are the guarded external programs that core invocations in
// the release gate must never start.
var releaseSentinels = []string{
	"ssh-keygen", "ssh-add", "ssh", "gh", "git",
	"osascript", "sudo", "security", "dscl",
	"gpg", "gpg2", "brew", "winget", "npm", "npx", "uv", "uvx", "cargo", "go", "pip", "pip3", "pipx",
}

func isMacOSOnlyReleaseCase(name string) bool {
	switch name {
	case caseReleaseUpgradeFromBash, caseReleasePrivateSeed, caseReleaseDispatcher:
		return true
	default:
		return false
	}
}

type releaseGateOptions struct {
	binPath    string
	target     string
	revision   string
	distDir    string
	version    string
	repoRoot   string
	compatPath string
}

type releaseHarness struct {
	*installHarness
	cases              []ReleaseCase
	machine            map[string]string
	outsideContainment map[string]string
	account            string
	repoRoot           string
	distDir            string
	releaseVer         string
	upgradeVer         string
	upgradeDistDir     string
	artifact           ReleaseArtifact
	osVer              string
	toolchain          string
	emulated           *bool
	compatPath         string
	compat             Compat
	captures           [][]byte
	stateForbidden     []string
	evidenceForbidden  []string

	aliceKey       fleetKeyPair
	seedFixtureKey fleetKeyPair
	hostStateKey   fleetKeyPair

	// Populated on darwin/* for upgrade-from-bash, private-seed and dispatcher.
	bashHome                string
	bashStubBin             string
	keyDir                  string
	fakeOriginBare          string
	fakeOriginWork          string
	localBinBare            string
	checkoutClone           string
	commitA                 string
	commitB                 string
	commitC                 string
	stagedOld               string
	stagedNew               string
	brewInstalled           bool
	gateTapReady            bool
	downgradeNoPlanVerified bool
}

func writeReleaseDefinitions(dir string) error {
	assets := map[string]string{
		"files/upgrade.txt":  "managed-machine release gate upgrade config\n",
		"files/recovery.txt": "managed-machine release gate recovery config\n",
	}
	for rel, body := range assets {
		if err := write(filepath.Join(dir, filepath.FromSlash(rel)), body); err != nil {
			return err
		}
	}
	defs := map[string]string{
		"mm-gate-upgrade-cfg.yaml": `kind: config
schema_version: 1
id: mm-gate-upgrade-cfg
description: Release gate upgrade configuration fixture.
categories: [walkthrough]
targets: [user]
operations:
  - id: op-upgrade-file
    provider: file
    destination: upgrade-file
    source: files/upgrade.txt
`,
		"mm-gate-recovery-cfg.yaml": `kind: config
schema_version: 1
id: mm-gate-recovery-cfg
description: Release gate recovery configuration fixture.
categories: [walkthrough]
targets: [user]
operations:
  - id: op-recovery-file
    provider: file
    destination: recovery-file
    source: files/recovery.txt
`,
	}
	for name, body := range defs {
		if err := write(filepath.Join(dir, name), body); err != nil {
			return err
		}
	}
	return nil
}

func findReleaseRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if _, err := os.Stat(filepath.Join(dir, "Formula", "managed-machine.rb")); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("could not locate managed-machine repository root")
}

func newReleaseHarness(opts releaseGateOptions) (*releaseHarness, error) {
	repoRoot := opts.repoRoot
	if repoRoot == "" {
		var err error
		repoRoot, err = findReleaseRepoRoot()
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		repoRoot, err = filepath.Abs(repoRoot)
		if err != nil {
			return nil, err
		}
	}

	var initialBinRaw []byte
	if opts.binPath != "" {
		var err error
		initialBinRaw, err = os.ReadFile(opts.binPath)
		if err != nil {
			return nil, err
		}
	}

	h, err := newInstallHarnessWith(opts.binPath, opts.target, opts.revision, initialBinRaw, installHarnessOptions{
		sentinels:   releaseSentinels,
		definitions: writeReleaseDefinitions,
		extraDirs: []string{
			"desired", "upgrade", "recovery", "keydir", "repos", "formulae",
			"staged", "bash-home", "bash-stubs", "extracted-bin", "config-cycle",
			"fleet-policy", "fleet-state", "legacy-refusal-home", "installer-case",
		},
		homeSubdir: "home dir ü 日本",
	})
	if err != nil {
		return nil, err
	}
	h.ev.Kind = releaseEvidenceKind
	h.ev.Mode = releaseMode

	account, err := fleet.DefaultUsername(runtime.GOOS)
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	aliceKey, err := generateFleetKey("alice@workstation")
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	seedFixtureKey, err := generateFleetKey("seed@fixture")
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	hostStateKey, err := generateFleetKey("host@fixture")
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	stateForbidden := []string{
		fleetPrivateKeyHeader,
		fleetPrivateKeyBody,
		fleetPrivateKeyFooter,
		releaseSeedMarkerZSH,
		releaseSeedMarkerAuthKeys,
		aliceKey.seedHex, aliceKey.seedB64,
		seedFixtureKey.seedHex, seedFixtureKey.seedB64, seedFixtureKey.base64Blob,
		hostStateKey.seedHex, hostStateKey.seedB64, hostStateKey.base64Blob,
	}
	evidenceForbidden := append([]string{}, stateForbidden...)
	evidenceForbidden = append(evidenceForbidden,
		aliceKey.base64Blob,
		h.root, filepath.ToSlash(h.root),
		h.realHome, filepath.ToSlash(h.realHome),
	)
	if h.paths.tmp != "" {
		evidenceForbidden = append(evidenceForbidden, h.paths.tmp, filepath.ToSlash(h.paths.tmp))
	}

	compat := Compat{Status: "not_applicable"}
	if strings.HasPrefix(opts.target, "darwin/") {
		compat = Compat{Status: "pending"}
	}

	rh := &releaseHarness{
		installHarness:    h,
		cases:             []ReleaseCase{},
		account:           account,
		repoRoot:          repoRoot,
		osVer:             osVersion(),
		compatPath:        opts.compatPath,
		compat:            compat,
		keyDir:            filepath.Join(h.root, "keydir"),
		bashHome:          filepath.Join(h.root, "bash-home"),
		bashStubBin:       filepath.Join(h.root, "bash-stubs"),
		stagedOld:         filepath.Join(h.root, "staged", "old"),
		stagedNew:         filepath.Join(h.root, "staged", "new"),
		aliceKey:          aliceKey,
		seedFixtureKey:    seedFixtureKey,
		hostStateKey:      hostStateKey,
		stateForbidden:    stateForbidden,
		evidenceForbidden: evidenceForbidden,
	}

	if err := rh.prepareDist(opts.distDir, opts.version, opts.binPath, initialBinRaw); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	if err := rh.writeKeyDirectory(); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	machine, err := rh.releaseMachineSnapshot()
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	rh.machine = machine

	outside, err := rh.staticHarnessSnapshot()
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	rh.outsideContainment = outside

	return rh, nil
}

func (h *releaseHarness) writeKeyDirectory() error {
	keysSub := filepath.Join(h.keyDir, "keys")
	if err := os.MkdirAll(keysSub, 0o755); err != nil {
		return err
	}
	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %q
    label: workstation
    published_at: %q
    revision: 1
`, h.aliceKey.fingerprint, h.aliceKey.pubLine, fleetPublishedAt)
	return write(filepath.Join(keysSub, "alice.yaml"), aliceYAML)
}

func (h *releaseHarness) releaseMachineSnapshot() (map[string]string, error) {
	out, err := h.machineSnapshot()
	if err != nil {
		return nil, err
	}
	sshDir := filepath.Join(h.realHome, ".ssh")
	if _, err := os.Stat(sshDir); err != nil {
		out["ssh/.ssh"] = "absent"
	} else {
		out["ssh/.ssh"] = "present"
		if err := hashTree(sshDir, "ssh/.ssh", out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (h *releaseHarness) staticHarnessSnapshot() (map[string]string, error) {
	out := map[string]string{}
	for _, rel := range []string{"defs", "sentinels", "keydir"} {
		if err := hashTree(filepath.Join(h.root, rel), rel, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// prepareDist resolves or synthesizes the local dist directory with SHA256SUMS
// and the five release archives, extracts managed-machine[.exe] from the
// target's archive into the harness, and sets h.bin and h.ev.BinarySHA256 to
// that extracted binary. For macOS upgrade-from-bash, if the repository VERSION
// is still 0.8.12 (pre-release PR), upgradeVer is set to 0.9.0 so Homebrew
// upgrades from 0.8.12 to 0.9.0 and retains the 0.8.12 keg.
func (h *releaseHarness) prepareDist(distFlag, verFlag, binFlag string, binRaw []byte) error {
	verFlag = strings.TrimPrefix(strings.TrimSpace(verFlag), "v")
	if distFlag != "" {
		absDist, err := filepath.Abs(distFlag)
		if err != nil {
			return err
		}
		ver, err := readDistVersion(absDist, h.target)
		if err != nil {
			return err
		}
		if verFlag != "" && ver != verFlag {
			return fmt.Errorf("-dist archive version (%s) does not match -version (%s)", ver, verFlag)
		}
		archiveName := targetArchiveName(ver, h.target)
		archivePath := filepath.Join(absDist, archiveName)
		archiveBytes, err := os.ReadFile(archivePath)
		if err != nil {
			return err
		}
		distBinBytes, err := extractArchiveBinaryBytes(archivePath)
		if err != nil {
			return fmt.Errorf("extract %s from -dist: %w", filepath.Base(archivePath), err)
		}
		if binFlag != "" && sum(distBinBytes) != sum(binRaw) {
			return fmt.Errorf("-dist binary digest (%s) does not match -binary digest (%s)", sum(distBinBytes)[:12], sum(binRaw)[:12])
		}
		extractedBin := filepath.Join(h.root, "extracted-bin", exe("managed-machine"))
		if err := writeExecutableAtomically(extractedBin, distBinBytes); err != nil {
			return err
		}
		h.bin = extractedBin
		h.ev.BinarySHA256 = sum(distBinBytes)
		h.distDir = absDist
		h.releaseVer = ver
		h.artifact = ReleaseArtifact{Name: archiveName, SHA256: sum(archiveBytes)}
	} else {
		ver := verFlag
		if ver == "" {
			vRaw, err := os.ReadFile(filepath.Join(h.repoRoot, "VERSION"))
			if err != nil {
				return err
			}
			ver = strings.TrimSpace(string(vRaw))
		}
		if ver == "" {
			ver = "0.9.0"
		}
		distDir := filepath.Join(h.root, "dist")
		if err := buildFullDist(h.repoRoot, ver, distDir); err != nil {
			return err
		}
		archiveName := targetArchiveName(ver, h.target)
		archivePath := filepath.Join(distDir, archiveName)
		archiveBytes, err := os.ReadFile(archivePath)
		if err != nil {
			return err
		}
		distBinBytes, err := extractArchiveBinaryBytes(archivePath)
		if err != nil {
			return err
		}
		extractedBin := filepath.Join(h.root, "extracted-bin", exe("managed-machine"))
		if err := writeExecutableAtomically(extractedBin, distBinBytes); err != nil {
			return err
		}
		h.bin = extractedBin
		h.ev.BinarySHA256 = sum(distBinBytes)
		h.distDir = distDir
		h.releaseVer = ver
		h.artifact = ReleaseArtifact{Name: archiveName, SHA256: sum(archiveBytes)}
	}

	h.upgradeVer = h.releaseVer
	h.upgradeDistDir = h.distDir
	if strings.HasPrefix(h.target, "darwin/") && h.releaseVer == "0.8.12" {
		h.upgradeVer = "0.9.0"
		upDist := filepath.Join(h.root, "dist-upgrade")
		if err := buildTargetDist(h.repoRoot, h.upgradeVer, "darwin/arm64", upDist); err != nil {
			return err
		}
		h.upgradeDistDir = upDist
	}
	return nil
}

func targetArchiveName(version, target string) string {
	goos := targetOS(target)
	goarch := targetArch(target)
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("managed-machine_%s_%s_%s%s", version, goos, goarch, ext)
}

func readDistVersion(distDir, target string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(distDir, "SHA256SUMS"))
	if err != nil {
		return "", fmt.Errorf("read SHA256SUMS: %w", err)
	}
	goos, goarch := targetOS(target), targetArch(target)
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	suffix := fmt.Sprintf("_%s_%s%s", goos, goarch, ext)
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		sumHex, name, ok := strings.Cut(strings.TrimSpace(line), "  ")
		if !ok || len(sumHex) != 64 {
			continue
		}
		if strings.HasPrefix(name, "managed-machine_") && strings.HasSuffix(name, suffix) {
			ver := strings.TrimSuffix(strings.TrimPrefix(name, "managed-machine_"), suffix)
			archiveBytes, err := os.ReadFile(filepath.Join(distDir, name))
			if err != nil {
				return "", err
			}
			if sum(archiveBytes) != sumHex {
				return "", fmt.Errorf("SHA256SUMS digest mismatch for %s", name)
			}
			return ver, nil
		}
	}
	return "", fmt.Errorf("no %s archive found in %s/SHA256SUMS", target, distDir)
}

// buildFullDist compiles cmd/managed-machine for all five firstReleaseTargets
// with -X main.version=ver and writes the five archives plus sorted SHA256SUMS.
func buildFullDist(repoRoot, ver, distDir string) error {
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return err
	}
	licRaw, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		return err
	}
	licBytes := bytes.ReplaceAll(licRaw, []byte("\r\n"), []byte("\n"))

	type entry struct {
		name string
		sha  string
	}
	var entries []entry
	for _, tg := range firstReleaseTargets {
		goos, goarch := targetOS(tg), targetArch(tg)
		binName := "managed-machine"
		if goos == "windows" {
			binName = "managed-machine.exe"
		}
		tmpBin := filepath.Join(distDir, fmt.Sprintf(".tmp-%s-%s-%s", goos, goarch, binName))
		if err := compileCoreBinary(repoRoot, ver, goos, goarch, tmpBin); err != nil {
			os.Remove(tmpBin)
			return err
		}
		binBytes, err := os.ReadFile(tmpBin)
		os.Remove(tmpBin)
		if err != nil {
			return err
		}
		archiveName := targetArchiveName(ver, tg)
		var archiveBytes []byte
		if goos == "windows" {
			archiveBytes, err = packReleaseZip(binName, binBytes, licBytes)
		} else {
			archiveBytes, err = packReleaseTarGz(binName, binBytes, licBytes)
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(distDir, archiveName), archiveBytes, 0o644); err != nil {
			return err
		}
		entries = append(entries, entry{name: archiveName, sha: sum(archiveBytes)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })
	var sums strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sums, "%s  %s\n", e.sha, e.name)
	}
	return os.WriteFile(filepath.Join(distDir, "SHA256SUMS"), []byte(sums.String()), 0o644)
}

// buildTargetDist compiles cmd/managed-machine for target with -X main.version=ver
// and packages it into distDir alongside SHA256SUMS.
func buildTargetDist(repoRoot, ver, target, distDir string) error {
	if err := os.MkdirAll(distDir, 0o755); err != nil {
		return err
	}
	goos, goarch := targetOS(target), targetArch(target)
	binName := "managed-machine"
	if goos == "windows" {
		binName = "managed-machine.exe"
	}
	tmpBin := filepath.Join(distDir, ".tmp-"+binName)
	defer os.Remove(tmpBin)
	if err := compileCoreBinary(repoRoot, ver, goos, goarch, tmpBin); err != nil {
		return err
	}
	binBytes, err := os.ReadFile(tmpBin)
	if err != nil {
		return err
	}
	licRaw, err := os.ReadFile(filepath.Join(repoRoot, "LICENSE"))
	if err != nil {
		return err
	}
	licBytes := bytes.ReplaceAll(licRaw, []byte("\r\n"), []byte("\n"))

	archiveName := targetArchiveName(ver, target)
	var archiveBytes []byte
	if goos == "windows" {
		archiveBytes, err = packReleaseZip(binName, binBytes, licBytes)
	} else {
		archiveBytes, err = packReleaseTarGz(binName, binBytes, licBytes)
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(distDir, archiveName), archiveBytes, 0o644); err != nil {
		return err
	}
	sumsLine := fmt.Sprintf("%s  %s\n", sum(archiveBytes), archiveName)
	return os.WriteFile(filepath.Join(distDir, "SHA256SUMS"), []byte(sumsLine), 0o644)
}

func compileCoreBinary(repoRoot, ver, goos, goarch, outPath string) error {
	ldflags := "-s -w -X main.version=" + ver
	cmd := exec.Command("go", "build", "-buildvcs=false", "-ldflags", ldflags, "-o", outPath, "./cmd/managed-machine")
	cmd.Dir = repoRoot
	env := make([]string, 0, len(os.Environ())+5)
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "GOOS", "GOARCH", "GOARM", "GOAMD64", "GOARM64", "CGO_ENABLED", "GOFLAGS", "GOTOOLCHAIN":
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"GOOS="+goos,
		"GOARCH="+goarch,
		"CGO_ENABLED=0",
		"GOFLAGS=-trimpath",
		"GOTOOLCHAIN=local",
	)
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build (%s/%s %s): %v: %s", goos, goarch, ver, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

var releaseArchiveEpoch = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

func packReleaseTarGz(binName string, binBytes, licBytes []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	gz.Name = ""
	gz.Comment = ""
	gz.Extra = nil
	gz.ModTime = time.Time{}
	gz.OS = 255
	tw := tar.NewWriter(gz)
	type item struct {
		name string
		mode int64
		data []byte
	}
	items := []item{
		{name: "LICENSE", mode: 0o644, data: licBytes},
		{name: binName, mode: 0o755, data: binBytes},
	}
	for _, it := range items {
		hdr := &tar.Header{
			Typeflag: tar.TypeReg,
			Name:     it.name,
			Size:     int64(len(it.data)),
			Mode:     it.mode,
			ModTime:  releaseArchiveEpoch,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := tw.Write(it.data); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func packReleaseZip(binName string, binBytes, licBytes []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, flate.BestCompression)
	})
	type item struct {
		name string
		mode fs.FileMode
		data []byte
	}
	items := []item{
		{name: "LICENSE", mode: 0o644, data: licBytes},
		{name: binName, mode: 0o755, data: binBytes},
	}
	for _, it := range items {
		fh := &zip.FileHeader{
			Name:   it.name,
			Method: zip.Deflate,
		}
		fh.SetModTime(releaseArchiveEpoch)
		fh.Modified = time.Time{}
		fh.SetMode(it.mode)
		fh.Extra = nil
		w, err := zw.CreateHeader(fh)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(it.data); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func extractArchiveBinaryBytes(archivePath string) ([]byte, error) {
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(archivePath, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			return nil, err
		}
		for _, f := range zr.File {
			if f.Name == "managed-machine.exe" || f.Name == "managed-machine" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(rc)
			}
		}
		return nil, fmt.Errorf("binary not found in %s", filepath.Base(archivePath))
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if hdr.Name == "managed-machine" || hdr.Name == "managed-machine.exe" {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("binary not found in %s", filepath.Base(archivePath))
}

// releaseCmd runs the S6 release gate for one target and writes its evidence
// record.
func releaseCmd(args []string) error {
	fl := flag.NewFlagSet("release", flag.ContinueOnError)
	bin := fl.String("binary", "", "optional managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	dist := fl.String("dist", "", "release dist directory containing SHA256SUMS and archives")
	ver := fl.String("version", "", "release version X.Y.Z under test")
	repo := fl.String("repo", "", "optional repository root checkout path")
	compat := fl.String("compat", "", "optional F01 compatibility harness output file (macOS)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *tgt == "" || *out == "" || (*dist == "" && *bin == "") {
		return errors.New("release needs -target, -evidence and -dist (or -binary)")
	}
	var absBin string
	if *bin != "" {
		var err error
		absBin, err = filepath.Abs(*bin)
		if err != nil {
			return err
		}
	}
	h, b, err := runReleaseGateWith(releaseGateOptions{
		binPath:    absBin,
		target:     *tgt,
		revision:   *rev,
		distDir:    *dist,
		version:    *ver,
		repoRoot:   *repo,
		compatPath: *compat,
	})
	if err != nil {
		return err
	}
	defer os.RemoveAll(h.root)
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d steps, %d cases, result %s\n", *tgt, len(h.ev.Steps), len(h.cases), h.ev.Result)
	if h.ev.Result != "pass" {
		for _, f := range h.ev.Failures {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		return errors.New("the release gate failed")
	}
	return nil
}

// runReleaseGate builds the harness for one target, runs every S6 release case
// and returns the harness and the scrubbed evidence JSON bytes.
func runReleaseGate(bin, target, revision, distFlag string) (*releaseHarness, []byte, error) {
	return runReleaseGateWith(releaseGateOptions{
		binPath:  bin,
		target:   target,
		revision: revision,
		distDir:  distFlag,
	})
}

func runReleaseGateWith(opts releaseGateOptions) (*releaseHarness, []byte, error) {
	h, err := newReleaseHarness(opts)
	if err != nil {
		return nil, nil, err
	}
	defer h.cleanupBrew()
	h.run()
	ev := ReleaseEvidence{
		InstallEvidence: *h.ev,
		Version:         h.releaseVer,
		Artifact:        h.artifact,
		OSVersion:       h.osVer,
		Toolchain:       h.toolchain,
		Emulated:        h.emulated,
		Compat:          h.compat,
		Cases:           h.cases,
	}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return h, nil, err
	}
	b = h.scrubReleaseEvidence(b)
	if leak := h.grepEvidenceBytes(b); leak != "" {
		h.step("evidence zero-hits grep", leak, false)
		h.setGuardsFailure(leak)
		h.finish()
		ev = ReleaseEvidence{
			InstallEvidence: *h.ev,
			Version:         h.releaseVer,
			Artifact:        h.artifact,
			OSVersion:       h.osVer,
			Toolchain:       h.toolchain,
			Emulated:        h.emulated,
			Compat:          h.compat,
			Cases:           h.cases,
		}
		b, err = json.MarshalIndent(ev, "", "  ")
		if err != nil {
			return h, nil, err
		}
		b = h.scrubReleaseEvidence(b)
	}
	return h, b, nil
}

var accountWordReCache = map[string]*regexp.Regexp{}

func (h *releaseHarness) scrubAccount(s string) string {
	if h.account == "" {
		return s
	}
	re, ok := accountWordReCache[h.account]
	if !ok {
		re = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(h.account) + `($|[^A-Za-z0-9_.-])`)
		accountWordReCache[h.account] = re
	}
	// Apply twice in case of adjacent occurrences sharing a separator.
	s = re.ReplaceAllString(s, "${1}<account>${2}")
	return re.ReplaceAllString(s, "${1}<account>${2}")
}

func (h *releaseHarness) cleanText(s string) string {
	return h.scrubAccount(h.paths.clean(s))
}

func (h *releaseHarness) scrubReleaseEvidence(b []byte) []byte {
	b = h.paths.scrub(b)
	if h.account != "" {
		b = []byte(h.scrubAccount(string(b)))
	}
	return append(b, '\n')
}

func (h *releaseHarness) grepEvidenceBytes(b []byte) string {
	if hit := grepFleetBytes(b, "evidence", h.evidenceForbidden); hit != "" {
		return hit
	}
	if h.account != "" {
		re := accountWordReCache[h.account]
		if re == nil {
			re = regexp.MustCompile(`(^|[^A-Za-z0-9_.-])` + regexp.QuoteMeta(h.account) + `($|[^A-Za-z0-9_.-])`)
		}
		if re.Match(b) {
			return "forbidden account name leaked in evidence"
		}
	}
	return ""
}

func (h *releaseHarness) recordCase(name, expected, observed string, ok bool, detail string) {
	clean := h.cleanText(detail)
	h.cases = append(h.cases, ReleaseCase{
		Case:     name,
		Expected: expected,
		Observed: observed,
		OK:       ok,
		Detail:   clean,
	})
	h.step("case "+name, clean, ok)
}

func (h *releaseHarness) setGuardsFailure(detail string) {
	clean := h.cleanText(detail)
	for i := range h.cases {
		if h.cases[i].Case == caseReleaseGuards {
			h.cases[i].Observed = observedFailed
			h.cases[i].OK = false
			h.cases[i].Detail = clean
			return
		}
	}
}

func (h *releaseHarness) orderCases() {
	order := make(map[string]int, len(releaseCaseNames))
	for i, name := range releaseCaseNames {
		order[name] = i
	}
	sort.SliceStable(h.cases, func(i, j int) bool {
		oi, oki := order[h.cases[i].Case]
		oj, okj := order[h.cases[j].Case]
		if oki && okj {
			return oi < oj
		}
		return i < j
	})
}

func (h *releaseHarness) run() {
	if !h.probe() {
		h.finish()
		return
	}
	h.runChecksums()
	h.runArchiveLayout()
	h.runVersion()
	h.runReproducibleBuild()
	h.runWalkthrough()
	h.runConfigCycle()
	h.runFleetRead()
	h.runLegacyRefusal()
	h.runInstaller()
	h.runUpgrade()
	h.runUpgradeFromBash()
	h.runPrivateSeed()
	h.runDispatcher()
	h.runRecovery()
	h.runGuards()
	h.orderCases()
	h.finish()
}

// runChecksums proves `checksums` (D10): SHA256SUMS names all five archives
// for the five targets; the target's archive digest matches; `mm-release verify
// -dist <dist>` succeeds; and a 1-byte-flipped copy fails `mm-release verify`.
func (h *releaseHarness) runChecksums() {
	const name = caseReleaseChecksums
	rawSums, err := os.ReadFile(filepath.Join(h.distDir, "SHA256SUMS"))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "read SHA256SUMS: "+err.Error())
		return
	}
	lines := strings.Split(strings.TrimSpace(string(rawSums)), "\n")
	if len(lines) != len(firstReleaseTargets) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("SHA256SUMS has %d lines, want %d", len(lines), len(firstReleaseTargets)))
		return
	}
	byName := map[string]string{}
	for _, line := range lines {
		sha, fname, ok := strings.Cut(line, "  ")
		if !ok || len(sha) != 64 {
			h.recordCase(name, "ok", observedFailed, false, "malformed SHA256SUMS line: "+line)
			return
		}
		byName[fname] = sha
	}
	for _, tg := range firstReleaseTargets {
		wantName := targetArchiveName(h.releaseVer, tg)
		if _, ok := byName[wantName]; !ok {
			h.recordCase(name, "ok", observedFailed, false, "SHA256SUMS missing "+wantName)
			return
		}
	}
	wantTargetName := targetArchiveName(h.releaseVer, h.target)
	targetBytes, err := os.ReadFile(filepath.Join(h.distDir, wantTargetName))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	actualTargetSHA := sum(targetBytes)
	if actualTargetSHA != byName[wantTargetName] || actualTargetSHA != h.artifact.SHA256 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("target archive %s digest %s != SHA256SUMS %s", wantTargetName, actualTargetSHA, byName[wantTargetName]))
		return
	}

	// Build mm-release once and run `mm-release verify -dist <distDir> -version <ver>`.
	mmReleaseBin := filepath.Join(h.root, exe("mm-release"))
	buildCmd := exec.Command("go", "build", "-o", mmReleaseBin, "./cmd/mm-release")
	buildCmd.Dir = h.repoRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("build mm-release: %v (%s)", err, firstLineOf(out)))
		return
	}
	vCmd := exec.Command(mmReleaseBin, "verify", "-dist", h.distDir)
	vCmd.Dir = h.repoRoot
	if out, err := vCmd.CombinedOutput(); err != nil {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("mm-release verify failed: %v (%s)", err, firstLineOf(out)))
		return
	}

	// Copy distDir, flip 1 byte in the target archive, and verify `mm-release verify` fails.
	tamperedDir := filepath.Join(h.root, "checksums-tampered")
	if err := copyTree(h.distDir, tamperedDir); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "copy dist for tamper check: "+err.Error())
		return
	}
	defer os.RemoveAll(tamperedDir)
	flipped := append([]byte{}, targetBytes...)
	flipped[len(flipped)/2] ^= 0xff
	if err := os.WriteFile(filepath.Join(tamperedDir, wantTargetName), flipped, 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	badCmd := exec.Command(mmReleaseBin, "verify", "-dist", tamperedDir)
	badCmd.Dir = h.repoRoot
	badOut, badErr := badCmd.CombinedOutput()
	if badErr == nil || !strings.Contains(string(badOut), wantTargetName) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("mm-release verify accepted 1-byte-flipped %s (output %q)", wantTargetName, firstLineOf(badOut)))
		return
	}

	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("SHA256SUMS names all 5 archives, %s matches %s, mm-release verify succeeded, and 1-byte-flipped archive failed verification",
			wantTargetName, actualTargetSHA[:12]))
}

// runArchiveLayout proves `archive-layout` (D10): the target archive has the
// exact sorted root entries (LICENSE 0644, managed-machine[.exe] 0755), uid/gid
// 0, empty uname/gname, constant mtime (1980-01-01T00:00:00Z), gzip header
// empty/OS=255 or zip deflate with no extra fields, and binary digest equal to
// binary_sha256.
func (h *releaseHarness) runArchiveLayout() {
	const name = caseReleaseArchiveLayout
	archiveName := targetArchiveName(h.releaseVer, h.target)
	archivePath := filepath.Join(h.distDir, archiveName)
	raw, err := os.ReadFile(archivePath)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	licRaw, err := os.ReadFile(filepath.Join(h.repoRoot, "LICENSE"))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	wantLic := bytes.ReplaceAll(licRaw, []byte("\r\n"), []byte("\n"))

	if strings.HasSuffix(archiveName, ".zip") {
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			h.recordCase(name, "ok", observedFailed, false, "open zip: "+err.Error())
			return
		}
		if len(zr.File) != 2 || zr.File[0].Name != "LICENSE" || zr.File[1].Name != "managed-machine.exe" {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("unexpected zip entries: %v", len(zr.File)))
			return
		}
		for i, f := range zr.File {
			if strings.ContainsAny(f.Name, `/\`) {
				h.recordCase(name, "ok", observedFailed, false, "zip entry has directory prefix: "+f.Name)
				return
			}
			if f.Method != zip.Deflate {
				h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("zip entry %s method = %d, want Deflate", f.Name, f.Method))
				return
			}
			if len(f.Extra) != 0 || f.Comment != "" {
				h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("zip entry %s has extra fields (%d bytes) or comment", f.Name, len(f.Extra)))
				return
			}
			// DOS date 1980-01-01 is 0x0021 (year 0 since 1980, month 1, day 1), time 00:00:00 is 0.
			if f.ModifiedDate != 0x0021 || f.ModifiedTime != 0 {
				h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("zip entry %s DOS timestamp = (%#x, %#x), want (0x21, 0x0)", f.Name, f.ModifiedDate, f.ModifiedTime))
				return
			}
			wantMode := fs.FileMode(0o644)
			if i == 1 {
				wantMode = 0o755
			}
			if f.Mode().Perm() != wantMode {
				h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("zip entry %s mode = %04o, want %04o", f.Name, f.Mode().Perm(), wantMode))
				return
			}
			rc, err := f.Open()
			if err != nil {
				h.recordCase(name, "ok", observedFailed, false, err.Error())
				return
			}
			payload, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				h.recordCase(name, "ok", observedFailed, false, err.Error())
				return
			}
			if i == 0 && !bytes.Equal(payload, wantLic) {
				h.recordCase(name, "ok", observedFailed, false, "zip LICENSE payload mismatch")
				return
			}
			if i == 1 && sum(payload) != h.ev.BinarySHA256 {
				h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("zip binary sha256 %s != %s", sum(payload), h.ev.BinarySHA256))
				return
			}
		}
		h.recordCase(name, "ok", "ok", true,
			fmt.Sprintf("%s verified: sorted root entries [LICENSE 0644, managed-machine.exe 0755], deflate with empty extra fields, epoch 1980-01-01T00:00:00Z, binary sha256 %s",
				archiveName, h.ev.BinarySHA256[:12]))
		return
	}

	if len(raw) < 10 || raw[0] != 0x1f || raw[1] != 0x8b || raw[2] != 8 || raw[3] != 0 ||
		raw[4] != 0 || raw[5] != 0 || raw[6] != 0 || raw[7] != 0 || raw[9] != 255 {
		h.recordCase(name, "ok", observedFailed, false, "gzip header does not have empty flags/mtime and OS=255")
		return
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "open gzip: "+err.Error())
		return
	}
	defer gz.Close()
	if gz.Name != "" || gz.Comment != "" || len(gz.Extra) != 0 || gz.OS != 255 {
		h.recordCase(name, "ok", observedFailed, false, "gzip reader header fields not empty/OS=255")
		return
	}
	tr := tar.NewReader(gz)
	type wantTarEntry struct {
		name string
		mode int64
	}
	wants := []wantTarEntry{
		{name: "LICENSE", mode: 0o644},
		{name: "managed-machine", mode: 0o755},
	}
	for i, w := range wants {
		hdr, err := tr.Next()
		if err != nil {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("tar entry %d (%s): %v", i, w.name, err))
			return
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Name != w.name || strings.Contains(hdr.Name, "/") {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("tar entry %d = %q (type %v), want %q", i, hdr.Name, hdr.Typeflag, w.name))
			return
		}
		if hdr.Mode != w.mode || hdr.Uid != 0 || hdr.Gid != 0 || hdr.Uname != "" || hdr.Gname != "" {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("tar entry %s metadata mode=%04o uid=%d gid=%d uname=%q gname=%q",
				hdr.Name, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Uname, hdr.Gname))
			return
		}
		if !hdr.ModTime.UTC().Equal(releaseArchiveEpoch) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("tar entry %s mtime = %s, want %s",
				hdr.Name, hdr.ModTime.UTC().Format(time.RFC3339), releaseArchiveEpoch.Format(time.RFC3339)))
			return
		}
		payload, err := io.ReadAll(tr)
		if err != nil {
			h.recordCase(name, "ok", observedFailed, false, err.Error())
			return
		}
		if i == 0 && !bytes.Equal(payload, wantLic) {
			h.recordCase(name, "ok", observedFailed, false, "tar LICENSE payload mismatch")
			return
		}
		if i == 1 && sum(payload) != h.ev.BinarySHA256 {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("tar binary sha256 %s != %s", sum(payload), h.ev.BinarySHA256))
			return
		}
	}
	if _, err := tr.Next(); err != io.EOF {
		h.recordCase(name, "ok", observedFailed, false, "tar archive has unexpected extra entries")
		return
	}
	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("%s verified: sorted root entries [LICENSE 0644, managed-machine 0755], uid/gid 0, empty uname/gname, mtime 1980-01-01T00:00:00Z, gzip OS=255, binary sha256 %s",
			archiveName, h.ev.BinarySHA256[:12]))
}

// runVersion proves `version` (D10): `--version` prints `<X.Y.Z>\n` matching
// VERSION; `version` (default, `--json`, `--human`) prints `version, target,
// toolchain`; `platform` (default, `--json`, `--human`) equals the target with
// `emulated: false`.
func (h *releaseHarness) runVersion() {
	const name = caseReleaseVersion
	vDiskRaw, err := os.ReadFile(filepath.Join(h.repoRoot, "VERSION"))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "read VERSION: "+err.Error())
		return
	}
	vDisk := strings.TrimSpace(string(vDiskRaw))
	if vDisk == "" {
		h.recordCase(name, "ok", observedFailed, false, "VERSION file is empty")
		return
	}
	if vDisk != h.releaseVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("VERSION is %q, the archive under test is %q", vDisk, h.releaseVer))
		return
	}
	wantToolchain, err := pinnedToolchain(h.repoRoot)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	code, vOut, vErr := h.execBinaryEnv(h.bin, nil, "--version")
	if code != 0 || len(vErr) != 0 || string(vOut) != h.releaseVer+"\n" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("--version exited %d: got %q (stderr %q), want %q",
			code, string(vOut), firstLineOf(vErr), h.releaseVer+"\n"))
		return
	}
	for _, badFlag := range []string{"--json", "--human"} {
		bCode, bOut, bErr := h.execBinaryEnv(h.bin, nil, "--version", badFlag)
		if bCode != 2 || len(bOut) != 0 || !strings.Contains(string(bErr), diag.Usage) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("--version %s exited %d (stderr %q), want 2 with %s",
				badFlag, bCode, firstLineOf(bErr), diag.Usage))
			return
		}
	}

	// `version` in default, --json, and --human modes.
	code, defOut, defErr := h.execBinaryEnv(h.bin, nil, "version")
	if code != 0 || len(defErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("version exited %d: %s", code, firstLineOf(defErr, defOut)))
		return
	}
	keys, err := objectKeys(bytes.TrimSpace(defOut))
	if err != nil || !sameStrings(keys, []string{"version", "target", "toolchain"}) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("version keys = %v (%v), want [version target toolchain]", keys, err))
		return
	}
	recs, err := parseJSONL(defOut)
	if err != nil || len(recs) != 1 || str(recs[0], "version") != h.releaseVer || str(recs[0], "target") != h.target || str(recs[0], "toolchain") != wantToolchain {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("version record = %v (%v)", recs, err))
		return
	}

	jCode, jOut, jErr := h.execBinaryEnv(h.bin, nil, "version", "--json")
	if jCode != 0 || len(jErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("version --json exited %d: %s", jCode, firstLineOf(jErr)))
		return
	}
	jKeys, err := objectKeys(bytes.TrimSpace(jOut))
	if err != nil || !sameStrings(jKeys, []string{"version", "target", "toolchain"}) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("version --json keys = %v (%v)", jKeys, err))
		return
	}
	if jRecs, err := parseDocument(jOut); err != nil || !sameRecords(recs, jRecs) {
		h.recordCase(name, "ok", observedFailed, false, "version --json differs from default mode")
		return
	}

	hCode, hOut, hErr := h.execBinaryEnv(h.bin, nil, "version", "--human")
	if hCode != 0 || len(hErr) != 0 || json.Valid(bytes.TrimSpace(hOut)) ||
		!strings.Contains(string(hOut), h.releaseVer) || !strings.Contains(string(hOut), h.target) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("version --human exited %d: %q", hCode, firstLineOf(hOut)))
		return
	}

	// `platform` in default, --json, and --human modes.
	pCode, pOut, pErr := h.execBinaryEnv(h.bin, nil, "platform")
	if pCode != 0 || len(pErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("platform exited %d: %s", pCode, firstLineOf(pErr)))
		return
	}
	pRecs, err := parseJSONL(pOut)
	if err != nil || len(pRecs) != 1 || str(pRecs[0], "platform") != h.target || pRecs[0]["emulated"] != false {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("platform record = %v (%v)", pRecs, err))
		return
	}
	pjCode, pjOut, pjErr := h.execBinaryEnv(h.bin, nil, "platform", "--json")
	if pjCode != 0 || len(pjErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("platform --json exited %d: %s", pjCode, firstLineOf(pjErr)))
		return
	}
	pjRecs, err := parseDocument(pjOut)
	if err != nil || !sameRecords(pRecs, pjRecs) {
		h.recordCase(name, "ok", observedFailed, false, "platform --json differs from default mode")
		return
	}
	phCode, phOut, phErr := h.execBinaryEnv(h.bin, nil, "platform", "--human")
	if phCode != 0 || len(phErr) != 0 || json.Valid(bytes.TrimSpace(phOut)) || !strings.Contains(string(phOut), h.target) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("platform --human exited %d: %q", phCode, firstLineOf(phOut)))
		return
	}
	h.toolchain = wantToolchain
	native := false
	h.emulated = &native

	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("--version printed %s (VERSION %s); version and platform verified across default, --json and --human with emulated=false",
			h.releaseVer, vDisk))
}

// runReproducibleBuild proves `reproducible-build` (D3, D10): running the D3
// `go build` command on the runner produces a binary whose SHA-256 equals the
// archive's binary_sha256.
func (h *releaseHarness) runReproducibleBuild() {
	const name = caseReleaseReproducibleBuild
	rebuiltPath := filepath.Join(h.root, exe("rebuilt-managed-machine"))
	defer os.Remove(rebuiltPath)
	if err := compileCoreBinary(h.repoRoot, h.releaseVer, targetOS(h.target), targetArch(h.target), rebuiltPath); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	rebuiltBytes, err := os.ReadFile(rebuiltPath)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	rebuiltSHA := sum(rebuiltBytes)
	if rebuiltSHA != h.ev.BinarySHA256 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("rebuilt binary sha256 %s != archive binary_sha256 %s", rebuiltSHA, h.ev.BinarySHA256))
		return
	}
	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("D3 go build reproduced binary_sha256 %s byte-for-byte", rebuiltSHA[:12]))
}

// runWalkthrough proves `walkthrough` (F08, D10): runs the F08 steps
// (`software --available`, `skill` listing, content and search, continuation,
// help precedence, three presentation modes, rejections and guards) against the
// extracted archive binary.
func (h *releaseHarness) runWalkthrough() {
	const name = caseReleaseWalkthrough
	work := filepath.Join(h.root, "f08-walkthrough")
	if err := os.MkdirAll(work, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	wh := &harness{
		bin:         h.bin,
		sentinelLog: filepath.Join(work, "guard", "sentinels.log"),
		marker:      filepath.Join(work, "guard", "extension-ran"),
		l:           newLayout(filepath.Join(work, "mm walkthrough ü")),
		ev: &Evidence{
			SchemaVersion:  1,
			Story:          "F08",
			SourceRevision: h.ev.SourceRevision,
			BinarySHA256:   h.ev.BinarySHA256,
			Target:         h.target,
			Runtime:        runtime.GOOS + "/" + runtime.GOARCH,
			OSVersion:      h.osVer,
			Compat:         Compat{Status: "pass"},
			Failures:       []string{},
		},
	}
	sentinelDir := filepath.Join(work, "guard", "bin")
	if err := installSentinels(sentinelDir); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	wh.ev.Guards.Sentinels = len(sentinels)
	if err := wh.l.build(wh.marker); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	wh.env = wh.childEnv(sentinelDir)
	before, err := snapshot(wh.l.root)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	wh.ev.Guards.HarnessEntries = len(before)
	wh.walk()
	after, err := snapshot(wh.l.root)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	wh.guards(before, after)
	wh.finish()
	if wh.ev.Result != "pass" || len(wh.ev.Failures) > 0 {
		h.recordCase(name, "ok", observedFailed, false, strings.Join(wh.ev.Failures, "; "))
		return
	}
	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("F08 walkthrough passed (%d steps, %d sentinels guarded, 0 filesystem changes)", len(wh.ev.Steps), wh.ev.Guards.Sentinels))
}

// runConfigCycle proves `config-cycle` (D10): AC-T-04 with a fixture
// definition; `--check` exits 0 when in sync, then 3 (MM_CONFIG_DRIFT_FOUND)
// after an external edit.
func (h *releaseHarness) runConfigCycle() {
	const name = caseReleaseConfigCycle
	cfgDir := filepath.Join(h.root, "config-cycle")
	stateDir := filepath.Join(cfgDir, "state")
	desiredFile := filepath.Join(cfgDir, "desired.yaml")
	destFile := filepath.Join(cfgDir, "dest", "cycle.txt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	desiredYAML := fmt.Sprintf("target: user\nconfig:\n  mm-gate-upgrade-cfg:\n    destinations:\n      upgrade-file: %q\n",
		filepath.ToSlash(destFile))
	if err := write(desiredFile, desiredYAML); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	cfgEnv := []string{
		"MANAGED_MACHINE_STATE_DIR=" + stateDir,
		config.DesiredEnv + "=" + desiredFile,
		"MANAGED_MACHINE_SOURCE_DIR=" + h.defs,
	}
	if problem := h.runConfigACT04WithBinary(h.bin, cfgEnv, stateDir, "mm-gate-upgrade-cfg", destFile); problem != "" {
		h.recordCase(name, "ok", observedFailed, false, problem)
		return
	}
	if err := os.WriteFile(destFile, []byte("external drift edit\n"), 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, _, errOut := h.execBinaryEnv(h.bin, cfgEnv, "config", "mm-gate-upgrade-cfg", "--check", "--user")
	if code != 3 || !strings.Contains(string(errOut), diag.ConfigDriftFound) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("config --check after external edit exited %d (stderr %q), want 3 with %s",
			code, firstLineOf(errOut), diag.ConfigDriftFound))
		return
	}
	h.recordCase(name, "ok", "ok", true,
		"AC-T-04 5-step cycle passed, --check exited 0 in sync and exited 3 with MM_CONFIG_DRIFT_FOUND after external edit")
}

// runFleetRead proves `fleet-read` (D10): `fleet --keys`, `fleet --access` and
// `status --section fleet` against a fixture policy tree.
func (h *releaseHarness) runFleetRead() {
	const name = caseReleaseFleetRead
	policyDir := filepath.Join(h.root, "fleet-policy")
	stateDir := filepath.Join(h.root, "fleet-state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	kdYAML := fmt.Sprintf("schema_version: 1\nkind: key-directory-source\npath: %q\n", filepath.ToSlash(h.keyDir))
	if err := write(filepath.Join(policyDir, filepath.FromSlash(policy.KeyDirectoryRelPath)), kdYAML); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	groupYAML := fmt.Sprintf(`schema_version: 1
kind: access-group
id: ops
description: Release gate fleet-read fixture group.
maintained_by:
  - alice
accounts:
  - %s
approved_fingerprints:
  - %s
`, h.account, h.aliceKey.fingerprint)
	if err := write(filepath.Join(policyDir, filepath.FromSlash(policy.AccessGroupsDir), "ops.yaml"), groupYAML); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	fEnv := []string{
		"MANAGED_MACHINE_POLICY_DIR=" + policyDir,
		"MANAGED_MACHINE_STATE_DIR=" + stateDir,
	}

	// 1. fleet --keys (default, --json, --human).
	code, kOut, kErr := h.execBinaryEnv(h.bin, fEnv, "fleet", "--keys")
	if code != 0 || len(kErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --keys exited %d: %s", code, firstLineOf(kErr, kOut)))
		return
	}
	kRecs, err := parseJSONL(kOut)
	if err != nil || len(kRecs) != 2 || !strings.HasPrefix(str(kRecs[0], "directory"), "file:") || str(kRecs[1], "owner") != "alice" || str(kRecs[1], "fingerprint") != h.aliceKey.fingerprint {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --keys records = %v (%v)", kRecs, err))
		return
	}
	if jCode, jOut, jErr := h.execBinaryEnv(h.bin, fEnv, "fleet", "--keys", "--json"); jCode != 0 || len(jErr) != 0 || !json.Valid(bytes.TrimSpace(jOut)) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --keys --json exited %d", jCode))
		return
	}
	if hCode, hOut, hErr := h.execBinaryEnv(h.bin, fEnv, "fleet", "--keys", "--human"); hCode != 0 || len(hErr) != 0 || !strings.Contains(string(hOut), h.aliceKey.fingerprint) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --keys --human exited %d", hCode))
		return
	}

	// 2. fleet --access (default, --json, --human).
	code, aOut, aErr := h.execBinaryEnv(h.bin, fEnv, "fleet", "--access")
	if code != 0 || len(aErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --access exited %d: %s", code, firstLineOf(aErr, aOut)))
		return
	}
	aRecs, err := parseJSONL(aOut)
	if err != nil || len(aRecs) != 1 || str(aRecs[0], "group") != "ops" || str(aRecs[0], "owner") != "alice" || str(aRecs[0], "state") != "authorized" || str(aRecs[0], "fingerprint") != h.aliceKey.fingerprint {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --access records = %v (%v)", aRecs, err))
		return
	}
	if jCode, jOut, jErr := h.execBinaryEnv(h.bin, fEnv, "fleet", "--access", "--json"); jCode != 0 || len(jErr) != 0 || !json.Valid(bytes.TrimSpace(jOut)) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --access --json exited %d", jCode))
		return
	}
	if hCode, hOut, hErr := h.execBinaryEnv(h.bin, fEnv, "fleet", "--access", "--human"); hCode != 0 || len(hErr) != 0 || !strings.Contains(string(hOut), h.aliceKey.fingerprint) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fleet --access --human exited %d", hCode))
		return
	}

	// 3. status --section fleet (default, --json, --human).
	code, sOut, sErr := h.execBinaryEnv(h.bin, fEnv, "status", "--section", "fleet")
	if code != 0 || len(sErr) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("status --section fleet exited %d: %s", code, firstLineOf(sErr, sOut)))
		return
	}
	sRecs, err := parseJSONL(sOut)
	if err != nil || len(sRecs) == 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("status --section fleet records = %v (%v)", sRecs, err))
		return
	}
	hasPolicy, hasKD := false, false
	for _, r := range sRecs {
		if str(r, "resource") == "policy" && str(r, "state") == "present" {
			hasPolicy = true
		}
		if str(r, "resource") == "directory" && str(r, "state") == "present" {
			hasKD = true
		}
	}
	if !hasPolicy || !hasKD {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("status --section fleet missing policy or key-directory present: %v", sRecs))
		return
	}

	h.recordCase(name, "ok", "ok", true,
		"fleet --keys, fleet --access and status --section fleet verified against fixture policy tree across default, --json and --human")
}

// runLegacyRefusal proves `legacy-refusal` (D5, D10): `setup --list`, `catalog
// list`, `account doctor x`, `fleet list`, `skill path`, `status --section ssh`
// and `--update` (and the other D5 legacy routes) invoked on the core each exit
// 2 with MM_LEGACY_ROUTE_UNSUPPORTED, writing nothing and starting no process.
func (h *releaseHarness) runLegacyRefusal() {
	const name = caseReleaseLegacyRefusal
	refHome := filepath.Join(h.root, "legacy-refusal-home")
	refState := filepath.Join(h.root, "legacy-refusal-state")
	refEnv := []string{
		"HOME=" + refHome,
		"USERPROFILE=" + refHome,
		"MANAGED_MACHINE_STATE_DIR=" + refState,
	}
	routes := [][]string{
		{"setup", "--list"},
		{"catalog", "list"},
		{"adopt"},
		{"add-agent", "x"},
		{"account", "doctor", "x"},
		{"ssh", "status"},
		{"--bootstrap"},
		{"--update"},
		{"fleet", "remove", "x"},
		{"status", "--section", "bootstrap"},
		{"status", "--section", "update"},
		{"setup", "--list", "--json"},
		{"setup", "--list", "--human"},
	}
	for _, args := range routes {
		code, out, errOut := h.execBinaryEnv(h.bin, refEnv, args...)
		if code != 2 || len(out) != 0 || !strings.Contains(string(errOut), diag.LegacyRouteUnsupported) || !strings.Contains(string(errOut), "Homebrew-installed managed-machine on macOS") {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("core %v exited %d (stdout %q, stderr %q), want exit 2 with %s",
				args, code, firstLineOf(out), firstLineOf(errOut), diag.LegacyRouteUnsupported))
			return
		}
	}
	removedRoutes := [][]string{
		{"fleet", "list"},
		{"status", "--section", "ssh"},
		{"status", "--section", "machine"},
		{"status", "--section", "gh"},
		{"status", "--section", "catalog"},
	}
	for _, args := range removedRoutes {
		code, out, errOut := h.execBinaryEnv(h.bin, refEnv, args...)
		if code != 2 || len(out) != 0 || !strings.Contains(string(errOut), diag.LegacyRouteRemoved) {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("core %v exited %d (stdout %q, stderr %q), want exit 2 with %s",
				args, code, firstLineOf(out), firstLineOf(errOut), diag.LegacyRouteRemoved))
			return
		}
	}
	if _, err := os.Stat(refState); !errors.Is(err, os.ErrNotExist) {
		h.recordCase(name, "ok", observedFailed, false, "legacy-refusal created state directory")
		return
	}
	ents, err := os.ReadDir(refHome)
	if err != nil || len(ents) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("legacy-refusal wrote files into home: %v (%v)", len(ents), err))
		return
	}
	if b, err := os.ReadFile(h.log); err == nil && len(bytes.TrimSpace(b)) > 0 {
		h.recordCase(name, "ok", observedFailed, false, "legacy-refusal started sentinel: "+firstLineOf(b))
		return
	}
	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("%d legacy routes refused on the core with exit 2 and %s, 0 files written, 0 processes started",
			len(routes), diag.LegacyRouteUnsupported))
}

// runInstaller proves `installer` (D4, D10): on Linux and Windows, installs
// from the directory base and from a loopback HTTPS base into a harness prefix,
// verifies identical digest and `--version`, verifies idempotent second run,
// and verifies a tampered archive is refused with exit 1 and no file written;
// on darwin, `install-core.sh` refuses with exit 2 naming
// Formula/managed-machine.rb.
func (h *releaseHarness) runInstaller() {
	const name = caseReleaseInstaller
	instRoot := filepath.Join(h.root, "installer-case")
	instHome := filepath.Join(instRoot, "home")
	if err := os.MkdirAll(instHome, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	archiveName := targetArchiveName(h.releaseVer, h.target)
	wantBinSHA := h.ev.BinarySHA256

	if runtime.GOOS == "darwin" {
		prefixDir := filepath.Join(instRoot, "darwin-prefix")
		installerPath := filepath.Join(h.repoRoot, "install-core.sh")
		code, out, errOut := h.runExternalScript("sh", []string{installerPath, "--prefix", prefixDir, "--release-base", h.distDir}, []string{
			"HOME=" + instHome,
		})
		if code != 2 || len(out) != 0 || !strings.Contains(string(errOut), "Formula/managed-machine.rb") {
			h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("install-core.sh on darwin exited %d (stdout %q, stderr %q), want exit 2 naming Formula/managed-machine.rb",
				code, firstLineOf(out), firstLineOf(errOut)))
			return
		}
		if _, err := os.Stat(filepath.Join(prefixDir, "managed-machine")); !errors.Is(err, os.ErrNotExist) {
			h.recordCase(name, "ok", observedFailed, false, "install-core.sh on darwin wrote a binary before refusing")
			return
		}
		h.recordCase(name, "ok", "ok", true,
			"install-core.sh on darwin exited 2 naming Formula/managed-machine.rb with no file written")
		return
	}

	// Linux and Windows:
	// 1. Directory base install + idempotent second run.
	dirPrefix := filepath.Join(instRoot, "dir-prefix")
	installedBin, _, err := h.runPlatformInstaller(dirPrefix, h.distDir, instHome, nil)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "directory install: "+err.Error())
		return
	}
	code, vOut, vErr := h.execBinaryEnv(installedBin, nil, "--version")
	if code != 0 || len(vErr) != 0 || strings.TrimSpace(string(vOut)) != h.releaseVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("installed binary --version exited %d: %q", code, firstLineOf(vOut, vErr)))
		return
	}
	if _, err := h.reinstallCandidateIdempotent(dirPrefix, installedBin, archiveName, wantBinSHA); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "idempotent rerun: "+err.Error())
		return
	}

	// 2. Loopback HTTPS base install.
	srv := newFixtureTLSServer(http.FileServer(http.Dir(h.distDir)))
	defer srv.Close()
	caPath := filepath.Join(instRoot, "ca.pem")
	if err := os.WriteFile(caPath, pemCertificate(srv.Certificate().Raw), 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(instRoot, "ca.cer"), srv.Certificate().Raw, 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	// install-core.ps1 downloads through .NET, which reads the Windows
	// store and ignores SSL_CERT_FILE.
	untrust, err := trustFixtureCertificate(caPath)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "the fixture certificate could not be trusted: "+err.Error())
		return
	}
	defer untrust()
	httpsPrefix := filepath.Join(instRoot, "https-prefix")
	httpsBin, _, err := h.runPlatformInstaller(httpsPrefix, srv.URL, instHome, []string{
		"CURL_CA_BUNDLE=" + caPath,
		"SSL_CERT_FILE=" + caPath,
	})
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "loopback HTTPS install: "+err.Error())
		return
	}
	httpsBytes, err := os.ReadFile(httpsBin)
	if err != nil || sum(httpsBytes) != wantBinSHA {
		h.recordCase(name, "ok", observedFailed, false, "HTTPS installed binary digest mismatch")
		return
	}
	code, vOut, vErr = h.execBinaryEnv(httpsBin, nil, "--version")
	if code != 0 || len(vErr) != 0 || strings.TrimSpace(string(vOut)) != h.releaseVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("HTTPS installed binary --version exited %d: %q", code, firstLineOf(vOut, vErr)))
		return
	}

	// 3. Tampered archive is refused with exit 1 and no file written.
	tamperedDist := filepath.Join(instRoot, "tampered-dist")
	if err := copyTree(h.distDir, tamperedDist); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	origArc, err := os.ReadFile(filepath.Join(tamperedDist, archiveName))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	origArc[len(origArc)/2] ^= 0xff
	if err := os.WriteFile(filepath.Join(tamperedDist, archiveName), origArc, 0o644); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	tamperedPrefix := filepath.Join(instRoot, "tampered-prefix")
	if err := h.verifyTamperedInstallerRefused(tamperedPrefix, tamperedDist, instHome); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("installed %s from directory and loopback HTTPS bases (sha256 %s), verified --version %s, idempotent rerun and tampered-archive exit 1 refusal with no file written",
			archiveName, wantBinSHA[:12], h.releaseVer))
}

func (h *releaseHarness) runPlatformInstaller(prefixDir, releaseBase, instHome string, extraEnv []string) (string, string, error) {
	wantBinSHA := h.ev.BinarySHA256
	if runtime.GOOS == "windows" {
		installerPath := filepath.Join(h.repoRoot, "install-core.ps1")
		shell := "powershell.exe"
		if p, err := exec.LookPath("pwsh.exe"); err == nil && p != "" {
			shell = p
		}
		// The https base is verified against the Windows store, where the
		// installer case trusts its fixture certificate: no validation
		// callback, which pwsh cannot run on the TLS thread anyway.
		args := []string{
			"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", installerPath,
			"-ReleaseBase", releaseBase,
			"-Prefix", prefixDir,
		}
		env := append([]string{
			"USERPROFILE=" + instHome,
			"HOME=" + instHome,
			"LOCALAPPDATA=" + filepath.Join(instHome, "AppData", "Local"),
		}, extraEnv...)
		code, out, errOut := h.runExternalScript(shell, args, env)
		if code != 0 {
			return "", "", fmt.Errorf("install-core.ps1 exited %d: %s", code, firstLineOf(errOut, out))
		}
		if !strings.Contains(string(out), "Installed managed-machine to ") || !strings.Contains(strings.ToLower(string(out)), wantBinSHA) {
			return "", "", fmt.Errorf("install-core.ps1 output missing installed banner or digest: %s", firstLineOf(out))
		}
		binPath := filepath.Join(prefixDir, "managed-machine.exe")
		gotBytes, err := os.ReadFile(binPath)
		if err != nil {
			return "", "", err
		}
		if sum(gotBytes) != wantBinSHA {
			return "", "", fmt.Errorf("installed binary digest %s != archive digest %s", sum(gotBytes), wantBinSHA)
		}
		return binPath, fmt.Sprintf("install-core.ps1 installed %s (%s)", h.releaseVer, wantBinSHA[:12]), nil
	}

	installerPath := filepath.Join(h.repoRoot, "install-core.sh")
	env := append([]string{"HOME=" + instHome}, extraEnv...)
	code, out, errOut := h.runExternalScript("sh", []string{
		installerPath, "--prefix", prefixDir, "--release-base", releaseBase,
	}, env)
	if code != 0 {
		return "", "", fmt.Errorf("install-core.sh exited %d: %s", code, firstLineOf(errOut, out))
	}
	if !strings.Contains(string(out), "Installed managed-machine to ") || !strings.Contains(strings.ToLower(string(out)), wantBinSHA) {
		return "", "", fmt.Errorf("install-core.sh output missing installed banner or digest: %s", firstLineOf(out))
	}
	binPath := filepath.Join(prefixDir, "managed-machine")
	gotBytes, err := os.ReadFile(binPath)
	if err != nil {
		return "", "", err
	}
	if sum(gotBytes) != wantBinSHA {
		return "", "", fmt.Errorf("installed binary digest %s != archive digest %s", sum(gotBytes), wantBinSHA)
	}
	return binPath, fmt.Sprintf("install-core.sh installed %s (%s)", h.releaseVer, wantBinSHA[:12]), nil
}

func (h *releaseHarness) verifyTamperedInstallerRefused(prefixDir, tamperedDist, instHome string) error {
	if runtime.GOOS == "windows" {
		installerPath := filepath.Join(h.repoRoot, "install-core.ps1")
		shell := "powershell.exe"
		if p, err := exec.LookPath("pwsh.exe"); err == nil && p != "" {
			shell = p
		}
		code, out, errOut := h.runExternalScript(shell, []string{
			"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", installerPath,
			"-ReleaseBase", tamperedDist,
			"-Prefix", prefixDir,
		}, []string{
			"USERPROFILE=" + instHome,
			"HOME=" + instHome,
			"LOCALAPPDATA=" + filepath.Join(instHome, "AppData", "Local"),
		})
		if code != 1 {
			return fmt.Errorf("tampered install-core.ps1 exited %d (stderr %q, stdout %q), want exit 1", code, firstLineOf(errOut), firstLineOf(out))
		}
		if _, err := os.Stat(filepath.Join(prefixDir, "managed-machine.exe")); !errors.Is(err, os.ErrNotExist) {
			return errors.New("tampered install-core.ps1 wrote managed-machine.exe")
		}
		return nil
	}
	installerPath := filepath.Join(h.repoRoot, "install-core.sh")
	code, out, errOut := h.runExternalScript("sh", []string{
		installerPath, "--prefix", prefixDir, "--release-base", tamperedDist,
	}, []string{"HOME=" + instHome})
	if code != 1 {
		return fmt.Errorf("tampered install-core.sh exited %d (stderr %q, stdout %q), want exit 1", code, firstLineOf(errOut), firstLineOf(out))
	}
	if _, err := os.Stat(filepath.Join(prefixDir, "managed-machine")); !errors.Is(err, os.ErrNotExist) {
		return errors.New("tampered install-core.sh wrote managed-machine")
	}
	return nil
}

// runUpgrade proves `upgrade` (D6): installs the candidate through the
// installer from the local dist/ into a harness prefix, runs the AC-T-04
// cycle, reinstalls idempotently with identical digest, reads the state with
// the natively rebuilt `next-version fixture` (-X main.version=<X.Y.Z>+gate),
// verifies MM_CONFIG_NO_PLAN, restores the release binary and reads the state
// again, and verifies schema_version 1 across plans, journals, receipts and
// ledgers in both directions.
func (h *releaseHarness) runUpgrade() {
	const name = caseReleaseUpgrade
	upgradeDir := filepath.Join(h.root, "upgrade")
	prefixDir := filepath.Join(upgradeDir, "prefix")
	stateDir := filepath.Join(upgradeDir, "state")
	desiredFile := filepath.Join(upgradeDir, "desired.yaml")
	destFile := filepath.Join(upgradeDir, "dest", "upgrade.txt")
	if err := os.MkdirAll(prefixDir, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	desiredYAML := fmt.Sprintf("target: user\nconfig:\n  mm-gate-upgrade-cfg:\n    destinations:\n      upgrade-file: %q\n",
		filepath.ToSlash(destFile))
	if err := write(desiredFile, desiredYAML); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}

	archiveName := targetArchiveName(h.releaseVer, h.target)
	archivePath := filepath.Join(h.distDir, archiveName)
	wantBinBytes, err := extractArchiveBinaryBytes(archivePath)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "extract archive binary: "+err.Error())
		return
	}
	wantBinSHA := sum(wantBinBytes)

	installedBin, installDetail, err := h.installCandidateFromDist(prefixDir, archiveName, wantBinBytes, wantBinSHA)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	h.step("upgrade installer first run", installDetail, true)

	upEnv := []string{
		"MANAGED_MACHINE_STATE_DIR=" + stateDir,
		config.DesiredEnv + "=" + desiredFile,
		"MANAGED_MACHINE_SOURCE_DIR=" + h.defs,
	}

	// Run the AC-T-04 cycle with the installed candidate binary.
	if problem := h.runConfigACT04WithBinary(installedBin, upEnv, stateDir, "mm-gate-upgrade-cfg", destFile); problem != "" {
		h.recordCase(name, "ok", observedFailed, false, "AC-T-04 cycle: "+problem)
		return
	}

	// Reinstall idempotently and verify nothing is rewritten.
	if detail, err := h.reinstallCandidateIdempotent(prefixDir, installedBin, archiveName, wantBinSHA); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "idempotent reinstall: "+err.Error())
		return
	} else {
		h.step("upgrade installer idempotent rerun", detail, true)
	}

	if err := verifyStateSchemaV1(stateDir); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "schema_version check after candidate apply: "+err.Error())
		return
	}

	// Build the next-version fixture natively with -X main.version=<X.Y.Z>+gate
	// (labeled a fixture in the evidence, never a release).
	nextFixtureVer := h.releaseVer + "+gate"
	nextBinPath := filepath.Join(upgradeDir, exe("next-version-fixture"))
	if err := compileCoreBinary(h.repoRoot, nextFixtureVer, runtime.GOOS, runtime.GOARCH, nextBinPath); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "build next-version fixture: "+err.Error())
		return
	}
	nextBytes, err := os.ReadFile(nextBinPath)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if err := writeExecutableAtomically(installedBin, nextBytes); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "stage next-version fixture: "+err.Error())
		return
	}

	code, vOut, vErr := h.execBinaryEnv(installedBin, upEnv, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != nextFixtureVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("next-version fixture --version exited %d: got %q (stderr %q), want %q",
			code, strings.TrimSpace(string(vOut)), firstLineOf(vErr), nextFixtureVer))
		return
	}
	if problem := h.verifyInstalledStateReadAndNoPlan(installedBin, upEnv, "mm-gate-upgrade-cfg"); problem != "" {
		h.recordCase(name, "ok", observedFailed, false, "next-version fixture state check: "+problem)
		return
	}
	if err := verifyStateSchemaV1(stateDir); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "schema_version check under next-version fixture: "+err.Error())
		return
	}
	h.step("upgrade next-version fixture",
		fmt.Sprintf("natively rebuilt next-version fixture (%s, fixture only, not a release) read state present/observed and refused replay with MM_CONFIG_NO_PLAN", nextFixtureVer),
		true)

	// Restore the release binary and read the state again.
	if err := writeExecutableAtomically(installedBin, wantBinBytes); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "restore release binary: "+err.Error())
		return
	}
	code, vOut, vErr = h.execBinaryEnv(installedBin, upEnv, "--version")
	if code != 0 || strings.TrimSpace(string(vOut)) != h.releaseVer {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("restored release binary --version exited %d: got %q (stderr %q), want %q",
			code, strings.TrimSpace(string(vOut)), firstLineOf(vErr), h.releaseVer))
		return
	}
	if problem := h.verifyInstalledStateReadAndNoPlan(installedBin, upEnv, "mm-gate-upgrade-cfg"); problem != "" {
		h.recordCase(name, "ok", observedFailed, false, "restored release binary state check: "+problem)
		return
	}
	if err := verifyStateSchemaV1(stateDir); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "schema_version check after restore: "+err.Error())
		return
	}
	if !strings.HasPrefix(h.target, "darwin/") || !h.canRunBrew() {
		h.downgradeNoPlanVerified = true
	}

	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("candidate %s installed from local dist, AC-T-04 cycle applied, idempotent reinstall verified, next-version fixture (%s, fixture) and restored release binary read schema_version 1 state with MM_CONFIG_NO_PLAN",
			h.releaseVer, nextFixtureVer))
}

func writeExecutableAtomically(dest string, data []byte) error {
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func (h *releaseHarness) installCandidateFromDist(prefixDir, archiveName string, wantBinBytes []byte, wantBinSHA string) (string, string, error) {
	instHome := filepath.Join(h.root, "upgrade", "installer-home")
	if err := os.MkdirAll(instHome, 0o755); err != nil {
		return "", "", err
	}

	switch runtime.GOOS {
	case "linux":
		installerPath := filepath.Join(h.repoRoot, "install-core.sh")
		code, out, errOut := h.runExternalScript("sh", []string{installerPath, "--prefix", prefixDir, "--release-base", h.distDir}, []string{
			"HOME=" + instHome,
		})
		if code != 0 {
			return "", "", fmt.Errorf("install-core.sh exited %d: %s", code, firstLineOf(errOut, out))
		}
		if !strings.Contains(string(out), "Installed managed-machine to ") || !strings.Contains(strings.ToLower(string(out)), wantBinSHA) {
			return "", "", fmt.Errorf("install-core.sh output missing installed banner or digest: %s", firstLineOf(out))
		}
		binPath := filepath.Join(prefixDir, "managed-machine")
		gotBytes, err := os.ReadFile(binPath)
		if err != nil {
			return "", "", err
		}
		if sum(gotBytes) != wantBinSHA {
			return "", "", fmt.Errorf("installed binary digest %s != archive digest %s", sum(gotBytes), wantBinSHA)
		}
		return binPath, fmt.Sprintf("install-core.sh installed %s from local dist (%s)", h.releaseVer, wantBinSHA[:12]), nil

	case "windows":
		installerPath := filepath.Join(h.repoRoot, "install-core.ps1")
		shell := "powershell.exe"
		if p, err := exec.LookPath("pwsh.exe"); err == nil && p != "" {
			shell = p
		}
		code, out, errOut := h.runExternalScript(shell, []string{
			"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", installerPath,
			"-ReleaseBase", h.distDir,
			"-Prefix", prefixDir,
		}, []string{
			"USERPROFILE=" + instHome,
			"HOME=" + instHome,
			"LOCALAPPDATA=" + filepath.Join(instHome, "AppData", "Local"),
		})
		if code != 0 {
			return "", "", fmt.Errorf("install-core.ps1 exited %d: %s", code, firstLineOf(errOut, out))
		}
		if !strings.Contains(string(out), "Installed managed-machine to ") || !strings.Contains(strings.ToLower(string(out)), wantBinSHA) {
			return "", "", fmt.Errorf("install-core.ps1 output missing installed banner or digest: %s", firstLineOf(out))
		}
		binPath := filepath.Join(prefixDir, "managed-machine.exe")
		gotBytes, err := os.ReadFile(binPath)
		if err != nil {
			return "", "", err
		}
		if sum(gotBytes) != wantBinSHA {
			return "", "", fmt.Errorf("installed binary digest %s != archive digest %s", sum(gotBytes), wantBinSHA)
		}
		return binPath, fmt.Sprintf("install-core.ps1 installed %s from local dist (%s)", h.releaseVer, wantBinSHA[:12]), nil

	default:
		// On macOS, install-core.sh refuses with exit 2 naming Formula/managed-machine.rb,
		// and the verified darwin_arm64 archive from dist/ is staged into prefixDir.
		installerPath := filepath.Join(h.repoRoot, "install-core.sh")
		code, out, errOut := h.runExternalScript("sh", []string{installerPath, "--prefix", prefixDir, "--release-base", h.distDir}, []string{
			"HOME=" + instHome,
		})
		if code != 2 || !strings.Contains(string(errOut), "Formula/managed-machine.rb") {
			return "", "", fmt.Errorf("install-core.sh on darwin exited %d (stderr %q, stdout %q), want exit 2 naming Formula/managed-machine.rb",
				code, firstLineOf(errOut), firstLineOf(out))
		}
		binPath := filepath.Join(prefixDir, "managed-machine")
		if err := writeExecutableAtomically(binPath, wantBinBytes); err != nil {
			return "", "", err
		}
		return binPath, fmt.Sprintf("install-core.sh refused darwin with exit 2 naming Formula/managed-machine.rb and verified archive %s staged (%s)",
			archiveName, wantBinSHA[:12]), nil
	}
}

func (h *releaseHarness) reinstallCandidateIdempotent(prefixDir, installedBin, archiveName, wantBinSHA string) (string, error) {
	beforeInfo, err := os.Stat(installedBin)
	if err != nil {
		return "", err
	}
	instHome := filepath.Join(h.root, "upgrade", "installer-home")

	switch runtime.GOOS {
	case "linux":
		installerPath := filepath.Join(h.repoRoot, "install-core.sh")
		code, out, errOut := h.runExternalScript("sh", []string{installerPath, "--prefix", prefixDir, "--release-base", h.distDir}, []string{
			"HOME=" + instHome,
		})
		if code != 0 {
			return "", fmt.Errorf("install-core.sh rerun exited %d: %s", code, firstLineOf(errOut, out))
		}
		if !strings.Contains(string(out), "already up to date") || !strings.Contains(strings.ToLower(string(out)), wantBinSHA) {
			return "", fmt.Errorf("install-core.sh rerun did not report already up to date with digest: %s", firstLineOf(out))
		}
	case "windows":
		installerPath := filepath.Join(h.repoRoot, "install-core.ps1")
		shell := "powershell.exe"
		if p, err := exec.LookPath("pwsh.exe"); err == nil && p != "" {
			shell = p
		}
		code, out, errOut := h.runExternalScript(shell, []string{
			"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-File", installerPath,
			"-ReleaseBase", h.distDir,
			"-Prefix", prefixDir,
		}, []string{
			"USERPROFILE=" + instHome,
			"HOME=" + instHome,
			"LOCALAPPDATA=" + filepath.Join(instHome, "AppData", "Local"),
		})
		if code != 0 {
			return "", fmt.Errorf("install-core.ps1 rerun exited %d: %s", code, firstLineOf(errOut, out))
		}
		if !strings.Contains(string(out), "already up to date") || !strings.Contains(strings.ToLower(string(out)), wantBinSHA) {
			return "", fmt.Errorf("install-core.ps1 rerun did not report already up to date with digest: %s", firstLineOf(out))
		}
	default:
		gotBytes, err := os.ReadFile(installedBin)
		if err != nil {
			return "", err
		}
		if sum(gotBytes) != wantBinSHA {
			return "", fmt.Errorf("binary digest changed: %s != %s", sum(gotBytes), wantBinSHA)
		}
		return fmt.Sprintf("installed binary already up to date (%s)", wantBinSHA[:12]), nil
	}

	afterInfo, err := os.Stat(installedBin)
	if err != nil {
		return "", err
	}
	if !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		return "", errors.New("idempotent installer rerun rewrote the binary on disk")
	}
	gotBytes, err := os.ReadFile(installedBin)
	if err != nil {
		return "", err
	}
	if sum(gotBytes) != wantBinSHA {
		return "", fmt.Errorf("binary digest after rerun %s != %s", sum(gotBytes), wantBinSHA)
	}
	return fmt.Sprintf("installer rerun reported already up to date (%s) without rewriting binary", wantBinSHA[:12]), nil
}

func (h *releaseHarness) runExternalScript(prog string, args, extraEnv []string) (int, []byte, []byte) {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME", k == "USERPROFILE", k == "LOCALAPPDATA", k == "APPDATA":
			continue
		case strings.HasPrefix(k, "MANAGED_MACHINE_"), k == "SSL_CERT_FILE":
			continue
		}
		env = append(env, kv)
	}
	env = append(env, extraEnv...)
	c := exec.Command(prog, args...)
	c.Env, c.Dir = env, h.root
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	if o.Len() > 0 {
		h.captures = append(h.captures, o.Bytes())
	}
	if e.Len() > 0 {
		h.captures = append(h.captures, e.Bytes())
	}
	return code, o.Bytes(), e.Bytes()
}

func (h *releaseHarness) execBinaryEnv(binPath string, extra []string, args ...string) (int, []byte, []byte) {
	env := append([]string{}, h.env...)
	env = append(env, extra...)
	c := exec.Command(binPath, args...)
	c.Env, c.Dir = env, h.home
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	if o.Len() > 0 {
		h.captures = append(h.captures, o.Bytes())
	}
	if e.Len() > 0 {
		h.captures = append(h.captures, e.Bytes())
	}
	return code, o.Bytes(), e.Bytes()
}

// runConfigACT04WithBinary drives binPath through the five-step AC-T-04
// configuration cycle against stateDir and extraEnv.
func (h *releaseHarness) runConfigACT04WithBinary(binPath string, extraEnv []string, stateDir, defID, destFile string) string {
	// 1. --plan writes a saved plan with 1 create operation.
	code, out, errOut := h.execBinaryEnv(binPath, extraEnv, "config", defID, "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 1 (--plan) exited %d: %s", code, firstLineOf(errOut, out))
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 2 {
		return fmt.Sprintf("step 1 (--plan) want 2 records, got %d (%v)", len(recs), err)
	}
	plan1 := str(recs[0], "plan")
	if !planIDPattern.MatchString(plan1) || str(recs[1], "action") != "create" {
		return fmt.Sprintf("step 1 unexpected plan records: %v", recs)
	}

	// 2. --apply executes plan1.
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 2 (--apply) exited %d: %s", code, firstLineOf(errOut, out))
	}
	applyRecs, err := parseJSONL(out)
	if err != nil || len(applyRecs) != 1 || str(applyRecs[0], "state") != "applied" {
		return fmt.Sprintf("step 2 (--apply) unexpected records: %v (%v)", applyRecs, err)
	}
	if _, err := os.Stat(destFile); err != nil {
		return fmt.Sprintf("step 2 destination not written: %v", err)
	}
	h.recordPlanFromDir(stateDir, plan1)

	// 3. status --section config reports present/observed and --check exits 0.
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "status", "--section", "config", defID)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 3 (status) exited %d: %s", code, firstLineOf(errOut, out))
	}
	stRecs, err := parseJSONL(out)
	if err != nil || len(stRecs) != 1 || str(stRecs[0], "state") != "present" || str(stRecs[0], "evidence") != "observed" {
		return fmt.Sprintf("step 3 (status) unexpected records: %v (%v)", stRecs, err)
	}
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--check", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 3 (--check) exited %d: %s", code, firstLineOf(errOut, out))
	}

	// 4. Fresh --plan has 0 operations (noop) and --apply succeeds unchanged.
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 4 (second --plan) exited %d: %s", code, firstLineOf(errOut, out))
	}
	recs2, err := parseJSONL(out)
	if err != nil || len(recs2) != 1 {
		return fmt.Sprintf("step 4 (second --plan) want 1 header record (0 ops), got %d (%v)", len(recs2), err)
	}
	plan2 := str(recs2[0], "plan")
	if plan2 == "" || plan2 == plan1 {
		return fmt.Sprintf("step 4 reused plan %s", plan1)
	}
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("step 4 (second --apply) exited %d: %s", code, firstLineOf(errOut, out))
	}
	h.recordPlanFromDir(stateDir, plan2)

	// 5. Second --apply without a saved plan fails with MM_CONFIG_NO_PLAN (exit 2).
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--apply", "--user")
	if code != 2 || len(out) != 0 || !strings.Contains(string(errOut), diag.ConfigNoPlan) {
		return fmt.Sprintf("step 5 (repeat --apply) exited %d (stdout %q, stderr %q), want exit 2 with %s",
			code, firstLineOf(out), firstLineOf(errOut), diag.ConfigNoPlan)
	}
	return ""
}

func (h *releaseHarness) verifyInstalledStateReadAndNoPlan(binPath string, extraEnv []string, defID string) string {
	code, out, errOut := h.execBinaryEnv(binPath, extraEnv, "status", "--section", "config", defID)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("status --section config exited %d: %s", code, firstLineOf(errOut, out))
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 1 || str(recs[0], "state") != "present" || str(recs[0], "evidence") != "observed" {
		return fmt.Sprintf("status --section config unexpected records: %v (%v)", recs, err)
	}
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--apply", "--user")
	if code != 2 || len(out) != 0 || !strings.Contains(string(errOut), diag.ConfigNoPlan) {
		return fmt.Sprintf("config --apply exited %d (stderr %q), want exit 2 with %s", code, firstLineOf(errOut), diag.ConfigNoPlan)
	}
	code, out, errOut = h.execBinaryEnv(binPath, extraEnv, "config", defID, "--check", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("config --check exited %d: %s", code, firstLineOf(errOut, out))
	}
	return ""
}

func (h *releaseHarness) recordPlanFromDir(stateDir, id string) {
	b, err := os.ReadFile(filepath.Join(stateDir, "plans", id+".json"))
	if err != nil {
		return
	}
	var doc planDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		return
	}
	receipt := ""
	if jb, err := os.ReadFile(filepath.Join(stateDir, "journal", id+".jsonl")); err == nil {
		var recs []journalRecord
		for _, line := range strings.Split(strings.TrimRight(string(jb), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var r journalRecord
			if json.Unmarshal([]byte(line), &r) == nil {
				recs = append(recs, r)
			}
		}
		receipt = journalReceipt(recs)
	}
	h.ev.Plans = append(h.ev.Plans, PlanRecord{Plan: id, State: doc.State, Receipt: receipt})
}

func verifyStateSchemaV1(stateDir string) error {
	planFiles, err := filepath.Glob(filepath.Join(stateDir, "plans", "plan-*.json"))
	if err != nil {
		return err
	}
	if len(planFiles) == 0 {
		return errors.New("no plan documents in state directory")
	}
	for _, pf := range planFiles {
		b, err := os.ReadFile(pf)
		if err != nil {
			return err
		}
		var doc plan.Document
		if err := json.Unmarshal(b, &doc); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(pf), err)
		}
		if err := doc.Validate(); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(pf), err)
		}
	}
	journalFiles, err := filepath.Glob(filepath.Join(stateDir, "journal", "plan-*.jsonl"))
	if err != nil {
		return err
	}
	if len(journalFiles) == 0 {
		return errors.New("no journal files in state directory")
	}
	for _, jf := range journalFiles {
		b, err := os.ReadFile(jf)
		if err != nil {
			return err
		}
		var recs []plan.Record
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if line == "" {
				continue
			}
			var r plan.Record
			if err := json.Unmarshal([]byte(line), &r); err != nil {
				return fmt.Errorf("%s: %w", filepath.Base(jf), err)
			}
			recs = append(recs, r)
		}
		if len(recs) == 0 {
			return fmt.Errorf("%s: empty journal", filepath.Base(jf))
		}
		last := recs[len(recs)-1]
		if last.Action != plan.ActionReceipt || last.State == "" || last.Outcome == "" {
			return fmt.Errorf("%s: missing final receipt record (got action=%q outcome=%q state=%q)",
				filepath.Base(jf), last.Action, last.Outcome, last.State)
		}
	}
	ledgerPath := filepath.Join(stateDir, "ownership.jsonl")
	lb, err := os.ReadFile(ledgerPath)
	if err != nil {
		return fmt.Errorf("ownership.jsonl: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(lb)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return errors.New("ownership.jsonl is empty")
	}
	for _, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			return fmt.Errorf("ownership.jsonl: %w", err)
		}
		if m["schema_version"] != float64(1) {
			return fmt.Errorf("ownership.jsonl: schema_version = %v, want 1", m["schema_version"])
		}
	}
	return nil
}

// runRecovery proves `recovery` per D9 on all five targets:
//  1. An interrupted apply (the executing process killed mid-run while holding
//     the store lock) yields an `interrupted` receipt and state `failed` on the
//     next command, `--from-plan` refuses the failed plan with MM_CONFIG_NO_PLAN,
//     and a fresh plan applies cleanly.
//  2. A truncated plan file in plans/ makes `config --plan` exit 1 with a
//     diagnostic naming the file while `status` still exits 0; removing the
//     truncated file restores planning.
//  3. No consumed plan is replayed across the downgrade and re-upgrade of this
//     story's other cases.
func (h *releaseHarness) runRecovery() {
	const name = caseReleaseRecovery
	recDir := filepath.Join(h.root, "recovery")
	stateDir := filepath.Join(recDir, "state")
	desiredFile := filepath.Join(recDir, "desired.yaml")
	destFile := filepath.Join(recDir, "dest", "recovery.txt")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	desiredYAML := fmt.Sprintf("target: user\nconfig:\n  mm-gate-recovery-cfg:\n    destinations:\n      recovery-file: %q\n",
		filepath.ToSlash(destFile))
	if err := write(desiredFile, desiredYAML); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	recEnv := []string{
		"MANAGED_MACHINE_STATE_DIR=" + stateDir,
		config.DesiredEnv + "=" + desiredFile,
		"MANAGED_MACHINE_SOURCE_DIR=" + h.defs,
	}

	// 1. Plan a config change, then start a child process that opens the store,
	// begins execution (moving the plan to `executing` and journaling `started`
	// on the first operation), and kill the child mid-run.
	code, out, errOut := h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("initial recovery --plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) == 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("initial recovery --plan records: %v", err))
		return
	}
	killedPlanID := str(recs[0], "plan")
	if !planIDPattern.MatchString(killedPlanID) {
		h.recordCase(name, "ok", observedFailed, false, "invalid plan id: "+killedPlanID)
		return
	}

	selfExe, err := os.Executable()
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, "os.Executable: "+err.Error())
		return
	}
	child := exec.Command(selfExe, "_interrupt-child", "-state", stateDir, "-plan", killedPlanID)
	child.Env = append(append([]string{}, h.env...), recEnv...)
	child.Dir = h.home
	isolateProcessGroup(child)
	if err := child.Start(); err != nil {
		h.recordCase(name, "ok", observedFailed, false, "start executing child: "+err.Error())
		return
	}
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()

	started := false
	childExited := false
	deadline := time.Now().Add(30 * time.Second)
	journalFile := filepath.Join(stateDir, "journal", killedPlanID+".jsonl")
	for time.Now().Before(deadline) {
		if jb, rerr := os.ReadFile(journalFile); rerr == nil && strings.Contains(string(jb), `"outcome":"started"`) {
			started = true
			break
		}
		select {
		case <-done:
			childExited = true
		default:
		}
		if childExited {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !started {
		_ = killProcessGroup(child)
		if !childExited {
			<-done
		}
		h.recordCase(name, "ok", observedFailed, false, "executing child never journaled started")
		return
	}

	// While the child holds the OS lock, a concurrent mutating command fails with MM_CONFIG_BUSY.
	busyCode, _, busyErr := h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--apply", "--user")
	if busyCode != 1 || !strings.Contains(string(busyErr), diag.ConfigBusy) {
		_ = killProcessGroup(child)
		if !childExited {
			<-done
		}
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("concurrent apply while lock held exited %d (stderr %q), want 1 with %s",
			busyCode, firstLineOf(busyErr), diag.ConfigBusy))
		return
	}

	// Kill the executing child process group mid-run.
	if err := killProcessGroup(child); err != nil {
		if !childExited {
			<-done
		}
		h.recordCase(name, "ok", observedFailed, false, "kill executing child: "+err.Error())
		return
	}
	if !childExited {
		<-done
	}

	// Next mutating command opens the store, recovers killedPlanID as failed
	// with an interrupted receipt, and refuses --from-plan on the failed plan.
	fromCode, fromOut, fromErr := h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--apply", "--from-plan", killedPlanID, "--user")
	if fromCode != 2 || len(fromOut) != 0 || !strings.Contains(string(fromErr), diag.ConfigNoPlan) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("--from-plan on interrupted plan exited %d (stderr %q), want 2 with %s",
			fromCode, firstLineOf(fromErr), diag.ConfigNoPlan))
		return
	}
	h.recordPlanFromDir(stateDir, killedPlanID)

	planBytes, err := os.ReadFile(filepath.Join(stateDir, "plans", killedPlanID+".json"))
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	var recoveredDoc plan.Document
	if err := json.Unmarshal(planBytes, &recoveredDoc); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	if recoveredDoc.State != plan.Failed {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("interrupted plan state = %q, want %q", recoveredDoc.State, plan.Failed))
		return
	}
	jb, err := os.ReadFile(journalFile)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	var jRecs []journalRecord
	for _, line := range strings.Split(strings.TrimSpace(string(jb)), "\n") {
		if line == "" {
			continue
		}
		var r journalRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			jRecs = append(jRecs, r)
		}
	}
	if gotReceipt := journalReceipt(jRecs); gotReceipt != "interrupted" {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("interrupted plan receipt = %q, want \"interrupted\"", gotReceipt))
		return
	}

	// A fresh plan applies cleanly after recovery.
	code, out, errOut = h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fresh --plan after interruption exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	freshRecs, err := parseJSONL(out)
	if err != nil || len(freshRecs) == 0 {
		h.recordCase(name, "ok", observedFailed, false, "fresh --plan produced no records")
		return
	}
	freshPlanID := str(freshRecs[0], "plan")
	if freshPlanID == "" || freshPlanID == killedPlanID {
		h.recordCase(name, "ok", observedFailed, false, "fresh --plan reused interrupted plan "+killedPlanID)
		return
	}
	code, out, errOut = h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("fresh --apply after interruption exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.recordPlanFromDir(stateDir, freshPlanID)

	// 2. Truncated plan file (finding F1): `config --plan` exits 1 naming the
	// file while `status` exits 0; removing the file restores `--plan`.
	truncatedName := "plan-20261022T000000Z-dead.json"
	truncatedPath := filepath.Join(stateDir, "plans", truncatedName)
	if err := os.WriteFile(truncatedPath, []byte(`{"plan":`), 0o600); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--plan", "--user")
	if code != 1 || len(out) != 0 || !strings.Contains(string(errOut), truncatedName) {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("config --plan with truncated plan exited %d (stderr %q), want exit 1 naming %s",
			code, firstLineOf(errOut), truncatedName))
		return
	}
	code, out, errOut = h.execBinaryEnv(h.bin, recEnv, "status", "--section", "config", "mm-gate-recovery-cfg")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("status with truncated plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	if err := os.Remove(truncatedPath); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.execBinaryEnv(h.bin, recEnv, "config", "mm-gate-recovery-cfg", "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("config --plan after removing truncated file exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}

	// 3. Verify no consumed plan was replayed across downgrade and re-upgrade.
	if !h.downgradeNoPlanVerified {
		h.recordCase(name, "ok", observedFailed, false, "downgrade and re-upgrade consumed-plan non-replay was not verified")
		return
	}

	h.recordCase(name, "ok", "ok", true,
		fmt.Sprintf("interrupted plan %s recovered as failed with interrupted receipt, fresh plan %s applied, truncated %s failed config --plan (exit 1) while status exited 0 and removal restored planning, and no consumed plan replayed across downgrade/re-upgrade",
			killedPlanID, freshPlanID, truncatedName))
}

type blockingMachine struct{}

func (blockingMachine) Bindings(_ context.Context, d *plan.Document) (plan.Bindings, error) {
	return d.Bindings, nil
}

func (blockingMachine) Check(_ context.Context, p plan.Precondition) (string, error) {
	return p.Observed, nil
}

func (blockingMachine) Step(op plan.Operation) (execute.Step, error) {
	return blockingStep{id: op.ID}, nil
}

type blockingStep struct{ id string }

func (blockingStep) Apply(ctx context.Context) (execute.Evidence, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return nil, errors.New("timed out")
	}
}

func (blockingStep) Verify(_ context.Context) (execute.Evidence, error) {
	return execute.Evidence{}, nil
}

// interruptChildCmd is an internal helper invoked by `mm-walkthrough release`
// to hold the plan-store lock and block inside `execute.Run` after moving a
// saved plan to `executing` and journaling `started`.
func interruptChildCmd(args []string) error {
	fl := flag.NewFlagSet("_interrupt-child", flag.ContinueOnError)
	stateDir := fl.String("state", "", "state directory")
	planID := fl.String("plan", "", "plan id")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *stateDir == "" || *planID == "" {
		return errors.New("_interrupt-child needs -state and -plan")
	}
	st, err := plan.Open(*stateDir, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = execute.Run(context.Background(), st, *planID, blockingMachine{})
	return err
}

// runGuards proves `guards`: no sentinel started by the core; the runner's
// real ~/.ssh, shell profiles, ~/.config/managed-machine and platform state
// directory are byte-identical by hash; static harness fixtures are unchanged;
// and zero hits exist for fixture seed markers and key bytes across the state
// directories and captures.
func (h *releaseHarness) runGuards() {
	h.ev.Guards.SentinelInvocations = []string{}
	if b, err := os.ReadFile(h.log); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			h.ev.Guards.SentinelInvocations = append(h.ev.Guards.SentinelInvocations, h.cleanText(line))
		}
	}
	afterMachine, err := h.releaseMachineSnapshot()
	if err != nil {
		h.ev.Guards.ProfileChanges = []string{"the profiles could not be read: " + err.Error()}
		h.recordCase(caseReleaseGuards, "intact", observedFailed, false, "the profiles could not be read")
		return
	}
	changes := profileDiff(h.machine, afterMachine)
	if changes == nil {
		changes = []string{}
	}
	h.ev.Guards.ProfileChanges = changes

	afterStatic, err := h.staticHarnessSnapshot()
	if err != nil {
		h.recordCase(caseReleaseGuards, "intact", observedFailed, false, "static harness snapshot: "+err.Error())
		return
	}
	staticDiff := profileDiff(h.outsideContainment, afterStatic)

	problem := ""
	switch {
	case len(h.ev.Guards.SentinelInvocations) > 0:
		problem = "a guarded program was started: " + strings.Join(h.ev.Guards.SentinelInvocations, "; ")
	case len(changes) > 0:
		problem = "the profiles or real home changed: " + strings.Join(changes, "; ")
	case len(staticDiff) > 0:
		problem = "static harness directories changed: " + strings.Join(staticDiff, "; ")
	default:
		problem = h.grepStateAndCaptures()
	}
	if problem != "" {
		h.recordCase(caseReleaseGuards, "intact", observedFailed, false, problem)
		return
	}
	h.recordCase(caseReleaseGuards, "intact", "intact", true,
		"no guarded program was started, real ~/.ssh, shell profiles, ~/.config/managed-machine and state directory are byte-identical, and zero fixture seed markers or key bytes leaked")
}

func (h *releaseHarness) grepStateAndCaptures() string {
	for _, dir := range []string{
		h.state,
		filepath.Join(h.root, "config-cycle", "state"),
		filepath.Join(h.root, "fleet-state"),
		filepath.Join(h.root, "upgrade", "state"),
		filepath.Join(h.root, "recovery", "state"),
		filepath.Join(h.bashHome, "Library", "Application Support", "managed-machine"),
		filepath.Join(h.root, "private-seed-home", "Library", "Application Support", "managed-machine"),
		filepath.Join(h.root, "dispatcher-home", "Library", "Application Support", "managed-machine"),
	} {
		if hit := grepFleetTree(dir, "state", h.stateForbidden); hit != "" {
			return hit
		}
	}
	for i, capBytes := range h.captures {
		if hit := grepFleetBytes(capBytes, fmt.Sprintf("capture[%d]", i), []string{
			fleetPrivateKeyHeader, fleetPrivateKeyBody, fleetPrivateKeyFooter,
			releaseSeedMarkerZSH, releaseSeedMarkerAuthKeys,
			h.aliceKey.seedHex, h.aliceKey.seedB64,
			h.seedFixtureKey.seedHex, h.seedFixtureKey.seedB64,
			h.hostStateKey.seedHex, h.hostStateKey.seedB64,
		}); hit != "" {
			return hit
		}
	}
	return ""
}

// releaseAggregateCmd checks that five S6 release-gate records (and any
// accompanying provider-gate and install-gate records) belong together: one per
// first-release target, the same source revision and version, every gate with
// result pass, every S6 case present, the three macOS cases
// (`upgrade-from-bash`, `private-seed`, `dispatcher`) recorded `ok` on
// `darwin/*` and `n-a` with the first-release reason on the other four targets,
// every portable case recorded `ok`/`intact` on all five, every artifact.sha256
// matching SHA256SUMS when -checksums is provided, and writes support-matrix.md
// when -out (or -checksums) is provided.
func releaseAggregateCmd(args []string) error {
	fl := flag.NewFlagSet("release-aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	verFlag := fl.String("version", "", "expected release version X.Y.Z")
	checksumsFlag := fl.String("checksums", "", "optional path to dist/SHA256SUMS")
	outFlag := fl.String("out", "", "optional output path for support-matrix.md")
	runIDFlag := fl.String("run-id", "", "optional GitHub Actions workflow run ID")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("release-aggregate needs -source-revision")
	}
	wantVer := strings.TrimPrefix(strings.TrimSpace(*verFlag), "v")
	files := fl.Args()

	var problems []string
	var sumsByName map[string]string
	if *checksumsFlag != "" {
		rawSums, err := os.ReadFile(*checksumsFlag)
		if err != nil {
			problems = append(problems, fmt.Sprintf("read %s: %v", filepath.Base(*checksumsFlag), err))
		} else {
			sumsByName = map[string]string{}
			for _, line := range strings.Split(strings.TrimSpace(string(rawSums)), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				sha, name, ok := strings.Cut(line, "  ")
				if !ok || len(sha) != 64 {
					problems = append(problems, "malformed SHA256SUMS line: "+line)
					continue
				}
				sumsByName[name] = sha
			}
			if len(sumsByName) != len(firstReleaseTargets) {
				problems = append(problems, fmt.Sprintf("SHA256SUMS has %d entries, want %d", len(sumsByName), len(firstReleaseTargets)))
			}
		}
	}

	byTarget := map[string]ReleaseEvidence{}
	byProvider := map[string]ProviderEvidence{}
	byInstall := map[string]InstallEvidence{}
	byAuxKind := map[string]map[string]InstallEvidence{}
	releaseCount := 0
	observedVersion := wantVer

	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var base InstallEvidence
		if err := json.Unmarshal(b, &base); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		switch base.Kind {
		case releaseEvidenceKind:
			releaseCount++
			var ev ReleaseEvidence
			if err := json.Unmarshal(b, &ev); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
				continue
			}
			if ev.Mode != releaseMode {
				problems = append(problems, fmt.Sprintf("%s ran in %q mode, not %q", ev.Target, ev.Mode, releaseMode))
			}
			if _, dup := byTarget[ev.Target]; dup {
				problems = append(problems, "two records for "+ev.Target)
			}
			byTarget[ev.Target] = ev
			if ev.SourceRevision != *rev {
				problems = append(problems, fmt.Sprintf("%s ran %q, not %s", ev.Target, ev.SourceRevision, *rev))
			}
			if ev.Version != "" {
				if observedVersion == "" {
					observedVersion = ev.Version
				} else if ev.Version != observedVersion {
					problems = append(problems, fmt.Sprintf("%s has version %q, want %q", ev.Target, ev.Version, observedVersion))
				}
			} else if wantVer != "" {
				problems = append(problems, fmt.Sprintf("%s is missing version (want %q)", ev.Target, wantVer))
			}
			if sumsByName != nil {
				wantSHA, ok := sumsByName[ev.Artifact.Name]
				if !ok || ev.Artifact.Name == "" {
					problems = append(problems, fmt.Sprintf("%s artifact %q not in SHA256SUMS", ev.Target, ev.Artifact.Name))
				} else if ev.Artifact.SHA256 != wantSHA {
					problems = append(problems, fmt.Sprintf("%s artifact.sha256 %q != SHA256SUMS %q", ev.Target, ev.Artifact.SHA256, wantSHA))
				}
			}
			wantCompat := "not_applicable"
			if strings.HasPrefix(ev.Target, "darwin/") {
				wantCompat = "pass"
			}
			if ev.Compat.Status != wantCompat {
				problems = append(problems, fmt.Sprintf("%s f01_compatibility = %q, want %q", ev.Target, ev.Compat.Status, wantCompat))
			}
			if ev.Emulated == nil || *ev.Emulated {
				problems = append(problems, fmt.Sprintf("%s does not carry emulated: false from its native platform probe", ev.Target))
			}
			if ev.Toolchain != runtime.Version() {
				problems = append(problems, fmt.Sprintf("%s was built with toolchain %q, want %q", ev.Target, ev.Toolchain, runtime.Version()))
			}
			if ev.Result != "pass" || len(ev.Failures) > 0 {
				problems = append(problems, ev.Target+" did not pass")
			}
			seenCases := map[string]bool{}
			for _, c := range ev.Cases {
				if seenCases[c.Case] {
					problems = append(problems, fmt.Sprintf("%s: duplicate case %s", ev.Target, c.Case))
				}
				seenCases[c.Case] = true
				if !c.OK || c.Observed != c.Expected {
					problems = append(problems, fmt.Sprintf("%s: case %s is %s (expected %s)", ev.Target, c.Case, c.Observed, c.Expected))
					continue
				}
				if isMacOSOnlyReleaseCase(c.Case) {
					if strings.HasPrefix(ev.Target, "darwin/") {
						if c.Observed != "ok" {
							problems = append(problems, fmt.Sprintf("%s: case %s is %q, want \"ok\" on darwin", ev.Target, c.Case, c.Observed))
						}
					} else {
						if c.Observed != "n-a" {
							problems = append(problems, fmt.Sprintf("%s: case %s is %q, want \"n-a\" outside darwin", ev.Target, c.Case, c.Observed))
						}
						if c.Detail != releaseFirstReleaseReason {
							problems = append(problems, fmt.Sprintf("%s: case %s is \"n-a\" with reason %q, want %q", ev.Target, c.Case, c.Detail, releaseFirstReleaseReason))
						}
					}
					continue
				}
				if c.Observed == "n-a" {
					problems = append(problems, fmt.Sprintf("%s: case %s is \"n-a\"; only upgrade-from-bash, private-seed and dispatcher may be n-a", ev.Target, c.Case))
				}
			}

		case providerEvidenceKind:
			var pev ProviderEvidence
			if err := json.Unmarshal(b, &pev); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
				continue
			}
			if _, dup := byProvider[pev.Target]; dup {
				problems = append(problems, "two provider records for "+pev.Target)
			}
			byProvider[pev.Target] = pev
			if pev.SourceRevision != *rev {
				problems = append(problems, fmt.Sprintf("provider-gate %s ran %q, not %s", pev.Target, pev.SourceRevision, *rev))
			}
			if pev.Result != "pass" || len(pev.Failures) > 0 {
				problems = append(problems, "provider-gate "+pev.Target+" did not pass")
			}

		case installEvidenceKind:
			if _, dup := byInstall[base.Target]; dup {
				problems = append(problems, "two install records for "+base.Target)
			}
			byInstall[base.Target] = base
			if base.SourceRevision != *rev {
				problems = append(problems, fmt.Sprintf("install-gate %s ran %q, not %s", base.Target, base.SourceRevision, *rev))
			}
			if base.Result != "pass" || len(base.Failures) > 0 {
				problems = append(problems, "install-gate "+base.Target+" did not pass")
			}

		case configEvidenceKind, fleetEvidenceKind:
			if byAuxKind[base.Kind] == nil {
				byAuxKind[base.Kind] = map[string]InstallEvidence{}
			}
			if _, dup := byAuxKind[base.Kind][base.Target]; dup {
				problems = append(problems, fmt.Sprintf("two %s records for %s", base.Kind, base.Target))
			}
			byAuxKind[base.Kind][base.Target] = base
			if base.SourceRevision != *rev {
				problems = append(problems, fmt.Sprintf("%s %s ran %q, not %s", base.Kind, base.Target, base.SourceRevision, *rev))
			}
			if base.Result != "pass" || len(base.Failures) > 0 {
				problems = append(problems, fmt.Sprintf("%s %s did not pass", base.Kind, base.Target))
			}

		default:
			problems = append(problems, fmt.Sprintf("%s is a %q record, not %q", base.Target, base.Kind, releaseEvidenceKind))
		}
	}

	// A published support matrix claims providers and delivery, so it needs
	// the provider and install gate records of every target.
	writeMatrix := *outFlag != "" || *checksumsFlag != ""
	for _, t := range firstReleaseTargets {
		ev, ok := byTarget[t]
		if !ok {
			problems = append(problems, "no evidence for "+t)
			continue
		}
		for _, name := range releaseCaseNames {
			if !releaseCasePresent(ev, name) {
				problems = append(problems, fmt.Sprintf("%s: no case for %s", t, name))
			}
		}
		if len(byProvider) > 0 || writeMatrix {
			if _, ok := byProvider[t]; !ok {
				problems = append(problems, "no provider-gate evidence for "+t)
			}
		}
		if len(byInstall) > 0 || writeMatrix {
			if _, ok := byInstall[t]; !ok {
				problems = append(problems, "no install-gate evidence for "+t)
			}
		}
		for kind, m := range byAuxKind {
			if _, ok := m[t]; !ok {
				problems = append(problems, fmt.Sprintf("no %s evidence for %s", kind, t))
			}
		}
	}
	if releaseCount != len(firstReleaseTargets) {
		problems = append(problems, fmt.Sprintf("%d release records, want %d", releaseCount, len(firstReleaseTargets)))
	}

	sort.Strings(problems)
	printReleaseMatrix(byTarget)
	fmt.Printf("\nsource revision `%s`\n", *rev)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("FAIL:", p)
		}
		return errors.New("release-aggregate failed")
	}

	outPath := *outFlag
	if outPath == "" && *checksumsFlag != "" {
		outPath = "support-matrix.md"
	}
	if outPath != "" {
		matrixMD := renderSupportMatrix(observedVersion, *rev, *runIDFlag, byTarget, byProvider, byInstall)
		if err := os.WriteFile(outPath, []byte(matrixMD), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// renderSupportMatrix renders the D8 support-matrix.md document from the
// observed release, provider and install gate evidence records.
func renderSupportMatrix(version, rev, runID string, byRelease map[string]ReleaseEvidence, byProvider map[string]ProviderEvidence, byInstall map[string]InstallEvidence) string {
	ver := version
	if ver == "" {
		for _, tg := range firstReleaseTargets {
			if v := byRelease[tg].Version; v != "" {
				ver = v
				break
			}
		}
	}
	if ver == "" {
		ver = "0.9.0"
	}

	matrixTargets := []string{
		"darwin/arm64",
		"linux/amd64",
		"linux/arm64",
		"windows/amd64",
		"windows/arm64",
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Support Matrix (`v%s`)\n\n", ver)
	fmt.Fprintf(&b, "- **Version**: `v%s`\n", ver)
	fmt.Fprintf(&b, "- **Source revision**: `%s`\n", rev)
	if runID != "" {
		fmt.Fprintf(&b, "- **Workflow run**: `%s`\n", runID)
	}
	b.WriteString("\n")
	b.WriteString("Every cell in this matrix is generated by `mm-walkthrough release-aggregate` from the per-target evidence records of the release workflow run (`release-gate-*.json`, `provider-gate-*.json`, `install-gate-*.json`) and obeys one rule: **`tested` only when a native runner job in that run proved it; `refused by design` when the product refuses that path and a test proved the refusal; `n-a` when the capability does not exist on that OS; `untested` when no native runner job ran it**.\n\n")
	b.WriteString("| Target | Minimum OS | Hosted runner and observed OS version | Delivery | Legacy Bash routes | Providers (user / custom / global) | Upgrade path tested | Evidence |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")

	for _, tg := range matrixTargets {
		ev := byRelease[tg]
		osVer := strings.TrimSpace(ev.OSVersion)
		if osVer == "" {
			osVer = "observed in evidence"
		}
		arcName := ev.Artifact.Name
		if arcName == "" {
			arcName = targetArchiveName(ver, tg)
		}
		slug := strings.ReplaceAll(tg, "/", "-")

		var minOS, runnerCol, deliveryCol, legacyCol, provCol, upgradeCol string
		switch tg {
		case "darwin/arm64":
			minOS = "macOS 15 (`depends_on macos: :sequoia`, `depends_on arch: :arm64`)"
			runnerCol = fmt.Sprintf("`macos-15` (%s)", osVer)
			deliveryCol = fmt.Sprintf("`Formula/managed-machine.rb` + `core` resource (`%s`; `install-core.sh` refused by design, exit 2)", arcName)
			legacyCol = "`tested`: routed by `bin/managed-machine` under compatibility B; F01 goldens pass"
			provCol = renderProviderCell(byProvider[tg])
			upgradeCol = fmt.Sprintf("`tested`: `v0.8.12` -> `v%s` -> `v0.8.12` -> `v%s` via `brew install`/`upgrade` of derived formula, plus `%s` -> `%s+gate` -> `%s`", ver, ver, ver, ver, ver)

		case "linux/amd64":
			minOS = "Ubuntu 24.04 LTS (`glibc` >= 2.39; static binary `CGO_ENABLED=0`)"
			runnerCol = fmt.Sprintf("`ubuntu-24.04` (%s)", osVer)
			deliveryCol = fmt.Sprintf("`tested`: `install-core.sh` + `%s`", arcName)
			legacyCol = "`refused by design`: `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2)"
			provCol = renderProviderCell(byProvider[tg])
			upgradeCol = fmt.Sprintf("`tested`: `%s` -> `%s+gate` -> `%s` (first release on Linux; no earlier release exists)", ver, ver, ver)

		case "linux/arm64":
			minOS = "Ubuntu 24.04 LTS (`glibc` >= 2.39; static binary `CGO_ENABLED=0`)"
			runnerCol = fmt.Sprintf("`ubuntu-24.04-arm` (%s)", osVer)
			deliveryCol = fmt.Sprintf("`tested`: `install-core.sh` + `%s`", arcName)
			legacyCol = "`refused by design`: `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2)"
			provCol = renderProviderCell(byProvider[tg])
			upgradeCol = fmt.Sprintf("`tested`: `%s` -> `%s+gate` -> `%s` (first release on Linux; no earlier release exists)", ver, ver, ver)

		case "windows/amd64":
			minOS = "Windows 11 24H2 / Windows Server 2025"
			runnerCol = fmt.Sprintf("`windows-2025` (%s; server stand-in per S1)", osVer)
			deliveryCol = fmt.Sprintf("`tested`: `install-core.ps1` + `%s`", arcName)
			legacyCol = "`refused by design`: `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2)"
			provCol = renderProviderCell(byProvider[tg])
			upgradeCol = fmt.Sprintf("`tested`: `%s` -> `%s+gate` -> `%s` (first release on Windows; no earlier release exists)", ver, ver, ver)

		case "windows/arm64":
			minOS = "Windows 11 24H2 (`arm64`)"
			runnerCol = fmt.Sprintf("`windows-11-arm` (%s)", osVer)
			deliveryCol = fmt.Sprintf("`tested`: `install-core.ps1` + `%s`", arcName)
			legacyCol = "`refused by design`: `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2)"
			provCol = renderProviderCell(byProvider[tg])
			upgradeCol = fmt.Sprintf("`tested`: `%s` -> `%s+gate` -> `%s` (first release on Windows; no earlier release exists)", ver, ver, ver)
		}

		evCol := fmt.Sprintf("`release-gate-%s.json`, `provider-gate-%s.json`, `install-gate-%s.json`", slug, slug, slug)
		if _, ok := byInstall[tg]; !ok {
			deliveryCol = "`untested`: no install-gate evidence"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s | %s | %s | %s |\n",
			tg, minOS, runnerCol, deliveryCol, legacyCol, provCol, upgradeCol, evCol)
	}

	b.WriteString("\n## Untested and Unsupported Surfaces\n\n")
	b.WriteString("Rows explicitly declared **`untested`** (never claimed as supported without native CI evidence):\n\n")
	b.WriteString("- `darwin/amd64` (Intel macOS): **not a target**; `Formula/managed-machine.rb` declares `depends_on arch: :arm64` and `install.sh` refuses non-`arm64` Macs.\n")
	b.WriteString("- macOS 14 Sonoma and earlier: **not supported**; `Formula/managed-machine.rb` declares `depends_on macos: :sequoia`.\n")
	b.WriteString("- Other Linux distributions (Debian, Fedora, Alpine/musl, Arch, NixOS): **`untested`**. The Linux binaries are static (`CGO_ENABLED=0`), so the core and `file`/`line`/`block`/`symlink`/`archive`/`git` providers have no libc runtime dependency, while external-tool providers (`npm`, `uv`, `cargo`, `go`, `brew`) depend on those tools being on `PATH`; no CI job runs outside Ubuntu 24.04.\n")
	b.WriteString("- Windows 11 `x64` workstation proper: covered by the `windows-2025` Server stand-in per the S1 runner note; desktop-specific behavior outside the tested surface is **`untested`**.\n")
	b.WriteString("- `install.sh` (the macOS curl bootstrap that clones to `~/managed-machine` and runs `scripts/bootstrap`) and `managed-machine --update` (which refuses agent/CI harnesses): **`untested` in CI** by design; verified by unit/characterization tests (`tests/install.test.sh`, `tests/update-refusal.test.sh`) and human runs.\n")
	return b.String()
}

func releaseCasePresent(ev ReleaseEvidence, name string) bool {
	for _, c := range ev.Cases {
		if c.Case == name {
			return true
		}
	}
	return false
}

func printReleaseMatrix(byTarget map[string]ReleaseEvidence) {
	fmt.Printf("| case | %s |\n", strings.Join(firstReleaseTargets, " | "))
	fmt.Printf("|---|%s\n", strings.Repeat("---|", len(firstReleaseTargets)))
	for _, name := range releaseCaseNames {
		fmt.Printf("| %s |", name)
		for _, t := range firstReleaseTargets {
			fmt.Printf(" %s |", releaseCell(byTarget, name, t))
		}
		fmt.Println()
	}
}

func releaseCell(byTarget map[string]ReleaseEvidence, name, target string) string {
	ev, ok := byTarget[target]
	if !ok {
		return "FAIL"
	}
	seen, failed := false, false
	observed := ""
	for _, c := range ev.Cases {
		if c.Case != name {
			continue
		}
		seen = true
		observed = c.Observed
		if !c.OK || c.Observed != c.Expected {
			failed = true
		}
		if isMacOSOnlyReleaseCase(name) {
			if strings.HasPrefix(target, "darwin/") && c.Observed != "ok" {
				failed = true
			}
			if !strings.HasPrefix(target, "darwin/") && (c.Observed != "n-a" || c.Detail != releaseFirstReleaseReason) {
				failed = true
			}
		} else if c.Observed == "n-a" {
			failed = true
		}
	}
	if !seen || failed {
		return "FAIL"
	}
	if isMacOSOnlyReleaseCase(name) && observed == "n-a" {
		return "n-a"
	}
	return "ok"
}

// pinnedToolchain returns the Go toolchain the release is built with, as
// runtime.Version() reports it: "go" plus the go directive of go.mod.
func pinnedToolchain(repoRoot string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "go "); ok && v != "" {
			return "go" + strings.TrimSpace(v), nil
		}
	}
	return "", errors.New("go.mod has no go directive")
}

// renderProviderCell derives one provider cell from the combinations the
// provider gate recorded for the target: every provider it ran, with the
// target kinds and what was observed. Without a record the cell is
// untested; the matrix claims nothing the evidence does not carry.
func renderProviderCell(pev ProviderEvidence) string {
	if len(pev.Combinations) == 0 {
		return "`untested`: no provider-gate evidence"
	}
	var order []string
	kinds := map[string][]string{}
	for _, c := range pev.Combinations {
		if !c.OK {
			return "FAIL: provider " + c.Provider + " (" + c.TargetKind + ")"
		}
		if _, seen := kinds[c.Provider]; !seen {
			order = append(order, c.Provider)
		}
		kinds[c.Provider] = append(kinds[c.Provider], c.TargetKind+": "+c.Observed)
	}
	parts := make([]string, 0, len(order))
	for _, p := range order {
		parts = append(parts, fmt.Sprintf("`%s` (%s)", p, strings.Join(kinds[p], ", ")))
	}
	return strings.Join(parts, "; ")
}
