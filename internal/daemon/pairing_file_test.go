package daemon

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
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
