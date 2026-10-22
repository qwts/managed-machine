package directory

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/core/internal/compose"
	"github.com/qwts/managed-machine/core/internal/config"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

func sshString(s string) []byte {
	return sshBytes([]byte(s))
}

func sshBytes(b []byte) []byte {
	out := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(out[:4], uint32(len(b)))
	copy(out[4:], b)
	return out
}

func sshMpint(x *big.Int) []byte {
	b := x.Bytes()
	if len(b) > 0 && b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return sshBytes(b)
}

type generatedFixtureKey struct {
	name            string
	keyType         string
	bits            int
	comment         string
	typeDisplay     string
	keyLine         string
	wantFingerprint string
	wantMachineID   string
}

func buildInProcessKeys(t *testing.T) []generatedFixtureKey {
	t.Helper()

	// 1. Ed25519 generated in-process via crypto/ed25519.
	edSeed := bytes.Repeat([]byte{0x11}, ed25519.SeedSize)
	edPriv := ed25519.NewKeyFromSeed(edSeed)
	edPub := edPriv.Public().(ed25519.PublicKey)
	var edBlob bytes.Buffer
	edBlob.Write(sshString(KeyTypeEd25519))
	edBlob.Write(sshBytes(edPub))

	// 2. RSA 2048-bit generated in-process via crypto/rsa from deterministic primes.
	p1 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 1024), big.NewInt(1093337))
	qBase := new(big.Int).Mul(big.NewInt(3), new(big.Int).Lsh(big.NewInt(1), 1022))
	q1 := new(big.Int).Add(qBase, big.NewInt(1))
	for !q1.ProbablyPrime(20) {
		q1.Add(q1, big.NewInt(2))
	}
	rsaPriv := &rsa.PrivateKey{
		PublicKey: rsa.PublicKey{
			N: new(big.Int).Mul(p1, q1),
			E: 65537,
		},
		D:      new(big.Int),
		Primes: []*big.Int{p1, q1},
	}
	phi := new(big.Int).Mul(new(big.Int).Sub(p1, big.NewInt(1)), new(big.Int).Sub(q1, big.NewInt(1)))
	rsaPriv.D.ModInverse(big.NewInt(65537), phi)
	rsaPriv.Precompute()
	if err := rsaPriv.Validate(); err != nil {
		t.Fatalf("deterministic RSA key validation: %v", err)
	}
	var rsaBlob bytes.Buffer
	rsaBlob.Write(sshString(KeyTypeRSA))
	rsaBlob.Write(sshMpint(big.NewInt(int64(rsaPriv.PublicKey.E))))
	rsaBlob.Write(sshMpint(rsaPriv.PublicKey.N))

	// 3. ECDSA P-256 generated in-process via crypto/ecdh + crypto/ecdsa.
	p256Key, err := ecdh.P256().NewPrivateKey(bytes.Repeat([]byte{0x22}, 32))
	if err != nil {
		t.Fatal(err)
	}
	p256Bytes := p256Key.PublicKey().Bytes()
	if _, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), p256Bytes); err != nil {
		t.Fatal(err)
	}
	var ecdsa256Blob bytes.Buffer
	ecdsa256Blob.Write(sshString(KeyTypeECDSAP256))
	ecdsa256Blob.Write(sshString("nistp256"))
	ecdsa256Blob.Write(sshBytes(p256Bytes))

	// 4. ECDSA P-384 generated in-process via crypto/ecdh + crypto/ecdsa.
	p384Key, err := ecdh.P384().NewPrivateKey(bytes.Repeat([]byte{0x33}, 48))
	if err != nil {
		t.Fatal(err)
	}
	p384Bytes := p384Key.PublicKey().Bytes()
	if _, err := ecdsa.ParseUncompressedPublicKey(elliptic.P384(), p384Bytes); err != nil {
		t.Fatal(err)
	}
	var ecdsa384Blob bytes.Buffer
	ecdsa384Blob.Write(sshString(KeyTypeECDSAP384))
	ecdsa384Blob.Write(sshString("nistp384"))
	ecdsa384Blob.Write(sshBytes(p384Bytes))

	// 5. ECDSA P-521 generated in-process via crypto/ecdh + crypto/ecdsa.
	p521Seed := make([]byte, 66)
	p521Seed[65] = 0x55
	p521Key, err := ecdh.P521().NewPrivateKey(p521Seed)
	if err != nil {
		t.Fatal(err)
	}
	p521Bytes := p521Key.PublicKey().Bytes()
	if _, err := ecdsa.ParseUncompressedPublicKey(elliptic.P521(), p521Bytes); err != nil {
		t.Fatal(err)
	}
	var ecdsa521Blob bytes.Buffer
	ecdsa521Blob.Write(sshString(KeyTypeECDSAP521))
	ecdsa521Blob.Write(sshString("nistp521"))
	ecdsa521Blob.Write(sshBytes(p521Bytes))

	// 6. sk-ssh-ed25519@openssh.com generated in-process.
	skEdSeed := bytes.Repeat([]byte{0x66}, ed25519.SeedSize)
	skEdPub := ed25519.NewKeyFromSeed(skEdSeed).Public().(ed25519.PublicKey)
	var skEdBlob bytes.Buffer
	skEdBlob.Write(sshString(KeyTypeSKEd25519))
	skEdBlob.Write(sshBytes(skEdPub))
	skEdBlob.Write(sshString("ssh:"))

	// 7. sk-ecdsa-sha2-nistp256@openssh.com generated in-process.
	skP256Key, err := ecdh.P256().NewPrivateKey(bytes.Repeat([]byte{0x77}, 32))
	if err != nil {
		t.Fatal(err)
	}
	var skECDSABlob bytes.Buffer
	skECDSABlob.Write(sshString(KeyTypeSKECDSAP256))
	skECDSABlob.Write(sshString("nistp256"))
	skECDSABlob.Write(sshBytes(skP256Key.PublicKey().Bytes()))
	skECDSABlob.Write(sshString("ssh:"))

	makeLine := func(kt string, blob []byte, comment string) string {
		return kt + " " + base64.StdEncoding.EncodeToString(blob) + " " + comment
	}

	return []generatedFixtureKey{
		{
			name:            "ed25519",
			keyType:         KeyTypeEd25519,
			bits:            256,
			comment:         "alice@ed25519",
			typeDisplay:     "ED25519",
			keyLine:         makeLine(KeyTypeEd25519, edBlob.Bytes(), "alice@ed25519"),
			wantFingerprint: "SHA256:2HS8bJpYBy7bDfWIQN304coFKT0s6CVPABFLYq+IX5E",
			wantMachineID:   "sha256-2HS8bJpYBy7bDfWIQN304coFKT0s6CVPABFLYq-IX5E",
		},
		{
			name:            "rsa",
			keyType:         KeyTypeRSA,
			bits:            2048,
			comment:         "alice@rsa",
			typeDisplay:     "RSA",
			keyLine:         makeLine(KeyTypeRSA, rsaBlob.Bytes(), "alice@rsa"),
			wantFingerprint: "SHA256:0D4RaisWNfEwQcJjfueQ0/levOna7S8r8d7DSKReBfI",
			wantMachineID:   "sha256-0D4RaisWNfEwQcJjfueQ0_levOna7S8r8d7DSKReBfI",
		},
		{
			name:            "ecdsa-nistp256",
			keyType:         KeyTypeECDSAP256,
			bits:            256,
			comment:         "alice@ecdsa256",
			typeDisplay:     "ECDSA",
			keyLine:         makeLine(KeyTypeECDSAP256, ecdsa256Blob.Bytes(), "alice@ecdsa256"),
			wantFingerprint: "SHA256:IgQ00+VOw4obH2k1NNB1sWpwMIBSBPb0mLhy2uP77tE",
			wantMachineID:   "sha256-IgQ00-VOw4obH2k1NNB1sWpwMIBSBPb0mLhy2uP77tE",
		},
		{
			name:            "ecdsa-nistp384",
			keyType:         KeyTypeECDSAP384,
			bits:            384,
			comment:         "alice@ecdsa384",
			typeDisplay:     "ECDSA",
			keyLine:         makeLine(KeyTypeECDSAP384, ecdsa384Blob.Bytes(), "alice@ecdsa384"),
			wantFingerprint: "SHA256:LNGyYCYlUSS+7Wr/SuguxPmL6RZ+cn5SWq4V1HQ+L0U",
			wantMachineID:   "sha256-LNGyYCYlUSS-7Wr_SuguxPmL6RZ-cn5SWq4V1HQ-L0U",
		},
		{
			name:            "ecdsa-nistp521",
			keyType:         KeyTypeECDSAP521,
			bits:            521,
			comment:         "alice@ecdsa521",
			typeDisplay:     "ECDSA",
			keyLine:         makeLine(KeyTypeECDSAP521, ecdsa521Blob.Bytes(), "alice@ecdsa521"),
			wantFingerprint: "SHA256:No4m+PEfvHohQSVyO40uHgMLnRlk5epOIaH99NPLfas",
			wantMachineID:   "sha256-No4m-PEfvHohQSVyO40uHgMLnRlk5epOIaH99NPLfas",
		},
		{
			name:            "sk-ed25519",
			keyType:         KeyTypeSKEd25519,
			bits:            256,
			comment:         "alice@sk-ed25519",
			typeDisplay:     "ED25519-SK",
			keyLine:         makeLine(KeyTypeSKEd25519, skEdBlob.Bytes(), "alice@sk-ed25519"),
			wantFingerprint: "SHA256:M2u3SNKsEEylnMUCa9KhdWyolEBGrAqysU5Wi0n4wYM",
			wantMachineID:   "sha256-M2u3SNKsEEylnMUCa9KhdWyolEBGrAqysU5Wi0n4wYM",
		},
		{
			name:            "sk-ecdsa-nistp256",
			keyType:         KeyTypeSKECDSAP256,
			bits:            256,
			comment:         "alice@sk-ecdsa256",
			typeDisplay:     "ECDSA-SK",
			keyLine:         makeLine(KeyTypeSKECDSAP256, skECDSABlob.Bytes(), "alice@sk-ecdsa256"),
			wantFingerprint: "SHA256:sP6LCFD1cbV6SlYvgd6PrmhKrxZx1I8BzIzwOjOjJoA",
			wantMachineID:   "sha256-sP6LCFD1cbV6SlYvgd6PrmhKrxZx1I8BzIzwOjOjJoA",
		},
	}
}

