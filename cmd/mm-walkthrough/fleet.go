package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/config/provider/block"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/status"
)

// The S5 fleet gate (issue #319) drives the built managed-machine binary
// through the D9 fleet-access case list in an isolated harness on all five
// first-release targets, then aggregates the five per-target records in the
// s5-gate workflow job. It has no refusal mode: fleet touches only files in
// the harness, so every hosted runner and local test runs the full case list
// natively.

// fleetEvidenceKind names the evidence record of the S5 fleet gate.
const fleetEvidenceKind = "s5-fleet-gate"

// fleetMode is the mode recorded on every S5 fleet gate run.
const fleetMode = "fleet"

// FleetEvidence is the per-target record of the S5 fleet gate: the shared
// InstallEvidence base plus the D9 case matrix. The field order is the
// serialized key order.
type FleetEvidence struct {
	InstallEvidence
	Cases []FleetCase `json:"cases"`
}

// FleetCase is one D9 fleet case on one target. The field order is the
// serialized key order.
type FleetCase struct {
	Case     string `json:"case"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
}

// D9 case names in the fixed order the gate runs and the aggregate renders.
const (
	caseFleetDirectoryPath                = "directory-path"
	caseFleetDirectoryGist                = "directory-gist"
	caseFleetPublicationGrantsNothing     = "publication-grants-nothing"
	caseFleetRecordedFingerprintUntrusted = "recorded-fingerprint-untrusted"
	caseFleetApprovalIndependent          = "approval-independent-of-publication"
	caseFleetRevocationPolicy             = "revocation-policy"
	caseFleetRevocationDirectory          = "revocation-directory"
	caseFleetACT04Cycle                   = "ac-t-04-cycle"
	caseFleetCheckExits                   = "check-exits"
	caseFleetConflictForeignBlock         = "conflict-foreign-block"
	caseFleetLegacyTransition             = "legacy-transition"
	caseFleetPublishStage                 = "publish-stage"
	caseFleetStatusFleet                  = "status-fleet"
	caseFleetLegacyCutoverRefusal         = "legacy-cutover-refusal"
	caseFleetGuards                       = "guards"
)

var fleetCaseNames = []string{
	caseFleetDirectoryPath,
	caseFleetDirectoryGist,
	caseFleetPublicationGrantsNothing,
	caseFleetRecordedFingerprintUntrusted,
	caseFleetApprovalIndependent,
	caseFleetRevocationPolicy,
	caseFleetRevocationDirectory,
	caseFleetACT04Cycle,
	caseFleetCheckExits,
	caseFleetConflictForeignBlock,
	caseFleetLegacyTransition,
	caseFleetPublishStage,
	caseFleetStatusFleet,
	caseFleetLegacyCutoverRefusal,
	caseFleetGuards,
}

// fleetSentinels are the SSH, GitHub, git, elevation, keychain, directory
// service and package-manager programs the fleet gate must never start.
var fleetSentinels = []string{
	"ssh-keygen", "ssh-add", "ssh", "gh", "git",
	"osascript", "sudo", "security", "dscl",
	"gpg", "gpg2", "brew", "winget", "npm", "npx", "uv", "uvx", "cargo", "go", "pip", "pip3", "pipx",
}

// Synthetic OpenSSH private-key fixture fragments used only to verify that
// `fleet --publish --key` rejects private-key files with MM_FLEET_KEY_INVALID
// without echoing any bytes and that zero private-key bytes appear anywhere in
// the state directory, harness home, captures or evidence record.
const (
	fleetPrivateKeyHeader = "-----BEGIN OPENSSH PRIVATE KEY-----"
	fleetPrivateKeyBody   = "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZWQyNTUxOQAAACC00000000000000000000000000000000000000000" // gitleaks:allow
	fleetPrivateKeyFooter = "-----END OPENSSH PRIVATE KEY-----"
)

const (
	fleetGistID       = "0123456789abcdef0123456789abcdef"
	fleetGistRevision = "1111111111111111111111111111111111111111"
	fleetPublishedAt  = "2026-10-22T00:00:00Z"
	fleetCutoverAt    = "2026-10-22T00:00:00Z"
	fleetUserOwnedKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIUserOwnedLineStayByteIdentical0000000000000 user@workstation"
)

// fleetKeyPair holds the public half of an in-process Ed25519 key pair. The
// private half is zeroed immediately after generation and never written to
// disk; its seed encodings are added to the zero-hits forbidden list.
type fleetKeyPair struct {
	keyType     string
	base64Blob  string
	pubLine     string
	fingerprint string
	machineID   string
	seedHex     string
	seedB64     string
}

// generateFleetKey generates an Ed25519 key pair in-process using
// crypto/ed25519, builds the OpenSSH wire-format public key line, computes
// its SHA-256 fingerprint via directory.ParsePublicKey, and zeroes the
// private key bytes in memory.
func generateFleetKey(comment string) (fleetKeyPair, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return fleetKeyPair{}, err
	}
	seed := priv.Seed()
	seedHex := hex.EncodeToString(seed)
	seedB64 := base64.StdEncoding.EncodeToString(seed)
	for i := range priv {
		priv[i] = 0
	}
	for i := range seed {
		seed[i] = 0
	}

	const keyType = directory.KeyTypeEd25519
	wire := make([]byte, 0, 4+len(keyType)+4+len(pub))
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(keyType)))
	wire = append(wire, lenBuf[:]...)
	wire = append(wire, keyType...)
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pub)))
	wire = append(wire, lenBuf[:]...)
	wire = append(wire, pub...)

	b64 := base64.StdEncoding.EncodeToString(wire)
	line := keyType + " " + b64
	if comment != "" {
		line += " " + comment
	}
	pk, err := directory.ParsePublicKey(line)
	if err != nil {
		return fleetKeyPair{}, err
	}
	return fleetKeyPair{
		keyType:     pk.KeyType,
		base64Blob:  pk.Base64Blob,
		pubLine:     line,
		fingerprint: pk.Fingerprint,
		machineID:   pk.MachineID,
		seedHex:     seedHex,
		seedB64:     seedB64,
	}, nil
}

// fleetHarness is the isolated environment of the S5 fleet gate.
type fleetHarness struct {
	*installHarness
	cases              []FleetCase
	machine            map[string]string
	outsideContainment map[string]string
	account            string
	compatPath         string
	importBin          string
	policyDir          string
	keyDir             string
	pubDir             string
	desired            string
	unreadableDesired  string
	unreadableDest     string
	authKeys           string
	legacyCheckout     string
	importOut          string
	captures           [][]byte
	forbiddenStrings   []string

	aliceKey1     fleetKeyPair
	aliceKey2     fleetKeyPair
	bobKey1       fleetKeyPair
	tamperedKey   fleetKeyPair
	malloryKey    fleetKeyPair
	spoofKey      fleetKeyPair
	unpubKey      fleetKeyPair
	legacyKey     fleetKeyPair
	importGoodKey fleetKeyPair
}

func writeFleetDefinitions(_ string) error {
	return nil
}

// newFleetHarness builds the isolated environment for one S5 fleet gate run:
// a fresh home with spaces and non-ASCII, state directory, fixture policy
// directory (plain directory, no git), desired configuration mapping
// authorized_keys into the harness home, sentinel binaries, and in-process
// Ed25519 keys.
func newFleetHarness(bin, target, revision, compatPath, importBin string, raw []byte) (*fleetHarness, error) {
	h, err := newInstallHarnessWith(bin, target, revision, raw, installHarnessOptions{
		sentinels:   fleetSentinels,
		definitions: writeFleetDefinitions,
		extraDirs:   []string{"desired", "policy", "keydir", "pubs", "legacy-checkout"},
		homeSubdir:  "home dir ü 日本",
	})
	if err != nil {
		return nil, err
	}
	h.ev.Kind = fleetEvidenceKind
	h.ev.Mode = fleetMode

	account, err := fleet.DefaultUsername(runtime.GOOS)
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	policyDir := filepath.Join(h.root, "policy")
	keyDir := filepath.Join(h.root, "keydir")
	pubDir := filepath.Join(h.root, "pubs")
	desiredPath := filepath.Join(h.root, "desired", "user-desired.yaml")
	unreadableDesired := filepath.Join(h.root, "desired", "unreadable-desired.yaml")
	unreadableDest := filepath.Join(h.root, "desired", "unreadable-dir-dest")
	authKeys := filepath.Join(h.home, ".ssh", "authorized_keys")
	legacyCheckout := filepath.Join(h.root, "legacy-checkout")
	importOut := filepath.Join(h.root, "import-out")

	h.env = append(h.env,
		policy.PolicyEnv+"="+policyDir,
		config.DesiredEnv+"="+desiredPath,
	)

	fh := &fleetHarness{
		installHarness:    h,
		cases:             []FleetCase{},
		account:           account,
		compatPath:        compatPath,
		importBin:         importBin,
		policyDir:         policyDir,
		keyDir:            keyDir,
		pubDir:            pubDir,
		desired:           desiredPath,
		unreadableDesired: unreadableDesired,
		unreadableDest:    unreadableDest,
		authKeys:          authKeys,
		legacyCheckout:    legacyCheckout,
		importOut:         importOut,
		forbiddenStrings: []string{
			fleetPrivateKeyHeader,
			fleetPrivateKeyBody,
			fleetPrivateKeyFooter,
		},
	}

	if err := fh.generateKeys(); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	if err := fh.writeDesiredFiles(); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	if err := fh.writeLegacyImportFixtures(); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	machine, err := fh.fleetMachineSnapshot()
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	fh.machine = machine

	outside, err := fh.staticHarnessSnapshot()
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	fh.outsideContainment = outside

	return fh, nil
}

func (h *fleetHarness) generateKeys() error {
	type slot struct {
		comment      string
		unauthorized bool
		dst          *fleetKeyPair
	}
	slots := []slot{
		{"alice@workstation", false, &h.aliceKey1},
		{"alice@laptop", false, &h.aliceKey2},
		{"bob@workstation", false, &h.bobKey1},
		{"tampered@invalid", true, &h.tamperedKey},
		{"mallory@unapproved", true, &h.malloryKey},
		{"spoof@untrusted", true, &h.spoofKey},
		{"unpub@pending", true, &h.unpubKey},
		{"legacy@old-fleet", true, &h.legacyKey},
		{"imported@legacy", false, &h.importGoodKey},
	}
	seenFP := map[string]bool{}
	for _, s := range slots {
		kp, err := generateFleetKey(s.comment)
		if err != nil {
			return err
		}
		if seenFP[kp.fingerprint] {
			return fmt.Errorf("duplicate generated key fingerprint %s", kp.fingerprint)
		}
		seenFP[kp.fingerprint] = true
		*s.dst = kp
		h.forbiddenStrings = append(h.forbiddenStrings, kp.seedHex, kp.seedB64)
		if s.unauthorized {
			h.forbiddenStrings = append(h.forbiddenStrings, kp.base64Blob)
		}
	}
	return nil
}

func (h *fleetHarness) writeDesiredFiles() error {
	if err := os.MkdirAll(h.unreadableDest, 0o755); err != nil {
		return err
	}
	userDesired := fmt.Sprintf(`target: user
fleet:
  destinations:
    authorized_keys: %q
`, filepath.ToSlash(h.authKeys))
	if err := write(h.desired, userDesired); err != nil {
		return err
	}
	unreadable := fmt.Sprintf(`target: user
fleet:
  destinations:
    authorized_keys: %q
`, filepath.ToSlash(h.unreadableDest))
	return write(h.unreadableDesired, unreadable)
}

func (h *fleetHarness) writeLegacyImportFixtures() error {
	m1 := fmt.Sprintf(`schema_version = 1
machine_id = %q
hostname = "alpha-box"
managed_at = %q
ssh_key_fingerprint = %q
public_key = %q
`, h.importGoodKey.machineID, fleetPublishedAt, h.importGoodKey.fingerprint, h.importGoodKey.pubLine)
	if err := write(filepath.Join(h.legacyCheckout, "fleet", "machines", "alpha.toml"), m1); err != nil {
		return err
	}
	// beta.toml records aliceKey1's fingerprint against legacyKey's public key
	// bytes, so mm-fleet-import must report fingerprint_match=false and exclude
	// it from both output YAML files.
	m2 := fmt.Sprintf(`schema_version = 1
machine_id = %q
hostname = "beta-tampered"
managed_at = %q
ssh_key_fingerprint = %q
public_key = %q
`, h.legacyKey.machineID, fleetPublishedAt, h.aliceKey1.fingerprint, h.legacyKey.pubLine)
	return write(filepath.Join(h.legacyCheckout, "fleet", "machines", "beta.toml"), m2)
}

// fleetMachineSnapshot hashes the runner's real ~/.ssh, shell profiles,
// ~/.config/managed-machine and platform state directory (without the
// isolated harness home).
func (h *fleetHarness) fleetMachineSnapshot() (map[string]string, error) {
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

// staticHarnessSnapshot hashes the static fixture directories inside h.root
// (desired, defs, sentinels, legacy-checkout) to verify that core invocations
// never mutate files outside the destination and state/publication directories.
func (h *fleetHarness) staticHarnessSnapshot() (map[string]string, error) {
	out := map[string]string{}
	for _, rel := range []string{"desired", "defs", "sentinels", "legacy-checkout"} {
		if err := hashTree(filepath.Join(h.root, rel), rel, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// writePolicyConfig writes fleet/key-directory.yaml, fleet/access-groups/ops.yaml
// and optionally fleet/cutover.yaml into h.policyDir.
func (h *fleetHarness) writePolicyConfig(kdSource string, isGist bool, approved []string, withCutover bool) error {
	var kdYAML string
	if isGist {
		kdYAML = fmt.Sprintf("schema_version: 1\nkind: key-directory-source\ngist: %s\n", kdSource)
	} else {
		kdYAML = fmt.Sprintf("schema_version: 1\nkind: key-directory-source\npath: %q\n", filepath.ToSlash(kdSource))
	}
	if err := write(filepath.Join(h.policyDir, filepath.FromSlash(policy.KeyDirectoryRelPath)), kdYAML); err != nil {
		return err
	}

	var fpLines strings.Builder
	if len(approved) == 0 {
		fpLines.WriteString(" []\n")
	} else {
		fpLines.WriteString("\n")
		for _, fp := range approved {
			fmt.Fprintf(&fpLines, "  - %s\n", fp)
		}
	}
	groupYAML := fmt.Sprintf(`schema_version: 1
kind: access-group
id: ops
description: Fleet gate access group.
maintained_by:
  - alice
  - bob
accounts:
  - %s
approved_fingerprints:%s`, h.account, fpLines.String())
	if err := write(filepath.Join(h.policyDir, filepath.FromSlash(policy.AccessGroupsDir), "ops.yaml"), groupYAML); err != nil {
		return err
	}

	cutoverPath := filepath.Join(h.policyDir, filepath.FromSlash(policy.CutoverRelPath))
	if !withCutover {
		_ = os.Remove(cutoverPath)
		return nil
	}
	cutoverYAML := fmt.Sprintf(`schema_version: 1
kind: fleet-cutover
cut_over_at: %q
reviewed_by:
  - alice
note: S5 fleet gate cutover.
`, fleetCutoverAt)
	return write(cutoverPath, cutoverYAML)
}

// writeKeyEntry writes one owner YAML file into h.keyDir/keys/<owner>.yaml.
func (h *fleetHarness) writeKeyEntry(owner, body string) error {
	return write(filepath.Join(h.keyDir, "keys", owner+".yaml"), body)
}

func (h *fleetHarness) cleanKeyDirectory() error {
	keysSub := filepath.Join(h.keyDir, "keys")
	if err := os.RemoveAll(keysSub); err != nil {
		return err
	}
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
  - fingerprint: %s
    key: %q
    label: laptop
    published_at: %q
    revision: 2
`, h.aliceKey1.fingerprint, h.aliceKey1.pubLine, fleetPublishedAt,
		h.aliceKey2.fingerprint, h.aliceKey2.pubLine, fleetPublishedAt)
	if err := h.writeKeyEntry("alice", aliceYAML); err != nil {
		return err
	}
	bobYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: bob
