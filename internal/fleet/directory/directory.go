// Package directory parses and validates fleet key-directory entries
// ("kind: key-directory", S5 story L01; decision D3, ADR-0010), computes
// OpenSSH SHA-256 public-key fingerprints from wire-format key blobs using
// only the Go standard library, and derives legacy-compatible machine_id
// identifiers.
package directory

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/schema"
)

// SchemaVersion is the only key-directory format version accepted.
const SchemaVersion = 1

// KindKeyDirectory is the top-level kind required on every key-directory
// entry document.
const KindKeyDirectory = "key-directory"

// Key validation states and details (D3).
const (
	StateValid   = "valid"
	StateInvalid = "invalid"

	DetailFingerprintMismatch = "fingerprint-mismatch"
	DetailMalformedKey        = "malformed-key"
	DetailDuplicate           = "duplicate"
)

// Accepted OpenSSH public key type names (D3).
const (
	KeyTypeEd25519     = "ssh-ed25519"
	KeyTypeRSA         = "ssh-rsa"
	KeyTypeECDSAP256   = "ecdsa-sha2-nistp256"
	KeyTypeECDSAP384   = "ecdsa-sha2-nistp384"
	KeyTypeECDSAP521   = "ecdsa-sha2-nistp521"
	KeyTypeSKEd25519   = "sk-ssh-ed25519@openssh.com"
	KeyTypeSKECDSAP256 = "sk-ecdsa-sha2-nistp256@openssh.com"
)

var allowedKeyTypes = map[string]bool{
	KeyTypeEd25519:     true,
	KeyTypeRSA:         true,
	KeyTypeECDSAP256:   true,
	KeyTypeECDSAP384:   true,
	KeyTypeECDSAP521:   true,
	KeyTypeSKEd25519:   true,
	KeyTypeSKECDSAP256: true,
}

// KnownKeyType reports whether t is one of the D3 supported OpenSSH key
// types.
func KnownKeyType(t string) bool { return allowedKeyTypes[t] }

// Sentinels for directory errors so callers can branch with errors.Is.
var (
	ErrUnavailable = errors.New("key directory unavailable")
	ErrFetchFailed = errors.New("key directory fetch failed")
)

// UnavailableError reports a missing path: directory or uncached gist
// directory (MM_FLEET_DIRECTORY_UNAVAILABLE).
type UnavailableError struct {
	Message string
}

func (e *UnavailableError) Error() string { return e.Message }
func (e *UnavailableError) Is(target error) bool {
	return target == ErrUnavailable
}

// FetchError reports a failed remote key-directory fetch
// (MM_FLEET_DIRECTORY_FETCH_FAILED).
type FetchError struct {
	Message string
}

func (e *FetchError) Error() string { return e.Message }
func (e *FetchError) Is(target error) bool {
	return target == ErrFetchFailed
}

// Diagnostic maps an error from this package to its accepted diagnostic
// identifier.
func Diagnostic(err error) string {
	switch {
	case errors.Is(err, ErrUnavailable):
		return diag.FleetDirectoryUnavailable
	case errors.Is(err, ErrFetchFailed):
		return diag.FleetDirectoryFetchFailed
	default:
		return diag.FleetDirectoryInvalid
	}
}

// ToDiagnostic builds a diag.Diagnostic for err using Diagnostic(err).
func ToDiagnostic(err error) diag.Diagnostic {
	return diag.New(Diagnostic(err), err.Error())
}

// PublicKey is a parsed OpenSSH public key line and its computed digests.
type PublicKey struct {
	KeyType     string
	Blob        []byte
	Base64Blob  string
	Comment     string
	Fingerprint string
	MachineID   string
}

