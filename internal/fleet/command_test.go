package fleet_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/software"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/source/remote"
	"github.com/qwts/managed-machine/internal/statedir"
	"github.com/qwts/managed-machine/internal/target"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

type fixtureKey struct {
	line        string
	blobB64     string
	fingerprint string
}

func makeEd25519Key(t *testing.T, seedByte byte, comment string) fixtureKey {
	t.Helper()
	seed := bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	var blob bytes.Buffer
	writeSSHBytes(&blob, []byte(directory.KeyTypeEd25519))
	writeSSHBytes(&blob, pub)

	b64 := base64.StdEncoding.EncodeToString(blob.Bytes())
	line := directory.KeyTypeEd25519 + " " + b64 + " " + comment
	pk, err := directory.ParsePublicKey(line)
	if err != nil {
		t.Fatalf("ParsePublicKey(%q): %v", comment, err)
	}
	return fixtureKey{
		line:        line,
		blobB64:     b64,
		fingerprint: pk.Fingerprint,
	}
}

func writeSSHBytes(buf *bytes.Buffer, b []byte) {
	var lenBuf [4]byte
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(b)))
	buf.Write(lenBuf[:])
	buf.Write(b)
}

type fixtureSet struct {
	aliceLow        fixtureKey
	aliceHigh       fixtureKey
	aliceUnapproved fixtureKey
	bobValid        fixtureKey
	bobMismatchRec  fixtureKey
	bobMismatchAct  fixtureKey
	carolUnapproved fixtureKey
	unpublished     fixtureKey

	aliceYAML string
	bobYAML   string
	carolYAML string
}

func newFixtureSet(t *testing.T) fixtureSet {
	t.Helper()
	kA := makeEd25519Key(t, 0x11, "alice@workstation-a")
	kB := makeEd25519Key(t, 0x12, "alice@workstation-b")
	low, high := kA, kB
	if low.fingerprint > high.fingerprint {
		low, high = high, low
	}
	unapproved := makeEd25519Key(t, 0x13, "alice@unapproved-laptop")
	bobValid := makeEd25519Key(t, 0x14, "bob@desktop")
	bobMismatchRec := makeEd25519Key(t, 0x15, "bob@recorded")
	bobMismatchAct := makeEd25519Key(t, 0x16, "bob@actual")
	carolUnapproved := makeEd25519Key(t, 0x17, "carol@unapproved")
	unpublished := makeEd25519Key(t, 0x18, "dave@unpublished")

	for name, tc := range map[string]struct {
		got, want string
	}{
		"aliceLow":        {low.fingerprint, "SHA256:2HS8bJpYBy7bDfWIQN304coFKT0s6CVPABFLYq+IX5E"},
		"aliceHigh":       {high.fingerprint, "SHA256:fEwl/gSmEu2rNjSfMxOVgeWCDJoI93yKQrq9RNR+wJ8"},
		"aliceUnapproved": {unapproved.fingerprint, "SHA256:KLOzCSIr0cqZ0hUBDTNXnmKKPjDJeC0pRivP6FKHNbo"},
		"bobValid":        {bobValid.fingerprint, "SHA256:Sy7Qz9Ql8tk5vYkt3wypNP4pnGftzz06aeXKZcuFbxw"},
		"bobMismatchRec":  {bobMismatchRec.fingerprint, "SHA256:AVClTvWgXEolKWcHEcUxJSIs44sDGtNJ5xliK7332fo"},
		"bobMismatchAct":  {bobMismatchAct.fingerprint, "SHA256:zy3XWYgXmmoLu7KlZ0n6o38lFWakIaFLG8L+9bc31ng"},
		"carolUnapproved": {carolUnapproved.fingerprint, "SHA256:sva0xH1CfFYJWLaHjJp6BU8VPMuXUOjCoy754DcF0tk"},
		"unpublished":     {unpublished.fingerprint, "SHA256:FHnkzsBz7f3dqcYQZJQyowjlHgsBFgnGnG6b00R1Wo4"},
	} {
		if tc.got != tc.want {
			t.Fatalf("%s fingerprint = %q, want %q", name, tc.got, tc.want)
		}
	}

	// Intentionally list high before low in alice.yaml so tests prove
	// owner-then-fingerprint ordering on output.
	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    label: alice-high
    published_at: "2026-10-22T01:00:00Z"
    revision: 2
  - fingerprint: %s
    key: %s
    label: alice-low
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: alice-unapproved
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		high.fingerprint, high.line,
		low.fingerprint, low.line,
		unapproved.fingerprint, unapproved.line,
	)

	bobYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: bob
keys:
  - fingerprint: %s
    key: %s
    label: bob-workstation
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: bob-bad-fp
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		bobValid.fingerprint, bobValid.line,
		bobMismatchRec.fingerprint, bobMismatchAct.line,
	)

	carolYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: carol
keys:
  - fingerprint: %s
    key: %s
    label: carol-laptop
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		carolUnapproved.fingerprint, carolUnapproved.line,
	)

	return fixtureSet{
		aliceLow:        low,
		aliceHigh:       high,
		aliceUnapproved: unapproved,
		bobValid:        bobValid,
		bobMismatchRec:  bobMismatchRec,
		bobMismatchAct:  bobMismatchAct,
		carolUnapproved: carolUnapproved,
		unpublished:     unpublished,
		aliceYAML:       aliceYAML,
		bobYAML:         bobYAML,
		carolYAML:       carolYAML,
	}
}

type testHarness struct {
	t          *testing.T
	base       string
	stateDir   string
	policyDir  string
	keyDir     string
	binDir     string
	sentinel   string
	env        map[string]string
	goos       string
	username   string
	now        time.Time
	client     *http.Client
	runnerHits int64
}

