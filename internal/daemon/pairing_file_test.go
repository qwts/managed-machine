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

func peerPath(t *testing.T, dir, client, suffix string) string {
	t.Helper()
	peers := filepath.Join(dir, "peers")
	if err := os.MkdirAll(peers, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(peers, client+suffix)
}

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
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".pub"), livePub, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".next"), nextPub, 0o600); err != nil {
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
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".confirm"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
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
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".pub"), livePub, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".revoked"), []byte("revoked\n"), 0o600); err != nil {
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
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".next"), nextPub, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nextPub)
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".confirm"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err = loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pairing.allows(nextPub) || pairing.allows(livePub) {
		t.Fatal("confirmed key did not replace the revoked pairing")
	}
	if _, err := os.Stat(peerPath(t, dir, "terminal", ".revoked")); !os.IsNotExist(err) {
		t.Fatal("revocation marker survived re-registration")
	}
	g.Pairing = pairing
	raw = signed(t, nextPriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "again"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("re-registered key refused: %s", id)
	}
}

func TestGateRereadsRevocationBeforeAuthorization(t *testing.T) {
	dir := t.TempDir()
	livePub, livePriv := keyPair(t)
	nextPub, nextPriv := keyPair(t)
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".pub"), livePub, 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err := loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: pairing, Dir: dir, PeerUID: 7, Now: func() time.Time { return now }}
	raw := signed(t, livePriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "live"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("live key refused: %s", id)
	}
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".revoked"), []byte("revoked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	raw = signed(t, livePriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "revoked"})
	if _, id, _ := g.verify(7, raw); id != diag.DaemonUnknownKey {
		t.Fatalf("revoked request got %s", id)
	}
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".next"), nextPub, 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(nextPub)
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".confirm"), []byte(hex.EncodeToString(sum[:])), 0o600); err != nil {
		t.Fatal(err)
	}
	raw = signed(t, nextPriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "again"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("re-registered key refused: %s", id)
	}
	raw = signed(t, livePriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "old"})
	if _, id, _ := g.verify(7, raw); id != diag.DaemonUnknownKey {
		t.Fatalf("old key after re-registration got %s", id)
	}
}

func TestLoadPairingKeepsTheOtherClient(t *testing.T) {
	dir := t.TempDir()
	terminal, _ := keyPair(t)
	daemon, daemonPriv := keyPair(t)
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".pub"), terminal, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peerPath(t, dir, "agent-daemon", ".pub"), daemon, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".revoked"), []byte("revoked\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err := loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if pairing.allows(terminal) || !pairing.allows(daemon) {
		t.Fatal("revoking terminal dropped agent-daemon")
	}
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: pairing, Dir: dir, PeerUID: 7, Now: func() time.Time { return now }}
	raw := signed(t, daemonPriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "daemon"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("agent-daemon refused: %s", id)
	}
}

func TestLoadPairingMigratesLegacyTerminalKey(t *testing.T) {
	dir := t.TempDir()
	pub, _ := keyPair(t)
	if err := os.WriteFile(filepath.Join(dir, "peer.pub"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
	pairing, err := loadPairing(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !pairing.allows(pub) {
		t.Fatal("migrated terminal key was not live")
	}
	if _, err := os.Stat(filepath.Join(dir, "peer.pub")); !os.IsNotExist(err) {
		t.Fatal("legacy peer.pub was left in place")
	}
	if _, err := os.Stat(filepath.Join(dir, "peers", "terminal.pub")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadPairingRefusesMixedState(t *testing.T) {
	dir := t.TempDir()
	pub, _ := keyPair(t)
	if err := os.WriteFile(peerPath(t, dir, "terminal", ".pub"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peer.pub"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPairing(dir); err == nil || err.Error() != "pairing state is mixed" {
		t.Fatalf("mixed pairing got %v", err)
	}
}

func TestLoadPairingRefusesAnUnknownClientFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "peers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "peers", "other.pub"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadPairing(dir); err == nil || err.Error() != "pairing file is not a client key" {
		t.Fatalf("unknown client got %v", err)
	}
}
