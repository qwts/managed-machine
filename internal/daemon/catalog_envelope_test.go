package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testCatalogKey(t *testing.T) (ed25519.PrivateKey, CatalogKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv, CatalogKey{
		PublicKey: "ed25519:" + base64.StdEncoding.EncodeToString(pub),
		KeyID:     catalogKeyID(pub),
	}
}

func signCatalog(priv ed25519.PrivateKey, catalog []byte, version, issued int64) CatalogEnvelope {
	pub := priv.Public().(ed25519.PublicKey)
	id := catalogKeyID(pub)
	sig := ed25519.Sign(priv, catalogSignedBytes(id, version, issued, catalog))
	return CatalogEnvelope{
		Format:   catalogFormat,
		KeyID:    id,
		Version:  version,
		IssuedAt: issued,
		Catalog:  base64.StdEncoding.EncodeToString(catalog),
		Sig:      base64.StdEncoding.EncodeToString(sig),
	}
}

func TestAcceptCatalogVerifiesTheEnvelope(t *testing.T) {
	priv, key := testCatalogKey(t)
	raw := []byte(`{"items":[]}`)
	env := signCatalog(priv, raw, 5, 1700000000)
	floor := CatalogFloor{KeyID: key.KeyID, MinVersion: 5}
	got, err := AcceptCatalog(key, floor, &env, 0)
	if err != nil || string(got) != string(raw) {
		t.Fatalf("accept: %v %q", err, got)
	}
	want := "agent-bot-catalog/1\n" + key.KeyID + "\n5\n1700000000\n"
	if !strings.HasPrefix(string(catalogSignedBytes(key.KeyID, 5, 1700000000, raw)), want) {
		t.Fatalf("signed bytes %q", catalogSignedBytes(key.KeyID, 5, 1700000000, raw))
	}
	if !strings.HasSuffix(string(catalogSignedBytes(key.KeyID, 5, 1700000000, raw)), "\n") {
		t.Fatal("signed bytes must end with LF")
	}
}

func TestAcceptCatalogRefusesABadEnvelope(t *testing.T) {
	priv, key := testCatalogKey(t)
	raw := []byte(`{"items":[]}`)
	env := signCatalog(priv, raw, 3, 1700000000)
	floor := CatalogFloor{KeyID: key.KeyID, MinVersion: 1}
	mutations := []func(*CatalogEnvelope){
		func(e *CatalogEnvelope) {
			e.Catalog = base64.StdEncoding.EncodeToString([]byte(`{"items":[{"name":"x"}]}`))
		},
		func(e *CatalogEnvelope) { e.Version = 99 },
		func(e *CatalogEnvelope) { e.IssuedAt++ },
		func(e *CatalogEnvelope) {
			b, _ := base64.StdEncoding.DecodeString(e.Sig)
			b[0] ^= 1
			e.Sig = base64.StdEncoding.EncodeToString(b)
		},
		func(e *CatalogEnvelope) { e.Sig = "" },
		func(e *CatalogEnvelope) { e.Format = "agent-bot-catalog/2" },
		func(e *CatalogEnvelope) { e.KeyID = "0000000000000000" },
		func(e *CatalogEnvelope) { e.Version = 0 },
		func(e *CatalogEnvelope) { e.Catalog = "" },
	}
	for i, mutate := range mutations {
		next := env
		mutate(&next)
		if _, err := AcceptCatalog(key, floor, &next, 0); err == nil {
			t.Fatalf("mutation %d accepted", i)
		}
	}
	if _, err := AcceptCatalog(key, floor, nil, 0); !errors.Is(err, ErrCatalogFormat) {
		t.Fatalf("nil envelope: %v", err)
	}
}

func TestAcceptCatalogRefusesTheWrongKey(t *testing.T) {
	priv, key := testCatalogKey(t)
	env := signCatalog(priv, []byte(`{"items":[]}`), 1, 1700000000)
	_, other := testCatalogKey(t)
	floor := CatalogFloor{KeyID: key.KeyID, MinVersion: 1}
	if _, err := AcceptCatalog(other, floor, &env, 0); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("other key: %v", err)
	}
	raw := make([]byte, ed25519.PublicKeySize)
	if _, err := parseCatalogPublicKey(string(raw)); !errors.Is(err, ErrCatalogNoKey) {
		t.Fatalf("raw pairing key: %v", err)
	}
	if _, err := AcceptCatalog(CatalogKey{}, floor, &env, 0); !errors.Is(err, ErrCatalogNoKey) {
		t.Fatalf("missing key: %v", err)
	}
	broken := key
	broken.KeyID = "ffffffffffffffff"
	if _, err := AcceptCatalog(broken, floor, &env, 0); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("key id mismatch: %v", err)
	}
	broken = key
	broken.PublicKey = "ed25519:not-valid"
	if _, err := AcceptCatalog(broken, floor, &env, 0); !errors.Is(err, ErrCatalogFormat) {
		t.Fatalf("malformed key: %v", err)
	}
}

func TestAcceptCatalogHonorsTheFloorAndTheProcessMemory(t *testing.T) {
	priv, key := testCatalogKey(t)
	env := signCatalog(priv, []byte(`{"items":[]}`), 5, 1700000000)
	same := CatalogFloor{KeyID: key.KeyID, MinVersion: 5}
	if _, err := AcceptCatalog(key, same, &env, 0); err != nil {
		t.Fatalf("at floor after a restart: %v", err)
	}
	if _, err := AcceptCatalog(key, CatalogFloor{KeyID: key.KeyID, MinVersion: 6}, &env, 0); !errors.Is(err, ErrCatalogRollback) {
		t.Fatalf("below floor: %v", err)
	}
	if _, err := AcceptCatalog(key, same, &env, 5); !errors.Is(err, ErrCatalogRollback) {
		t.Fatalf("already applied in this process: %v", err)
	}
	if _, err := AcceptCatalog(key, CatalogFloor{KeyID: "ffffffffffffffff", MinVersion: 1}, &env, 0); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("floor for another key: %v", err)
	}
	if (CatalogFloor{KeyID: "ffffffffffffffff", MinVersion: 9}).appliedFloor(key.KeyID) != 0 {
		t.Fatal("another key's floor applied to this key")
	}
	if (CatalogFloor{KeyID: key.KeyID, MinVersion: 9}).appliedFloor(key.KeyID) != 8 {
		t.Fatal("same key floor bound")
	}
}

func TestReceiveCatalogTreatsANullEnvelopeAsUnpublished(t *testing.T) {
	priv, key := testCatalogKey(t)
	floor := CatalogFloor{KeyID: key.KeyID, MinVersion: 0}
	catalog, published, err := ReceiveCatalog(key, floor, nil, 0)
	if err != nil || published || catalog != nil {
		t.Fatalf("unpublished: published=%v err=%v catalog=%q", published, err, catalog)
	}
	if _, _, err := ReceiveCatalog(key, CatalogFloor{KeyID: "ffffffffffffffff", MinVersion: 0}, nil, 0); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("floor for another key: %v", err)
	}
	if _, _, err := ReceiveCatalog(CatalogKey{}, floor, nil, 0); !errors.Is(err, ErrCatalogNoKey) {
		t.Fatalf("missing key: %v", err)
	}
	raw := []byte(`{"items":[]}`)
	env := signCatalog(priv, raw, 1, 1700000000)
	got, published, err := ReceiveCatalog(key, CatalogFloor{KeyID: key.KeyID, MinVersion: 1}, &env, 0)
	if err != nil || !published || string(got) != string(raw) {
		t.Fatalf("published: published=%v err=%v catalog=%q", published, err, got)
	}
}
