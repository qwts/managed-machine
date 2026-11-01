package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
)

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	seed, err := os.ReadFile(path)
	if err == nil && len(seed) == ed25519.SeedSize {
		priv := ed25519.NewKeyFromSeed(seed)
		if err := writeDaemonPub(path, priv); err != nil {
			return nil, err
		}
		return priv, nil
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
		return nil, err
	}
	if err := writeDaemonPub(path, priv); err != nil {
		return nil, err
	}
	return priv, nil
}

func writeDaemonPub(keyPath string, priv ed25519.PrivateKey) error {
	pubKey, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return os.ErrInvalid
	}
	dir := filepath.Dir(keyPath)
	mode := os.FileMode(0o600)
	if isSharedPrefixDir(dir) {
		mode = 0o640
	}
	pubPath := filepath.Join(dir, "daemon.pub")
	if err := os.WriteFile(pubPath, pubKey, mode); err != nil {
		return err
	}
	if err := os.Chmod(pubPath, mode); err != nil {
		return err
	}
	if mode == 0o640 {
		ensureStaffGroup(pubPath)
	}
	return nil
}

func isSharedPrefixDir(dir string) bool {
	clean := filepath.Clean(dir)
	return filepath.Base(clean) == "agent-machine" && filepath.Base(filepath.Dir(clean)) == "var"
}

func ensureStaffGroup(path string) {
	// On macOS, /opt/homebrew/var has group admin (80); changing the shared
	// agent-machine files to group staff (20) lets the local owner `user`
	// account read daemon.pub, dial daemon.sock, and stage peers/agent-daemon.*.
	_ = os.Chown(path, -1, 20)
}

func loadPublic(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, os.ErrInvalid
	}
	return ed25519.PublicKey(b), nil
}
