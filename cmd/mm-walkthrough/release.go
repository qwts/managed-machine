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
	"github.com/qwts/managed-machine/internal/plan"
)

// The S6 release gate (issue #332; decisions D5, D6, D7, D9, D10) drives a
// built managed-machine binary and release dist directory through the S6
// release migration cases on all five first-release targets:
//   - upgrade-from-bash (darwin/arm64; n-a on Linux and Windows)
//   - private-seed      (darwin/arm64; n-a on Linux and Windows)
//   - dispatcher        (darwin/arm64; n-a on Linux and Windows)
//   - upgrade           (all five targets)
//   - recovery          (all five targets)
//   - guards            (all five targets)

const (
	releaseEvidenceKind = "s6-release-gate"
	releaseMode         = "release"
)

// ReleaseEvidence is the per-target record of the S6 release gate: the shared
// InstallEvidence base plus the S6 case matrix. The field order is the
// serialized key order.
type ReleaseEvidence struct {
	InstallEvidence
	Cases []ReleaseCase `json:"cases"`
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
	caseReleaseUpgradeFromBash = "upgrade-from-bash"
	caseReleasePrivateSeed     = "private-seed"
	caseReleaseDispatcher      = "dispatcher"
	caseReleaseUpgrade         = "upgrade"
	caseReleaseRecovery        = "recovery"
	caseReleaseGuards          = "guards"
)

var releaseCaseNames = []string{
	caseReleaseUpgradeFromBash,
	caseReleasePrivateSeed,
	caseReleaseDispatcher,
	caseReleaseUpgrade,
	caseReleaseRecovery,
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

func newReleaseHarness(bin, target, revision, distFlag string, raw []byte) (*releaseHarness, error) {
	repoRoot, err := findReleaseRepoRoot()
	if err != nil {
		return nil, err
	}
	h, err := newInstallHarnessWith(bin, target, revision, raw, installHarnessOptions{
		sentinels:   releaseSentinels,
		definitions: writeReleaseDefinitions,
		extraDirs:   []string{"desired", "upgrade", "recovery", "keydir", "repos", "formulae", "staged", "bash-home", "bash-stubs"},
		homeSubdir:  "home dir ü 日本",
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

	rh := &releaseHarness{
		installHarness:    h,
		cases:             []ReleaseCase{},
		account:           account,
		repoRoot:          repoRoot,
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

	if err := rh.prepareDist(distFlag); err != nil {
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
// and the target archive(s). For macOS upgrade-from-bash, if the repository
// VERSION is still 0.8.12 (pre-release PR), upgradeVer is set to 0.9.0 so
// Homebrew upgrades from 0.8.12 to 0.9.0 and retains the 0.8.12 keg.
func (h *releaseHarness) prepareDist(distFlag string) error {
	if distFlag != "" {
		absDist, err := filepath.Abs(distFlag)
		if err != nil {
			return err
		}
		ver, err := readDistVersion(absDist, h.target)
		if err != nil {
			return err
		}
		archivePath := filepath.Join(absDist, targetArchiveName(ver, h.target))
		distBinBytes, err := extractArchiveBinaryBytes(archivePath)
		if err != nil {
			return fmt.Errorf("extract %s from -dist: %w", filepath.Base(archivePath), err)
		}
		if sum(distBinBytes) != h.ev.BinarySHA256 {
			return fmt.Errorf("-dist binary digest (%s) does not match -binary digest (%s)", sum(distBinBytes)[:12], h.ev.BinarySHA256[:12])
		}
		h.distDir = absDist
		h.releaseVer = ver
	} else {
		vRaw, err := os.ReadFile(filepath.Join(h.repoRoot, "VERSION"))
		if err != nil {
			return err
		}
		ver := strings.TrimSpace(string(vRaw))
		if ver == "" {
			ver = "0.9.0"
		}
		distDir := filepath.Join(h.root, "dist")
		if err := buildTargetDist(h.repoRoot, ver, h.target, distDir); err != nil {
			return err
		}
		h.distDir = distDir
		h.releaseVer = ver
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
	cmd := exec.Command("go", "build", "-modcacherw", "-buildvcs=false", "-ldflags", ldflags, "-o", outPath, "./cmd/managed-machine")
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
	bin := fl.String("binary", "", "managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	dist := fl.String("dist", "", "optional release dist directory containing SHA256SUMS and archives")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *bin == "" || *tgt == "" || *out == "" {
		return errors.New("release needs -binary, -target and -evidence")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	h, b, err := runReleaseGate(absBin, *tgt, *rev, *dist)
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
	raw, err := os.ReadFile(bin)
	if err != nil {
		return nil, nil, err
	}
	h, err := newReleaseHarness(bin, target, revision, distFlag, raw)
	if err != nil {
		return nil, nil, err
	}
	defer h.cleanupBrew()
	h.run()
	ev := ReleaseEvidence{InstallEvidence: *h.ev, Cases: h.cases}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return h, nil, err
	}
	b = h.scrubReleaseEvidence(b)
	if leak := h.grepEvidenceBytes(b); leak != "" {
		h.step("evidence zero-hits grep", leak, false)
		h.setGuardsFailure(leak)
		h.finish()
		ev = ReleaseEvidence{InstallEvidence: *h.ev, Cases: h.cases}
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

func (h *releaseHarness) run() {
	if !h.probe() {
		h.finish()
		return
	}
	h.runUpgradeFromBash()
	h.runPrivateSeed()
	h.runDispatcher()
	h.runUpgrade()
	h.runRecovery()
	h.runGuards()
	h.finish()
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

// releaseAggregateCmd checks that five S6 release-gate records belong
// together: one per first-release target, the same source revision, every
// gate in release mode with result pass, every S6 case present, the three
// macOS cases (`upgrade-from-bash`, `private-seed`, `dispatcher`) recorded
// `ok` on `darwin/*` and `n-a` with the first-release reason on the other four
// targets, and `upgrade`, `recovery` and `guards` recorded `ok`/`intact` on
// all five.
func releaseAggregateCmd(args []string) error {
	fl := flag.NewFlagSet("release-aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("release-aggregate needs -source-revision")
	}
	files := fl.Args()
	var problems []string
	byTarget := map[string]ReleaseEvidence{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var ev ReleaseEvidence
		if err := json.Unmarshal(b, &ev); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		if ev.Kind != releaseEvidenceKind {
			problems = append(problems, fmt.Sprintf("%s is a %q record, not %q", ev.Target, ev.Kind, releaseEvidenceKind))
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
	}
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
	}
	if len(files) != len(firstReleaseTargets) {
		problems = append(problems, fmt.Sprintf("%d records, want %d", len(files), len(firstReleaseTargets)))
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
	return nil
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