keys:
  - fingerprint: %s
    key: %q
    label: workstation
    published_at: %q
    revision: 1
`, h.bobKey1.fingerprint, h.bobKey1.pubLine, fleetPublishedAt)
	return h.writeKeyEntry("bob", bobYAML)
}

// fleetCmd runs the S5 fleet gate for one target and writes its evidence
// record.
func fleetCmd(args []string) error {
	fl := flag.NewFlagSet("fleet", flag.ContinueOnError)
	bin := fl.String("binary", "", "managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	compat := fl.String("compat", "", "F01 compatibility harness output to embed (macOS)")
	importBin := fl.String("import-binary", "", "optional prebuilt mm-fleet-import binary")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *bin == "" || *tgt == "" || *out == "" {
		return errors.New("fleet needs -binary, -target and -evidence")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	h, b, err := runFleetGate(absBin, *tgt, *rev, *compat, *importBin)
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
		return errors.New("the fleet gate failed")
	}
	return nil
}

// runFleetGate builds the harness for one target, runs every D9 fleet case and
// returns the harness and the scrubbed evidence JSON bytes.
func runFleetGate(bin, target, revision, compatPath, importBin string) (*fleetHarness, []byte, error) {
	raw, err := os.ReadFile(bin)
	if err != nil {
		return nil, nil, err
	}
	h, err := newFleetHarness(bin, target, revision, compatPath, importBin, raw)
	if err != nil {
		return nil, nil, err
	}
	h.run()
	ev := FleetEvidence{InstallEvidence: *h.ev, Cases: h.cases}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return h, nil, err
	}
	b = append(h.paths.scrub(b), '\n')
	if leak := grepFleetBytes(b, "evidence", h.forbiddenStrings); leak != "" {
		h.step("evidence zero-hits grep", leak, false)
		h.setGuardsFailure(leak)
		h.finish()
		ev = FleetEvidence{InstallEvidence: *h.ev, Cases: h.cases}
		b, err = json.MarshalIndent(ev, "", "  ")
		if err != nil {
			return h, nil, err
		}
		b = append(h.paths.scrub(b), '\n')
	}
	return h, b, nil
}

func (h *fleetHarness) runCore(args ...string) (int, []byte, []byte) {
	return h.runCoreEnv(nil, args...)
}

func (h *fleetHarness) runCoreEnv(extra []string, args ...string) (int, []byte, []byte) {
	code, out, errOut := h.coreEnv(extra, args...)
	if len(out) > 0 {
		h.captures = append(h.captures, out)
	}
	if len(errOut) > 0 {
		h.captures = append(h.captures, errOut)
	}
	return code, out, errOut
}

// runTriple runs a read-only or idempotent command in default JSONL, --json
// and --human modes and verifies that all three succeed and agree.
func (h *fleetHarness) runTriple(extra []string, args ...string) ([]record, string) {
	code, out, errOut := h.runCoreEnv(extra, args...)
	if code != 0 || len(errOut) != 0 {
		return nil, fmt.Sprintf("%s exited %d: %s", strings.Join(args, " "), code, firstLineOf(errOut, out))
	}
	recs, err := parseJSONL(out)
	if err != nil {
		return nil, fmt.Sprintf("%s default JSONL: %v", strings.Join(args, " "), err)
	}
	if len(recs) == 0 {
		return nil, fmt.Sprintf("%s produced no records", strings.Join(args, " "))
	}

	jargs := append(append([]string{}, args...), "--json")
	jc, jout, jerrOut := h.runCoreEnv(extra, jargs...)
	if jc != 0 || len(jerrOut) != 0 {
		return nil, fmt.Sprintf("%s --json exited %d: %s", strings.Join(args, " "), jc, firstLineOf(jerrOut, jout))
	}
	jrecs, jerr := parseDocument(jout)
	if jerr != nil {
		return nil, fmt.Sprintf("%s --json: %v", strings.Join(args, " "), jerr)
	}
	if !sameFleetRecords(recs, jrecs) {
		return nil, fmt.Sprintf("%s --json records differ from default JSONL", strings.Join(args, " "))
	}

	hargs := append(append([]string{}, args...), "--human")
	hc, hout, herrOut := h.runCoreEnv(extra, hargs...)
	if hc != 0 || len(herrOut) != 0 {
		return nil, fmt.Sprintf("%s --human exited %d: %s", strings.Join(args, " "), hc, firstLineOf(herrOut, hout))
	}
	if len(bytes.TrimSpace(hout)) == 0 || json.Valid(bytes.TrimSpace(hout)) {
		return nil, fmt.Sprintf("%s --human did not render non-JSON text", strings.Join(args, " "))
	}
	return recs, ""
}

// sameFleetRecords compares two record slices while allowing the live wall-clock
// `at` timestamp on summary or status records to tick between two process
// invocations.
func sameFleetRecords(a, b []record) bool {
	if len(a) != len(b) {
		return false
	}
	stripAt := func(in []record) []record {
		out := make([]record, len(in))
		for i, r := range in {
			cp := make(record, len(r))
			for k, v := range r {
				if k == "at" {
					continue
				}
				cp[k] = v
			}
			out[i] = cp
		}
		return out
	}
	return sameRecords(stripAt(a), stripAt(b))
}

func (h *fleetHarness) recordCase(name, expected, observed string, ok bool, detail string) {
	clean := h.paths.clean(detail)
	h.cases = append(h.cases, FleetCase{
		Case:     name,
		Expected: expected,
		Observed: observed,
		OK:       ok,
		Detail:   clean,
	})
	h.step("case "+name, clean, ok)
}

func (h *fleetHarness) setGuardsFailure(detail string) {
	clean := h.paths.clean(detail)
	for i := range h.cases {
		if h.cases[i].Case == caseFleetGuards {
			h.cases[i].Observed = observedFailed
			h.cases[i].OK = false
			h.cases[i].Detail = clean
			return
		}
	}
}

// run executes the platform probe and all 15 D9 fleet cases in order.
func (h *fleetHarness) run() {
	if !h.probe() {
		h.finish()
		return
	}
	h.runDirectoryPath()
	h.runDirectoryGist()
	h.runPublicationGrantsNothing()
	h.runRecordedFingerprintUntrusted()
	h.runApprovalIndependent()
	h.runRevocationPolicy()
	h.runRevocationDirectory()
	h.runACT04Cycle()
	h.runCheckExits()
	h.runConflictForeignBlock()
	h.runLegacyTransition()
	h.runPublishStage()
	h.runStatusFleet()
	h.runLegacyCutoverRefusal()
	h.runGuards()
	h.finish()
}

// runDirectoryPath proves `directory-path`: two owners listed with computed
// fingerprints, and a tampered recorded fingerprint marked `invalid` with
// `fingerprint-mismatch` and excluded from `--access`.
func (h *fleetHarness) runDirectoryPath() {
	const name = caseFleetDirectoryPath
	// Write alice.yaml with aliceKey1, aliceKey2 and a tampered entry whose
	// recorded fingerprint is aliceKey1's but whose key bytes are tamperedKey's.
	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %q
    label: workstation
    published_at: %q
    revision: 1
  - fingerprint: %s
    key: %q
    label: laptop
    published_at: %q
    revision: 2
  - fingerprint: %s
    key: %q
    label: tampered
    published_at: %q
    revision: 3
`, h.aliceKey1.fingerprint, h.aliceKey1.pubLine, fleetPublishedAt,
		h.aliceKey2.fingerprint, h.aliceKey2.pubLine, fleetPublishedAt,
		h.aliceKey1.fingerprint, h.tamperedKey.pubLine, fleetPublishedAt)
	if err := h.writeKeyEntry("alice", aliceYAML); err != nil {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, err.Error())
		return
	}
	bobYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: bob