// ParsePublicKey parses a single-line OpenSSH public key
// "<type> <base64 blob> [comment]", checks that <type> is supported and
// equals the wire blob's embedded type string, and computes its SHA-256
// fingerprint and machine_id using only the standard library (D3).
func ParsePublicKey(line string) (PublicKey, error) {
	if line == "" || strings.ContainsAny(line, "\r\n") {
		return PublicKey{}, errors.New("public key must be a single non-empty line")
	}
	if line != strings.TrimLeft(line, " \t") {
		return PublicKey{}, errors.New("public key must not have leading whitespace")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return PublicKey{}, errors.New("public key must have <type> and <base64 blob>")
	}
	keyType := fields[0]
	b64 := fields[1]
	if !KnownKeyType(keyType) {
		return PublicKey{}, fmt.Errorf("unsupported key type %q", keyType)
	}
	blob, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(blob) == 0 || base64.StdEncoding.EncodeToString(blob) != b64 {
		return PublicKey{}, errors.New("malformed base64 key blob")
	}
	if len(blob) <= 4 {
		return PublicKey{}, errors.New("key blob is too short for embedded type header")
	}
	typeLen := binary.BigEndian.Uint32(blob[:4])
	if typeLen == 0 || uint64(4)+uint64(typeLen) >= uint64(len(blob)) {
		return PublicKey{}, errors.New("key blob has truncated or empty payload")
	}
	embeddedType := string(blob[4 : 4+typeLen])
	if embeddedType != keyType {
		return PublicKey{}, fmt.Errorf("blob embedded type %q differs from prefix %q", embeddedType, keyType)
	}

	var comment string
	if rest := strings.TrimSpace(line[len(keyType):]); len(rest) > len(b64) {
		comment = strings.TrimSpace(rest[len(b64):])
	}
	fp := FingerprintFromBlob(blob)
	mid := MachineIDFromBlob(blob)
	return PublicKey{
		KeyType:     keyType,
		Blob:        blob,
		Base64Blob:  b64,
		Comment:     comment,
		Fingerprint: fp,
		MachineID:   mid,
	}, nil
}

