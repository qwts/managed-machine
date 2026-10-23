package fleet_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/output"
)

func writeKeyFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func readEnrollmentRecord(t *testing.T, stateDir string) (fleet.Enrollment, *output.Record) {
	t.Helper()
	p := filepath.Join(stateDir, "fleet", "enrollment.json")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read enrollment.json: %v", err)
	}
	recs, err := output.ReadCompact(data)
	if err != nil {
		t.Fatalf("ReadCompact enrollment.json: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("enrollment.json records = %d, want 1", len(recs))
	}
	assertFieldOrder(t, "enrollment.json", recs[0].Fields(), []string{
		"schema_version", "machine_id", "owner", "fingerprint", "key_type", "directory", "outcome", "at",
	})
	var e fleet.Enrollment
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatalf("Unmarshal enrollment.json: %v", err)
	}
	return e, recs[0]
}

func hashTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			fmt.Fprintf(&b, "dir:%s\n", rel)
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		fmt.Fprintf(&b, "file:%s:%s\n", rel, hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("hashTree(%s): %v", root, err)
	}
	return b.String()
}

// Acceptance 1: `fleet --publish --owner alice --key <pub>` to a `path:`
// directory writes `keys/alice.yaml` atomically (temporary file then rename)
// with `kind`, the key, the computed fingerprint, `label`, `published_at` from
// an injected clock and `revision` = highest plus one; running it again with
// the same key changes only `published_at` (idempotent, revision unchanged); a
// second key appends; the resulting file validates through the L01 loader and
// byte-matches a golden.
func TestPublishPathDirectoryAtomicIdempotentAppendAndGolden(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	// Start with a clean keys/ directory containing only bob.yaml so alice.yaml
	// is created fresh on the first publish, re-published idempotently, and
	// then appended with a second key.
	alicePath := filepath.Join(h.keyDir, "keys", "alice.yaml")
	carolPath := filepath.Join(h.keyDir, "keys", "carol.yaml")
	if err := os.Remove(alicePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(carolPath); err != nil {
		t.Fatal(err)
	}

	pub1 := writeKeyFile(t, fx.aliceLow.line+"\n")
	pub2 := writeKeyFile(t, fx.aliceHigh.line+"\n")

	// 1. First publish for alice: creates keys/alice.yaml with revision 1.
	h.now = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	out1, err1, code1 := h.run("--publish", "--owner", "alice", "--key", pub1, "--label", "alice-low")
	if code1 != 0 || err1 != "" {
		t.Fatalf("first publish: exit=%d stderr=%q", code1, err1)
	}
	recs1, err := output.ReadCompact([]byte(out1))
	if err != nil || len(recs1) != 1 {
		t.Fatalf("first publish records: %v (%q)", err, out1)
	}
	assertFieldOrder(t, "path publication record", recs1[0].Fields(), []string{"owner", "fingerprint", "directory"})
	if ow, _ := recs1[0].Get("owner"); ow != "alice" {
		t.Errorf("owner = %v, want alice", ow)
	}
	if fp, _ := recs1[0].Get("fingerprint"); fp != fx.aliceLow.fingerprint {
		t.Errorf("fingerprint = %v, want %s", fp, fx.aliceLow.fingerprint)
	}

	bytes1, err := os.ReadFile(alicePath)
	if err != nil {
		t.Fatalf("read alice.yaml after first publish: %v", err)
	}
	entry1, err := directory.ParseEntry("keys/alice.yaml", "alice.yaml", bytes1)
	if err != nil {
		t.Fatalf("ParseEntry after first publish: %v", err)
	}
	if entry1.Kind != directory.KindKeyDirectory || entry1.ID != "alice" || len(entry1.Keys) != 1 {
		t.Fatalf("unexpected entry1: %+v", entry1)
	}
	if got := entry1.Keys[0]; got.Fingerprint != fx.aliceLow.fingerprint || got.Key != fx.aliceLow.line || got.Label != "alice-low" || got.PublishedAt != "2026-10-22T02:00:00Z" || got.Revision != 1 {
		t.Fatalf("unexpected key after first publish: %+v", got)
	}

	wantMID1, err := directory.MachineIDFromFingerprint(fx.aliceLow.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	enroll1, _ := readEnrollmentRecord(t, h.stateDir)
	if enroll1.SchemaVersion != 1 || enroll1.MachineID != wantMID1 || enroll1.Owner != "alice" ||
		enroll1.Fingerprint != fx.aliceLow.fingerprint || enroll1.KeyType != directory.KeyTypeEd25519 || // gitleaks:allow
		enroll1.Outcome != fleet.OutcomeWritten || enroll1.At != "2026-10-22T02:00:00Z" ||
		!strings.HasPrefix(enroll1.Directory, "file:") {
		t.Errorf("enrollment after first publish = %+v", enroll1)
	}

	// 2. Second publish with the same key at a later clock: changes ONLY
	// published_at (idempotent, revision stays 1).
	h.now = time.Date(2026, 10, 22, 3, 30, 0, 0, time.UTC)
	out2, err2, code2 := h.run("--publish", "--owner", "alice", "--key", pub1)
	if code2 != 0 || err2 != "" || out2 == "" {
		t.Fatalf("idempotent publish: exit=%d stderr=%q", code2, err2)
	}
	bytes2, err := os.ReadFile(alicePath)
	if err != nil {
		t.Fatal(err)
	}
	wantIdempotent := strings.Replace(string(bytes1), `"2026-10-22T02:00:00Z"`, `"2026-10-22T03:30:00Z"`, 1)
	if string(bytes2) != wantIdempotent {
		t.Errorf("idempotent publish changed more than published_at:\ngot:\n%s\nwant:\n%s", string(bytes2), wantIdempotent)
	}
	entry2, err := directory.ParseEntry("keys/alice.yaml", "alice.yaml", bytes2)
	if err != nil {
		t.Fatalf("ParseEntry after idempotent publish: %v", err)
	}
	if len(entry2.Keys) != 1 || entry2.Keys[0].Revision != 1 || entry2.Keys[0].PublishedAt != "2026-10-22T03:30:00Z" || entry2.Keys[0].Label != "alice-low" {
		t.Fatalf("entry2 = %+v", entry2)
	}

	// 3. Publish a second key: appends with revision = highest + 1 (2).
	h.now = time.Date(2026, 10, 22, 4, 0, 0, 0, time.UTC)
	out3, err3, code3 := h.run("--publish", "--owner", "alice", "--key", pub2, "--label", "alice-high")
	if code3 != 0 || err3 != "" {
		t.Fatalf("second key publish: exit=%d stderr=%q:\n%s", code3, err3, out3)
	}
	bytes3, err := os.ReadFile(alicePath)
	if err != nil {
		t.Fatal(err)
	}
	entry3, err := directory.ParseEntry("keys/alice.yaml", "alice.yaml", bytes3)
	if err != nil {
		t.Fatalf("ParseEntry after second key: %v", err)
	}
	if len(entry3.Keys) != 2 || entry3.Keys[0].Revision != 1 || entry3.Keys[1].Revision != 2 {
		t.Fatalf("entry3 = %+v", entry3)
	}
	loadedDir, err := directory.LoadPath(h.keyDir)
	if err != nil {
		t.Fatalf("LoadPath after second key: %v", err)
	}

	// Ensure no leftover temporary files exist in keys/.
	dirEnts, err := os.ReadDir(filepath.Join(h.keyDir, "keys"))
	if err != nil {
		t.Fatal(err)
	}
	for _, de := range dirEnts {
		if strings.HasPrefix(de.Name(), ".mm-") {
			t.Errorf("leftover temp file in keys/: %s", de.Name())
		}
	}

	// Golden check on the resulting keys/alice.yaml.
	goldenPath := filepath.Join("testdata", "publish-alice.yaml.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, bytes3, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	wantGolden, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}
	if string(bytes3) != string(wantGolden) {
		t.Errorf("alice.yaml differs from %s:\ngot:\n%s\nwant:\n%s", goldenPath, string(bytes3), string(wantGolden))
	}

	// Verify enrollment.json after second key.
	wantMID2, err := directory.MachineIDFromFingerprint(fx.aliceHigh.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	enroll3, _ := readEnrollmentRecord(t, h.stateDir)
	if enroll3.MachineID != wantMID2 || enroll3.Fingerprint != fx.aliceHigh.fingerprint ||
		enroll3.Directory != loadedDir.Identity || enroll3.Outcome != fleet.OutcomeWritten ||
		enroll3.At != "2026-10-22T04:00:00Z" {
		t.Errorf("enrollment3 = %+v, want dir %s", enroll3, loadedDir.Identity)
	}

	// Presentation modes (--json and --human) on --publish.
	pub3 := writeKeyFile(t, fx.aliceUnapproved.line+"\n")
	jsonOut, jsonErr, jsonCode := h.run("--publish", "--owner", "alice", "--key", pub3, "--label", "alice-third", "--json")
	if jsonCode != 0 || jsonErr != "" {
		t.Fatalf("--publish --json: exit=%d stderr=%q", jsonCode, jsonErr)
	}
	var jsonDoc map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &jsonDoc); err != nil {
		t.Fatalf("unmarshal --json: %v", err)
	}
	if jsonDoc["owner"] != "alice" || jsonDoc["fingerprint"] != fx.aliceUnapproved.fingerprint {
		t.Errorf("--json doc = %+v", jsonDoc)
	}
	entry4, err := directory.ParseEntry("keys/alice.yaml", "alice.yaml", mustReadFile(t, alicePath))
	if err != nil {
		t.Fatal(err)
	}
	if len(entry4.Keys) != 3 || entry4.Keys[2].Revision != 3 {
		t.Errorf("expected third key at revision 3, got %+v", entry4.Keys)
	}

	humanOut, humanErr, humanCode := h.run("--publish", "--owner", "alice", "--key", pub3, "--human")
	if humanCode != 0 || humanErr != "" {
		t.Fatalf("--publish --human: exit=%d stderr=%q", humanCode, humanErr)
	}
	for _, col := range []string{"OWNER", "FINGERPRINT", "DIRECTORY"} {
		if !strings.Contains(humanOut, col) {
			t.Errorf("--human output missing %s:\n%s", col, humanOut)
		}
	}
}

// Acceptance 2: With a `gist:` directory the command never contacts the
// network (fixture server records zero hits), writes
// `<state>/fleet/staged/alice.yaml` built from the cached entry, and prints
// the `publication` record whose `next_step` is exactly
// `gh gist edit <id> -f alice.yaml <staged path>`; nothing under the harness
// `.ssh` changes (hash before and after).
func TestPublishGistDirectoryStagesWithoutNetworkAndPreservesSSH(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	// Set up an isolated harness home with a populated .ssh directory.
	fakeHome := filepath.Join(h.base, "home")
	sshDir := filepath.Join(fakeHome, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"authorized_keys": fx.aliceLow.line + "\n",
		"id_ed25519":      "FAKE-PRIVATE-KEY-MATERIAL\n",
		"id_ed25519.pub":  fx.aliceLow.line + "\n",
		"config":          "Host *\n  IdentitiesOnly yes\n",
	} {
		if err := os.WriteFile(filepath.Join(sshDir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h.env["HOME"] = fakeHome
	h.env["USERPROFILE"] = fakeHome
	sshHashBefore := hashTree(t, sshDir)

	const gistID = "0123456789abcdef0123456789abcdef"
	const rev1 = "0123456789abcdef0123456789abcdef01234567"
	gs := newGistServer(t, gistID, rev1, map[string]string{
		"alice.yaml": fx.aliceYAML,
		"bob.yaml":   fx.bobYAML,
		"carol.yaml": fx.carolYAML,
	})
	h.client = gs.client
	h.env[fleet.GitHubAPIEnv] = gs.srv.URL
	writePolicyCheckout(t, h.policyDir, "gist: "+gistID+"\n", fx)

	// Seed the gist cache via --refresh, then reset the server hit counter.
	if _, errOut, code := h.run("--refresh"); code != 0 || errOut != "" {
		t.Fatalf("seed refresh: exit=%d stderr=%q", code, errOut)
	}
	atomic.StoreInt64(&gs.hits, 0)

	newKeyPath := writeKeyFile(t, fx.unpublished.line+"\n")
	out, errOut, code := h.run("--publish", "--owner", "alice", "--key", newKeyPath, "--label", "alice-staged")
	if code != 0 || errOut != "" {
		t.Fatalf("gist publish: exit=%d stderr=%q", code, errOut)
	}
	if got := atomic.LoadInt64(&gs.hits); got != 0 {
		t.Fatalf("gist publish made %d HTTP requests, want 0", got)
	}

	wantStaged := filepath.Join(h.stateDir, "fleet", "staged", "alice.yaml")
	wantNextStep := fmt.Sprintf("gh gist edit %s -f alice.yaml %s", gistID, wantStaged)
	wantDirID := "gist:" + gistID + "@" + rev1

	recs, err := output.ReadCompact([]byte(out))
	if err != nil || len(recs) != 1 {
		t.Fatalf("ReadCompact gist publish: %v (%q)", err, out)
	}
	assertFieldOrder(t, "gist publication record", recs[0].Fields(), []string{
		"owner", "fingerprint", "directory", "staged", "next_step",
	})
	if got, _ := recs[0].Get("owner"); got != "alice" {
		t.Errorf("owner = %v, want alice", got)
	}
	if got, _ := recs[0].Get("fingerprint"); got != fx.unpublished.fingerprint {
		t.Errorf("fingerprint = %v, want %s", got, fx.unpublished.fingerprint)
	}
	if got, _ := recs[0].Get("directory"); got != wantDirID {
		t.Errorf("directory = %v, want %s", got, wantDirID)
	}
	if got, _ := recs[0].Get("staged"); got != wantStaged {
		t.Errorf("staged = %v, want %s", got, wantStaged)
	}
	if got, _ := recs[0].Get("next_step"); got != wantNextStep {
		t.Errorf("next_step = %q, want %q", got, wantNextStep)
	}

	// Verify staged file was built from cached alice.yaml (3 existing keys + 1 new key at revision 3).
	stagedBytes, err := os.ReadFile(wantStaged)
	if err != nil {
		t.Fatalf("read staged alice.yaml: %v", err)
	}
	stagedEntry, err := directory.ParseEntry("alice.yaml", "alice.yaml", stagedBytes)
	if err != nil {
		t.Fatalf("ParseEntry staged alice.yaml: %v", err)
	}
	if len(stagedEntry.Keys) != 4 {
		t.Fatalf("staged keys = %d, want 4", len(stagedEntry.Keys))
	}
	lastKey := stagedEntry.Keys[3]
	if lastKey.Fingerprint != fx.unpublished.fingerprint || lastKey.Label != "alice-staged" ||
		lastKey.Revision != 3 || lastKey.PublishedAt != "2026-10-22T02:00:00Z" {
		t.Errorf("staged last key = %+v", lastKey)
	}

	// Verify enrollment.json has outcome "staged" and D3 machine_id.
	wantMID, err := directory.MachineIDFromFingerprint(fx.unpublished.fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	enroll, _ := readEnrollmentRecord(t, h.stateDir)
	if enroll.SchemaVersion != 1 || enroll.MachineID != wantMID || enroll.Owner != "alice" ||
		enroll.Fingerprint != fx.unpublished.fingerprint || enroll.KeyType != directory.KeyTypeEd25519 ||
		enroll.Directory != wantDirID || enroll.Outcome != fleet.OutcomeStaged ||
		enroll.At != "2026-10-22T02:00:00Z" {
		t.Errorf("gist enrollment = %+v", enroll)
	}

	// Verify nothing under .ssh changed.
	sshHashAfter := hashTree(t, sshDir)
	if sshHashAfter != sshHashBefore {
		t.Errorf(".ssh changed during publish:\nbefore:\n%s\nafter:\n%s", sshHashBefore, sshHashAfter)
	}
}

// Acceptance 3a: `--key` naming a private key, an empty file, a file whose
// first token is not a D3 key type, or a missing file is `MM_FLEET_KEY_INVALID`
// (exit 2) and the diagnostic message contains none of the file's bytes.
func TestPublishKeyInvalidAndDiagnosticContainsNoFileBytes(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	// Build a key whose prefix is ssh-ed25519 but whose wire blob embeds ssh-rsa.
	var badEmbeddedBlob []byte
	badEmbeddedBlob = append(badEmbeddedBlob, 0, 0, 0, 7)
	badEmbeddedBlob = append(badEmbeddedBlob, []byte("ssh-rsa")...)
	badEmbeddedBlob = append(badEmbeddedBlob, []byte("payload-bytes")...)

	cases := []struct {
		name      string
		content   string
		missing   bool
		isDir     bool
		forbidden []string
	}{
		{
			name: "private key file",
			content: "-----BEGIN OPENSSH PRIVATE KEY-----\n" +
				"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n" + // gitleaks:allow
				"-----END OPENSSH PRIVATE KEY-----\n",
			forbidden: []string{
				"-----BEGIN",
				"PRIVATE",
				"OPENSSH",
				"b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW",
				"-----END",
			},
		},
		{
			name:      "empty file",
			content:   "",
			forbidden: nil,
		},
		{
			name:      "whitespace only file",
			content:   "   \t\r\n  ",
			forbidden: nil,
		},
		{
			name:      "first token not a D3 key type (ssh-dss)",
			content:   "ssh-dss AAAAB3NzaC1kc3MAAACBAP1/U4EddRIpUt9KnC7s5Of2EbdSPO9EAMMeP4C2USZpRV1AIlH7WT2NWPq/xfW6MPbLm1Vs14E7gB00b/JmYLdrmVClpMS+f6AR7ECLCT7up1/63xhv4O1fnxqimFQ8E+4P208UewwI1VBNaFpEy9nXzrith1yrv8iIDGZ3RSAH dsa-secret-comment\n",
			forbidden: []string{"ssh-dss", "AAAAB3NzaC1kc3MAAACBAP1", "dsa-secret-comment"},
		},
		{
			name:      "first token arbitrary secret text",
			content:   "my-secret-token-alpha-99887766 second-token-beta-55443322\n", // gitleaks:allow
			forbidden: []string{"my-secret-token-alpha-99887766", "second-token-beta-55443322"},
		},
		{
			name:      "malformed base64 with valid key type prefix",
			content:   "ssh-ed25519 !!!not_valid_base64_secret_payload!!! secret-comment-tag\n",
			forbidden: []string{"!!!not_valid_base64_secret_payload!!!", "secret-comment-tag"},
		},
		{
			name:      "embedded type mismatch in wire blob",
			content:   "ssh-ed25519 " + base64.StdEncoding.EncodeToString(badEmbeddedBlob) + " embedded-secret-comment\n",
			forbidden: []string{base64.StdEncoding.EncodeToString(badEmbeddedBlob), "ssh-rsa", "embedded-secret-comment", "payload-bytes"},
		},
		{
			name:      "multiline file starting with valid key",
			content:   fx.unpublished.line + "\nPRIVATE_SECOND_LINE_BYTES\n",
			forbidden: []string{fx.unpublished.blobB64, "PRIVATE_SECOND_LINE_BYTES", "dave@unpublished"},
		},
		{
			name:    "missing file",
			missing: true,
		},
		{
			name:  "directory path instead of file",
			isDir: true,
		},
	}

	aliceBefore := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml"))

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var keyPath string
			switch {
			case tc.missing:
				keyPath = filepath.Join(t.TempDir(), "nonexistent-key.pub")
			case tc.isDir:
				keyPath = t.TempDir()
			default:
				keyPath = writeKeyFile(t, tc.content)
			}

			out, errOut, code := h.run("--publish", "--owner", "alice", "--key", keyPath)
			if code != 2 || out != "" {
				t.Fatalf("%s: exit=%d stdout=%q stderr=%q, want exit 2 and empty stdout", tc.name, code, out, errOut)
			}
			if !strings.Contains(errOut, `"diagnostic":"MM_FLEET_KEY_INVALID"`) {
				t.Fatalf("%s: stderr=%q, want MM_FLEET_KEY_INVALID", tc.name, errOut)
			}
			for _, tok := range tc.forbidden {
				if tok != "" && strings.Contains(errOut, tok) {
					t.Errorf("%s: diagnostic leaked file bytes %q in stderr: %s", tc.name, tok, errOut)
				}
			}
		})
	}

	// Ensure alice.yaml and enrollment.json were never touched by failed publishes.
	if got := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml")); string(got) != string(aliceBefore) {
		t.Errorf("alice.yaml was modified by failed --publish")
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "fleet", "enrollment.json")); !os.IsNotExist(err) {
		t.Errorf("enrollment.json should not exist after failed --publish: %v", err)
	}
}

// Acceptance 3b: `--unpublish` of an unknown fingerprint is
// `MM_FLEET_KEY_UNKNOWN`; `--unpublish` of a published fingerprint removes
// exactly that key (path) or stages the removal (gist).
func TestUnpublishUnknownAndPublishedKeysPathAndGist(t *testing.T) {
	t.Run("path directory", func(t *testing.T) {
		fx := newFixtureSet(t)
		h := newHarness(t, fx)

		// 1. Unknown fingerprint -> MM_FLEET_KEY_UNKNOWN (exit 2).
		for _, args := range [][]string{
			{"--unpublish", fx.unpublished.fingerprint},
			{"--unpublish", "SHA256:0000000000000000000000000000000000000000000"},
			{"--unpublish", fx.aliceLow.fingerprint, "--owner", "bob"},
		} {
			out, errOut, code := h.run(args...)
			if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_KEY_UNKNOWN"`) {
				t.Fatalf("%v: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_KEY_UNKNOWN", args, code, out, errOut)
			}
		}

		// 2. Unpublish fx.aliceLow from alice.yaml: removes only aliceLow,
		// preserves aliceHigh and aliceUnapproved.
		bobBefore := mustReadFile(t, filepath.Join(h.keyDir, "keys", "bob.yaml"))
		out, errOut, code := h.run("--unpublish", fx.aliceLow.fingerprint)
		if code != 0 || errOut != "" {
			t.Fatalf("unpublish aliceLow: exit=%d stderr=%q", code, errOut)
		}
		recs, err := output.ReadCompact([]byte(out))
		if err != nil || len(recs) != 1 {
			t.Fatalf("unpublish records: %v (%q)", err, out)
		}
		assertFieldOrder(t, "unpublish path record", recs[0].Fields(), []string{"owner", "fingerprint", "directory"})
		if ow, _ := recs[0].Get("owner"); ow != "alice" {
			t.Errorf("owner = %v, want alice", ow)
		}
		if fp, _ := recs[0].Get("fingerprint"); fp != fx.aliceLow.fingerprint {
			t.Errorf("fingerprint = %v, want %s", fp, fx.aliceLow.fingerprint)
		}

		aliceBytes := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml"))
		aliceEntry, err := directory.ParseEntry("keys/alice.yaml", "alice.yaml", aliceBytes)
		if err != nil {
			t.Fatalf("ParseEntry alice.yaml after unpublish: %v", err)
		}
		if len(aliceEntry.Keys) != 2 ||
			aliceEntry.Keys[0].Fingerprint != fx.aliceHigh.fingerprint ||
			aliceEntry.Keys[1].Fingerprint != fx.aliceUnapproved.fingerprint {
			t.Fatalf("alice keys after unpublish = %+v", aliceEntry.Keys)
		}
		if got := mustReadFile(t, filepath.Join(h.keyDir, "keys", "bob.yaml")); string(got) != string(bobBefore) {
			t.Errorf("bob.yaml was modified during alice unpublish")
		}

		wantMID, err := directory.MachineIDFromFingerprint(fx.aliceLow.fingerprint)
		if err != nil {
			t.Fatal(err)
		}
		enroll, _ := readEnrollmentRecord(t, h.stateDir)
		if enroll.Owner != "alice" || enroll.Fingerprint != fx.aliceLow.fingerprint ||
			enroll.MachineID != wantMID || enroll.Outcome != fleet.OutcomeWritten {
			t.Errorf("enrollment after path unpublish = %+v", enroll)
		}

		// Re-running --unpublish for the now-removed key returns MM_FLEET_KEY_UNKNOWN.
		if _, errOut2, code2 := h.run("--unpublish", fx.aliceLow.fingerprint); code2 != 2 || !strings.Contains(errOut2, `"diagnostic":"MM_FLEET_KEY_UNKNOWN"`) {
			t.Errorf("second unpublish of removed key: exit=%d stderr=%q, want 2 MM_FLEET_KEY_UNKNOWN", code2, errOut2)
		}

		// Unpublishing carol's sole key leaves keys: [] and still validates.
		if _, errOut3, code3 := h.run("--unpublish", fx.carolUnapproved.fingerprint); code3 != 0 || errOut3 != "" {
			t.Fatalf("unpublish carol sole key: exit=%d stderr=%q", code3, errOut3)
		}
		if _, err := directory.LoadPath(h.keyDir); err != nil {
			t.Fatalf("LoadPath after emptying carol.yaml: %v", err)
		}
	})

	t.Run("gist directory", func(t *testing.T) {
		fx := newFixtureSet(t)
		h := newHarness(t, fx)

		const gistID = "0123456789abcdef0123456789abcdef"
		const rev1 = "0123456789abcdef0123456789abcdef01234567"
		gs := newGistServer(t, gistID, rev1, map[string]string{
			"alice.yaml": fx.aliceYAML,
			"bob.yaml":   fx.bobYAML,
			"carol.yaml": fx.carolYAML,
		})
		h.client = gs.client
		h.env[fleet.GitHubAPIEnv] = gs.srv.URL
		writePolicyCheckout(t, h.policyDir, "gist: "+gistID+"\n", fx)

		if _, errOut, code := h.run("--refresh"); code != 0 || errOut != "" {
			t.Fatalf("seed refresh: exit=%d stderr=%q", code, errOut)
		}
		atomic.StoreInt64(&gs.hits, 0)

		// Unknown fingerprint on gist -> MM_FLEET_KEY_UNKNOWN, 0 network hits.
		out, errOut, code := h.run("--unpublish", fx.unpublished.fingerprint)
		if code != 2 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_KEY_UNKNOWN"`) {
			t.Fatalf("gist unknown unpublish: exit=%d stdout=%q stderr=%q", code, out, errOut)
		}
		if got := atomic.LoadInt64(&gs.hits); got != 0 {
			t.Fatalf("gist unknown unpublish made %d HTTP requests, want 0", got)
		}

		// Published fingerprint on gist -> stages removal, 0 network hits.
		out, errOut, code = h.run("--unpublish", fx.aliceLow.fingerprint)
		if code != 0 || errOut != "" {
			t.Fatalf("gist unpublish: exit=%d stderr=%q", code, errOut)
		}
		if got := atomic.LoadInt64(&gs.hits); got != 0 {
			t.Fatalf("gist unpublish made %d HTTP requests, want 0", got)
		}

		wantStaged := filepath.Join(h.stateDir, "fleet", "staged", "alice.yaml")
		wantNextStep := fmt.Sprintf("gh gist edit %s -f alice.yaml %s", gistID, wantStaged)
		recs, err := output.ReadCompact([]byte(out))
		if err != nil || len(recs) != 1 {
			t.Fatalf("ReadCompact gist unpublish: %v (%q)", err, out)
		}
		assertFieldOrder(t, "gist unpublish record", recs[0].Fields(), []string{
			"owner", "fingerprint", "directory", "staged", "next_step",
		})
		if got, _ := recs[0].Get("next_step"); got != wantNextStep {
			t.Errorf("next_step = %q, want %q", got, wantNextStep)
		}

		stagedEntry, err := directory.ParseEntry("alice.yaml", "alice.yaml", mustReadFile(t, wantStaged))
		if err != nil {
			t.Fatalf("ParseEntry staged alice.yaml: %v", err)
		}
		if len(stagedEntry.Keys) != 2 ||
			stagedEntry.Keys[0].Fingerprint != fx.aliceHigh.fingerprint ||
			stagedEntry.Keys[1].Fingerprint != fx.aliceUnapproved.fingerprint {
			t.Fatalf("staged keys after unpublish = %+v", stagedEntry.Keys)
		}

		enroll, _ := readEnrollmentRecord(t, h.stateDir)
		if enroll.Outcome != fleet.OutcomeStaged || enroll.Owner != "alice" || enroll.Fingerprint != fx.aliceLow.fingerprint {
			t.Errorf("enrollment after gist unpublish = %+v", enroll)
		}
	})
}