keys:
  - fingerprint: %s
    key: %q
    label: workstation
    published_at: %q
    revision: 1
`, h.bobKey1.fingerprint, h.bobKey1.pubLine, fleetPublishedAt)
	if err := h.writeKeyEntry("bob", bobYAML); err != nil {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, err.Error())
		return
	}

	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint, h.tamperedKey.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, err.Error())
		return
	}

	recs, problem := h.runTriple(nil, "fleet", "--keys")
	if problem != "" {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, problem)
		return
	}
	if len(recs) != 5 {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, fmt.Sprintf("want header + 4 key records, got %d", len(recs)))
		return
	}
	if recs[0]["owners"] != float64(2) || recs[0]["keys"] != float64(4) {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, fmt.Sprintf("unexpected directory summary: %v", recs[0]))
		return
	}

	seenValid := map[string]string{}
	sawTamperedInvalid := false
	for _, r := range recs[1:] {
		fp := str(r, "fingerprint")
		st := str(r, "state")
		switch fp {
		case h.tamperedKey.fingerprint:
			if st == directory.StateInvalid && str(r, "detail") == directory.DetailFingerprintMismatch {
				sawTamperedInvalid = true
			}
		default:
			if st == fleet.KeyStatePublished {
				seenValid[fp] = str(r, "owner")
			}
		}
	}
	if !sawTamperedInvalid {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, fmt.Sprintf("tampered key not marked invalid fingerprint-mismatch: %v", recs[1:]))
		return
	}
	if seenValid[h.aliceKey1.fingerprint] != "alice" || seenValid[h.aliceKey2.fingerprint] != "alice" || seenValid[h.bobKey1.fingerprint] != "bob" {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, fmt.Sprintf("valid keys mismatch: %v", seenValid))
		return
	}

	// Owner positional filter (`fleet alice`).
	ownerRecs, problem := h.runTriple(nil, "fleet", "alice")
	if problem != "" || len(ownerRecs) != 4 || ownerRecs[0]["owners"] != float64(1) {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, fmt.Sprintf("owner filter failed (%s): %v", problem, ownerRecs))
		return
	}

	// Verify --access excludes the invalid tampered entry even when its
	// computed fingerprint is listed in approved_fingerprints.
	accRecs, problem := h.runTriple(nil, "fleet", "--access")
	if problem != "" {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, problem)
		return
	}
	for _, r := range accRecs {
		if str(r, "fingerprint") == h.tamperedKey.fingerprint && str(r, "state") == fleet.AccessStateAuthorized {
			h.recordCase(name, "valid+invalid-excluded", observedFailed, false, "tampered invalid entry was authorized in --access")
			return
		}
	}

	if err := h.cleanKeyDirectory(); err != nil {
		h.recordCase(name, "valid+invalid-excluded", observedFailed, false, err.Error())
		return
	}
	h.recordCase(name, "valid+invalid-excluded", "valid+invalid-excluded", true,
		"path directory listed alice and bob with computed fingerprints and excluded tampered entry")
}

// runDirectoryGist proves `directory-gist`: unpinned `gist:` resolved to a
// revision by `--refresh`, cached, read without the server, a second refresh
// `unchanged`, and a truncated file refused with
// `MM_FLEET_DIRECTORY_FETCH_FAILED` while leaving the cached snapshot intact.
func (h *fleetHarness) runDirectoryGist() {
	const name = caseFleetDirectoryGist

	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	if err := h.writePolicyConfig(fleetGistID, true, approved, true); err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}

	// Before --refresh, an uncached gist directory fails with MM_FLEET_DIRECTORY_UNAVAILABLE (exit 2).
	code, out, errOut := h.runCore("fleet", "--keys")
	if p := checkDiagnostic(code, 2, out, errOut, diag.FleetDirectoryUnavailable); p != "" {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, "uncached gist: "+p)
		return
	}

	aliceFile, err := os.ReadFile(filepath.Join(h.keyDir, "keys", "alice.yaml"))
	if err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}
	bobFile, err := os.ReadFile(filepath.Join(h.keyDir, "keys", "bob.yaml"))
	if err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}

	truncatedMode := false
	srv := newFixtureTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/gists/" + fleetGistID:
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"id":      fleetGistID,
				"history": []map[string]any{{"version": fleetGistRevision}},
				"files": map[string]any{
					"alice.yaml": map[string]any{"filename": "alice.yaml", "truncated": truncatedMode, "content": string(aliceFile)},
					"bob.yaml":   map[string]any{"filename": "bob.yaml", "truncated": false, "content": string(bobFile)},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "/gists/" + fleetGistID + "/" + fleetGistRevision:
			w.Header().Set("Content-Type", "application/json")
			resp := map[string]any{
				"id":      fleetGistID,
				"history": []map[string]any{{"version": fleetGistRevision}},
				"files": map[string]any{
					"alice.yaml": map[string]any{"filename": "alice.yaml", "truncated": truncatedMode, "content": string(aliceFile)},
					"bob.yaml":   map[string]any{"filename": "bob.yaml", "truncated": false, "content": string(bobFile)},
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ca := filepath.Join(h.root, "ca.pem")
	if err := os.WriteFile(ca, pemCertificate(srv.Certificate().Raw), 0o644); err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}
	if err := os.WriteFile(filepath.Join(h.root, "ca.cer"), srv.Certificate().Raw, 0o644); err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}
	untrust, err := trustFixtureCertificate(ca)
	if err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}
	defer untrust()

	gistEnv := []string{
		"MANAGED_MACHINE_GITHUB_API=" + srv.URL,
		"SSL_CERT_FILE=" + ca,
	}

	// First --refresh resolves the unpinned gist to fleetGistRevision and writes the cache.
	code, out, errOut = h.runCoreEnv(gistEnv, "fleet", "--refresh")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false,
			fmt.Sprintf("first --refresh exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	refRecs, err := parseJSONL(out)
	if err != nil || len(refRecs) != 1 {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false,
			fmt.Sprintf("first --refresh records (%v): %v", err, refRecs))
		return
	}
	wantIdentity := "gist:" + fleetGistID + "@" + fleetGistRevision
	if str(refRecs[0], "outcome") != "refreshed" || str(refRecs[0], "revision") != fleetGistRevision ||
		str(refRecs[0], "directory") != wantIdentity || refRecs[0]["keys"] != float64(3) {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false,
			fmt.Sprintf("unexpected first --refresh record: %v", refRecs[0]))
		return
	}

	// Second --refresh in all three output modes reports outcome "unchanged".
	unchRecs, problem := h.runTriple(gistEnv, "fleet", "--refresh")
	if problem != "" || len(unchRecs) != 1 || str(unchRecs[0], "outcome") != "unchanged" || str(unchRecs[0], "revision") != fleetGistRevision {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false,
			fmt.Sprintf("second --refresh (%s): %v", problem, unchRecs))
		return
	}

	// Truncated file response is refused with MM_FLEET_DIRECTORY_FETCH_FAILED.
	truncatedMode = true
	code, out, errOut = h.runCoreEnv(gistEnv, "fleet", "--refresh")
	if p := checkDiagnostic(code, 1, out, errOut, diag.FleetDirectoryFetchFailed); p != "" {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, "truncated gist: "+p)
		return
	}

	// Stop the server and prove `fleet --keys` reads from the local cache with no network.
	srv.Close()
	cachedRecs, problem := h.runTriple(nil, "fleet", "--keys")
	if problem != "" || len(cachedRecs) != 4 || str(cachedRecs[0], "directory") != wantIdentity {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false,
			fmt.Sprintf("offline cached --keys (%s): %v", problem, cachedRecs))
		return
	}

	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", observedFailed, false, err.Error())
		return
	}
	h.recordCase(name, "refreshed+cached+unchanged+truncated-refused", "refreshed+cached+unchanged+truncated-refused", true,
		"loopback HTTPS gist refreshed, cached for offline reads, reported unchanged on repeat, and refused truncated files")
}

// runPublicationGrantsNothing proves `publication-grants-nothing`: owner
// `mallory` published in the key directory and in no access group is absent
// from `--access`, absent from the applied authorized_keys block, and absent
// from the destination, state directory, captures and evidence.
func (h *fleetHarness) runPublicationGrantsNothing() {
	const name = caseFleetPublicationGrantsNothing

	malloryYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: mallory
keys:
  - fingerprint: %s
    key: %q
    label: rogue
    published_at: %q
    revision: 1
`, h.malloryKey.fingerprint, h.malloryKey.pubLine, fleetPublishedAt)
	if err := h.writeKeyEntry("mallory", malloryYAML); err != nil {
		h.recordCase(name, "absent", observedFailed, false, err.Error())
		return
	}
	defer os.Remove(filepath.Join(h.keyDir, "keys", "mallory.yaml"))

	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "absent", observedFailed, false, err.Error())
		return
	}

	// Verify mallory is published in --keys.
	keyRecs, problem := h.runTriple(nil, "fleet", "--keys")
	if problem != "" {
		h.recordCase(name, "absent", observedFailed, false, problem)
		return
	}
	sawMalloryPublished := false
	for _, r := range keyRecs[1:] {
		if str(r, "owner") == "mallory" && str(r, "fingerprint") == h.malloryKey.fingerprint && str(r, "state") == fleet.KeyStatePublished {
			sawMalloryPublished = true
		}
	}
	if !sawMalloryPublished {
		h.recordCase(name, "absent", observedFailed, false, "mallory was not listed as published in --keys")
		return
	}

	// Verify mallory is completely absent from --access.
	accRecs, problem := h.runTriple(nil, "fleet", "--access")
	if problem != "" {
		h.recordCase(name, "absent", observedFailed, false, problem)
		return
	}
	for _, r := range accRecs {
		if str(r, "owner") == "mallory" || str(r, "fingerprint") == h.malloryKey.fingerprint {
			h.recordCase(name, "absent", observedFailed, false, fmt.Sprintf("mallory appeared in --access: %v", r))
			return
		}
	}

	// Plan and apply, then verify mallory's blob and fingerprint are absent from authorized_keys.
	planID, _, problem := h.planAndApply(approved)
	if problem != "" {
		h.recordCase(name, "absent", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID)

	destBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "absent", observedFailed, false, err.Error())
		return
	}
	if bytes.Contains(destBytes, []byte(h.malloryKey.base64Blob)) || bytes.Contains(destBytes, []byte("mallory")) {
		h.recordCase(name, "absent", observedFailed, false, "mallory appeared in authorized_keys")
		return
	}
	if leak := grepFleetTree(h.state, "state", []string{h.malloryKey.base64Blob}); leak != "" {
		h.recordCase(name, "absent", observedFailed, false, leak)
		return
	}
	h.recordCase(name, "absent", "absent", true,
		"mallory is published in key directory but absent from --access, authorized_keys, state and captures")
}

