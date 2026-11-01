package daemon

import (
	"crypto/ed25519"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
)

func keyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func signed(t *testing.T, priv ed25519.PrivateKey, r Request) []byte {
	t.Helper()
	if err := Sign(priv, &r); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGateAcceptsLiveKey(t *testing.T) {
	pub, priv := keyPair(t)
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: Pairing{Live: pub}, PeerUID: 501, Now: func() time.Time { return now }}
	raw := signed(t, priv, Request{Verb: "status", Soul: "iron", Harness: "cursor", Timestamp: now.Unix(), Nonce: "n1"})
	req, id, _ := g.verify(501, raw)
	if id != "" || req.Verb != "status" {
		t.Fatalf("id %q req %#v", id, req)
	}
	raw2 := signed(t, priv, Request{Verb: "status", Soul: "iron", Harness: "cursor", Timestamp: now.Unix(), Nonce: "n2"})
	if _, id2, _ := g.verify(502, raw2); id2 != diag.DaemonPeerRefused {
		t.Fatalf("expected non-owner peer uid 502 to be refused with %s, got %q", diag.DaemonPeerRefused, id2)
	}
}

func TestGateRefusals(t *testing.T) {
	pub, priv := keyPair(t)
	_, other := keyPair(t)
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: Pairing{Live: pub}, PeerUID: 501, Now: func() time.Time { return now }}
	good := Request{Verb: "status", Timestamp: now.Unix(), Nonce: "once"}
	cases := []struct {
		name string
		uid  int
		raw  []byte
		want string
	}{
		{"unsigned", 501, []byte(`{"verb":"status","timestamp":1700000000,"nonce":"x"}`), diag.DaemonUnsigned},
		{"unknown key", 501, signed(t, other, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "k"}), diag.DaemonUnknownKey},
		{"same uid unregistered", 501, signed(t, other, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "u"}), diag.DaemonUnknownKey},
		{"stale", 501, signed(t, priv, Request{Verb: "status", Timestamp: now.Add(-time.Hour).Unix(), Nonce: "s"}), diag.DaemonStale},
		{"peer", 502, signed(t, priv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "p"}), diag.DaemonPeerRefused},
		{"verb", 501, signed(t, priv, Request{Verb: "shell", Timestamp: now.Unix(), Nonce: "v"}), diag.DaemonVerbUnknown},
		{"extra field", 501, []byte(`{"verb":"status","url":"https://evil","timestamp":1700000000,"nonce":"e","public_key":"aa","signature":"bb"}`), diag.DaemonTampered},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, id, _ := g.verify(tc.uid, tc.raw)
			if id != tc.want {
				t.Fatalf("got %s want %s", id, tc.want)
			}
		})
	}
	raw := signed(t, priv, good)
	if _, id, _ := g.verify(501, raw); id != "" {
		t.Fatal(id)
	}
	body := append([]byte(nil), raw...)
	flipped := strings.Replace(string(body), "status", "apply", 1)
	if _, id, _ := g.verify(501, []byte(flipped)); id != diag.DaemonTampered {
		t.Fatalf("tamper %s", id)
	}
	if _, id, _ := g.verify(501, raw); id != diag.DaemonReplay {
		t.Fatalf("replay %s", id)
	}
}

func TestStageKeepsLiveKey(t *testing.T) {
	live, livePriv := keyPair(t)
	staged, stagedPriv := keyPair(t)
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: Pairing{Live: live}, PeerUID: 7, Now: func() time.Time { return now }}
	g.Pairing.Stage(staged)
	raw := signed(t, livePriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "live"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("live key refused: %s", id)
	}
	raw = signed(t, stagedPriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "staged"})
	if _, id, _ := g.verify(7, raw); id != diag.DaemonUnknownKey {
		t.Fatalf("staged key accepted early: %s", id)
	}
	g.Pairing.Confirm()
	raw = signed(t, stagedPriv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "now"})
	if _, id, _ := g.verify(7, raw); id != "" {
		t.Fatalf("confirmed key refused: %s", id)
	}
}