// Acceptance 4: Both actions write `<state>/fleet/enrollment.json` with the D5
// fields and `machine_id` per D3; a read-only state directory yields
// `MM_FLEET_PUBLISH_FAILED` (exit 1) on Unix.
func TestReadOnlyStateDirectoryYieldsPublishFailed(t *testing.T) {
	fx := newFixtureSet(t)
	pubPath := writeKeyFile(t, fx.unpublished.line+"\n")

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		t.Run("unix read-only state directory", func(t *testing.T) {
			h := newHarness(t, fx)
			if err := os.MkdirAll(h.stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(h.stateDir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(h.stateDir, 0o755) })

			aliceBefore := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml"))

			out, errOut, code := h.run("--publish", "--owner", "alice", "--key", pubPath)
			if code != 1 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_PUBLISH_FAILED"`) {
				t.Fatalf("read-only stateDir --publish: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_PUBLISH_FAILED", code, out, errOut)
			}
			if got := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml")); string(got) != string(aliceBefore) {
				t.Errorf("alice.yaml was modified despite read-only stateDir failure")
			}

			out, errOut, code = h.run("--unpublish", fx.aliceLow.fingerprint)
			if code != 1 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_PUBLISH_FAILED"`) {
				t.Fatalf("read-only stateDir --unpublish: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_PUBLISH_FAILED", code, out, errOut)
			}
			if got := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml")); string(got) != string(aliceBefore) {
				t.Errorf("alice.yaml was modified despite read-only stateDir unpublish failure")
			}
		})
	}

	t.Run("blocked state fleet path on all platforms", func(t *testing.T) {
		h := newHarness(t, fx)
		if err := os.MkdirAll(h.stateDir, 0o755); err != nil {
			t.Fatal(err)
		}
		// Place a regular file at <state>/fleet so creating <state>/fleet/enrollment.json fails on Unix and Windows.
		if err := os.WriteFile(filepath.Join(h.stateDir, "fleet"), []byte("blocker"), 0o644); err != nil {
			t.Fatal(err)
		}
		aliceBefore := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml"))

		out, errOut, code := h.run("--publish", "--owner", "alice", "--key", pubPath)
		if code != 1 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_PUBLISH_FAILED"`) {
			t.Fatalf("blocked state/fleet --publish: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_PUBLISH_FAILED", code, out, errOut)
		}
		if got := mustReadFile(t, filepath.Join(h.keyDir, "keys", "alice.yaml")); string(got) != string(aliceBefore) {
			t.Errorf("alice.yaml was modified despite blocked state/fleet")
		}
	})
}