// runRecordedFingerprintUntrusted proves `recorded-fingerprint-untrusted`: an
// entry whose recorded `fingerprint` equals an approved fingerprint but whose
// public key bytes differ is marked invalid and never authorized.
func (h *fleetHarness) runRecordedFingerprintUntrusted() {
	const name = caseFleetRecordedFingerprintUntrusted

	spoofYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: spoof
keys:
  - fingerprint: %s
    key: %q
    label: spoofed
    published_at: %q
    revision: 1
`, h.aliceKey1.fingerprint, h.spoofKey.pubLine, fleetPublishedAt)
	if err := h.writeKeyEntry("spoof", spoofYAML); err != nil {
		h.recordCase(name, "untrusted", observedFailed, false, err.Error())
		return
	}
	defer os.Remove(filepath.Join(h.keyDir, "keys", "spoof.yaml"))

	keyRecs, problem := h.runTriple(nil, "fleet", "--keys")
	if problem != "" {
		h.recordCase(name, "untrusted", observedFailed, false, problem)
		return
	}
	sawSpoofInvalid := false
	for _, r := range keyRecs[1:] {
		if str(r, "owner") == "spoof" && str(r, "fingerprint") == h.spoofKey.fingerprint &&
			str(r, "state") == directory.StateInvalid && str(r, "detail") == directory.DetailFingerprintMismatch {
			sawSpoofInvalid = true
		}
	}
	if !sawSpoofInvalid {
		h.recordCase(name, "untrusted", observedFailed, false, fmt.Sprintf("spoof entry was not marked invalid: %v", keyRecs[1:]))
		return
	}

	accRecs, problem := h.runTriple(nil, "fleet", "--access")
	if problem != "" {
		h.recordCase(name, "untrusted", observedFailed, false, problem)
		return
	}
	for _, r := range accRecs {
		if str(r, "owner") == "spoof" || str(r, "fingerprint") == h.spoofKey.fingerprint {
			h.recordCase(name, "untrusted", observedFailed, false, fmt.Sprintf("spoof entry appeared in --access: %v", r))
			return
		}
	}

	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	planID, _, problem := h.planAndApply(approved)
	if problem != "" {
		h.recordCase(name, "untrusted", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID)

	destBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "untrusted", observedFailed, false, err.Error())
		return
	}
	if bytes.Contains(destBytes, []byte(h.spoofKey.base64Blob)) || bytes.Contains(destBytes, []byte("spoof")) {
		h.recordCase(name, "untrusted", observedFailed, false, "spoof key appeared in authorized_keys")
		return
	}
	h.recordCase(name, "untrusted", "untrusted", true,
		"entry with approved recorded fingerprint but different key bytes was marked invalid and never authorized")
}

// runApprovalIndependent proves `approval-independent-of-publication`: a
// fingerprint approved in policy but unpublished in the directory is reported
// as `approved-unpublished` by `--access` and omitted from the authorized_keys
// block.
func (h *fleetHarness) runApprovalIndependent() {
	const name = caseFleetApprovalIndependent

	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint, h.unpubKey.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "approved-unpublished", observedFailed, false, err.Error())
		return
	}

	accRecs, problem := h.runTriple(nil, "fleet", "--access")
	if problem != "" {
		h.recordCase(name, "approved-unpublished", observedFailed, false, problem)
		return
	}
	sawUnpub := false
	authCount := 0
	for _, r := range accRecs {
		switch str(r, "state") {
		case fleet.AccessStateAuthorized:
			authCount++
		case fleet.AccessStateApprovedUnpublished:
			if str(r, "fingerprint") == h.unpubKey.fingerprint && str(r, "owner") == "" && str(r, "group") == "ops" {
				sawUnpub = true
			}
		}
	}
	if !sawUnpub || authCount != 3 {
		h.recordCase(name, "approved-unpublished", observedFailed, false, fmt.Sprintf("unexpected --access records: %v", accRecs))
		return
	}

	planID, _, problem := h.planAndApply([]string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint})
	if problem != "" {
		h.recordCase(name, "approved-unpublished", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID)

	destBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "approved-unpublished", observedFailed, false, err.Error())
		return
	}
	if bytes.Contains(destBytes, []byte(h.unpubKey.base64Blob)) {
		h.recordCase(name, "approved-unpublished", observedFailed, false, "unpublished key appeared in authorized_keys")
		return
	}
	h.recordCase(name, "approved-unpublished", "approved-unpublished", true,
		"approved fingerprint without published key is approved-unpublished in --access and absent from authorized_keys")
}

// runRevocationPolicy proves `revocation-policy`: removing a fingerprint from
// the access-group fixture produces an `update` plan that drops the line and
// leaves `fleet --check` exiting 0.
func (h *fleetHarness) runRevocationPolicy() {
	const name = caseFleetRevocationPolicy

	// Remove aliceKey2 from approved_fingerprints while keeping it published in alice.yaml.
	remaining := []string{h.aliceKey1.fingerprint, h.bobKey1.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, remaining, true); err != nil {
		h.recordCase(name, "revoked-by-policy", observedFailed, false, err.Error())
		return
	}

	planID, action, problem := h.planAndApply(remaining)
	if problem != "" {
		h.recordCase(name, "revoked-by-policy", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID)
	if action != "update" {
		h.recordCase(name, "revoked-by-policy", observedFailed, false, fmt.Sprintf("plan action = %q, want update", action))
		return
	}

	destBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "revoked-by-policy", observedFailed, false, err.Error())
		return
	}
	if bytes.Contains(destBytes, []byte(h.aliceKey2.base64Blob)) {
		h.recordCase(name, "revoked-by-policy", observedFailed, false, "revoked key remained in authorized_keys")
		return
	}
	if !bytes.Contains(destBytes, []byte(h.aliceKey1.base64Blob)) || !bytes.Contains(destBytes, []byte(h.bobKey1.base64Blob)) {
		h.recordCase(name, "revoked-by-policy", observedFailed, false, "remaining authorized keys missing from authorized_keys")
		return
	}

	code, out, errOut := h.runCore("fleet", "--check")
	if code != 0 || len(out) != 0 || len(errOut) != 0 {
		h.recordCase(name, "revoked-by-policy", observedFailed, false,
			fmt.Sprintf("fleet --check exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.recordCase(name, "revoked-by-policy", "revoked-by-policy", true,
		"removing fingerprint from access group planned update, dropped key line and passed --check 0")
}

// runRevocationDirectory proves `revocation-directory`: running `fleet
// --unpublish` for a key that remains approved in policy drops the line on the
// next plan/apply and shows `approved-unpublished` in `--access`.
func (h *fleetHarness) runRevocationDirectory() {
	const name = caseFleetRevocationDirectory

	// Re-approve aliceKey2 and apply so all three keys are in authorized_keys.
	allThree := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, allThree, true); err != nil {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, err.Error())
		return
	}
	planID, _, problem := h.planAndApply(allThree)
	if problem != "" {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID)

	// Unpublish aliceKey2 while it remains approved in ops.yaml.
	code, out, errOut := h.runCore("fleet", "--unpublish", h.aliceKey2.fingerprint, "--owner", "alice")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false,
			fmt.Sprintf("--unpublish exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	unpubRecs, err := parseJSONL(out)
	if err != nil || len(unpubRecs) != 1 || str(unpubRecs[0], "owner") != "alice" || str(unpubRecs[0], "fingerprint") != h.aliceKey2.fingerprint {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, fmt.Sprintf("unexpected --unpublish output: %v", unpubRecs))
		return
	}

	// Verify --access now reports aliceKey2 as approved-unpublished.
	accRecs, problem := h.runTriple(nil, "fleet", "--access")
	if problem != "" {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, problem)
		return
	}
	sawApprovedUnpub := false
	for _, r := range accRecs {
		if str(r, "fingerprint") == h.aliceKey2.fingerprint && str(r, "state") == fleet.AccessStateApprovedUnpublished {
			sawApprovedUnpub = true
		}
	}
	if !sawApprovedUnpub {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false,
			fmt.Sprintf("unpublished key not shown as approved-unpublished: %v", accRecs))
		return
	}

	// Plan and apply drops aliceKey2 from authorized_keys.
	remaining := []string{h.aliceKey1.fingerprint, h.bobKey1.fingerprint}
	planID2, action, problem := h.planAndApply(remaining)
	if problem != "" {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID2)
	if action != "update" {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, fmt.Sprintf("plan action = %q, want update", action))
		return
	}
	destBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, err.Error())
		return
	}
	if bytes.Contains(destBytes, []byte(h.aliceKey2.base64Blob)) {
		h.recordCase(name, "revoked-by-unpublish", observedFailed, false, "unpublished key remained in authorized_keys")
		return
	}
	h.recordCase(name, "revoked-by-unpublish", "revoked-by-unpublish", true,
		"--unpublish while approved moved key to approved-unpublished and dropped line on apply")
}

