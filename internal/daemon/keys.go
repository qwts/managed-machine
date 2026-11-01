package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

type keyStore interface {
	Load() ([]byte, bool, error)
	Save(seed []byte) error
	Delete() error
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	return loadOrCreateKeyWithStore(path, defaultKeyStore(path))
}

func loadOrCreateKeyWithStore(path string, store keyStore) (ed25519.PrivateKey, error) {
	if store != nil {
		seed, ok, err := store.Load()
		if err != nil {
			// Fail closed on locked keychain, denied access, or malformed store entry:
			// never generate a replacement key or fall back to a plaintext file.
			return nil, err
		}
		_ = os.Remove(path)
		if ok {
			priv := ed25519.NewKeyFromSeed(seed)
			if err := writeDaemonPub(path, priv); err != nil {
				return nil, err
			}
			return priv, nil
		}
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		if err := store.Save(priv.Seed()); err != nil {
			return nil, err
		}
		if err := writeDaemonPub(path, priv); err != nil {
			return nil, err
		}
		return priv, nil
	}
	seed, err := os.ReadFile(path)
	if err == nil && len(seed) == ed25519.SeedSize {
		if err := stripDarwinACL(path); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
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
	_ = os.Remove(path)
	if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
		return nil, err
	}
	if err := stripDarwinACL(path); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
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
	pubPath := filepath.Join(dir, "daemon.pub")
	_ = os.Remove(pubPath)
	if err := os.WriteFile(pubPath, pubKey, 0o600); err != nil {
		return err
	}
	if err := stripDarwinACL(pubPath); err != nil {
		return err
	}
	return os.Chmod(pubPath, 0o600)
}

func stripDarwinACL(path string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return exec.Command("/bin/chmod", "-N", path).Run()
}

func verifyPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symlinked daemon directory %s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("daemon path %s is not a directory", path)
	}
	return verifyDirOwner(path, info)
}

// purgeLegacySharedState removes an exposed <prefix>/var/agent-machine
// directory from v0.11.2/v0.11.3, unlinks a ~/.config/agent-machine symlink
// that pointed at it, and rotates daemon.key, client.key, and peers/ because
// the shared directory was readable by other local accounts.
func purgeLegacySharedState(dir, legacySharedDir string, store keyStore) error {
	rotated := false
	if info, err := os.Lstat(dir); err == nil && info.Mode()&os.ModeSymlink != 0 {
		target, lerr := os.Readlink(dir)
		if lerr != nil {
			return lerr
		}
		if legacySharedDir != "" && filepath.Clean(target) == filepath.Clean(legacySharedDir) {
			if err := os.Remove(dir); err != nil {
				return err
			}
			rotated = true
		} else {
			return fmt.Errorf("refusing symlinked daemon directory %s", dir)
		}
	}
	if legacySharedDir != "" {
		if _, err := os.Lstat(legacySharedDir); err == nil {
			if err := os.RemoveAll(legacySharedDir); err != nil {
				return err
			}
			rotated = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if rotated {
		_ = os.Remove(filepath.Join(dir, "daemon.key"))
		_ = os.Remove(filepath.Join(dir, "daemon.pub"))
		_ = os.Remove(filepath.Join(dir, "client.key"))
		_ = os.RemoveAll(filepath.Join(dir, "peers"))
		if store != nil {
			if err := store.Delete(); err != nil {
				return err
			}
		}
	}
	return nil
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
