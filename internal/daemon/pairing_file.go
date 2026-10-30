package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// loadPairing reads the live client key from peer.pub. peer.next is staged
// and does not replace the live key unless peer.confirm contains the SHA-256
// of peer.next. A wrong confirm file leaves the live key in place.
func loadPairing(dir string) (Pairing, error) {
	var pairing Pairing
	if pub, err := loadPublic(filepath.Join(dir, "peer.pub")); err == nil {
		pairing.Live = pub
	} else if !errors.Is(err, os.ErrNotExist) {
		return pairing, err
	}
	next, err := loadPublic(filepath.Join(dir, "peer.next"))
	if errors.Is(err, os.ErrNotExist) {
		return pairing, nil
	}
	if err != nil {
		return pairing, err
	}
	pairing.Stage(next)
	confirm, err := os.ReadFile(filepath.Join(dir, "peer.confirm"))
	if errors.Is(err, os.ErrNotExist) {
		return pairing, nil
	}
	if err != nil {
		return pairing, err
	}
	sum := sha256.Sum256(next)
	if strings.TrimSpace(string(confirm)) != hex.EncodeToString(sum[:]) {
		return pairing, nil
	}
	pairing.Confirm()
	if err := os.WriteFile(filepath.Join(dir, "peer.pub"), pairing.Live, 0o600); err != nil {
		return pairing, err
	}
	_ = os.Remove(filepath.Join(dir, "peer.next"))
	_ = os.Remove(filepath.Join(dir, "peer.confirm"))
	return pairing, nil
}