// runACT04Cycle proves `ac-t-04-cycle`: plan, apply, status, fresh
// zero-operation plan, apply, and apply again failing with
// `MM_FLEET_NO_PLAN`.
func (h *fleetHarness) runACT04Cycle() {
	const name = caseFleetACT04Cycle

	// Reset authorized_keys and ownership ledger so the cycle starts from create.
	_ = os.Remove(h.authKeys)
	_ = os.Remove(fleet.FleetLedgerPath(h.state))
	approved := []string{h.aliceKey1.fingerprint, h.bobKey1.fingerprint}
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, err.Error())
		return
	}

	// Step 1: plan (in --human, --json and default modes; each run creates a fresh plan id).
	if hc, hout, herrOut := h.runCore("fleet", "--plan", "--human"); hc != 0 || len(herrOut) != 0 || len(bytes.TrimSpace(hout)) == 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("fleet --plan --human exited %d: %s", hc, firstLineOf(herrOut, hout)))
		return
	}
	jc, jout, jerrOut := h.runCore("fleet", "--plan", "--json")
	if jc != 0 || len(jerrOut) != 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("fleet --plan --json exited %d: %s", jc, firstLineOf(jerrOut, jout)))
		return
	}
	jrecs, jerr := parseDocument(jout)
	if jerr != nil || len(jrecs) < 2 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("fleet --plan --json (%v): %v", jerr, jrecs))
		return
	}
	h.recordPlan(str(jrecs[0], "plan"))

	// Run one final default `fleet --plan` so we hold the newest saved plan id.
	code, out, errOut := h.runCore("fleet", "--plan")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("fleet --plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	planRecs, err := parseJSONL(out)
	if err != nil || len(planRecs) < 2 || !sameRecords(planRecs[1:], jrecs[1:]) {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("unexpected plan records: %v", planRecs))
		return
	}
	createPlanID := str(planRecs[0], "plan")
	if createPlanID == "" || str(planRecs[1], "action") != "create" {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("want create plan, got %v", planRecs))
		return
	}

	// Step 2: apply the create plan.
	code, out, errOut = h.runCore("fleet", "--apply")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("fleet --apply exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	applyRecs, err := parseJSONL(out)
	if err != nil || len(applyRecs) != 1 || str(applyRecs[0], "plan") != createPlanID ||
		str(applyRecs[0], "action") != "create" || applyRecs[0]["verified"] != true {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("unexpected apply records: %v", applyRecs))
		return
	}
	h.recordPlan(createPlanID)

	// Step 3: status --section fleet reports authorized-keys present & observed.
	stRecs, problem := h.runTriple(nil, "status", "--section", "fleet")
	if problem != "" {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, problem)
		return
	}
	sawAuthPresent := false
	for _, r := range stRecs {
		if str(r, "resource") == status.FleetResourceAuthorizedKeys && str(r, "state") == "present" && str(r, "evidence") == "observed" {
			sawAuthPresent = true
		}
	}
	if !sawAuthPresent {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("status did not report authorized-keys present: %v", stRecs))
		return
	}

	// Step 4: fresh zero-operation plan when already in sync.
	code, out, errOut = h.runCore("fleet", "--plan")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("zero-op fleet --plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	noopRecs, err := parseJSONL(out)
	if err != nil || len(noopRecs) == 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("unexpected noop plan records: %v", noopRecs))
		return
	}
	noopPlanID := str(noopRecs[0], "plan")
	for _, r := range noopRecs[1:] {
		if _, hasOp := r["operation"]; hasOp {
			h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("expected zero operations in in-sync plan, got %v", r))
			return
		}
	}
	doc, err := h.fleetPlanDoc(noopPlanID)
	if err != nil || len(doc.Operations) != 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, fmt.Sprintf("saved noop plan operations = %v (%v)", doc.Operations, err))
		return
	}

	// Step 5: apply the zero-operation plan.
	beforeAuth, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runCore("fleet", "--apply")
	if code != 0 || len(out) != 0 || len(errOut) != 0 {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("noop fleet --apply exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	afterAuth, err := os.ReadFile(h.authKeys)
	if err != nil || !bytes.Equal(beforeAuth, afterAuth) {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, "noop apply modified authorized_keys")
		return
	}
	h.recordPlan(noopPlanID)
	noopDoc, err := h.planDoc(noopPlanID)
	if err != nil || noopDoc.State != string(plan.Applied) {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("noop plan state = %+v (%v), want applied", noopDoc, err))
		return
	}
	noopJournal, err := h.journal(noopPlanID)
	if err != nil || journalReceipt(noopJournal) != string(plan.OutcomeApplied) {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false,
			fmt.Sprintf("noop plan receipt = %q (%v), want applied", journalReceipt(noopJournal), err))
		return
	}

	// Step 6: second apply without a new plan fails with MM_FLEET_NO_PLAN (exit 2).
	code, out, errOut = h.runCore("fleet", "--apply")
	if p := checkDiagnostic(code, 2, out, errOut, diag.FleetNoPlan); p != "" {
		h.recordCase(name, "applied+noop+no-plan", observedFailed, false, "second apply: "+p)
		return
	}

	h.recordCase(name, "applied+noop+no-plan", "applied+noop+no-plan", true,
		"plan, apply, status, zero-operation plan, apply and second apply MM_FLEET_NO_PLAN succeeded")
}

// runCheckExits proves `check-exits`: exit 0 when in sync, exit 3 after an
// external edit inside the managed block, and exit 4 against an unreadable
// destination.
func (h *fleetHarness) runCheckExits() {
	const name = caseFleetCheckExits

	// 1. In sync -> exit 0 in default, --json and --human modes.
	for _, flagArg := range [][]string{{"fleet", "--check"}, {"fleet", "--check", "--json"}, {"fleet", "--check", "--human"}} {
		code, out, errOut := h.runCore(flagArg...)
		if code != 0 || len(errOut) != 0 {
			h.recordCase(name, "0+3+4", observedFailed, false,
				fmt.Sprintf("%s exited %d: %s", strings.Join(flagArg, " "), code, firstLineOf(errOut, out)))
			return
		}
	}

	// 2. External edit inside the managed block -> exit 3 with MM_FLEET_DRIFT_FOUND.
	origBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "0+3+4", observedFailed, false, err.Error())
		return
	}
	drifted := bytes.Replace(origBytes, []byte(h.aliceKey1.base64Blob), []byte(h.aliceKey2.base64Blob), 1)
	if err := os.WriteFile(h.authKeys, drifted, 0o600); err != nil {
		h.recordCase(name, "0+3+4", observedFailed, false, err.Error())
		return
	}
	code, out, errOut := h.runCore("fleet", "--check")
	if code != 3 {
		h.recordCase(name, "0+3+4", observedFailed, false, fmt.Sprintf("drifted --check exited %d, want 3", code))
		return
	}
	if p := checkDiagnosticLine(errOut, diag.FleetDriftFound); p != "" {
		h.recordCase(name, "0+3+4", observedFailed, false, "drifted --check stderr: "+p)
		return
	}
	driftRecs, err := parseJSONL(out)
	if err != nil || len(driftRecs) != 1 || str(driftRecs[0], "drift") != fleet.DriftBlock {
		h.recordCase(name, "0+3+4", observedFailed, false, fmt.Sprintf("drifted --check stdout: %v (%v)", driftRecs, err))
		return
	}
	if err := os.WriteFile(h.authKeys, origBytes, 0o600); err != nil {
		h.recordCase(name, "0+3+4", observedFailed, false, err.Error())
		return
	}

	// 3. Unreadable destination (directory at authorized_keys path) -> exit 4 with MM_FLEET_CHECK_INCOMPLETE.
	code, out, errOut = h.runCoreEnv([]string{config.DesiredEnv + "=" + h.unreadableDesired}, "fleet", "--check")
	if p := checkDiagnostic(code, 4, out, errOut, diag.FleetCheckIncomplete); p != "" {
		h.recordCase(name, "0+3+4", observedFailed, false, "unreadable --check: "+p)
		return
	}

	h.recordCase(name, "0+3+4", "0+3+4", true,
		"--check exited 0 when in sync, 3 after external edit and 4 on unreadable destination")
}