func TestInProcessFingerprintsAndMachineIDGoldens(t *testing.T) {
	keys := buildInProcessKeys(t)

	var fpGolden, midGolden bytes.Buffer
	for _, tc := range keys {
		pk, err := ParsePublicKey(tc.keyLine)
		if err != nil {
			t.Fatalf("%s: ParsePublicKey failed: %v", tc.name, err)
		}
		if pk.KeyType != tc.keyType {
			t.Errorf("%s: keyType = %q, want %q", tc.name, pk.KeyType, tc.keyType)
		}
		if pk.Comment != tc.comment {
			t.Errorf("%s: comment = %q, want %q", tc.name, pk.Comment, tc.comment)
		}
		midFromFP, err := MachineIDFromFingerprint(pk.Fingerprint)
		if err != nil {
			t.Fatalf("%s: MachineIDFromFingerprint(%q): %v", tc.name, pk.Fingerprint, err)
		}
		if pk.MachineID != midFromFP {
			t.Errorf("%s: MachineID %q != MachineIDFromFingerprint %q", tc.name, pk.MachineID, midFromFP)
		}
		if !*update {
			if pk.Fingerprint != tc.wantFingerprint {
				t.Errorf("%s: fingerprint = %q, want %q", tc.name, pk.Fingerprint, tc.wantFingerprint)
			}
			if pk.MachineID != tc.wantMachineID {
				t.Errorf("%s: machine_id = %q, want %q", tc.name, pk.MachineID, tc.wantMachineID)
			}
		}
		// Format matching `ssh-keygen -lf <file> -E sha256`:
		// "<bits> <SHA256:...> <comment> (<TYPE>)\n"
		fmt.Fprintf(&fpGolden, "%d %s %s (%s)\n", tc.bits, pk.Fingerprint, tc.comment, tc.typeDisplay)
		fmt.Fprintf(&midGolden, "%s\t%s\t%s\n", tc.name, pk.Fingerprint, pk.MachineID)
	}

	fpGoldenPath := filepath.Join("testdata", "fingerprints.golden")
	midGoldenPath := filepath.Join("testdata", "machine-ids.golden")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fpGoldenPath, fpGolden.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(midGoldenPath, midGolden.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	wantFPBytes, err := os.ReadFile(fpGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fpGolden.Bytes(), wantFPBytes) {
		t.Errorf("fingerprints golden mismatch:\ngot:\n%s\nwant:\n%s", fpGolden.Bytes(), wantFPBytes)
	}

	wantMIDBytes, err := os.ReadFile(midGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(midGolden.Bytes(), wantMIDBytes) {
		t.Errorf("machine-ids golden mismatch:\ngot:\n%s\nwant:\n%s", midGolden.Bytes(), wantMIDBytes)
	}
}

func TestEntryBaseNameMustEqualID(t *testing.T) {
	keys := buildInProcessKeys(t)
	k0 := keys[0]
	pk, err := ParsePublicKey(k0.keyLine)
	if err != nil {
		t.Fatal(err)
	}
	entryYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    label: workstation
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`, pk.Fingerprint, k0.keyLine)

	entry, err := ParseEntry("keys/alice.yaml", "alice.yaml", []byte(entryYAML))
	if err != nil {
		t.Fatalf("matching base name failed: %v", err)
	}
	if entry.ID != "alice" || len(entry.Keys) != 1 || !entry.Keys[0].Valid() {
		t.Errorf("unexpected entry: %+v", entry)
	}

	_, err = ParseEntry("keys/bob.yaml", "bob.yaml", []byte(entryYAML))
	if err == nil {
		t.Fatal("accepted file whose base name differs from id")
	}
	if got := Diagnostic(err); got != diag.FleetDirectoryInvalid {
		t.Errorf("Diagnostic = %q, want %q", got, diag.FleetDirectoryInvalid)
	}
	if !strings.Contains(err.Error(), "keys/bob.yaml: id:") {
		t.Errorf("error %q does not name document and field id", err.Error())
	}
}

func TestKeyValidationStatesMismatchMalformedAndDuplicate(t *testing.T) {
	keys := buildInProcessKeys(t)
	k0, err := ParsePublicKey(keys[0].keyLine)
	if err != nil {
		t.Fatal(err)
	}
	k1, err := ParsePublicKey(keys[1].keyLine)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := ParsePublicKey(keys[2].keyLine)
	if err != nil {
		t.Fatal(err)
	}

	// Build a key whose blob has embedded type "ssh-rsa" but prefix "ssh-ed25519".
	prefixMismatchLine := KeyTypeEd25519 + " " + k1.Base64Blob + " mismatch-prefix"

	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    label: valid-ed25519
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: wrong-fingerprint
    published_at: "2026-10-22T01:00:00Z"
    revision: 2
  - fingerprint: %s
    key: %s
    label: embedded-type-mismatch
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: dup-key-alice
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`, k0.Fingerprint, keys[0].keyLine,
		k0.Fingerprint, keys[1].keyLine, // recorded k0 fingerprint, actual k1 blob
		k1.Fingerprint, prefixMismatchLine, // embedded type ssh-rsa != prefix ssh-ed25519
		k2.Fingerprint, keys[2].keyLine)

	bobYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: bob
keys:
  - fingerprint: %s
    key: %s
    label: dup-key-bob
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`, k2.Fingerprint, keys[2].keyLine)

	fsys := fstest.MapFS{
		"keys/alice.yaml": {Data: []byte(aliceYAML)},
		"keys/bob.yaml":   {Data: []byte(bobYAML)},
	}
	dir, err := LoadFS(fsys)
	if err != nil {
		t.Fatalf("LoadFS: %v", err)
	}
	if len(dir.Entries) != 2 || dir.Entries[0].ID != "alice" || dir.Entries[1].ID != "bob" {
		t.Fatalf("entries = %+v", dir.Entries)
	}

	aliceKeys := dir.Entries[0].Keys
	bobKeys := dir.Entries[1].Keys

	// 1. Valid key
	if aliceKeys[0].State != StateValid || aliceKeys[0].Detail != "" {
		t.Errorf("aliceKeys[0] = %+v, want valid", aliceKeys[0])
	}
	// 2. Recorded fingerprint disagrees -> invalid, fingerprint-mismatch
	if aliceKeys[1].State != StateInvalid || aliceKeys[1].Detail != DetailFingerprintMismatch {
		t.Errorf("aliceKeys[1] = %+v, want invalid/fingerprint-mismatch", aliceKeys[1])
	}
	// 3. Embedded type differs from prefix -> invalid, malformed-key
	if aliceKeys[2].State != StateInvalid || aliceKeys[2].Detail != DetailMalformedKey {
		t.Errorf("aliceKeys[2] = %+v, want invalid/malformed-key", aliceKeys[2])
	}
	// 4. Two entries with one blob (across alice and bob) -> both invalid, duplicate
	if aliceKeys[3].State != StateInvalid || aliceKeys[3].Detail != DetailDuplicate {
		t.Errorf("aliceKeys[3] = %+v, want invalid/duplicate", aliceKeys[3])
	}
	if bobKeys[0].State != StateInvalid || bobKeys[0].Detail != DetailDuplicate {
		t.Errorf("bobKeys[0] = %+v, want invalid/duplicate", bobKeys[0])
	}

	// Also verify two entries with one blob inside a single owner's document.
	singleDupYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    published_at: "2026-10-22T02:00:00Z"
    revision: 2
`, k0.Fingerprint, keys[0].keyLine, k0.Fingerprint, keys[0].keyLine)
	singleEntry, err := ParseEntry("keys/alice.yaml", "alice.yaml", []byte(singleDupYAML))
	if err != nil {
		t.Fatalf("ParseEntry single-file duplicate: %v", err)
	}
	for idx, k := range singleEntry.Keys {
		if k.State != StateInvalid || k.Detail != DetailDuplicate {
			t.Errorf("singleEntry.Keys[%d] = %+v, want invalid/duplicate", idx, k)
		}
	}
}

