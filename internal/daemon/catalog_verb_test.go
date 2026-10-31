package daemon

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
)

func pairAgentDaemon(t *testing.T, dir string, pub ed25519.PublicKey) {
	t.Helper()
	peers := filepath.Join(dir, "peers")
	if err := os.Mkdir(peers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(peers, "agent-daemon.pub"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
}

func catalogRequest(key CatalogKey, floor CatalogFloor, env *CatalogEnvelope) Request {
	return Request{
		Verb:       "catalog",
		CatalogKey: &key,
		Floor:      &floor,
		Envelope:   env,
	}
}

func TestCatalogVerbAcceptsASignedEnvelope(t *testing.T) {
	dir := t.TempDir()
	clientPub, _ := keyPair(t)
	pairAgentDaemon(t, dir, clientPub)
	catPriv, catKey := testCatalogKey(t)
	raw := []byte(`{"version":1,"items":[{"name":"jq","options":["stable"],"targets":["local"],"provider":"brew","formula":"jq"}]}`)
	env := signCatalog(catPriv, raw, 2, 1700000000)
	floor := CatalogFloor{KeyID: catKey.KeyID, MinVersion: 2}
	session := &catalogSession{}
	req := catalogRequest(catKey, floor, &env)
	req.PublicKey = base64.StdEncoding.EncodeToString(clientPub)
	state, message, id := session.accept(dir, req)
	if id != "" || state != "accepted" || message != "2" {
		t.Fatalf("accept %s %s %s", state, message, id)
	}
	planner := Planner{current: session.current}
	if _, ok := planner.catalog().get("jq"); !ok {
		t.Fatal("accepted catalog is not visible to plan")
	}
	again, _, id := session.accept(dir, req)
	if id != diag.DaemonPlanFailed || again != "" {
		t.Fatalf("replay %s %s", again, id)
	}
}

func TestCatalogVerbNullEnvelopeLeavesTheCatalog(t *testing.T) {
	dir := t.TempDir()
	clientPub, _ := keyPair(t)
	pairAgentDaemon(t, dir, clientPub)
	catPriv, catKey := testCatalogKey(t)
	raw := []byte(`{"version":1,"items":[{"name":"jq","targets":["local"],"provider":"brew","formula":"jq"}]}`)
	env := signCatalog(catPriv, raw, 1, 1700000000)
	floor := CatalogFloor{KeyID: catKey.KeyID, MinVersion: 1}
	session := &catalogSession{}
	req := catalogRequest(catKey, floor, &env)
	req.PublicKey = base64.StdEncoding.EncodeToString(clientPub)
	if _, _, id := session.accept(dir, req); id != "" {
		t.Fatal(id)
	}
	empty := catalogRequest(catKey, floor, nil)
	empty.PublicKey = req.PublicKey
	state, _, id := session.accept(dir, empty)
	if id != "" || state != "unpublished" {
		t.Fatalf("null %s %s", state, id)
	}
	if _, ok := session.current().get("jq"); !ok {
		t.Fatal("null envelope cleared the accepted catalog")
	}
}

func TestCatalogVerbRefusesTheTerminalKey(t *testing.T) {
	dir := t.TempDir()
	daemonPub, _ := keyPair(t)
	terminalPub, _ := keyPair(t)
	pairAgentDaemon(t, dir, daemonPub)
	if err := os.WriteFile(filepath.Join(dir, "peers", "terminal.pub"), terminalPub, 0o600); err != nil {
		t.Fatal(err)
	}
	_, catKey := testCatalogKey(t)
	floor := CatalogFloor{KeyID: catKey.KeyID, MinVersion: 0}
	session := &catalogSession{}
	req := catalogRequest(catKey, floor, nil)
	req.PublicKey = base64.StdEncoding.EncodeToString(terminalPub)
	if _, _, id := session.accept(dir, req); id != diag.DaemonUnknownKey {
		t.Fatalf("terminal %s", id)
	}
}

func TestGateRefusesCatalogFieldsOnAnotherVerb(t *testing.T) {
	pub, priv := keyPair(t)
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: Pairing{Live: pub}, PeerUID: 501, Now: func() time.Time { return now }}
	_, key := testCatalogKey(t)
	req := Request{
		Verb: "status", Timestamp: now.Unix(), Nonce: "c1",
		CatalogKey: &key,
	}
	_, id, _ := g.verify(501, signed(t, priv, req))
	if id != diag.DaemonUndeclared {
		t.Fatalf("status with catalog_key: %s", id)
	}
	bare := Request{Verb: "catalog", Timestamp: now.Unix(), Nonce: "c2"}
	_, id, _ = g.verify(501, signed(t, priv, bare))
	if id != diag.DaemonUnsigned {
		t.Fatalf("catalog without key: %s", id)
	}
	named := Request{Verb: "catalog", Name: "jq", Timestamp: now.Unix(), Nonce: "c3", CatalogKey: &key}
	_, id, _ = g.verify(501, signed(t, priv, named))
	if id != diag.DaemonUndeclared {
		t.Fatalf("catalog with a name: %s", id)
	}
	withSoul := Request{Verb: "catalog", Soul: "iron", Timestamp: now.Unix(), Nonce: "c4", CatalogKey: &key}
	if _, id, _ := g.verify(501, signed(t, priv, withSoul)); id != diag.DaemonUndeclared {
		t.Fatalf("catalog with a soul: %s", id)
	}
	nullKey := signed(t, priv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "c5"})
	nullKey = []byte(`{"verb":"status","catalog_key":null,` + string(nullKey[len(`{"verb":"status",`):]))
	if _, id, _ := g.verify(501, nullKey); id != diag.DaemonUndeclared {
		t.Fatalf("status with null catalog_key: %s", id)
	}
}

func TestCatalogVerbRefusesTrailingCatalogBytes(t *testing.T) {
	dir := t.TempDir()
	clientPub, _ := keyPair(t)
	pairAgentDaemon(t, dir, clientPub)
	catPriv, catKey := testCatalogKey(t)
	raw := []byte(`{"version":1,"items":[]} {"unexpected":true}`)
	env := signCatalog(catPriv, raw, 1, 1700000000)
	floor := CatalogFloor{KeyID: catKey.KeyID, MinVersion: 1}
	session := &catalogSession{}
	req := catalogRequest(catKey, floor, &env)
	req.PublicKey = base64.StdEncoding.EncodeToString(clientPub)
	if _, _, id := session.accept(dir, req); id != diag.DaemonTampered {
		t.Fatalf("trailing bytes: %s", id)
	}
}

func TestCatalogSignatureCoversTheEnvelope(t *testing.T) {
	pub, priv := keyPair(t)
	now := time.Unix(1_700_000_000, 0)
	g := &Gate{Pairing: Pairing{Live: pub}, PeerUID: 501, Now: func() time.Time { return now }}
	catPriv, key := testCatalogKey(t)
	env := signCatalog(catPriv, []byte(`{"version":1,"items":[]}`), 1, 1700000000)
	floor := CatalogFloor{KeyID: key.KeyID, MinVersion: 1}
	req := catalogRequest(key, floor, &env)
	req.Timestamp = now.Unix()
	req.Nonce = "env"
	raw := signed(t, priv, req)
	var decoded Request
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded.Envelope.Catalog = base64.StdEncoding.EncodeToString([]byte(`{"version":1,"items":[{"name":"x"}]}`))
	body, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, id, _ := g.verify(501, body); id != diag.DaemonTampered {
		t.Fatalf("mutated envelope: %s", id)
	}
}