// runConflictForeignBlock proves `conflict-foreign-block`: managed block
// markers present in the destination without an ownership ledger entry are
// refused by `--apply` with `MM_FLEET_CONFLICT` (`foreign-block`) while
// leaving the destination byte-identical.
func (h *fleetHarness) runConflictForeignBlock() {
	const name = caseFleetConflictForeignBlock

	// Remove the ownership ledger while keeping the managed-machine block markers in authorized_keys.
	_ = os.Remove(fleet.FleetLedgerPath(h.state))
	foreignContent := block.FormatBlock(fleet.BlockID, block.DefaultComment, h.aliceKey1.keyType+" "+h.aliceKey1.base64Blob+" alice", "\n")
	if err := os.WriteFile(h.authKeys, foreignContent, 0o600); err != nil {
		h.recordCase(name, "foreign-block-refused", observedFailed, false, err.Error())
		return
	}
	beforeHash := sum(foreignContent)

	code, out, errOut := h.runCore("fleet", "--plan")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "foreign-block-refused", observedFailed, false,
			fmt.Sprintf("conflict --plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	planRecs, err := parseJSONL(out)
	if err != nil || len(planRecs) < 2 || !strings.HasPrefix(str(planRecs[1], "action"), "conflict foreign-block ") {
		h.recordCase(name, "foreign-block-refused", observedFailed, false,
			fmt.Sprintf("expected conflict foreign-block plan action, got %v", planRecs))
		return
	}
	conflictPlanID := str(planRecs[0], "plan")
	h.recordPlan(conflictPlanID)

	code, out, errOut = h.runCore("fleet", "--apply")
	if p := checkDiagnostic(code, 1, out, errOut, diag.FleetConflict); p != "" {
		h.recordCase(name, "foreign-block-refused", observedFailed, false, "conflict --apply: "+p)
		return
	}
	if !bytes.Contains(errOut, []byte("foreign-block")) {
		h.recordCase(name, "foreign-block-refused", observedFailed, false, "conflict diagnostic did not name foreign-block: "+string(errOut))
		return
	}

	afterBytes, err := os.ReadFile(h.authKeys)
	if err != nil || sum(afterBytes) != beforeHash {
		h.recordCase(name, "foreign-block-refused", observedFailed, false, "foreign-block refusal mutated authorized_keys")
		return
	}
	h.recordCase(name, "foreign-block-refused", "foreign-block-refused", true,
		"markers without ownership ledger planned conflict foreign-block and refused --apply with destination byte-identical")
}

// runLegacyTransition proves `legacy-transition`: a destination with a
// user-owned line and a legacy `# BEGIN managed-machine` block reports exit 3
// `legacy-block` on `--check`, refuses `--plan` with `MM_FLEET_NOT_CUT_OVER`
// when `fleet/cutover.yaml` is absent, plans the `legacy_block` removal when
// `cutover.yaml` is present, and on `--apply` leaves the user-owned line
// byte-identical while removing the legacy block; plus `mm-fleet-import` over
// the fixture `fleet/machines/` directory produces an entry and access group
// whose fingerprints equal the computed ones.
func (h *fleetHarness) runLegacyTransition() {
	const name = caseFleetLegacyTransition

	_ = os.Remove(fleet.FleetLedgerPath(h.state))
	seeded := strings.Join([]string{
		fleetUserOwnedKey,
		fleet.LegacyManagedMachineBegin,
		h.legacyKey.pubLine,
		fleet.LegacyManagedMachineEnd,
		"",
	}, "\n")
	if err := os.WriteFile(h.authKeys, []byte(seeded), 0o600); err != nil {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, err.Error())
		return
	}

	approved := []string{h.aliceKey1.fingerprint, h.bobKey1.fingerprint}
	// First write policy WITHOUT cutover.yaml.
	if err := h.writePolicyConfig(h.keyDir, false, approved, false); err != nil {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, err.Error())
		return
	}

	// --check reports exit 3 with drift "legacy-block".
	code, out, errOut := h.runCore("fleet", "--check")
	if code != 3 {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			fmt.Sprintf("legacy --check exited %d, want 3", code))
		return
	}
	if p := checkDiagnosticLine(errOut, diag.FleetDriftFound); p != "" {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, p)
		return
	}
	checkRecs, err := parseJSONL(out)
	if err != nil {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, err.Error())
		return
	}
	sawLegacyDrift := false
	for _, r := range checkRecs {
		if str(r, "drift") == fleet.DriftLegacyBlock && str(r, "detail") == fleet.LegacyManagedMachineBegin {
			sawLegacyDrift = true
		}
	}
	if !sawLegacyDrift {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			fmt.Sprintf("expected legacy-block drift record, got %v", checkRecs))
		return
	}

	// --plan without cutover.yaml fails with MM_FLEET_NOT_CUT_OVER (exit 1).
	code, out, errOut = h.runCore("fleet", "--plan")
	if p := checkDiagnostic(code, 1, out, errOut, diag.FleetNotCutOver); p != "" {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, "without cutover.yaml: "+p)
		return
	}

	// Write cutover.yaml and run --plan: must include the legacy_block remove record.
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runCore("fleet", "--plan")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			fmt.Sprintf("legacy --plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	planRecs, err := parseJSONL(out)
	if err != nil || len(planRecs) < 3 {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			fmt.Sprintf("unexpected legacy plan records: %v", planRecs))
		return
	}
	planID := str(planRecs[0], "plan")
	sawLegacyRecord := false
	for _, r := range planRecs[1:] {
		if str(r, "legacy_block") == fleet.LegacyManagedMachineBegin &&
			r["keys"] == float64(1) && str(r, "action") == "remove" {
			sawLegacyRecord = true
		}
	}
	if !sawLegacyRecord {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			fmt.Sprintf("plan omitted legacy_block record: %v", planRecs))
		return
	}

	// Apply the transition plan.
	code, out, errOut = h.runCore("fleet", "--apply")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			fmt.Sprintf("legacy --apply exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.recordPlan(planID)

	afterBytes, err := os.ReadFile(h.authKeys)
	if err != nil {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, err.Error())
		return
	}
	afterText := string(afterBytes)
	if !strings.HasPrefix(afterText, fleetUserOwnedKey+"\n") {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			"user-owned line was not preserved byte-identical at start of authorized_keys")
		return
	}
	if strings.Contains(afterText, fleet.LegacyManagedMachineBegin) ||
		strings.Contains(afterText, fleet.LegacyManagedMachineEnd) ||
		strings.Contains(afterText, h.legacyKey.base64Blob) {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false,
			"legacy block or legacy key survived apply")
		return
	}

	// Run mm-fleet-import over the fixture fleet/machines/ directory.
	if problem := h.runFleetImportCheck(); problem != "" {
		h.recordCase(name, "cutover+legacy-replaced+imported", observedFailed, false, problem)
		return
	}

	h.recordCase(name, "cutover+legacy-replaced+imported", "cutover+legacy-replaced+imported", true,
		"--check legacy-block, MM_FLEET_NOT_CUT_OVER before cutover.yaml, legacy block removed with user line byte-identical, and mm-fleet-import verified")
}

func (h *fleetHarness) runFleetImportCheck() string {
	importBin, err := h.ensureImportBinary()
	if err != nil {
		return "build mm-fleet-import: " + err.Error()
	}
	_ = os.RemoveAll(h.importOut)
	c := exec.Command(importBin, h.legacyCheckout, "-owner", "alice", "-group", "ops", "-account", h.account, "-out", h.importOut)
	c.Env, c.Dir = h.env, h.home
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	if err := c.Run(); err != nil {
		return fmt.Sprintf("mm-fleet-import failed (%v): %s", err, firstLineOf(e.Bytes(), o.Bytes()))
	}
	if len(e.Bytes()) != 0 {
		return "mm-fleet-import stderr not empty: " + firstLineOf(e.Bytes())
	}
	h.captures = append(h.captures, o.Bytes())

	recs, err := parseJSONL(o.Bytes())
	if err != nil || len(recs) != 2 {
		return fmt.Sprintf("mm-fleet-import report records (%v): %v", err, recs)
	}
	sawImported := false
	sawExcluded := false
	for _, r := range recs {
		switch str(r, "machine_id") {
		case h.importGoodKey.machineID:
			if str(r, "computed_fingerprint") == h.importGoodKey.fingerprint &&
				r["fingerprint_match"] == true && str(r, "status") == "imported" {
				sawImported = true
			}
		case h.legacyKey.machineID:
			if str(r, "computed_fingerprint") == h.legacyKey.fingerprint &&
				str(r, "recorded_fingerprint") == h.aliceKey1.fingerprint &&
				r["fingerprint_match"] == false && str(r, "status") == "excluded" {
				sawExcluded = true
			}
		}
	}
	if !sawImported || !sawExcluded {
		return fmt.Sprintf("unexpected mm-fleet-import report: %v", recs)
	}

	loadedDir, err := directory.LoadPath(filepath.Join(h.importOut, "keys"))
	if err != nil {
		return "mm-fleet-import directory.LoadPath: " + err.Error()
	}
	if len(loadedDir.Entries) != 1 || len(loadedDir.Entries[0].Keys) != 1 ||
		loadedDir.Entries[0].Keys[0].Fingerprint != h.importGoodKey.fingerprint {
		return fmt.Sprintf("imported key directory = %+v", loadedDir)
	}

	groupBytes, err := os.ReadFile(filepath.Join(h.importOut, "fleet", "access-groups", "ops.yaml"))
	if err != nil {
		return err.Error()
	}
	grp, err := policy.ParseAccessGroup("fleet/access-groups/ops.yaml", groupBytes)
	if err != nil {
		return "mm-fleet-import ParseAccessGroup: " + err.Error()
	}
	if !sameStrings(grp.ApprovedFingerprints, []string{h.importGoodKey.fingerprint}) {
		return fmt.Sprintf("imported access group fingerprints = %v, want [%s]", grp.ApprovedFingerprints, h.importGoodKey.fingerprint)
	}
	return ""
}

func (h *fleetHarness) ensureImportBinary() (string, error) {
	if h.importBin != "" {
		return h.importBin, nil
	}
	pkgDir, err := findImportCmdDir()
	if err != nil {
		return "", err
	}
	name := "mm-fleet-import"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	outBin := filepath.Join(h.root, name)
	cmd := exec.Command("go", "build", "-o", outBin, ".")
	cmd.Dir = pkgDir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	h.importBin = outBin
	return outBin, nil
}