// FingerprintFromBlob returns "SHA256:" + unpadded standard base64 of
// sha256(blob), matching OpenSSH `ssh-keygen -lf <file> -E sha256`.
func FingerprintFromBlob(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// ComputeFingerprint parses an OpenSSH public key line and returns its
// computed SHA256 fingerprint.
func ComputeFingerprint(keyLine string) (string, error) {
	pk, err := ParsePublicKey(keyLine)
	if err != nil {
		return "", err
	}
	return pk.Fingerprint, nil
}

// MachineIDFromBlob returns "sha256-" + unpadded base64url of sha256(blob).
func MachineIDFromBlob(blob []byte) string {
	sum := sha256.Sum256(blob)
	return "sha256-" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// MachineIDFromFingerprint derives "sha256-<base64url>" from an OpenSSH
// "SHA256:<base64>" fingerprint, matching legacy machine_id_from_fingerprint
// in lib/fleet.sh.
func MachineIDFromFingerprint(fp string) (string, error) {
	if !policy.ValidFingerprint(fp) {
		return "", fmt.Errorf("invalid fingerprint %q", fp)
	}
	digest := strings.TrimPrefix(fp, "SHA256:")
	digest = strings.ReplaceAll(digest, "+", "-")
	digest = strings.ReplaceAll(digest, "/", "_")
	digest = strings.ReplaceAll(digest, "=", "")
	return "sha256-" + digest, nil
}

// Entry is one parsed "kind: key-directory" document for an owner id.
type Entry struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	ID            string `json:"id"`
	Keys          []Key  `json:"keys"`
}

// Key is one published key entry inside an owner's key-directory document.
type Key struct {
	Fingerprint         string `json:"fingerprint"`
	RecordedFingerprint string `json:"recorded_fingerprint,omitempty"`
	Key                 string `json:"key"`
	KeyType             string `json:"key_type,omitempty"`
	Blob                string `json:"-"`
	Label               string `json:"label,omitempty"`
	PublishedAt         string `json:"published_at"`
	Revision            int    `json:"revision"`
	MachineID           string `json:"machine_id,omitempty"`
	State               string `json:"state"`
	Detail              string `json:"detail,omitempty"`
}

// Valid reports whether the key passed parsing, fingerprint verification and
// cross-directory uniqueness.
func (k Key) Valid() bool {
	return k.State == StateValid
}

// AuthorizedLine returns the canonical authorized_keys line for this key
// ("<type> <base64 blob> <owner>"), dropping any published comment.
func (k Key) AuthorizedLine(owner string) string {
	return k.KeyType + " " + k.Blob + " " + owner
}

type rawEntry struct {
	SchemaVersion *int     `yaml:"schema_version"`
	Kind          string   `yaml:"kind"`
	ID            string   `yaml:"id"`
	Keys          []rawKey `yaml:"keys"`
}

type rawKey struct {
	Fingerprint string `yaml:"fingerprint"`
	Key         string `yaml:"key"`
	Label       string `yaml:"label"`
	PublishedAt string `yaml:"published_at"`
	Revision    *int   `yaml:"revision"`
}

// Directory is a loaded collection of key-directory entries with its
// deterministic content identity.
type Directory struct {
	Identity string  `json:"identity"`
	Entries  []Entry `json:"entries"`
}

// OwnerCount returns the number of owner entries in the directory.
func (d Directory) OwnerCount() int {
	return len(d.Entries)
}

// KeyCount returns the total number of keys across all owner entries.
func (d Directory) KeyCount() int {
	n := 0
	for _, e := range d.Entries {
		n += len(e.Keys)
	}
	return n
}

// ParseEntry decodes and validates one "kind: key-directory" YAML document.
// When fileName is non-empty, its base name without ".yaml" must equal the
// document's id field (D3).
func ParseEntry(doc, fileName string, data []byte) (Entry, error) {
	if err := checkDirectoryKind(doc, data); err != nil {
		return Entry{}, err
	}
	var raw rawEntry
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Entry{}, err
	}
	if raw.SchemaVersion == nil {
		return Entry{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "missing")
	}
	if *raw.SchemaVersion != SchemaVersion {
		return Entry{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "unsupported version %d; supported: %d", *raw.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(raw.Kind) != KindKeyDirectory {
		return Entry{}, schema.Errorf(schema.Invalid, doc, "kind", "expected kind %q; got %q", KindKeyDirectory, raw.Kind)
	}
	if !policy.ValidOwnerID(raw.ID) {
		return Entry{}, schema.Errorf(schema.Invalid, doc, "id", "invalid key-directory owner id %q", raw.ID)
	}
	if fileName != "" {
		base := path.Base(filepath.ToSlash(fileName))
		if path.Ext(base) != ".yaml" {
			return Entry{}, schema.Errorf(schema.Invalid, doc, "id", "file %s must have a .yaml extension", base)
		}
		want := strings.TrimSuffix(base, ".yaml")
		if raw.ID != want {
			return Entry{}, schema.Errorf(schema.Invalid, doc, "id", "file %s defines %q; the base name must equal id", base, raw.ID)
		}
	}

	keys := make([]Key, 0, len(raw.Keys))
	for i, rk := range raw.Keys {
		k, err := parseKey(doc, i, rk)
		if err != nil {
			return Entry{}, err
		}
		keys = append(keys, k)
	}
	entries := []Entry{{
		SchemaVersion: SchemaVersion,
		Kind:          KindKeyDirectory,
		ID:            raw.ID,
		Keys:          keys,
	}}
	markDuplicates(entries)
	return entries[0], nil
}

func checkDirectoryKind(doc string, data []byte) error {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil
	}
	k := strings.TrimSpace(probe.Kind)
	if k == "" {
		return schema.Errorf(schema.Invalid, doc, "kind", "missing; expected kind: %s", KindKeyDirectory)
	}
	if k != KindKeyDirectory {
		return schema.Errorf(schema.Invalid, doc, "kind", "document of kind %q is not allowed in a key directory; expected kind: %s", k, KindKeyDirectory)
	}
	return nil
}

func parseKey(doc string, i int, rk rawKey) (Key, error) {
	if rk.Revision == nil {
		return Key{}, schema.Errorf(schema.Invalid, doc, indexedKeyField(i, "revision"), "missing")
	}
	if *rk.Revision < 1 {
		return Key{}, schema.Errorf(schema.Invalid, doc, indexedKeyField(i, "revision"), "revision must be >= 1; got %d", *rk.Revision)
	}
	if strings.TrimSpace(rk.PublishedAt) == "" {
		return Key{}, schema.Errorf(schema.Invalid, doc, indexedKeyField(i, "published_at"), "missing")
	}
	if !policy.ValidRFC3339UTC(rk.PublishedAt) {
		return Key{}, schema.Errorf(schema.Invalid, doc, indexedKeyField(i, "published_at"), "invalid timestamp %q; must be RFC 3339 UTC", rk.PublishedAt)
	}
	if rk.Label != "" {
		if err := ValidateLabel(rk.Label); err != nil {
			return Key{}, schema.Errorf(schema.Invalid, doc, indexedKeyField(i, "label"), "%v", err)
		}
	}

	out := Key{
		Fingerprint:         rk.Fingerprint,
		RecordedFingerprint: rk.Fingerprint,
		Key:                 rk.Key,
		Label:               rk.Label,
		PublishedAt:         rk.PublishedAt,
		Revision:            *rk.Revision,
	}
	pk, err := ParsePublicKey(rk.Key)
	if err != nil {
		out.State = StateInvalid
		out.Detail = DetailMalformedKey
		return out, nil
	}
	out.KeyType = pk.KeyType
	out.Blob = pk.Base64Blob
	out.Fingerprint = pk.Fingerprint
	out.MachineID = pk.MachineID
	if rk.Fingerprint != pk.Fingerprint {
		out.State = StateInvalid
		out.Detail = DetailFingerprintMismatch
		return out, nil
	}
	out.RecordedFingerprint = ""
	out.State = StateValid
	return out, nil
}

// ValidateLabel checks that s is a printable single-line key label of at
// most 64 characters (D3).
func ValidateLabel(s string) error {
	if strings.ContainsAny(s, "\r\n") {
		return errors.New("label must not contain newlines")
	}
	if utf8.RuneCountInString(s) > 64 || len(s) > 64 {
		return fmt.Errorf("label %q exceeds 64 characters", s)
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("label %q contains non-printable characters", s)
		}
	}
	return nil
}

