package authz

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/core/internal/fleet/directory"
	"github.com/qwts/managed-machine/core/internal/fleet/policy"
)

type testKey struct {
	line        string
	blobB64     string
	fingerprint string
}

func makeEd25519Key(t *testing.T, seedByte byte, comment string) testKey {
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
	return testKey{
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

func TestEvaluatorThreeOwnersTwoGroups(t *testing.T) {
	// Generate deterministic keys in-process.
	kAliceA := makeEd25519Key(t, 0x01, "alice@laptop-a")
	kAliceB := makeEd25519Key(t, 0x02, "alice@laptop-b")
	// Order kAliceHigh and kAliceLow by fingerprint so YAML declaration order
	// is intentionally reversed (high first, low second) to verify that the
	// evaluator sorts by owner then fingerprint.
	kAliceLow, kAliceHigh := kAliceA, kAliceB
	if kAliceLow.fingerprint > kAliceHigh.fingerprint {
		kAliceLow, kAliceHigh = kAliceHigh, kAliceLow
	}

	kAliceUnapproved := makeEd25519Key(t, 0x03, "alice@unapproved")
	kBobValid := makeEd25519Key(t, 0x04, "bob@workstation")
	kBobActualOther := makeEd25519Key(t, 0x05, "bob@mismatch-actual")
	kBobRecordedApproved := makeEd25519Key(t, 0x06, "bob@mismatch-recorded")
	kCarolValid := makeEd25519Key(t, 0x07, "carol@desktop")
	kSharedDup := makeEd25519Key(t, 0x08, "shared@duplicate")
	kMalformedRecorded := makeEd25519Key(t, 0x09, "bob@malformed-recorded")
	kUnpublished := makeEd25519Key(t, 0x0a, "never@published")

	// Build a malformed key line whose prefix is ssh-rsa but embedded blob type is ssh-ed25519.
	malformedLine := directory.KeyTypeRSA + " " + kAliceLow.blobB64 + " bob@malformed"

	// Three owners: carol, bob, alice.
	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    label: high-fp
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: low-fp
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: unapproved-key
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		kAliceHigh.fingerprint, kAliceHigh.line,
		kAliceLow.fingerprint, kAliceLow.line,
		kAliceUnapproved.fingerprint, kAliceUnapproved.line,
	)

	bobYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: bob
keys:
  - fingerprint: %s
    key: %s
    label: bob-valid
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: bob-fingerprint-mismatch
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: bob-malformed
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: bob-dup
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		kBobValid.fingerprint, kBobValid.line,
		kBobRecordedApproved.fingerprint, kBobActualOther.line, // mismatch: recorded fingerprint is approved, key bytes differ
		kMalformedRecorded.fingerprint, malformedLine, // malformed: embedded type != prefix
		kSharedDup.fingerprint, kSharedDup.line, // duplicate across bob and carol
	)

	carolYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: carol
keys:
  - fingerprint: %s
    key: %s
    label: carol-valid
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: carol-dup
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		kCarolValid.fingerprint, kCarolValid.line,
		kSharedDup.fingerprint, kSharedDup.line, // duplicate across bob and carol
	)

	dir, err := directory.LoadFS(fstest.MapFS{
		"keys/alice.yaml": {Data: []byte(aliceYAML)},
		"keys/bob.yaml":   {Data: []byte(bobYAML)},
		"keys/carol.yaml": {Data: []byte(carolYAML)},
	})
	if err != nil {
		t.Fatalf("LoadFS directory: %v", err)
	}

	// Two groups: "admins" and "work", both naming "dev".
	groupAdminsYAML := fmt.Sprintf(`schema_version: 1
kind: access-group
id: admins
maintained_by: [alice]
accounts: [dev, root_admin]
approved_fingerprints:
  - %s
  - %s
  - %s
  - %s
  - %s
`,
		kAliceHigh.fingerprint,
		kBobValid.fingerprint,
		kBobRecordedApproved.fingerprint, // recorded on bob's mismatched key
		kSharedDup.fingerprint,           // duplicated across bob and carol
		kUnpublished.fingerprint,         // not in directory at all
	)

	groupWorkYAML := fmt.Sprintf(`schema_version: 1
kind: access-group
id: work
maintained_by: [bob, carol]
accounts: [dev]
approved_fingerprints:
  - %s
  - %s
  - %s
  - %s
`,
		kAliceLow.fingerprint,
		kBobValid.fingerprint, // approved by both groups
		kCarolValid.fingerprint,
		kMalformedRecorded.fingerprint, // recorded on bob's malformed key
	)

	gAdmins, err := policy.ParseAccessGroup("fleet/access-groups/admins.yaml", []byte(groupAdminsYAML))
	if err != nil {
		t.Fatalf("ParseAccessGroup(admins): %v", err)
	}
	gWork, err := policy.ParseAccessGroup("fleet/access-groups/work.yaml", []byte(groupWorkYAML))
	if err != nil {
		t.Fatalf("ParseAccessGroup(work): %v", err)
	}
	groups := []policy.AccessGroup{gWork, gAdmins}

	res := Evaluate(groups, dir, "dev", "darwin")

	// Authorized set must be in owner-then-fingerprint order:
	// 1. alice / kAliceLow
	// 2. alice / kAliceHigh
	// 3. bob / kBobValid
	// 4. carol / kCarolValid
	type pair struct {
		owner string
		fp    string
	}
	var gotAuth []pair
	for _, a := range res.Authorized {
		gotAuth = append(gotAuth, pair{owner: a.Owner, fp: a.Fingerprint})
	}
	wantAuth := []pair{
		{owner: "alice", fp: kAliceLow.fingerprint},
		{owner: "alice", fp: kAliceHigh.fingerprint},
		{owner: "bob", fp: kBobValid.fingerprint},
		{owner: "carol", fp: kCarolValid.fingerprint},
	}
	if !reflect.DeepEqual(gotAuth, wantAuth) {
		t.Fatalf("Authorized =\n%+v\nwant:\n%+v", gotAuth, wantAuth)
	}

	// Verify rendered authorized_keys lines replace published comments with owner id.
	wantLines := []string{
		directory.KeyTypeEd25519 + " " + kAliceLow.blobB64 + " alice",
		directory.KeyTypeEd25519 + " " + kAliceHigh.blobB64 + " alice",
		directory.KeyTypeEd25519 + " " + kBobValid.blobB64 + " bob",
		directory.KeyTypeEd25519 + " " + kCarolValid.blobB64 + " carol",
	}
	if gotLines := res.Lines(); !reflect.DeepEqual(gotLines, wantLines) {
		t.Errorf("Lines() =\n%v\nwant:\n%v", gotLines, wantLines)
	}

	// Verify bob's key records both groups in canonical order.
	if got := res.Authorized[2].Groups; !reflect.DeepEqual(got, []string{"admins", "work"}) || res.Authorized[2].Group != "admins" {
		t.Errorf("bob key groups = %v (primary %q), want [admins work]", got, res.Authorized[2].Group)
	}

	// Verify approved-unpublished contains the 4 approved fingerprints with no valid published key:
	// - kBobRecordedApproved (mismatch key)
	// - kMalformedRecorded (malformed key)
	// - kSharedDup (duplicate key)
	// - kUnpublished (not in directory)
	gotUnpub := res.UnpublishedFingerprints()
	wantUnpub := []string{
		kBobRecordedApproved.fingerprint,
		kMalformedRecorded.fingerprint,
		kSharedDup.fingerprint,
		kUnpublished.fingerprint,
	}
	// wantUnpub in sorted order:
	for i := 0; i < len(wantUnpub); i++ {
		for j := i + 1; j < len(wantUnpub); j++ {
			if wantUnpub[j] < wantUnpub[i] {
				wantUnpub[i], wantUnpub[j] = wantUnpub[j], wantUnpub[i]
			}
		}
	}
	if !reflect.DeepEqual(gotUnpub, wantUnpub) {
		t.Errorf("UnpublishedFingerprints() =\n%v\nwant:\n%v", gotUnpub, wantUnpub)
	}

	// Account named in no group returns an empty authorized and unpublished set.
	emptyRes := Evaluate(groups, dir, "nobody", "darwin")
	if len(emptyRes.Authorized) != 0 || len(emptyRes.ApprovedUnpublished) != 0 {
		t.Errorf("account in no group: got %+v, want empty sets", emptyRes)
	}
}

