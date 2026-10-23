package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/statedir"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

type fixtureKey struct {
	publicKey   string
	base64Blob  string
	fingerprint string
	machineID   string
}

func makeEd25519FixtureKey(t *testing.T, seedByte byte) fixtureKey {
	t.Helper()
	seed := bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	writePart := func(buf *bytes.Buffer, b []byte) {
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
		buf.Write(hdr[:])
		buf.Write(b)
	}
	var blob bytes.Buffer
	writePart(&blob, []byte(directory.KeyTypeEd25519))
	writePart(&blob, pub)

	b64 := base64.StdEncoding.EncodeToString(blob.Bytes())
	line := directory.KeyTypeEd25519 + " " + b64
	pk, err := directory.ParsePublicKey(line)
	if err != nil {
		t.Fatalf("ParsePublicKey(seed 0x%02x): %v", seedByte, err)
	}
	return fixtureKey{
		publicKey:   line,
		base64Blob:  b64,
		fingerprint: pk.Fingerprint,
		machineID:   pk.MachineID,
	}
}

func writeThreeRecordFixtureCheckout(t *testing.T, root string) (checkout string, k1, k2, k3 fixtureKey) {
	t.Helper()
	k1 = makeEd25519FixtureKey(t, 0x01)
	k2 = makeEd25519FixtureKey(t, 0x02)
	k3 = makeEd25519FixtureKey(t, 0x03)
	wrongKey := makeEd25519FixtureKey(t, 0x04)

	checkout = filepath.Join(root, "checkout")
	machinesDir := filepath.Join(checkout, "fleet", "machines")
	if err := os.MkdirAll(machinesDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeRecord := func(fileID, machineID, hostname, managedAt, fp, pubKey string) {
		content := fmt.Sprintf(`schema_version = 1
machine_id = %q
hostname = %q
managed_at = %q
managed_machine_ref = "v0.8.12"
local_bin_ref = "v1.2.4"
ssh_key_fingerprint = %q
public_key = %q
`, machineID, hostname, managedAt, fp, pubKey)
		if err := os.WriteFile(filepath.Join(machinesDir, fileID+".toml"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Two agreeing records and one disagreeing record whose recorded
	// ssh_key_fingerprint does not match its public_key bytes.
	writeRecord(k1.machineID, k1.machineID, "alpha-mac", "2026-10-15T01:00:00Z", k1.fingerprint, k1.publicKey)
	writeRecord(k2.machineID, k2.machineID, "bravo-mac", "2026-10-16T02:00:00Z", k2.fingerprint, k2.publicKey)
	writeRecord(k3.machineID, k3.machineID, "tampered-mac", "2026-10-17T03:00:00Z", wrongKey.fingerprint, k3.publicKey)

	return checkout, k1, k2, k3
}

func TestImportThreeRecordFixtureAndGoldens(t *testing.T) {
	tmp := t.TempDir()
	checkout, k1, k2, k3 := writeThreeRecordFixtureCheckout(t, tmp)
	stateRoot := filepath.Join(tmp, "state-root")
	getenv := func(k string) string {
		if k == statedir.Override {
			return stateRoot
		}
		return ""
	}

	// Place a .git file in checkout to confirm mm-fleet-import never reads or touches .git.
	gitMarker := filepath.Join(checkout, ".git")
	if err := os.WriteFile(gitMarker, []byte("unreadable-git-marker\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	outDir := filepath.Join(tmp, "out")
	var stdout, stderr bytes.Buffer
	code := run([]string{
		checkout,
		"-owner", "alice",
		"-group", "workstations",
		"-account", "alice_dev",
		"-out", outDir,
	}, "darwin", getenv, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run exited %d; stderr:\n%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("expected empty stderr, got:\n%s", stderr.String())
	}

	keysYAMLPath := filepath.Join(outDir, "keys", "alice.yaml")
	groupYAMLPath := filepath.Join(outDir, "fleet", "access-groups", "workstations.yaml")

	keysBytes, err := os.ReadFile(keysYAMLPath)
	if err != nil {
		t.Fatalf("read generated keys/alice.yaml: %v", err)
	}
	groupBytes, err := os.ReadFile(groupYAMLPath)
	if err != nil {
		t.Fatalf("read generated fleet/access-groups/workstations.yaml: %v", err)
	}

	// Validate generated YAML files through the L01 loaders.
	entry, err := directory.ParseEntry("keys/alice.yaml", "alice.yaml", keysBytes)
	if err != nil {
		t.Fatalf("directory.ParseEntry failed on generated keys/alice.yaml: %v", err)
	}
	if len(entry.Keys) != 2 {
		t.Fatalf("expected 2 agreed keys in keys/alice.yaml, got %d", len(entry.Keys))
	}
	for _, k := range entry.Keys {
		if !k.Valid() {
			t.Errorf("generated key %s is not valid: %s", k.Fingerprint, k.Detail)
		}
		if k.Fingerprint == k3.fingerprint {
			t.Errorf("disagreeing record %s must be excluded from keys/alice.yaml", k3.machineID)
		}
	}

	loadedDir, err := directory.LoadPath(filepath.Join(outDir, "keys"))
	if err != nil {
		t.Fatalf("directory.LoadPath failed on generated keys/: %v", err)
	}
	if len(loadedDir.Entries) != 1 || loadedDir.Entries[0].ID != "alice" {
		t.Fatalf("unexpected loaded directory entries: %+v", loadedDir.Entries)
	}

	group, err := policy.ParseAccessGroup("fleet/access-groups/workstations.yaml", groupBytes)
	if err != nil {
		t.Fatalf("policy.ParseAccessGroup failed on generated access group: %v", err)
	}
	if got := strings.Join(group.MaintainedBy, ","); got != "alice" {
		t.Errorf("maintained_by = %q, want alice", got)
	}
	if got := strings.Join(group.Accounts, ","); got != "alice_dev" {
		t.Errorf("accounts = %q, want alice_dev", got)
	}
	if len(group.ApprovedFingerprints) != 2 {
		t.Fatalf("expected 2 approved_fingerprints, got %v", group.ApprovedFingerprints)
	}
	for _, fp := range group.ApprovedFingerprints {
		if fp == k3.fingerprint {
			t.Errorf("disagreeing fingerprint %s must be excluded from approved_fingerprints", fp)
		}
	}

	// Also verify policy.Load succeeds on outDir once fleet/key-directory.yaml is present.
	kdYAML := fmt.Sprintf("schema_version: 1\nkind: key-directory-source\npath: %s\n", filepath.ToSlash(filepath.Join(outDir, "keys")))
	if err := os.WriteFile(filepath.Join(outDir, "fleet", "key-directory.yaml"), []byte(kdYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	loadedPolicy, err := policy.Load(outDir)
	if err != nil {
		t.Fatalf("policy.Load failed on generated output tree: %v", err)
	}
	if len(loadedPolicy.Groups) != 1 || loadedPolicy.Groups[0].ID != "workstations" {
		t.Fatalf("unexpected policy groups: %+v", loadedPolicy.Groups)
	}
	if err := os.Remove(filepath.Join(outDir, "fleet", "key-directory.yaml")); err != nil {
		t.Fatal(err)
	}

	// Verify --json and --human report outputs as well.
	var jsonOut, jsonErr bytes.Buffer
	outJSONDir := filepath.Join(tmp, "out-json")
	if code := run([]string{
		"--json",
		"-owner", "alice",
		"-group", "workstations",
		"-account", "alice_dev",
		"-out", outJSONDir,
		checkout,
	}, "darwin", getenv, &jsonOut, &jsonErr); code != 0 {
		t.Fatalf("--json run exited %d: %s", code, jsonErr.String())
	}

	var humanOut, humanErr bytes.Buffer
	outHumanDir := filepath.Join(tmp, "out-human")
	if code := run([]string{
		"--human",
		checkout,
		"-owner", "alice",
		"-group", "workstations",
		"-account", "alice_dev",
		"-out", outHumanDir,
	}, "darwin", getenv, &humanOut, &humanErr); code != 0 {
		t.Fatalf("--human run exited %d: %s", code, humanErr.String())
	}

	// Report records in all three modes must carry no hostname and no key bytes,
	// and must report the disagreeing record by machine_id.
	for modeName, reportText := range map[string]string{
		"compact": stdout.String(),
		"json":    jsonOut.String(),
		"human":   humanOut.String(),
	} {
		for _, host := range []string{"alpha-mac", "bravo-mac", "tampered-mac"} {
			if strings.Contains(reportText, host) {
				t.Errorf("%s report leaked hostname %q:\n%s", modeName, host, reportText)
			}
		}
		for _, blob := range []string{k1.base64Blob, k2.base64Blob, k3.base64Blob} {
			if strings.Contains(reportText, blob) {
				t.Errorf("%s report leaked key bytes %q:\n%s", modeName, blob, reportText)
			}
		}
		if !strings.Contains(reportText, k3.machineID) {
			t.Errorf("%s report did not include disagreeing machine_id %q:\n%s", modeName, k3.machineID, reportText)
		}
	}

	checkOrUpdateGolden(t, filepath.Join("testdata", "keys-owner.golden.yaml"), keysBytes)
	checkOrUpdateGolden(t, filepath.Join("testdata", "access-group.golden.yaml"), groupBytes)
	checkOrUpdateGolden(t, filepath.Join("testdata", "report.golden.jsonl"), stdout.Bytes())
	checkOrUpdateGolden(t, filepath.Join("testdata", "report.golden.json"), jsonOut.Bytes())
	checkOrUpdateGolden(t, filepath.Join("testdata", "report.golden.txt"), humanOut.Bytes())
}

func checkOrUpdateGolden(t *testing.T, path string, actual []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, actual, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	wantNorm := bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
	actualNorm := bytes.ReplaceAll(actual, []byte("\r\n"), []byte("\n"))
	if !bytes.Equal(actualNorm, wantNorm) {
		t.Errorf("golden mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", path, actualNorm, wantNorm)
	}
}

func TestRefusesOutputInsideCheckoutOrStateDir(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			tmp := t.TempDir()
			checkout, _, _, _ := writeThreeRecordFixtureCheckout(t, tmp)
			stateDir := filepath.Join(tmp, "state")
			if err := os.MkdirAll(stateDir, 0o755); err != nil {
				t.Fatal(err)
			}
			getenv := func(k string) string {
				if k == statedir.Override {
					return stateDir
				}
				return ""
			}

			badOuts := []string{
				checkout,
				filepath.Join(checkout, "sub"),
				filepath.Join(checkout, "fleet", "out"),
				stateDir,
				filepath.Join(stateDir, "sub"),
			}
			for _, badOut := range badOuts {
				var out, errb bytes.Buffer
				code := run([]string{
					checkout,
					"-owner", "alice",
					"-group", "workstations",
					"-account", "alice_dev",
					"-out", badOut,
				}, goos, getenv, &out, &errb)
				if code != 1 {
					t.Errorf("-out %q: expected exit 1, got %d (stderr: %s)", badOut, code, errb.String())
				}
				if out.Len() != 0 {
					t.Errorf("-out %q: expected empty stdout, got %q", badOut, out.String())
				}
				if _, err := os.Stat(filepath.Join(badOut, "keys")); err == nil {
					t.Errorf("-out %q: wrote keys/ inside forbidden directory", badOut)
				}
			}
		})
	}
}

func TestUsageAndValidationErrors(t *testing.T) {
	tmp := t.TempDir()
	checkout, _, _, _ := writeThreeRecordFixtureCheckout(t, tmp)
	stateDir := filepath.Join(tmp, "state")
	getenv := func(k string) string {
		if k == statedir.Override {
			return stateDir
		}
		return ""
	}

	var out, errb bytes.Buffer
	if code := run([]string{"-h"}, "darwin", getenv, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "usage:") {
		t.Fatalf("-h: exit %d, stdout %q", code, out.String())
	}

	usageCases := [][]string{
		{},
		{checkout},
		{checkout, "-owner", "alice", "-group", "workstations", "-account", "alice_dev"},
		{"--json", "--human", checkout, "-owner", "alice", "-group", "workstations", "-account", "alice_dev", "-out", filepath.Join(tmp, "o1")},
		{"--unknown-flag", checkout, "-owner", "alice", "-group", "workstations", "-account", "alice_dev", "-out", filepath.Join(tmp, "o2")},
	}
	for _, args := range usageCases {
		out.Reset()
		errb.Reset()
		if code := run(args, "darwin", getenv, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("args %v: expected exit 2 and empty stdout, got %d (%q)", args, code, out.String())
		}
	}

	validationCases := [][]string{
		{checkout, "-owner", "Invalid_Owner", "-group", "workstations", "-account", "alice_dev", "-out", filepath.Join(tmp, "v1")},
		{checkout, "-owner", "alice", "-group", "Invalid_Group", "-account", "alice_dev", "-out", filepath.Join(tmp, "v2")},
		{checkout, "-owner", "alice", "-group", "workstations", "-account", "-bad-account", "-out", filepath.Join(tmp, "v3")},
		{filepath.Join(tmp, "missing-checkout"), "-owner", "alice", "-group", "workstations", "-account", "alice_dev", "-out", filepath.Join(tmp, "v4")},
	}
	for _, args := range validationCases {
		out.Reset()
		errb.Reset()
		if code := run(args, "darwin", getenv, &out, &errb); code != 1 || out.Len() != 0 {
			t.Errorf("args %v: expected exit 1 and empty stdout, got %d (%q)", args, code, out.String())
		}
	}
}
