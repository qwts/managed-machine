package daemon

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
)

func TestLoadPairingKeepsLiveUntilConfirm(t *testing.T) {
	dir := t.TempDir()
	livePub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	nextPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peer.pub"), livePub, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peer.next"), nextPub, 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err := loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pairing.allows(livePub) || pairing.allows(nextPub) {
		t.Fatal("staged key replaced the live key before confirmation")
	}
	sum := sha256.Sum256(nextPub)
	if err := os.WriteFile(filepath.Join(dir, "peer.confirm"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err = loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pairing.allows(nextPub) || pairing.allows(livePub) {
		t.Fatal("confirmed key did not become live")
	}
}

func TestLoadPairingRevocationRefusesUntilConfirm(t *testing.T) {
	dir := t.TempDir()
	livePub, livePriv := keyPair(t)
	nextPub, nextPriv := keyPair(t)
	if err := os.WriteFile(filepath.Join(dir, "peer.pub"), livePub, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peer.revoked"), []byte("revoked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err := loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pairing.allows(livePub) {
		t.Fatal("revoked key stayed live")
	}
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: pairing, PeerUID: 7, Now: func() time.Time { return now }}
	raw := signed(t, livePriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "revoked"})
	if _, id, _ := g.verify(7, raw); id != diag.DaemonUnknownKey {
		t.Fatalf("revoked request got %s", id)
	}
	if err := os.WriteFile(filepath.Join(dir, "peer.next"), nextPub, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nextPub)
	if err := os.WriteFile(filepath.Join(dir, "peer.confirm"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err = loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pairing.allows(nextPub) || pairing.allows(livePub) {
		t.Fatal("confirmed key did not replace the revoked pairing")
	}
	if _, err := os.Stat(filepath.Join(dir, "peer.revoked")); !os.IsNotExist(err) {
		t.Fatal("revocation marker survived re-registration")
	}
	g.Pairing = pairing
	raw = signed(t, nextPriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "again"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("re-registered key refused: %s", id)
	}
}