func TestAccountCaseHandlingFollowsD1ByGOOS(t *testing.T) {
	kAlice := makeEd25519Key(t, 0x21, "alice@host")
	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`, kAlice.fingerprint, kAlice.line)

	dir, err := directory.LoadFS(fstest.MapFS{
		"keys/alice.yaml": {Data: []byte(aliceYAML)},
	})
	if err != nil {
		t.Fatal(err)
	}

	groupYAML := fmt.Sprintf(`schema_version: 1
kind: access-group
id: mixed-case
maintained_by: [alice]
accounts: [AliceUser]
approved_fingerprints:
  - %s
`, kAlice.fingerprint)
	g, err := policy.ParseAccessGroup("fleet/access-groups/mixed-case.yaml", []byte(groupYAML))
	if err != nil {
		t.Fatal(err)
	}
	groups := []policy.AccessGroup{g}

	// Unix (darwin and linux): account matching is case-sensitive.
	for _, goos := range []string{"darwin", "linux"} {
		exact := Evaluate(groups, dir, "AliceUser", goos)
		if len(exact.Authorized) != 1 {
			t.Errorf("%s exact case: got %d authorized, want 1", goos, len(exact.Authorized))
		}
		lower := Evaluate(groups, dir, "aliceuser", goos)
		if len(lower.Authorized) != 0 || len(lower.ApprovedUnpublished) != 0 {
			t.Errorf("%s lower case: got %+v, want empty set", goos, lower)
		}
		upper := Evaluate(groups, dir, "ALICEUSER", goos)
		if len(upper.Authorized) != 0 || len(upper.ApprovedUnpublished) != 0 {
			t.Errorf("%s upper case: got %+v, want empty set", goos, upper)
		}
	}

	// Windows: account matching is case-insensitive (and handles DOMAIN\user).
	for _, acct := range []string{"AliceUser", "aliceuser", "ALICEUSER", `WORKSTATION\aliceuser`} {
		winRes := Evaluate(groups, dir, acct, "windows")
		if len(winRes.Authorized) != 1 || winRes.Authorized[0].Fingerprint != kAlice.fingerprint {
			t.Errorf("windows account %q: got %+v, want 1 authorized key", acct, winRes)
		}
	}
}