// FormatEntry serializes e as a deterministic "kind: key-directory" YAML
// document and verifies that the output validates through ParseEntry (D3,
// D5).
func FormatEntry(e Entry) ([]byte, error) {
	var b bytes.Buffer
	sv := e.SchemaVersion
	if sv == 0 {
		sv = SchemaVersion
	}
	kind := e.Kind
	if kind == "" {
		kind = KindKeyDirectory
	}
	fmt.Fprintf(&b, "schema_version: %d\n", sv)
	fmt.Fprintf(&b, "kind: %s\n", kind)
	fmt.Fprintf(&b, "id: %s\n", e.ID)
	if len(e.Keys) == 0 {
		b.WriteString("keys: []\n")
	} else {
		b.WriteString("keys:\n")
		for _, k := range e.Keys {
			fp := k.Fingerprint
			if k.RecordedFingerprint != "" {
				fp = k.RecordedFingerprint
			}
			fmt.Fprintf(&b, "  - fingerprint: %s\n", formatYAMLScalar(fp))
			fmt.Fprintf(&b, "    key: %s\n", formatYAMLScalar(k.Key))
			if k.Label != "" {
				fmt.Fprintf(&b, "    label: %s\n", formatYAMLScalar(k.Label))
			}
			fmt.Fprintf(&b, "    published_at: %s\n", strconv.Quote(k.PublishedAt))
			fmt.Fprintf(&b, "    revision: %d\n", k.Revision)
		}
	}
	out := b.Bytes()
	if _, err := ParseEntry(e.ID+".yaml", e.ID+".yaml", out); err != nil {
		return nil, err
	}
	return out, nil
}

func formatYAMLScalar(s string) string {
	var probe struct {
		V any `yaml:"v"`
	}
	if s != "" && s == strings.TrimSpace(s) && !strings.ContainsAny(s, "\"'\\\r\n") {
		if err := yaml.Unmarshal([]byte("v: "+s+"\n"), &probe); err == nil {
			if str, ok := probe.V.(string); ok && str == s {
				return s
			}
		}
	}
	return strconv.Quote(s)
}

func indexedKeyField(i int, name string) string {
	return "keys[" + strconv.Itoa(i) + "]." + name
}

// markDuplicates marks every parseable key whose computed fingerprint
// appears more than once across the directory as invalid with detail
// "duplicate" (D3).
func markDuplicates(entries []Entry) {
	counts := map[string]int{}
	for _, e := range entries {
		for _, k := range e.Keys {
			if k.Blob != "" && k.Fingerprint != "" {
				counts[k.Fingerprint]++
			}
		}
	}
	for ei := range entries {
		for ki := range entries[ei].Keys {
			k := &entries[ei].Keys[ki]
			if k.Blob != "" && counts[k.Fingerprint] > 1 {
				k.State = StateInvalid
				k.Detail = DetailDuplicate
			}
		}
	}
}