// Acceptance 5: The runner seam records zero process starts in every publish
// test; `gh`, `git` and `ssh-keygen` are never on the fixture `PATH` and the
// tests pass regardless.
func TestPublishAndUnpublishWithEmptyPATHAndZeroProcessStarts(t *testing.T) {
	fx := newFixtureSet(t)
	h := newHarness(t, fx)

	// Point PATH to an empty directory so gh, git, ssh-keygen do not exist on PATH at all.
	emptyBin := t.TempDir()
	h.env["PATH"] = emptyBin

	pubPath := writeKeyFile(t, fx.unpublished.line+"\n")
	out, errOut, code := h.run("--publish", "--owner", "alice", "--key", pubPath, "--label", "no-path-tools")
	if code != 0 || errOut != "" || out == "" {
		t.Fatalf("publish with empty PATH: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	out, errOut, code = h.run("--unpublish", fx.unpublished.fingerprint)
	if code != 0 || errOut != "" || out == "" {
		t.Fatalf("unpublish with empty PATH: exit=%d stdout=%q stderr=%q", code, out, errOut)
	}
	if got := atomic.LoadInt64(&h.runnerHits); got != 0 {
		t.Fatalf("runnerHits = %d, want 0", got)
	}

	// Usage validation for --publish and --unpublish flags.
	for _, args := range [][]string{
		{"--publish"},
		{"--publish", "--owner", "alice"},
		{"--publish", "--key", pubPath},
		{"--publish", "--owner", "INVALID_OWNER", "--key", pubPath},
		{"--publish", "--owner", "alice", "--key", pubPath, "--label", strings.Repeat("x", 65)},
		{"--unpublish", fx.aliceLow.fingerprint, "--owner", "INVALID_OWNER"},
	} {
		o, e, c := h.run(args...)
		if c != 2 || o != "" || !strings.Contains(e, `"diagnostic":"MM_USAGE"`) {
			t.Errorf("%v: exit=%d stdout=%q stderr=%q, want 2 MM_USAGE", args, c, o, e)
		}
	}
}

func mustReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", p, err)
	}
	return b
}