func findImportCmdDir() (string, error) {
	candidates := []string{
		filepath.FromSlash("./cmd/mm-fleet-import"),
		filepath.FromSlash("../mm-fleet-import"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(filepath.Join(c, "main.go")); err == nil && !info.IsDir() {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("could not locate cmd/mm-fleet-import")
}

// runPublishStage proves `publish-stage`: `--publish` to a `path:` directory
// increments the revision and is idempotent on repeat; `--publish` to a
// `gist:` directory stages the entry and prints `next_step`; `enrollment.json`
// is written; and a private-key fixture given to `--key` is refused with
// `MM_FLEET_KEY_INVALID` without echoing any bytes.
func (h *fleetHarness) runPublishStage() {
	const name = caseFleetPublishStage

	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	if err := h.cleanKeyDirectory(); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}

	// Publish aliceKey2 (after unpublishing it first so alice has only rev 1).
	code, out, errOut := h.runCore("fleet", "--unpublish", h.aliceKey2.fingerprint, "--owner", "alice")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("prep --unpublish exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}

	pubFile := filepath.Join(h.pubDir, "alice-laptop.pub")
	if err := write(pubFile, h.aliceKey2.pubLine+"\n"); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}

	// First --publish to path: directory increments revision to 2.
	code, out, errOut = h.runCore("fleet", "--publish", "--owner", "alice", "--key", pubFile, "--label", "laptop")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("path --publish exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	pubRecs, err := parseJSONL(out)
	if err != nil || len(pubRecs) != 1 || str(pubRecs[0], "owner") != "alice" ||
		str(pubRecs[0], "fingerprint") != h.aliceKey2.fingerprint || str(pubRecs[0], "staged") != "" {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("unexpected path --publish record: %v", pubRecs))
		return
	}
	dir1, err := directory.LoadPath(h.keyDir)
	if err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}
	rev1 := findKeyRevision(dir1, "alice", h.aliceKey2.fingerprint)
	if rev1 != 2 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("published key revision = %d, want 2", rev1))
		return
	}

	// Second --publish of the same key is idempotent (revision stays 2, key count stays 2).
	code, out, errOut = h.runCore("fleet", "--publish", "--owner", "alice", "--key", pubFile, "--label", "laptop")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("idempotent --publish exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	dir2, err := directory.LoadPath(h.keyDir)
	if err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}
	if findKeyRevision(dir2, "alice", h.aliceKey2.fingerprint) != 2 || dir2.KeyCount() != 3 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("repeat --publish was not idempotent: %+v", dir2))
		return
	}

	// Switch to the cached gist directory and verify --publish stages the file and prints next_step.
	if err := h.writePolicyConfig(fleetGistID, true, approved, true); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runCore("fleet", "--publish", "--owner", "alice", "--key", pubFile, "--label", "laptop-staged")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("gist --publish exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	stageRecs, err := parseJSONL(out)
	if err != nil || len(stageRecs) != 1 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("gist --publish records (%v): %v", err, stageRecs))
		return
	}
	stagedPath := str(stageRecs[0], "staged")
	nextStep := str(stageRecs[0], "next_step")
	if stagedPath == "" || !strings.Contains(nextStep, "gh gist edit "+fleetGistID) {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("unexpected gist stage record: %v", stageRecs[0]))
		return
	}
	if _, err := os.Stat(stagedPath); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			"staged YAML file missing: "+err.Error())
		return
	}

	// Verify enrollment.json was written in the state directory.
	enrollBytes, err := os.ReadFile(filepath.Join(h.state, "fleet", "enrollment.json"))
	if err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			"reading enrollment.json: "+err.Error())
		return
	}
	var enroll fleet.Enrollment
	if err := json.Unmarshal(enrollBytes, &enroll); err != nil ||
		enroll.SchemaVersion != fleet.EnrollmentSchemaVersion ||
		enroll.Owner != "alice" ||
		enroll.Fingerprint != h.aliceKey2.fingerprint ||
		enroll.Outcome != fleet.OutcomeStaged {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("unexpected enrollment.json: %+v (%v)", enroll, err))
		return
	}

	// Restore path: policy and re-write enrollment.json via path: --publish.
	if err := h.writePolicyConfig(h.keyDir, false, approved, true); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runCore("fleet", "--publish", "--owner", "alice", "--key", pubFile, "--label", "laptop")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false,
			fmt.Sprintf("restore path --publish exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}

	// Private-key fixture passed to --key is refused with MM_FLEET_KEY_INVALID without echo.
	privPath := filepath.Join(h.pubDir, "bad-private.key")
	privContent := fleetPrivateKeyHeader + "\n" + fleetPrivateKeyBody + "\n" + fleetPrivateKeyFooter + "\n"
	if err := write(privPath, privContent); err != nil {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, err.Error())
		return
	}
	code, out, errOut = h.runCore("fleet", "--publish", "--owner", "alice", "--key", privPath)
	_ = os.Remove(privPath)
	if p := checkDiagnostic(code, 2, out, errOut, diag.FleetKeyInvalid); p != "" {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, "private key --publish: "+p)
		return
	}
	if leak := grepFleetBytes(append(append([]byte{}, out...), errOut...), "--publish private-key output", h.forbiddenStrings); leak != "" {
		h.recordCase(name, "published+idempotent+staged+private-refused", observedFailed, false, leak)
		return
	}

	h.recordCase(name, "published+idempotent+staged+private-refused", "published+idempotent+staged+private-refused", true,
		"--publish to path incremented revision and stayed idempotent, gist staged with next_step, enrollment.json written, and private key refused with MM_FLEET_KEY_INVALID")
}

func findKeyRevision(dir directory.Directory, owner, fp string) int {
	for _, e := range dir.Entries {
		if e.ID != owner {
			continue
		}
		for _, k := range e.Keys {
			if k.Fingerprint == fp {
				return k.Revision
			}
		}
	}
	return 0
}

// runStatusFleet proves `status-fleet`: all four fleet resources (`directory`,
// `policy`, `authorized-keys`, `enrollment`) reported with `recorded`,
// `observed` and `unknown` in one read-only run.
func (h *fleetHarness) runStatusFleet() {
	const name = caseFleetStatusFleet

	// Write a half-open block marker into authorized_keys so `authorized-keys`
	// reports state="unknown" and evidence="recorded" while `directory` and
	// `policy` report state="present" and evidence="observed" and `enrollment`
	// reports state="present" and evidence="recorded" — all four resources in
	// one exit-0 run.
	malformedBlock := "# managed-machine:begin " + fleet.BlockID + "\n" + h.aliceKey1.pubLine + "\n"
	if err := os.WriteFile(h.authKeys, []byte(malformedBlock), 0o600); err != nil {
		h.recordCase(name, "four-resources", observedFailed, false, err.Error())
		return
	}

	beforeState := map[string]string{}
	if err := hashTree(h.state, "state", beforeState); err != nil {
		h.recordCase(name, "four-resources", observedFailed, false, err.Error())
		return
	}

	code, out, errOut := h.runCore("status", "--section", "fleet")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(name, "four-resources", observedFailed, false,
			fmt.Sprintf("status --section fleet exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	recs, problem := parseStatusRecords(out)
	if problem != "" {
		h.recordCase(name, "four-resources", observedFailed, false, problem)
		return
	}
	if len(recs) != 4 {
		h.recordCase(name, "four-resources", observedFailed, false, fmt.Sprintf("want 4 fleet status records, got %d: %v", len(recs), recs))
		return
	}

	seenRes := map[string]record{}
	seenEv := map[string]bool{}
	seenState := map[string]bool{}
	for _, r := range recs {
		if str(r, "section") != status.SectionFleet || str(r, "target") != fleet.TargetUser {
			h.recordCase(name, "four-resources", observedFailed, false, fmt.Sprintf("unexpected fleet status header: %v", r))
			return
		}
		seenRes[str(r, "resource")] = r
		seenEv[str(r, "evidence")] = true
		seenState[str(r, "state")] = true
	}
	for _, want := range []string{
		status.FleetResourceDirectory,
		status.FleetResourcePolicy,
		status.FleetResourceAuthorizedKeys,
		status.FleetResourceEnrollment,
	} {
		if _, ok := seenRes[want]; !ok {
			h.recordCase(name, "four-resources", observedFailed, false, fmt.Sprintf("status omitted resource %q", want))
			return
		}
	}
	if !seenEv["observed"] || !seenEv["recorded"] || !seenState["present"] || !seenState["unknown"] {
		h.recordCase(name, "four-resources", observedFailed, false,
			fmt.Sprintf("status did not include observed+recorded and present+unknown in one run: %v", recs))
		return
	}

	// Verify status made zero mutations to the state directory.
	afterState := map[string]string{}
	if err := hashTree(h.state, "state", afterState); err != nil {
		h.recordCase(name, "four-resources", observedFailed, false, err.Error())
		return
	}
	if diff := profileDiff(beforeState, afterState); len(diff) > 0 {
		h.recordCase(name, "four-resources", observedFailed, false, "status mutated state directory: "+strings.Join(diff, "; "))
		return
	}

	// Restore clean authorized_keys and verify all four resources report present in all three output modes.
	approved := []string{h.aliceKey1.fingerprint, h.aliceKey2.fingerprint, h.bobKey1.fingerprint}
	if err := os.Remove(h.authKeys); err != nil && !errors.Is(err, os.ErrNotExist) {
		h.recordCase(name, "four-resources", observedFailed, false, err.Error())
		return
	}
	_ = os.Remove(fleet.FleetLedgerPath(h.state))
	planID, _, problem := h.planAndApply(approved)
	if problem != "" {
		h.recordCase(name, "four-resources", observedFailed, false, problem)
		return
	}
	h.recordPlan(planID)

	cleanRecs, problem := h.runTriple(nil, "status", "--section", "fleet")
	if problem != "" || len(cleanRecs) != 4 {
		h.recordCase(name, "four-resources", observedFailed, false, fmt.Sprintf("clean status (%s): %v", problem, cleanRecs))
		return
	}
	for _, r := range cleanRecs {
		if str(r, "state") != "present" {
			h.recordCase(name, "four-resources", observedFailed, false, fmt.Sprintf("expected all 4 resources present after apply, got %v", r))
			return
		}
	}

	h.recordCase(name, "four-resources", "four-resources", true,
		"status --section fleet reported directory, policy, authorized-keys and enrollment with recorded, observed and unknown in one read-only run")
}

// runLegacyCutoverRefusal proves `legacy-cutover-refusal`: on `darwin/*`,
// verifies the F01 compatibility output (or runs
// `tests/fleet-cutover-characterization.test.sh` when `-compat` was not
// supplied) and records `ok`; on all other targets, records `n-a`.
func (h *fleetHarness) runLegacyCutoverRefusal() {
	const name = caseFleetLegacyCutoverRefusal
	if !strings.HasPrefix(h.target, "darwin/") {
		h.recordCase(name, "n-a", "n-a", true, "legacy Bash fleet route is macOS-only")
		return
	}
	if ok, detail := h.checkCompatFile(); ok {
		h.recordCase(name, "ok", "ok", true, detail)
		return
	}
	scriptPath, err := findCutoverTestScript()
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	tmpHome := filepath.Join(h.root, "cutover-bash-home")
	if err := os.MkdirAll(tmpHome, 0o755); err != nil {
		h.recordCase(name, "ok", observedFailed, false, err.Error())
		return
	}
	cmd := exec.Command("bash", scriptPath)
	cmd.Dir = filepath.Dir(filepath.Dir(scriptPath))
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "HOME", "USERPROFILE", "GIT_CONFIG_GLOBAL", "GIT_CONFIG_NOSYSTEM":
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOME="+tmpHome,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+filepath.Join(tmpHome, ".gitconfig"),
	)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	h.captures = append(h.captures, out)
	if err != nil {
		h.recordCase(name, "ok", observedFailed, false, fmt.Sprintf("%v: %s", err, firstLineOf(out)))
		return
	}
	if !strings.Contains(string(out), fleetCutoverCompatLine) {
		h.recordCase(name, "ok", observedFailed, false, "missing pass line: "+firstLineOf(out))
		return
	}
	h.recordCase(name, "ok", "ok", true, fleetCutoverCompatLine)
}

func (h *fleetHarness) checkCompatFile() (bool, string) {
	candidates := []string{}
	if h.compatPath != "" {
		candidates = append(candidates, h.compatPath)
	} else {
		candidates = append(candidates, "f01-compat.txt", "../f01-compat.txt")
	}
	for _, p := range candidates {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if strings.TrimSpace(line) == fleetCutoverCompatLine {
				return true, fleetCutoverCompatLine
			}
		}
		if h.compatPath != "" {
			return false, "f01-compat file did not contain fleet cutover pass line"
		}
	}
	return false, ""
}