// FromEntries builds a Directory from parsed entries, sorting owners by id,
// applying cross-owner duplicate detection, and computing the normalized
// "file:<sha256>" identity.
func FromEntries(entries []Entry) (Directory, error) {
	cp := make([]Entry, len(entries))
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if seen[e.ID] {
			return Directory{}, schema.Errorf(schema.Conflict, e.ID+".yaml", "id", "duplicate key-directory owner id %q", e.ID)
		}
		seen[e.ID] = true
		keys := make([]Key, len(e.Keys))
		copy(keys, e.Keys)
		cp[i] = Entry{
			SchemaVersion: e.SchemaVersion,
			Kind:          e.Kind,
			ID:            e.ID,
			Keys:          keys,
		}
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	markDuplicates(cp)
	norm, err := NormalizeEntries(cp)
	if err != nil {
		return Directory{}, err
	}
	sum := sha256.Sum256(norm)
	return Directory{
		Identity: "file:" + hex.EncodeToString(sum[:]),
		Entries:  cp,
	}, nil
}

// NormalizeEntries serializes entries in owner-id order as compact JSON
// Lines.
func NormalizeEntries(entries []Entry) ([]byte, error) {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, e := range sorted {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// LoadPath reads a "path:" key directory from the OS filesystem. Entries
// live under "<dir>/keys/*.yaml" (or directly under "<dir>/*.yaml" for a
// flat directory). A missing directory returns ErrUnavailable
// (MM_FLEET_DIRECTORY_UNAVAILABLE); any policy or definition document found
// inside fails with MM_FLEET_DIRECTORY_INVALID.
func LoadPath(dir string) (Directory, error) {
	if strings.TrimSpace(dir) == "" {
		return Directory{}, &UnavailableError{Message: "key directory path is empty"}
	}
	fi, err := os.Stat(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Directory{}, &UnavailableError{Message: fmt.Sprintf("key directory %q does not exist", dir)}
		}
		return Directory{}, &UnavailableError{Message: fmt.Sprintf("key directory %q: %v", dir, err)}
	}
	if !fi.IsDir() {
		return Directory{}, &UnavailableError{Message: fmt.Sprintf("key directory %q is not a directory", dir)}
	}
	return LoadFS(os.DirFS(dir))
}

// LoadFS reads a key directory from fsys. When a "keys" subdirectory exists
// entries are read from "keys/*.yaml" and any top-level files are rejected;
// otherwise flat "*.yaml" files at the root of fsys are read (as in a gist
// snapshot).
func LoadFS(fsys fs.FS) (Directory, error) {
	if fsys == nil {
		return Directory{}, &UnavailableError{Message: "key directory filesystem is nil"}
	}
	rootEnts, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return Directory{}, &UnavailableError{Message: fmt.Sprintf("read key directory: %v", err)}
	}
	hasKeysDir := false
	for _, e := range rootEnts {
		if e.IsDir() && e.Name() == "keys" {
			hasKeysDir = true
			break
		}
	}

	entryDir := "."
	if hasKeysDir {
		entryDir = "keys"
		for _, e := range rootEnts {
			if e.Name() == "keys" {
				continue
			}
			rel := e.Name()
			if !e.IsDir() && path.Ext(rel) == ".yaml" {
				if data, rerr := fs.ReadFile(fsys, rel); rerr == nil {
					if err := checkDirectoryKind(rel, data); err != nil {
						return Directory{}, err
					}
				}
			}
			return Directory{}, schema.Errorf(schema.Invalid, rel, "kind", "unexpected file %q outside keys/ in key directory", rel)
		}
	}

	ents, err := fs.ReadDir(fsys, entryDir)
	if err != nil {
		return Directory{}, &UnavailableError{Message: fmt.Sprintf("read key directory %s: %v", entryDir, err)}
	}
	var entries []Entry
	for _, e := range ents {
		name := e.Name()
		rel := name
		if entryDir != "." {
			rel = entryDir + "/" + name
		}
		if e.IsDir() || !e.Type().IsRegular() || path.Ext(name) != ".yaml" {
			return Directory{}, schema.Errorf(schema.Invalid, rel, "", "not a .yaml key-directory file")
		}
		data, err := fs.ReadFile(fsys, rel)
		if err != nil {
			return Directory{}, schema.Errorf(schema.Invalid, rel, "", "read: %v", err)
		}
		entry, err := ParseEntry(rel, name, data)
		if err != nil {
			return Directory{}, err
		}
		entries = append(entries, entry)
	}
	return FromEntries(entries)
}

// LoadGistFS reads and validates a flat gist key-directory snapshot from
// fsys and assigns the pinned "gist:<id>@<revision>" identity (D3).
func LoadGistFS(identity string, fsys fs.FS) (Directory, error) {
	d, err := LoadFS(fsys)
	if err != nil {
		return Directory{}, err
	}
	d.Identity = identity
	return d, nil
}