func newHarness(t *testing.T, fx fixtureSet) *testHarness {
	t.Helper()
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	policyDir := filepath.Join(base, "policy")
	keyDir := filepath.Join(base, "key-directory")
	binDir := filepath.Join(base, "bin")
	sentinel := filepath.Join(base, "sentinel-invoked")

	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"git", "ssh-keygen", "ssh-add", "ssh", "gh", "osascript", "sudo", "security", "dscl"} {
		writeSentinel(t, binDir, tool, sentinel)
	}

	writePathKeyDirectory(t, keyDir, fx)
	writePolicyCheckout(t, policyDir, "path: '"+filepath.ToSlash(keyDir)+"'\n", fx)

	h := &testHarness{
		t:         t,
		base:      base,
		stateDir:  stateDir,
		policyDir: policyDir,
		keyDir:    keyDir,
		binDir:    binDir,
		sentinel:  sentinel,
		env: map[string]string{
			fleet.PolicyEnv:   policyDir,
			statedir.Override: stateDir,
			"PATH":            binDir,
		},
		goos:     "darwin",
		username: "dev",
		now:      time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC),
	}
	return h
}

func writeSentinel(t *testing.T, binDir, name, markerPath string) {
	t.Helper()
	posix := fmt.Sprintf("#!/bin/sh\necho %s >> %q\nexit 99\n", name, markerPath)
	p := filepath.Join(binDir, name)
	if err := os.WriteFile(p, []byte(posix), 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		bat := fmt.Sprintf("@echo off\r\necho %s >> \"%s\"\r\nexit /b 99\r\n", name, markerPath)
		for _, ext := range []string{".bat", ".cmd"} {
			if err := os.WriteFile(p+ext, []byte(bat), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func writePathKeyDirectory(t *testing.T, keyDir string, fx fixtureSet) {
	t.Helper()
	keysSub := filepath.Join(keyDir, "keys")
	if err := os.MkdirAll(keysSub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"alice.yaml": fx.aliceYAML,
		"bob.yaml":   fx.bobYAML,
		"carol.yaml": fx.carolYAML,
	} {
		if err := os.WriteFile(filepath.Join(keysSub, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writePolicyCheckout(t *testing.T, policyDir, kdSourceLine string, fx fixtureSet) {
	t.Helper()
	groupsDir := filepath.Join(policyDir, "fleet", "access-groups")
	if err := os.MkdirAll(groupsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kdYAML := "schema_version: 1\nkind: key-directory-source\n" + kdSourceLine
	if err := os.WriteFile(filepath.Join(policyDir, "fleet", "key-directory.yaml"), []byte(kdYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	groupAdmins := fmt.Sprintf(`schema_version: 1
kind: access-group
id: admins
description: Core administrators.
maintained_by: [alice]
accounts: [dev, root_admin]
approved_fingerprints:
  - %s
  - %s
  - %s
  - %s
`,
		fx.aliceHigh.fingerprint,
		fx.bobValid.fingerprint,
		fx.bobMismatchRec.fingerprint, // recorded on bob's mismatched key -> stays approved-unpublished
		fx.unpublished.fingerprint,    // never published -> stays approved-unpublished
	)
	groupWorkstations := fmt.Sprintf(`schema_version: 1
kind: access-group
id: workstations
description: Developer workstations.
maintained_by: [alice, bob]
accounts: [dev]
approved_fingerprints:
  - %s
  - %s
`,
		fx.aliceLow.fingerprint,
		fx.bobValid.fingerprint,
	)
	if err := os.WriteFile(filepath.Join(groupsDir, "admins.yaml"), []byte(groupAdmins), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(groupsDir, "workstations.yaml"), []byte(groupWorkstations), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (h *testHarness) run(args ...string) (string, string, int) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &stdout,
		Stderr: &stderr,
		Detect: func() (target.Host, error) {
			switch h.goos {
			case "windows":
				return target.Host{Target: target.WindowsAMD64}, nil
			case "linux":
				return target.Host{Target: target.LinuxAMD64}, nil
			default:
				return target.Host{Target: target.DarwinARM64}, nil
			}
		},
		Commands: []cli.Command{
			fleet.Command(fleet.Deps{
				Getenv:   func(k string) string { return h.env[k] },
				GOOS:     h.goos,
				Username: func() (string, error) { return h.username, nil },
				Now:      func() time.Time { return h.now },
				Client:   h.client,
				Runner: func(ctx context.Context, exe string, argv, env []string) (provider.Result, error) {
					atomic.AddInt64(&h.runnerHits, 1)
					return provider.Result{}, fmt.Errorf("unexpected process execution: %s %v", exe, argv)
				},
			}),
		},
	}
	code := app.Run(append([]string{"fleet"}, args...))
	if data, err := os.ReadFile(h.sentinel); err == nil {
		h.t.Fatalf("external binary invoked: %s", string(data))
	}
	if got := atomic.LoadInt64(&h.runnerHits); got != 0 {
		h.t.Fatalf("runner seam called %d times, want 0", got)
	}
	return stdout.String(), stderr.String(), code
}

// Acceptance 1: `fleet --keys` over a `path:` directory prints the D4
// `directory` record then one record per key in owner-then-fingerprint order
// with `state` `published` or `invalid` plus `detail`, in all three
// presentation modes with the fixed key order; `fleet <owner> --keys` filters
// to one owner; a positional with any other action is `MM_USAGE`; `--global`
// and `--custom` are `MM_USAGE` with the D1 message.
func TestKeysOverPathDirectoryPresentationsFilterAndUsage(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	out, errOut, code := h.run("--keys")
	if code != 0 || errOut != "" {
		t.Fatalf("fleet --keys: exit %d, stderr %q", code, errOut)
	}
	recs, err := output.ReadCompact([]byte(out))
	if err != nil {
		t.Fatalf("ReadCompact: %v", err)
	}
	// 1 leading directory record + 6 key records (3 alice, 2 bob, 1 carol).
	if len(recs) != 7 {
		t.Fatalf("got %d records, want 7:\n%s", len(recs), out)
	}

	// Verify leading directory record key order: directory, owners, keys, at.
	dirFields := recs[0].Fields()
	wantDirKeys := []string{"directory", "owners", "keys", "at"}
	assertFieldOrder(t, "directory record", dirFields, wantDirKeys)
	if dirID, _ := recs[0].Get("directory"); !strings.HasPrefix(fmt.Sprint(dirID), "file:") {
		t.Errorf("directory = %v, want file:<sha256>", dirID)
	}
	if owners, _ := recs[0].Get("owners"); fmt.Sprint(owners) != "3" {
		t.Errorf("owners = %v, want 3", owners)
	}
	if keys, _ := recs[0].Get("keys"); fmt.Sprint(keys) != "6" {
		t.Errorf("keys = %v, want 6", keys)
	}
	if at, _ := recs[0].Get("at"); at != "2026-10-22T02:00:00Z" {
		t.Errorf("at = %v, want 2026-10-22T02:00:00Z", at)
	}

	// Verify key records are sorted by owner then fingerprint, with state
	// "published" or "invalid" plus detail on invalid keys.
	type gotKey struct {
		owner  string
		fp     string
		state  string
		detail string
	}
	var gotKeys []gotKey
	for idx, r := range recs[1:] {
		st, _ := r.Get("state")
		det, hasDet := r.Get("detail")
		if st == "published" {
			assertFieldOrder(t, fmt.Sprintf("key[%d]", idx), r.Fields(),
				[]string{"owner", "fingerprint", "key_type", "label", "published_at", "revision", "state"})
			if hasDet {
				t.Errorf("key[%d] published record has unexpected detail %v", idx, det)
			}
		} else {
			assertFieldOrder(t, fmt.Sprintf("key[%d]", idx), r.Fields(),
				[]string{"owner", "fingerprint", "key_type", "label", "published_at", "revision", "state", "detail"})
		}
		ow, _ := r.Get("owner")
		fp, _ := r.Get("fingerprint")
		gotKeys = append(gotKeys, gotKey{
			owner:  fmt.Sprint(ow),
			fp:     fmt.Sprint(fp),
			state:  fmt.Sprint(st),
			detail: fmt.Sprint(det),
		})
	}

	// Verify alice's 3 keys are in fingerprint order (even though alice.yaml
	// declared aliceHigh before aliceLow).
	if gotKeys[0].owner != "alice" || gotKeys[1].owner != "alice" || gotKeys[2].owner != "alice" {
		t.Fatalf("first 3 keys are not alice: %+v", gotKeys)
	}
	if !(gotKeys[0].fp < gotKeys[1].fp && gotKeys[1].fp < gotKeys[2].fp) {
		t.Errorf("alice keys not in fingerprint order: %q, %q, %q", gotKeys[0].fp, gotKeys[1].fp, gotKeys[2].fp)
	}

	// Verify bob's 2 keys include one published and one invalid (fingerprint-mismatch).
	bobPublished := 0
	bobInvalid := 0
	for _, gk := range gotKeys[3:5] {
		if gk.owner != "bob" {
			t.Fatalf("expected bob key, got %+v", gk)
		}
		switch gk.state {
		case "published":
			bobPublished++
		case "invalid":
			bobInvalid++
			if gk.detail != directory.DetailFingerprintMismatch {
				t.Errorf("bob invalid detail = %q, want %q", gk.detail, directory.DetailFingerprintMismatch)
			}
		default:
			t.Errorf("unexpected state %q", gk.state)
		}
	}
	if bobPublished != 1 || bobInvalid != 1 {
		t.Errorf("bob keys: published=%d invalid=%d, want 1 and 1", bobPublished, bobInvalid)
	}

	// Bare `fleet` and `fleet --keys --user` produce identical output.
	bareOut, bareErr, bareCode := h.run()
	if bareCode != 0 || bareErr != "" || bareOut != out {
		t.Errorf("bare fleet differs from fleet --keys: code=%d stderr=%q", bareCode, bareErr)
	}
	userOut, userErr, userCode := h.run("--keys", "--user")
	if userCode != 0 || userErr != "" || userOut != out {
		t.Errorf("fleet --keys --user differs from fleet --keys: code=%d stderr=%q", userCode, userErr)
	}

	// Presentation modes: --json and --human.
	jsonOut, jsonErr, jsonCode := h.run("--keys", "--json")
	if jsonCode != 0 || jsonErr != "" {
		t.Fatalf("fleet --keys --json: exit %d, stderr %q", jsonCode, jsonErr)
	}
	var doc struct {
		Records []*output.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(jsonOut), &doc); err != nil {
		t.Fatalf("unmarshal --json: %v", err)
	}
	if len(doc.Records) != len(recs) {
		t.Errorf("--json records = %d, want %d", len(doc.Records), len(recs))
	}

	humanOut, humanErr, humanCode := h.run("--keys", "--human")
	if humanCode != 0 || humanErr != "" {
		t.Fatalf("fleet --keys --human: exit %d, stderr %q", humanCode, humanErr)
	}
	for _, col := range []string{"DIRECTORY", "OWNERS", "KEYS", "AT", "OWNER", "FINGERPRINT", "KEY_TYPE", "LABEL", "PUBLISHED_AT", "REVISION", "STATE", "DETAIL"} {
		if !strings.Contains(humanOut, col) {
			t.Errorf("--human output missing column %q:\n%s", col, humanOut)
		}
	}

	// `fleet <owner> --keys` filters to one owner.
	aliceOut, aliceErr, aliceCode := h.run("alice", "--keys")
	if aliceCode != 0 || aliceErr != "" {
		t.Fatalf("fleet alice --keys: exit %d, stderr %q", aliceCode, aliceErr)
	}
	aliceRecs, err := output.ReadCompact([]byte(aliceOut))
	if err != nil {
		t.Fatal(err)
	}
	if len(aliceRecs) != 4 { // 1 directory record + 3 alice keys
		t.Fatalf("fleet alice --keys: got %d records, want 4:\n%s", len(aliceRecs), aliceOut)
	}
	if owners, _ := aliceRecs[0].Get("owners"); fmt.Sprint(owners) != "1" {
		t.Errorf("filtered owners = %v, want 1", owners)
	}
	if keys, _ := aliceRecs[0].Get("keys"); fmt.Sprint(keys) != "3" {
		t.Errorf("filtered keys = %v, want 3", keys)
	}
	for _, r := range aliceRecs[1:] {
		if ow, _ := r.Get("owner"); ow != "alice" {
			t.Errorf("filtered record has owner %v, want alice", ow)
		}
	}

	// Positional with any non---keys action is MM_USAGE (exit 2).
	for _, args := range [][]string{
		{"alice", "--access"},
		{"alice", "--refresh"},
		{"alice", "--plan"},
		{"alice", "--apply"},
		{"alice", "--check"},
		{"alice", "--publish"},
		{"alice", "--unpublish", fx.aliceLow.fingerprint},
	} {
		o, e, c := h.run(args...)
		if c != 2 || o != "" || !strings.Contains(e, `"diagnostic":"MM_USAGE"`) {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q, want exit 2 MM_USAGE", args, c, o, e)
		}
	}

	// --global and --custom are MM_USAGE (exit 2) with the D1 message.
	customDir := filepath.Join(t.TempDir(), "custom-root")
	for _, args := range [][]string{
		{"--keys", "--global"},
		{"--access", "--global"},
		{"--keys", "--custom", customDir},
		{"--keys", "--custom=" + customDir},
		{"--access", "--custom", customDir},
	} {
		o, e, c := h.run(args...)
		if c != 2 || o != "" || !strings.Contains(e, `"diagnostic":"MM_USAGE"`) {
			t.Fatalf("%v: exit=%d stdout=%q stderr=%q, want exit 2 MM_USAGE", args, c, o, e)
		}
		for _, wantSub := range []string{"--user is the only target", "OS accounts", "Souls", "sshd"} {
			if !strings.Contains(e, wantSub) {
				t.Errorf("%v: stderr %q missing D1 phrase %q", args, e, wantSub)
			}
		}
	}
}

// Acceptance 2: `fleet --access` prints `account, fingerprint, state, owner,
// group` for the injected account, `authorized` only when a valid published
// key carries an approved fingerprint and `approved-unpublished` otherwise; a
// published key no group approves never appears (assert absence of its
// fingerprint in every output mode).
func TestAccessEvaluationAndAbsenceOfUnapprovedKeys(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	out, errOut, code := h.run("--access")
	if code != 0 || errOut != "" {
		t.Fatalf("fleet --access: exit %d, stderr %q", code, errOut)
	}
	recs, err := output.ReadCompact([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	// Expected for account "dev":
	// Authorized (owner-then-fingerprint):
	//   1. alice / aliceLow (workstations)
	//   2. alice / aliceHigh (admins)
	//   3. bob   / bobValid (admins, first in sorted group order [admins, workstations])
	// Approved-unpublished (fingerprint order):
	//   - bobMismatchRec (approved in admins, but bob's key has mismatched bytes -> invalid)
	//   - unpublished    (approved in admins, not in directory)
	if len(recs) != 5 {
		t.Fatalf("got %d access records, want 5:\n%s", len(recs), out)
	}

	for idx, r := range recs[:3] {
		assertFieldOrder(t, fmt.Sprintf("authorized[%d]", idx), r.Fields(),
			[]string{"account", "fingerprint", "state", "owner", "group"})
		if acct, _ := r.Get("account"); acct != "dev" {
			t.Errorf("authorized[%d] account = %v, want dev", idx, acct)
		}
		if st, _ := r.Get("state"); st != "authorized" {
			t.Errorf("authorized[%d] state = %v, want authorized", idx, st)
		}
	}
	for idx, r := range recs[3:] {
		assertFieldOrder(t, fmt.Sprintf("unpublished[%d]", idx), r.Fields(),
			[]string{"account", "fingerprint", "state", "group"})
		if _, hasOwner := r.Get("owner"); hasOwner {
			t.Errorf("unpublished[%d] unexpectedly has owner field", idx)
		}
		if st, _ := r.Get("state"); st != "approved-unpublished" {
			t.Errorf("unpublished[%d] state = %v, want approved-unpublished", idx, st)
		}
	}

	if fp0, _ := recs[0].Get("fingerprint"); fp0 != fx.aliceLow.fingerprint {
		t.Errorf("recs[0] fingerprint = %v, want %s", fp0, fx.aliceLow.fingerprint)
	}
	if g0, _ := recs[0].Get("group"); g0 != "workstations" {
		t.Errorf("recs[0] group = %v, want workstations", g0)
	}
	if fp1, _ := recs[1].Get("fingerprint"); fp1 != fx.aliceHigh.fingerprint {
		t.Errorf("recs[1] fingerprint = %v, want %s", fp1, fx.aliceHigh.fingerprint)
	}
	if g1, _ := recs[1].Get("group"); g1 != "admins" {
		t.Errorf("recs[1] group = %v, want admins", g1)
	}
	if fp2, _ := recs[2].Get("fingerprint"); fp2 != fx.bobValid.fingerprint {
		t.Errorf("recs[2] fingerprint = %v, want %s", fp2, fx.bobValid.fingerprint)
	}
	if g2, _ := recs[2].Get("group"); g2 != "admins" {
		t.Errorf("recs[2] group = %v, want admins", g2)
	}

	// Assert that unapproved published keys (aliceUnapproved, carolUnapproved,
	// bobMismatchAct) NEVER appear in ANY output mode (compact, --json, --human).
	jsonOut, jsonErr, jsonCode := h.run("--access", "--json")
	if jsonCode != 0 || jsonErr != "" {
		t.Fatalf("fleet --access --json: exit %d, stderr %q", jsonCode, jsonErr)
	}
	humanOut, humanErr, humanCode := h.run("--access", "--human")
	if humanCode != 0 || humanErr != "" {
		t.Fatalf("fleet --access --human: exit %d, stderr %q", humanCode, humanErr)
	}
	for modeName, stream := range map[string]string{
		"compact": out,
		"json":    jsonOut,
		"human":   humanOut,
	} {
		for label, unapprovedFP := range map[string]string{
			"alice-unapproved": fx.aliceUnapproved.fingerprint,
			"carol-unapproved": fx.carolUnapproved.fingerprint,
			"bob-mismatch-act": fx.bobMismatchAct.fingerprint,
		} {
			if strings.Contains(stream, unapprovedFP) {
				t.Errorf("%s output leaked unapproved fingerprint %s (%s):\n%s", modeName, label, unapprovedFP, stream)
			}
		}
	}

	// Windows DOMAIN\ prefix stripping and case-insensitive account matching (D1).
	h.goos = "windows"
	h.username = `CORP\DEV`
	winOut, winErr, winCode := h.run("--access")
	if winCode != 0 || winErr != "" {
		t.Fatalf("windows fleet --access: exit %d, stderr %q", winCode, winErr)
	}
	winRecs, err := output.ReadCompact([]byte(winOut))
	if err != nil {
		t.Fatal(err)
	}
	if len(winRecs) != 5 {
		t.Fatalf("windows access records = %d, want 5", len(winRecs))
	}
	if acct, _ := winRecs[0].Get("account"); acct != "DEV" {
		t.Errorf("windows stripped account = %v, want DEV", acct)
	}

	// Unix case-sensitive account matching: "DEV" does not match "dev".
	h.goos = "linux"
	h.username = "DEV"
	linuxOut, linuxErr, linuxCode := h.run("--access")
	if linuxCode != 0 || linuxErr != "" || linuxOut != "" {
		t.Errorf("linux uppercase DEV: exit=%d stdout=%q stderr=%q, want 0 and empty", linuxCode, linuxOut, linuxErr)
	}
}

// Acceptance 3: Policy directory resolution follows D2 with an injected getenv
// (MANAGED_MACHINE_POLICY_DIR, else <state>/managed-machine-config); a missing
// directory or missing fleet/key-directory.yaml is MM_FLEET_POLICY_UNAVAILABLE;
// the core never spawns git (runner seam call count zero).
func TestPolicyDirectoryResolutionAndUnavailableErrors(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	// 1. Default resolution: unset MANAGED_MACHINE_POLICY_DIR -> <state>/managed-machine-config.
	delete(h.env, fleet.PolicyEnv)
	defaultPolicyDir := filepath.Join(h.stateDir, policy.DefaultSubdir)

	// Before <state>/managed-machine-config exists -> MM_FLEET_POLICY_UNAVAILABLE (exit 2).
	out, errOut, code := h.run("--keys")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_POLICY_UNAVAILABLE"`) {
		t.Fatalf("missing default policy dir: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// Empty <state>/managed-machine-config without fleet/key-directory.yaml -> MM_FLEET_POLICY_UNAVAILABLE (exit 2).
	if err := os.MkdirAll(filepath.Join(defaultPolicyDir, "fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = h.run("--keys")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_POLICY_UNAVAILABLE"`) {
		t.Fatalf("missing fleet/key-directory.yaml: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// Populate <state>/managed-machine-config -> succeeds without MANAGED_MACHINE_POLICY_DIR.
	writePolicyCheckout(t, defaultPolicyDir, "path: '"+filepath.ToSlash(h.keyDir)+"'\n", fx)
	out, errOut, code = h.run("--keys")
	if code != 0 || errOut != "" || out == "" {
		t.Fatalf("populated default policy dir: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// 2. MANAGED_MACHINE_POLICY_DIR overrides <state>/managed-machine-config.
	missingOverride := filepath.Join(t.TempDir(), "missing-policy")
	h.env[fleet.PolicyEnv] = missingOverride
	out, errOut, code = h.run("--keys")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_POLICY_UNAVAILABLE"`) {
		t.Fatalf("missing override policy dir: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}

	// 3. Invalid policy document under fleet/ -> MM_FLEET_POLICY_INVALID (exit 2).
	badPolicyDir := filepath.Join(t.TempDir(), "bad-policy")
	if err := os.MkdirAll(filepath.Join(badPolicyDir, "fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badPolicyDir, "fleet", "key-directory.yaml"),
		[]byte("schema_version: 1\nkind: key-directory-source\ngist: NOT_LOWERCASE_HEX\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.env[fleet.PolicyEnv] = badPolicyDir
	out, errOut, code = h.run("--keys")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_POLICY_INVALID"`) {
		t.Fatalf("invalid policy: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
}

type gistServerFixture struct {
	srv        *httptest.Server
	client     *http.Client
	hits       int64
	gistID     string
	revision   string
	files      map[string]remoteGistFile
	redirectTo string
	rawHistory string
	rawGist    string
}

type remoteGistFile struct {
	Filename  string `json:"filename"`
	Size      int    `json:"size"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
}

func newGistServer(t *testing.T, gistID, revision string, files map[string]string) *gistServerFixture {
	t.Helper()
	gf := &gistServerFixture{
		gistID:   gistID,
		revision: revision,
		files:    make(map[string]remoteGistFile, len(files)),
	}
	for name, content := range files {
		gf.files[name] = remoteGistFile{
			Filename:  name,
			Size:      len(content),
			Truncated: false,
			Content:   content,
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&gf.hits, 1)
		if gf.redirectTo != "" {
			http.Redirect(w, r, gf.redirectTo, http.StatusFound)
			return
		}
		switch r.URL.Path {
		case "/gists/" + gf.gistID:
			if gf.rawHistory != "" {
				w.Write([]byte(gf.rawHistory))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"history": []map[string]string{{"version": gf.revision}},
			})
		case "/gists/" + gf.gistID + "/" + gf.revision:
			if gf.rawGist != "" {
				w.Write([]byte(gf.rawGist))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": gf.files,
			})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	gf.srv = srv
	gf.client = srv.Client()
	return gf
}

// Acceptance 4: `fleet --refresh` against a loopback TLS fixture serving
// `GET /gists/<id>` (history) and `GET /gists/<id>/<revision>` (files) resolves
// the revision, caches under `<state>/fleet/directory/` with the identity
// `gist:<id>@<revision>`, prints `directory, revision, keys, outcome`; a second
// refresh is `unchanged`; a truncated file, an oversized body, a redirect
// off-host, a non-https `MANAGED_MACHINE_GITHUB_API` and userinfo in it are
// each `MM_FLEET_DIRECTORY_FETCH_FAILED` or `MM_FLEET_POLICY_INVALID` as D3
// says; `--keys` with a configured but uncached gist is
// `MM_FLEET_DIRECTORY_UNAVAILABLE` and performs no request (fixture server
// records zero hits).
func TestGistRefreshCacheUncachedAndFailureModes(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	const gistID = "0123456789abcdef0123456789abcdef"
	const rev1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	gs := newGistServer(t, gistID, rev1, map[string]string{
		"alice.yaml": fx.aliceYAML,
		"bob.yaml":   fx.bobYAML,
		"carol.yaml": fx.carolYAML,
	})
	h.client = gs.client
	h.env[fleet.GitHubAPIEnv] = gs.srv.URL
	writePolicyCheckout(t, h.policyDir, "gist: "+gistID+"\n", fx)

	// 1. Before --refresh, `fleet --keys` and `fleet --access` return
	// MM_FLEET_DIRECTORY_UNAVAILABLE (exit 2) and make zero HTTP requests.
	out, errOut, code := h.run("--keys")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_DIRECTORY_UNAVAILABLE"`) {
		t.Fatalf("uncached gist --keys: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(errOut, "fleet --refresh") {
		t.Errorf("uncached diagnostic %q does not mention fleet --refresh", errOut)
	}
	if got := atomic.LoadInt64(&gs.hits); got != 0 {
		t.Fatalf("--keys made %d HTTP requests, want 0", got)
	}

	out, errOut, code = h.run("--access")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_DIRECTORY_UNAVAILABLE"`) {
		t.Fatalf("uncached gist --access: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := atomic.LoadInt64(&gs.hits); got != 0 {
		t.Fatalf("--access made %d HTTP requests, want 0", got)
	}

	// 2. First `fleet --refresh` resolves revision, caches under
	// <state>/fleet/directory/ with identity gist:<id>@<revision>, and prints
	// directory, revision, keys, outcome ("refreshed").
	out, errOut, code = h.run("--refresh")
	if code != 0 || errOut != "" {
		t.Fatalf("first refresh: exit=%d stderr=%q", code, errOut)
	}
	if got := atomic.LoadInt64(&gs.hits); got != 2 {
		t.Errorf("first refresh made %d HTTP requests (history + revision), want 2", got)
	}
	recs, err := output.ReadCompact([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d refresh records, want 1:\n%s", len(recs), out)
	}
	assertFieldOrder(t, "refresh record", recs[0].Fields(), []string{"directory", "revision", "keys", "outcome"})
	wantIdentity := "gist:" + gistID + "@" + rev1
	if d, _ := recs[0].Get("directory"); d != wantIdentity {
		t.Errorf("directory = %v, want %q", d, wantIdentity)
	}
	if r, _ := recs[0].Get("revision"); r != rev1 {
		t.Errorf("revision = %v, want %q", r, rev1)
	}
	if k, _ := recs[0].Get("keys"); fmt.Sprint(k) != "6" {
		t.Errorf("keys = %v, want 6", k)
	}
	if o, _ := recs[0].Get("outcome"); o != remote.OutcomeRefreshed {
		t.Errorf("outcome = %v, want %q", o, remote.OutcomeRefreshed)
	}

	// Verify cache directory exists at <state>/fleet/directory/.
	cacheDir := filepath.Join(h.stateDir, "fleet", "directory")
	if st, err := os.Stat(cacheDir); err != nil || !st.IsDir() {
		t.Fatalf("expected cache directory at %s: %v", cacheDir, err)
	}

	// 3. Second `fleet --refresh` reports outcome "unchanged".
	out2, errOut2, code2 := h.run("--refresh")
	if code2 != 0 || errOut2 != "" {
		t.Fatalf("second refresh: exit=%d stderr=%q", code2, errOut2)
	}
	recs2, err := output.ReadCompact([]byte(out2))
	if err != nil || len(recs2) != 1 {
		t.Fatalf("second refresh records: %v (%s)", err, out2)
	}
	if o, _ := recs2[0].Get("outcome"); o != remote.OutcomeUnchanged {
		t.Errorf("second refresh outcome = %v, want %q", o, remote.OutcomeUnchanged)
	}

	// 4. `fleet --keys` and `fleet --access` over the cached gist succeed with
	// zero new network hits and carry identity gist:<id>@<revision>.
	hitsBefore := atomic.LoadInt64(&gs.hits)
	keysOut, keysErr, keysCode := h.run("--keys")
	if keysCode != 0 || keysErr != "" {
		t.Fatalf("cached gist --keys: exit=%d stderr=%q", keysCode, keysErr)
	}
	if got := atomic.LoadInt64(&gs.hits) - hitsBefore; got != 0 {
		t.Errorf("cached gist --keys made %d HTTP requests, want 0", got)
	}
	keysRecs, err := output.ReadCompact([]byte(keysOut))
	if err != nil || len(keysRecs) != 7 {
		t.Fatalf("cached gist --keys records: %v (%d)", err, len(keysRecs))
	}
	if d, _ := keysRecs[0].Get("directory"); d != wantIdentity {
		t.Errorf("cached gist --keys directory = %v, want %q", d, wantIdentity)
	}

	accOut, accErr, accCode := h.run("--access")
	if accCode != 0 || accErr != "" {
		t.Fatalf("cached gist --access: exit=%d stderr=%q", accCode, accErr)
	}
	if got := atomic.LoadInt64(&gs.hits) - hitsBefore; got != 0 {
		t.Errorf("cached gist --access made %d HTTP requests, want 0", got)
	}
	accRecs, err := output.ReadCompact([]byte(accOut))
	if err != nil || len(accRecs) != 5 {
		t.Fatalf("cached gist --access records: %v (%d)", err, len(accRecs))
	}

	// 5. Failure modes for --refresh:
	// 5a. Non-https MANAGED_MACHINE_GITHUB_API -> MM_FLEET_POLICY_INVALID (exit 2), 0 hits.
	t.Run("non-https github api", func(t *testing.T) {
		hSub := newHarness(t, fx)
		hSub.client = gs.client
		writePolicyCheckout(t, hSub.policyDir, "gist: "+gistID+"\n", fx)
		hSub.env[fleet.GitHubAPIEnv] = "http://127.0.0.1:8443"
		before := atomic.LoadInt64(&gs.hits)
		o, e, c := hSub.run("--refresh")
		if c != 2 || o != "" || !strings.Contains(e, `"diagnostic":"MM_FLEET_POLICY_INVALID"`) {
			t.Errorf("non-https api: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_POLICY_INVALID", c, o, e)
		}
		if atomic.LoadInt64(&gs.hits) != before {
			t.Errorf("non-https api made network request")
		}
	})

	// 5b. Userinfo in MANAGED_MACHINE_GITHUB_API -> MM_FLEET_POLICY_INVALID (exit 2), 0 hits, secret not echoed.
	t.Run("userinfo in github api", func(t *testing.T) {
		hSub := newHarness(t, fx)
		hSub.client = gs.client
		writePolicyCheckout(t, hSub.policyDir, "gist: "+gistID+"\n", fx)
		secretToken := "super-secret-token-value" // gitleaks:allow
		hSub.env[fleet.GitHubAPIEnv] = "https://user:" + secretToken + "@api.github.com"
		before := atomic.LoadInt64(&gs.hits)
		o, e, c := hSub.run("--refresh")
		if c != 2 || o != "" || !strings.Contains(e, `"diagnostic":"MM_FLEET_POLICY_INVALID"`) {
			t.Errorf("userinfo api: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_POLICY_INVALID", c, o, e)
		}
		if strings.Contains(e, secretToken) {
			t.Errorf("userinfo secret leaked in stderr: %q", e)
		}
		if atomic.LoadInt64(&gs.hits) != before {
			t.Errorf("userinfo api made network request")
		}
	})

	// 5c. Truncated file in gist -> MM_FLEET_DIRECTORY_FETCH_FAILED (exit 1).
	t.Run("truncated file", func(t *testing.T) {
		gsTrunc := newGistServer(t, gistID, rev1, map[string]string{"alice.yaml": fx.aliceYAML})
		f := gsTrunc.files["alice.yaml"]
		f.Truncated = true
		gsTrunc.files["alice.yaml"] = f

		hSub := newHarness(t, fx)
		hSub.client = gsTrunc.client
		hSub.env[fleet.GitHubAPIEnv] = gsTrunc.srv.URL
		writePolicyCheckout(t, hSub.policyDir, "gist: "+gistID+"\n", fx)

		o, e, c := hSub.run("--refresh")
		if c != 1 || o != "" || !strings.Contains(e, `"diagnostic":"MM_FLEET_DIRECTORY_FETCH_FAILED"`) {
			t.Errorf("truncated file: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_DIRECTORY_FETCH_FAILED", c, o, e)
		}
		// Must not have cached the truncated gist.
		if _, errOut, kCode := hSub.run("--keys"); kCode != 2 || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_DIRECTORY_UNAVAILABLE"`) {
			t.Errorf("after truncated refresh, --keys gave exit=%d stderr=%q", kCode, errOut)
		}
	})

	// 5d. Oversized body (> MaxGistBytes and > MaxFileBytes) -> MM_FLEET_DIRECTORY_FETCH_FAILED (exit 1).
	t.Run("oversized body", func(t *testing.T) {
		gsBig := newGistServer(t, gistID, rev1, map[string]string{
			"alice.yaml": strings.Repeat("x", remote.MaxFileBytes+1),
		})
		hSub := newHarness(t, fx)
		hSub.client = gsBig.client
		hSub.env[fleet.GitHubAPIEnv] = gsBig.srv.URL
		writePolicyCheckout(t, hSub.policyDir, "gist: "+gistID+"\n", fx)

		o, e, c := hSub.run("--refresh")
		if c != 1 || o != "" || !strings.Contains(e, `"diagnostic":"MM_FLEET_DIRECTORY_FETCH_FAILED"`) {
			t.Errorf("oversized file: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_DIRECTORY_FETCH_FAILED", c, o, e)
		}

		gsBigResp := newGistServer(t, gistID, rev1, nil)
		gsBigResp.rawHistory = strings.Repeat("a", remote.MaxGistBytes+10)
		hSub.client = gsBigResp.client
		hSub.env[fleet.GitHubAPIEnv] = gsBigResp.srv.URL
		o, e, c = hSub.run("--refresh")
		if c != 1 || o != "" || !strings.Contains(e, `"diagnostic":"MM_FLEET_DIRECTORY_FETCH_FAILED"`) {
			t.Errorf("oversized history response: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_DIRECTORY_FETCH_FAILED", c, o, e)
		}
	})

	// 5e. Redirect off-host -> MM_FLEET_DIRECTORY_FETCH_FAILED (exit 1).
	t.Run("redirect off-host", func(t *testing.T) {
		other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"history":[{"version":"` + rev1 + `"}]}`))
		}))
		t.Cleanup(other.Close)

		gsRedir := newGistServer(t, gistID, rev1, map[string]string{"alice.yaml": fx.aliceYAML})
		gsRedir.redirectTo = other.URL + "/gists/" + gistID

		hSub := newHarness(t, fx)
		hSub.client = gsRedir.client
		hSub.env[fleet.GitHubAPIEnv] = gsRedir.srv.URL
		writePolicyCheckout(t, hSub.policyDir, "gist: "+gistID+"\n", fx)

		o, e, c := hSub.run("--refresh")
		if c != 1 || o != "" || !strings.Contains(e, `"diagnostic":"MM_FLEET_DIRECTORY_FETCH_FAILED"`) {
			t.Errorf("redirect off-host: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_DIRECTORY_FETCH_FAILED", c, o, e)
		}
	})
}

// Acceptance 5: The directory cache and the definitions snapshot cache are
// distinct directories; a key directory configured as
// `MANAGED_MACHINE_SOURCE_GIST` fails definition validation and a definitions
// gist configured as the key directory is `MM_FLEET_DIRECTORY_INVALID`.
func TestDirectoryCacheAndDefinitionsCacheAreDistinctAndCrossKindsFail(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	const keyGistID = "11111111111111111111111111111111"
	const defGistID = "22222222222222222222222222222222"
	const rev = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	const softwareDefYAML = `schema_version: 1
id: example
description: Example software definition.
categories: [tools]
recipes:
  - id: brew
    provider: brew
    package: example
    platforms: [darwin/arm64]
    targets: [global]
`

	// 1. Refreshing a fleet key-directory gist writes <state>/fleet/directory
	// and does not populate <state>/sources.
	keyServer := newGistServer(t, keyGistID, rev, map[string]string{"alice.yaml": fx.aliceYAML})
	h.client = keyServer.client
	h.env[fleet.GitHubAPIEnv] = keyServer.srv.URL
	writePolicyCheckout(t, h.policyDir, "gist: "+keyGistID+"\n", fx)

	if _, errOut, code := h.run("--refresh"); code != 0 || errOut != "" {
		t.Fatalf("fleet --refresh key gist: exit=%d stderr=%q", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "fleet", "directory")); err != nil {
		t.Fatalf("expected <state>/fleet/directory to exist: %v", err)
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "sources")); !os.IsNotExist(err) {
		t.Fatalf("<state>/sources should not exist after fleet --refresh: %v", err)
	}

	// Configuring the same gist in MANAGED_MACHINE_SOURCE_GIST without running
	// `software --refresh` finds no cached snapshot in <state>/sources.
	h.env[remote.GistEnv] = keyGistID + "@" + rev
	if _, _, err := software.LoadCatalog(func(k string) string { return h.env[k] }); err == nil || !strings.Contains(err.Error(), "no cached snapshot") {
		t.Fatalf("software.LoadCatalog without software --refresh: got %v, want no cached snapshot", err)
	}

	// 2. When the key-directory gist is refreshed as a definitions source
	// (`MANAGED_MACHINE_SOURCE_GIST`), definition validation fails on
	// `kind: key-directory` with MM_SOFTWARE_SOURCE_INVALID / MM_CONFIG_SOURCE_INVALID.
	var swOut, swErr bytes.Buffer
	swApp := &cli.App{
		Name:   "managed-machine",
		Stdout: &swOut,
		Stderr: &swErr,
		Detect: func() (target.Host, error) { return target.Host{Target: target.DarwinARM64}, nil },
		Commands: []cli.Command{
			software.Command(software.Deps{
				Catalog: func() ([]source.Entry, *registry.Registry, error) {
					return software.LoadCatalog(func(k string) string { return h.env[k] })
				},
				Getenv: func(k string) string { return h.env[k] },
				Client: keyServer.client,
			}),
			config.Command(config.Deps{
				Catalog: func() ([]config.Entry, error) {
					return config.LoadCatalog(func(k string) string { return h.env[k] })
				},
				Getenv: func(k string) string { return h.env[k] },
			}),
		},
	}
	if code := swApp.Run([]string{"software", "--refresh"}); code != 0 {
		t.Fatalf("software --refresh key gist: exit %d stderr %q", code, swErr.String())
	}
	swOut.Reset()
	swErr.Reset()
	if code := swApp.Run([]string{"software", "--available"}); code != 2 || !strings.Contains(swErr.String(), `"diagnostic":"MM_SOFTWARE_SOURCE_INVALID"`) {
		t.Errorf("software --available over key-directory gist: exit=%d stderr=%q, want 2 MM_SOFTWARE_SOURCE_INVALID", code, swErr.String())
	}
	swOut.Reset()
	swErr.Reset()
	if code := swApp.Run([]string{"config", "--available"}); code != 2 || !strings.Contains(swErr.String(), `"diagnostic":"MM_CONFIG_SOURCE_INVALID"`) {
		t.Errorf("config --available over key-directory gist: exit=%d stderr=%q, want 2 MM_CONFIG_SOURCE_INVALID", code, swErr.String())
	}

	// 3. A definitions gist configured as the fleet key directory fails with
	// MM_FLEET_DIRECTORY_INVALID (exit 2).
	defServer := newGistServer(t, defGistID, rev, map[string]string{"example.yaml": softwareDefYAML})
	h.client = defServer.client
	h.env[fleet.GitHubAPIEnv] = defServer.srv.URL
	writePolicyCheckout(t, h.policyDir, "gist: "+defGistID+"\n", fx)

	out, errOut, code := h.run("--refresh")
	if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_DIRECTORY_INVALID"`) {
		t.Fatalf("fleet --refresh over definitions gist: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_DIRECTORY_INVALID", code, out, errOut)
	}
}

func TestCommandGoldens(t *testing.T) {
	fx := newFixtureSet(t)
	const gistID = "0123456789abcdef0123456789abcdef"
	const rev = "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name     string
		args     []string
		useGist  bool
		seedGist bool
	}{
		{"keys-default", nil, false, false},
		{"keys-json", []string{"--keys", "--json"}, false, false},
		{"keys-human", []string{"--keys", "--human"}, false, false},
		{"keys-owner", []string{"alice", "--keys"}, false, false},
		{"access", []string{"--access"}, false, false},
		{"access-json", []string{"--access", "--json"}, false, false},
		{"access-human", []string{"--access", "--human"}, false, false},
		{"refresh", []string{"--refresh"}, true, false},
		{"refresh-unchanged", []string{"--refresh"}, true, true},
		{"keys-gist", []string{"--keys"}, true, true},
		{"help", []string{"-h"}, false, false},
		{"target-global", []string{"--keys", "--global"}, false, false},
		{"positional-with-access", []string{"alice", "--access"}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, fx)
			if tc.useGist {
				gs := newGistServer(t, gistID, rev, map[string]string{
					"alice.yaml": fx.aliceYAML,
					"bob.yaml":   fx.bobYAML,
					"carol.yaml": fx.carolYAML,
				})
				h.client = gs.client
				h.env[fleet.GitHubAPIEnv] = gs.srv.URL
				writePolicyCheckout(t, h.policyDir, "gist: "+gistID+"\n", fx)
				if tc.seedGist {
					if _, errOut, code := h.run("--refresh"); code != 0 {
						t.Fatalf("seed refresh: exit %d %q", code, errOut)
					}
				}
			}
			out, errOut, code := h.run(tc.args...)
			got := "exit " + strconv.Itoa(code) + "\n--- stdout\n" + out + "--- stderr\n" + errOut
			goldenPath := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v", goldenPath, err)
			}
			if got != string(want) {
				t.Errorf("differs from %s:\ngot:\n%s\nwant:\n%s", goldenPath, got, string(want))
			}
		})
	}
}

func assertFieldOrder(t *testing.T, label string, fields []output.Field, want []string) {
	t.Helper()
	if len(fields) != len(want) {
		got := make([]string, len(fields))
		for i, f := range fields {
			got[i] = f.Key
		}
		t.Fatalf("%s: got fields %v, want %v", label, got, want)
	}
	for i, f := range fields {
		if f.Key != want[i] {
			t.Errorf("%s: field[%d] = %q, want %q", label, i, f.Key, want[i])
		}
	}
}