func findCutoverTestScript() (string, error) {
	candidates := []string{
		filepath.FromSlash("tests/fleet-cutover-characterization.test.sh"),
		filepath.FromSlash("../../tests/fleet-cutover-characterization.test.sh"),
		filepath.FromSlash("../tests/fleet-cutover-characterization.test.sh"),
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return filepath.Abs(c)
		}
	}
	return "", errors.New("could not locate tests/fleet-cutover-characterization.test.sh")
}

// runGuards proves `guards`: no sentinel started; the runner's real `~/.ssh`,
// shell profiles and `~/.config/managed-machine` byte-identical by hash;
// nothing outside the harness destination and state directory changed; and
// zero hits for every unauthorized key blob and for the private-key fixture
// bytes across the state directory, the harness home and every capture.
func (h *fleetHarness) runGuards() {
	h.ev.Guards.SentinelInvocations = []string{}
	if b, err := os.ReadFile(h.log); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			h.ev.Guards.SentinelInvocations = append(h.ev.Guards.SentinelInvocations, h.paths.clean(line))
		}
	}
	afterMachine, err := h.fleetMachineSnapshot()
	if err != nil {
		h.ev.Guards.ProfileChanges = []string{"the profiles could not be read: " + err.Error()}
		h.recordCase(caseFleetGuards, "intact", observedFailed, false, "the profiles could not be read")
		return
	}
	changes := profileDiff(h.machine, afterMachine)
	if changes == nil {
		changes = []string{}
	}
	h.ev.Guards.ProfileChanges = changes

	afterStatic, err := h.staticHarnessSnapshot()
	if err != nil {
		h.recordCase(caseFleetGuards, "intact", observedFailed, false, "static harness snapshot: "+err.Error())
		return
	}
	staticDiff := profileDiff(h.outsideContainment, afterStatic)

	problem := ""
	switch {
	case len(h.ev.Guards.SentinelInvocations) > 0:
		problem = "a guarded program was started: " + strings.Join(h.ev.Guards.SentinelInvocations, "; ")
	case len(changes) > 0:
		problem = "the profiles or real ~/.ssh changed: " + strings.Join(changes, "; ")
	case len(staticDiff) > 0:
		problem = "static harness directories changed: " + strings.Join(staticDiff, "; ")
	default:
		problem = h.grepForbidden()
	}
	if problem != "" {
		h.recordCase(caseFleetGuards, "intact", observedFailed, false, problem)
		return
	}
	h.recordCase(caseFleetGuards, "intact", "intact", true,
		"no guarded program was started, real ~/.ssh, shell profiles and ~/.config/managed-machine are byte-identical, and zero unauthorized or private-key bytes leaked")
}

func (h *fleetHarness) grepForbidden() string {
	if hit := grepFleetTree(h.state, "state", h.forbiddenStrings); hit != "" {
		return hit
	}
	if hit := grepFleetTree(h.home, "home", h.forbiddenStrings); hit != "" {
		return hit
	}
	for i, capBytes := range h.captures {
		if hit := grepFleetBytes(capBytes, fmt.Sprintf("capture[%d]", i), h.forbiddenStrings); hit != "" {
			return hit
		}
	}
	return ""
}

func grepFleetTree(root, label string, forbidden []string) string {
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if d.Name() == ".lock" {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if hit := grepFleetBytes(b, label+"/"+filepath.ToSlash(rel), forbidden); hit != "" {
			found = hit
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func grepFleetBytes(b []byte, where string, forbidden []string) string {
	for _, s := range forbidden {
		if s != "" && bytes.Contains(b, []byte(s)) {
			return "forbidden fleet material leaked in " + where
		}
	}
	return ""
}

// planAndApply runs `fleet --plan` and `fleet --apply` and verifies that the
// resulting authorized_keys block contains only the expected fingerprints.
func (h *fleetHarness) planAndApply(wantAuthorizedFPs []string) (string, string, string) {
	code, out, errOut := h.runCore("fleet", "--plan")
	if code != 0 || len(errOut) != 0 {
		return "", "", fmt.Sprintf("fleet --plan exited %d: %s", code, firstLineOf(errOut, out))
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) == 0 {
		return "", "", fmt.Sprintf("fleet --plan records (%v): %v", err, recs)
	}
	planID := str(recs[0], "plan")
	action := "noop"
	var gotFPs []string
	for _, r := range recs[1:] {
		if a := str(r, "action"); a != "" && str(r, "operation") == fleet.OperationAuthorizedKeys {
			action = a
		}
		if str(r, "state") == fleet.AccessStateAuthorized {
			gotFPs = append(gotFPs, str(r, "fingerprint"))
		}
	}
	wantSorted := append([]string(nil), wantAuthorizedFPs...)
	sort.Strings(wantSorted)
	sort.Strings(gotFPs)
	if !sameStrings(gotFPs, wantSorted) {
		return planID, action, fmt.Sprintf("planned authorized fingerprints = %v, want %v", gotFPs, wantSorted)
	}

	code, out, errOut = h.runCore("fleet", "--apply")
	if code != 0 || len(errOut) != 0 {
		return planID, action, fmt.Sprintf("fleet --apply exited %d: %s", code, firstLineOf(errOut, out))
	}
	return planID, action, ""
}

func (h *fleetHarness) fleetPlanDoc(id string) (plan.Document, error) {
	var d plan.Document
	b, err := os.ReadFile(filepath.Join(h.state, "plans", id+".json"))
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	return d, nil
}

// fleetAggregateCmd checks that five S5 fleet-gate records belong together:
// one per first-release target, the same source revision, every gate in fleet
// mode with result pass, every D9 case present, and `legacy-cutover-refusal`
// recorded `ok` on `darwin/*` and `n-a` on the other four targets.
func fleetAggregateCmd(args []string) error {
	fl := flag.NewFlagSet("fleet-aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("fleet-aggregate needs -source-revision")
	}
	files := fl.Args()
	var problems []string
	byTarget := map[string]FleetEvidence{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var ev FleetEvidence
		if err := json.Unmarshal(b, &ev); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		if ev.Kind != fleetEvidenceKind {
			problems = append(problems, fmt.Sprintf("%s is a %q record, not %q", ev.Target, ev.Kind, fleetEvidenceKind))
		}
		if ev.Mode != fleetMode {
			problems = append(problems, fmt.Sprintf("%s ran in %q mode, not %q", ev.Target, ev.Mode, fleetMode))
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
		for _, c := range ev.Cases {
			if !c.OK {
				problems = append(problems, fmt.Sprintf("%s: case %s is %s", ev.Target, c.Case, c.Observed))
				continue
			}
			if c.Case == caseFleetLegacyCutoverRefusal {
				if strings.HasPrefix(ev.Target, "darwin/") {
					if c.Observed != "ok" {
						problems = append(problems, fmt.Sprintf("%s: case %s is %q, want \"ok\" on darwin", ev.Target, c.Case, c.Observed))
					}
				} else if c.Observed != "n-a" {
					problems = append(problems, fmt.Sprintf("%s: case %s is %q, want \"n-a\" outside darwin", ev.Target, c.Case, c.Observed))
				}
				continue
			}
			if c.Observed == "n-a" {
				problems = append(problems, fmt.Sprintf("%s: case %s is \"n-a\"; only %s may be n-a", ev.Target, c.Case, caseFleetLegacyCutoverRefusal))
			}
		}
	}
	for _, t := range firstReleaseTargets {
		ev, ok := byTarget[t]
		if !ok {
			problems = append(problems, "no evidence for "+t)
			continue
		}
		for _, name := range fleetCaseNames {
			if !fleetCasePresent(ev, name) {
				problems = append(problems, fmt.Sprintf("%s: no case for %s", t, name))
			}
		}
	}
	if len(files) != len(firstReleaseTargets) {
		problems = append(problems, fmt.Sprintf("%d records, want %d", len(files), len(firstReleaseTargets)))
	}
	sort.Strings(problems)
	printFleetMatrix(byTarget)
	fmt.Printf("\nsource revision `%s`\n", *rev)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("FAIL:", p)
		}
		return errors.New("fleet-aggregate failed")
	}
	return nil
}

func fleetCasePresent(ev FleetEvidence, name string) bool {
	for _, c := range ev.Cases {
		if c.Case == name {
			return true
		}
	}
	return false
}

func printFleetMatrix(byTarget map[string]FleetEvidence) {
	fmt.Printf("| case | %s |\n", strings.Join(firstReleaseTargets, " | "))
	fmt.Printf("|---|%s\n", strings.Repeat("---|", len(firstReleaseTargets)))
	for _, name := range fleetCaseNames {
		fmt.Printf("| %s |", name)
		for _, t := range firstReleaseTargets {
			fmt.Printf(" %s |", fleetCell(byTarget, name, t))
		}
		fmt.Println()
	}
}

func fleetCell(byTarget map[string]FleetEvidence, name, target string) string {
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
		if !c.OK {
			failed = true
		}
		if name == caseFleetLegacyCutoverRefusal {
			if strings.HasPrefix(target, "darwin/") && c.Observed != "ok" {
				failed = true
			}
			if !strings.HasPrefix(target, "darwin/") && c.Observed != "n-a" {
				failed = true
			}
		} else if c.Observed == "n-a" {
			failed = true
		}
	}
	if !seen || failed {
		return "FAIL"
	}
	if name == caseFleetLegacyCutoverRefusal && observed == "n-a" {
		return "n-a"
	}
	return "ok"
}
