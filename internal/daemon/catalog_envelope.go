package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
)

// Catalog envelope verification for the paired channel (true-line/agent-bot
// #71). The catalog public key, the per-key floor, and the envelope are
// trusted only when they arrive inside a message signed by the live
// agent-daemon pairing key. This file checks that envelope. It does not
// write peers/, catalog.pub, or a version file. A present catalog.json stays
// refused. The catalog verb carries catalog_key, floor, and envelope.

const catalogFormat = "agent-bot-catalog/1"

var (
	ErrCatalogNoKey     = errors.New("catalog signing key is not anchored")
	ErrCatalogFormat    = errors.New("catalog envelope invalid")
	ErrCatalogSignature = errors.New("catalog signature invalid")
	ErrCatalogKey       = errors.New("catalog signed by a different key")
	ErrCatalogRollback  = errors.New("catalog version is not newer than the applied version")
)

// CatalogKey is the public key delivered on the paired channel.
// PublicKey is "ed25519:" plus standard base64 of the raw 32 bytes.
// KeyID is the first 16 lowercase hex characters of sha256 of those bytes.
type CatalogKey struct {
	PublicKey string `json:"public_key"`
	KeyID     string `json:"key_id"`
}

// CatalogFloor is the rollback floor for one key id. MinVersion is the
// highest version published under KeyID. Zero means nothing published yet.
type CatalogFloor struct {
	KeyID      string `json:"key_id"`
	MinVersion int64  `json:"min_version"`
}

// CatalogEnvelope is one signed catalog. Catalog and Sig are standard base64.
type CatalogEnvelope struct {
	Format   string `json:"format"`
	KeyID    string `json:"key_id"`
	Version  int64  `json:"version"`
	IssuedAt int64  `json:"issued_at"`
	Catalog  string `json:"catalog"`
	Sig      string `json:"sig"`
}

// catalogKeyID is the first 16 lowercase hex characters of sha256(pub).
func catalogKeyID(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// catalogSignedBytes is the exact signed payload: format, key id, version,
// issued_at, and the sha256 hex of the raw catalog bytes, each followed by LF.
// Version and issued_at are base-10 with no padding.
func catalogSignedBytes(keyID string, version, issuedAt int64, catalog []byte) []byte {
	sum := sha256.Sum256(catalog)
	var b bytes.Buffer
	b.WriteString(catalogFormat + "\n" + keyID + "\n")
	b.WriteString(strconv.FormatInt(version, 10) + "\n")
	b.WriteString(strconv.FormatInt(issuedAt, 10) + "\n")
	b.WriteString(hex.EncodeToString(sum[:]) + "\n")
	return b.Bytes()
}

// parseCatalogPublicKey accepts only the ed25519 wire form. A raw pairing
// key is not a catalog key.
func parseCatalogPublicKey(v string) (ed25519.PublicKey, error) {
	b64, ok := strings.CutPrefix(strings.TrimSpace(v), "ed25519:")
	if !ok || b64 == "" {
		return nil, ErrCatalogNoKey
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, ErrCatalogFormat
	}
	return ed25519.PublicKey(raw), nil
}

// appliedFloor is the last-applied bound implied by the floor. The same key
// allows the published version again (min_version - 1). Any other key, or a
// floor that has not started, contributes no bound. This matches
// catalog.Floor.Applied in true-line/agent-bot.
func (f CatalogFloor) appliedFloor(keyID string) int64 {
	if f.KeyID == "" || f.KeyID != keyID || f.MinVersion <= 0 {
		return 0
	}
	return f.MinVersion - 1
}

func verifyCatalog(env *CatalogEnvelope, pub ed25519.PublicKey, applied int64) ([]byte, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, ErrCatalogNoKey
	}
	if env == nil || env.Format != catalogFormat || env.Version < 1 {
		return nil, ErrCatalogFormat
	}
	if env.KeyID != catalogKeyID(pub) {
		return nil, ErrCatalogKey
	}
	catalog, err := base64.StdEncoding.DecodeString(env.Catalog)
	if err != nil || len(catalog) == 0 {
		return nil, ErrCatalogFormat
	}
	sig, err := base64.StdEncoding.DecodeString(env.Sig)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrCatalogSignature
	}
	if !ed25519.Verify(pub, catalogSignedBytes(env.KeyID, env.Version, env.IssuedAt, catalog), sig) {
		return nil, ErrCatalogSignature
	}
	if env.Version <= applied {
		return nil, ErrCatalogRollback
	}
	return catalog, nil
}

func verifyCatalogFloor(env *CatalogEnvelope, pub ed25519.PublicKey, floor CatalogFloor, lastSameKey int64) ([]byte, error) {
	applied := floor.appliedFloor(catalogKeyID(pub))
	if lastSameKey > applied {
		applied = lastSameKey
	}
	if floor.KeyID != "" && floor.KeyID != catalogKeyID(pub) {
		return nil, ErrCatalogKey
	}
	return verifyCatalog(env, pub, applied)
}

// AcceptCatalog verifies one delivery. lastSameKey is the highest version
// this process already applied under the same key id, or 0 when it has not
// applied one. The stricter of that value and the floor wins. The returned
// bytes are the decoded catalog. Nothing is written to disk.
func AcceptCatalog(key CatalogKey, floor CatalogFloor, env *CatalogEnvelope, lastSameKey int64) ([]byte, error) {
	pub, err := parseCatalogPublicKey(key.PublicKey)
	if err != nil {
		return nil, err
	}
	if key.KeyID != catalogKeyID(pub) {
		return nil, ErrCatalogKey
	}
	return verifyCatalogFloor(env, pub, floor, lastSameKey)
}

// ReceiveCatalog accepts one paired delivery. A nil envelope is the server
// state before the first publish under this key: the key and floor are
// checked, and published is false. A present envelope is verified by
// AcceptCatalog. Nothing is written to disk.
func ReceiveCatalog(key CatalogKey, floor CatalogFloor, env *CatalogEnvelope, lastSameKey int64) (catalog []byte, published bool, err error) {
	pub, err := parseCatalogPublicKey(key.PublicKey)
	if err != nil {
		return nil, false, err
	}
	if key.KeyID != catalogKeyID(pub) {
		return nil, false, ErrCatalogKey
	}
	if floor.KeyID != "" && floor.KeyID != key.KeyID {
		return nil, false, ErrCatalogKey
	}
	if env == nil {
		return nil, false, nil
	}
	catalog, err = verifyCatalogFloor(env, pub, floor, lastSameKey)
	if err != nil {
		return nil, false, err
	}
	return catalog, true, nil
}