func TestPolicyAndDefinitionDocumentsInKeyDirectoryAreRejected(t *testing.T) {
	cases := []struct {
		name string
		file string
		yaml string
	}{
		{
			name: "access-group inside key directory",
			file: "keys/workstations.yaml",
			yaml: `schema_version: 1
kind: access-group
id: workstations
maintained_by: [alice]
accounts: [alice]
approved_fingerprints: []
`,
		},
		{
			name: "key-directory-source inside key directory",
			file: "keys/source.yaml",
			yaml: `schema_version: 1
kind: key-directory-source
gist: 0123456789abcdef
`,
		},
		{
			name: "fleet-cutover inside key directory",
			file: "keys/cutover.yaml",
			yaml: `schema_version: 1
kind: fleet-cutover
cut_over_at: "2026-10-22T01:00:00Z"
reviewed_by: [alice]
`,
		},
		{
			name: "config definition inside key directory",
			file: "keys/shell.yaml",
			yaml: `schema_version: 1
kind: config
id: shell
description: Shell configuration.
targets: [user]
operations:
  - id: rc
    provider: file
    destination: rc
    source: files/rc
`,
		},
		{
			name: "software definition without kind inside key directory",
			file: "keys/ripgrep.yaml",
			yaml: `schema_version: 1
id: ripgrep
description: Search tool.
recipes: []
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fsys := fstest.MapFS{
				tc.file: {Data: []byte(tc.yaml)},
			}
			_, err := LoadFS(fsys)
			if err == nil {
				t.Fatalf("accepted %s inside key directory", tc.name)
			}
			if got := Diagnostic(err); got != diag.FleetDirectoryInvalid {
				t.Errorf("Diagnostic = %q, want %q", got, diag.FleetDirectoryInvalid)
			}
			if !strings.Contains(err.Error(), tc.file+": kind:") {
				t.Errorf("error %q does not name document %q and field kind", err.Error(), tc.file)
			}
		})
	}
}

func TestKeyDirectoryDocumentInDefinitionsSourceFailsValidationAsBefore(t *testing.T) {
	keyDirDoc := []byte(`schema_version: 1
kind: key-directory
id: alice
keys: []
`)
	srcID := source.Identity{Kind: "local", Location: "fixtures/local"}
	fsys := fstest.MapFS{
		"d/alice.yaml": {Data: keyDirDoc},
	}

	if _, err := source.Load(srcID, fsys, "d", registry.Builtin()); err == nil || !errors.Is(err, schema.Err(schema.Invalid)) || !strings.Contains(err.Error(), `unknown kind "key-directory"`) {
		t.Errorf("source.Load on key-directory doc: got %v, want unknown kind error", err)
	}
	if _, err := config.Load(srcID, fsys, "d"); err == nil || !errors.Is(err, schema.Err(schema.Invalid)) || !strings.Contains(err.Error(), `unknown kind "key-directory"`) {
		t.Errorf("config.Load on key-directory doc: got %v, want unknown kind error", err)
	}
	if _, err := compose.Load(srcID, fsys, "d"); err == nil || !errors.Is(err, schema.Err(schema.Invalid)) || !strings.Contains(err.Error(), `unknown kind "key-directory"`) {
		t.Errorf("compose.Load on key-directory doc: got %v, want unknown kind error", err)
	}
}

func TestKeyStructuralValidationAndLoadPath(t *testing.T) {
	keys := buildInProcessKeys(t)
	k0, err := ParsePublicKey(keys[0].keyLine)
	if err != nil {
		t.Fatal(err)
	}
	baseYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    label: laptop
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`, k0.Fingerprint, keys[0].keyLine)

	cases := []struct {
		name      string
		edit      func(string) string
		wantField string
	}{
		{
			name:      "revision zero",
			edit:      func(s string) string { return strings.Replace(s, "revision: 1", "revision: 0", 1) },
			wantField: "keys[0].revision",
		},
		{
			name:      "missing revision",
			edit:      func(s string) string { return strings.Replace(s, "    revision: 1\n", "", 1) },
			wantField: "keys[0].revision",
		},
		{
			name: "non-UTC published_at",
			edit: func(s string) string {
				return strings.Replace(s, `"2026-10-22T01:00:00Z"`, `"2026-10-22T01:00:00+02:00"`, 1)
			},
			wantField: "keys[0].published_at",
		},
		{
			name: "label over 64 chars",
			edit: func(s string) string {
				return strings.Replace(s, "label: laptop", "label: "+strings.Repeat("x", 65), 1)
			},
			wantField: "keys[0].label",
		},
		{
			name: "label with newline",
			edit: func(s string) string {
				return strings.Replace(s, "label: laptop", "label: \"line1\\nline2\"", 1)
			},
			wantField: "keys[0].label",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edited := tc.edit(baseYAML)
			_, err := ParseEntry("keys/alice.yaml", "alice.yaml", []byte(edited))
			if err == nil {
				t.Fatalf("accepted invalid entry:\n%s", edited)
			}
			if got := Diagnostic(err); got != diag.FleetDirectoryInvalid {
				t.Errorf("Diagnostic = %q, want %q", got, diag.FleetDirectoryInvalid)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not mention field %q", err.Error(), tc.wantField)
			}
		})
	}

	// LoadPath on absent directory returns MM_FLEET_DIRECTORY_UNAVAILABLE.
	tmp := t.TempDir()
	if _, err := LoadPath(filepath.Join(tmp, "absent")); !errors.Is(err, ErrUnavailable) || Diagnostic(err) != diag.FleetDirectoryUnavailable {
		t.Errorf("LoadPath absent: got %v (%s), want MM_FLEET_DIRECTORY_UNAVAILABLE", err, Diagnostic(err))
	}

	// LoadPath on valid temp directory computes file:<sha256> identity.
	if err := os.MkdirAll(filepath.Join(tmp, "keys"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "keys", "alice.yaml"), []byte(baseYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := LoadPath(tmp)
	if err != nil {
		t.Fatalf("LoadPath valid: %v", err)
	}
	if !strings.HasPrefix(d.Identity, "file:") || len(d.Entries) != 1 {
		t.Errorf("LoadPath result = %+v", d)
	}
}

func TestFormatEntryRoundTrip(t *testing.T) {
	keys := buildInProcessKeys(t)
	k0, err := ParsePublicKey(keys[0].keyLine)
	if err != nil {
		t.Fatal(err)
	}
	entry := Entry{
		SchemaVersion: SchemaVersion,
		Kind:          KindKeyDirectory,
		ID:            "alice",
		Keys: []Key{
			{
				Fingerprint: k0.Fingerprint,
				Key:         keys[0].keyLine,
				Label:       "workstation: primary #1",
				PublishedAt: "2026-10-22T02:00:00Z",
				Revision:    1,
			},
		},
	}
	formatted, err := FormatEntry(entry)
	if err != nil {
		t.Fatalf("FormatEntry: %v", err)
	}
	parsed, err := ParseEntry("keys/alice.yaml", "alice.yaml", formatted)
	if err != nil {
		t.Fatalf("ParseEntry round-trip: %v", err)
	}
	if len(parsed.Keys) != 1 || parsed.Keys[0].Label != "workstation: primary #1" || parsed.Keys[0].Fingerprint != k0.Fingerprint {
		t.Errorf("round-trip parsed = %+v", parsed)
	}

	emptyEntry := Entry{
		SchemaVersion: SchemaVersion,
		Kind:          KindKeyDirectory,
		ID:            "alice",
	}
	emptyBytes, err := FormatEntry(emptyEntry)
	if err != nil {
		t.Fatalf("FormatEntry empty: %v", err)
	}
	if !strings.Contains(string(emptyBytes), "keys: []\n") {
		t.Errorf("empty entry YAML = %q, want keys: []", string(emptyBytes))
	}
}
