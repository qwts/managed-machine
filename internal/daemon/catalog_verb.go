package daemon

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"

	"github.com/qwts/managed-machine/internal/diag"
)

// catalogSession is the catalog accepted from the paired agent-daemon.
// It lives in this process only. A null envelope does not clear it.
type catalogSession struct {
	cat  Catalog
	last map[string]int64
}

func (s *catalogSession) current() Catalog {
	return s.cat
}

// accept verifies one catalog delivery and, when an envelope is published,
// replaces the in-memory catalog. The signing key has to be the live
// agent-daemon pairing key. Nothing is written to disk.
func (s *catalogSession) accept(dir string, req Request) (string, string, string) {
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", ErrCatalogNoKey.Error(), diag.DaemonUnknownKey
	}
	live, err := loadClientKey(filepath.Join(dir, "peers"), "agent-daemon")
	if err != nil || live == nil || !live.Equal(ed25519.PublicKey(pub)) {
		return "", "catalog is accepted only from the paired agent-daemon", diag.DaemonUnknownKey
	}
	floor := CatalogFloor{}
	if req.Floor != nil {
		floor = *req.Floor
	}
	last := int64(0)
	if s.last != nil {
		last = s.last[req.CatalogKey.KeyID]
	}
	raw, published, err := ReceiveCatalog(*req.CatalogKey, floor, req.Envelope, last)
	if err != nil {
		id, msg := catalogDiag(err)
		return "", msg, id
	}
	if !published {
		return "unpublished", "", ""
	}
	catalog, err := parseCatalogDocument(raw)
	if err != nil {
		id, msg := catalogDiag(err)
		return "", msg, id
	}
	if s.last == nil {
		s.last = map[string]int64{}
	}
	s.last[req.CatalogKey.KeyID] = req.Envelope.Version
	s.cat = catalog
	return "accepted", strconv.FormatInt(req.Envelope.Version, 10), ""
}

func parseCatalogDocument(raw []byte) (Catalog, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var catalog Catalog
	if err := dec.Decode(&catalog); err != nil {
		return Catalog{}, ErrCatalogFormat
	}
	if catalog.Version < 1 {
		return Catalog{}, errors.New("catalog version must be at least 1")
	}
	return catalog, nil
}

func catalogDiag(err error) (string, string) {
	switch {
	case errors.Is(err, ErrCatalogNoKey):
		return diag.DaemonUnsigned, err.Error()
	case errors.Is(err, ErrCatalogSignature), errors.Is(err, ErrCatalogFormat):
		return diag.DaemonTampered, err.Error()
	case errors.Is(err, ErrCatalogKey):
		return diag.DaemonUnknownKey, err.Error()
	case errors.Is(err, ErrCatalogRollback):
		return diag.DaemonPlanFailed, err.Error()
	default:
		return diag.DaemonTampered, err.Error()
	}
}
