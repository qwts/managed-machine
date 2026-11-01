package daemon

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

const keychainService = "io.qwts.agent-machine"
const keychainAccount = "daemon.key"

type keyStore interface {
	Load() ([]byte, bool, error)
	Save(seed []byte) error
}

type darwinKeychainStore struct{}

func (darwinKeychainStore) Load() ([]byte, bool, error) {
	out, err := exec.Command("/usr/bin/security", "find-generic-password", "-s", keychainService, "-a", keychainAccount, "-w").Output()
	if err != nil {
		return nil, false, nil
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(out)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, false, os.ErrInvalid
	}
	return seed, true, nil
}

func (darwinKeychainStore) Save(seed []byte) error {
	hexSeed := hex.EncodeToString(seed)
	return exec.Command("/usr/bin/security", "add-generic-password", "-U", "-s", keychainService, "-a", keychainAccount, "-w", hexSeed).Run()
}

func defaultKeyStore(path string) keyStore {
	if runtime.GOOS != "darwin" {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	want := filepath.Join(home, ".config", "agent-machine", "daemon.key")
	if filepath.Clean(path) != filepath.Clean(want) {
		return nil
	}
	return darwinKeychainStore{}
}

func loadOrCreateKey(path string) (ed25519.PrivateKey, error) {
	return loadOrCreateKeyWithStore(path, defaultKeyStore(path))
}

func loadOrCreateKeyWithStore(path string, store keyStore) (ed25519.PrivateKey, error) {
	if store != nil {
		if seed, ok, err := store.Load(); err != nil {
			return nil, err
		} else if ok {
			_ = os.Remove(path)
			priv := ed25519.NewKeyFromSeed(seed)
			if err := writeDaemonPub(path, priv); err != nil {
				return nil, err
			}
			return priv, nil
		}
	}
	seed, err := os.ReadFile(path)
	if err == nil && len(seed) == ed25519.SeedSize {
		if store != nil && store.Save(seed) == nil {
			_ = os.Remove(path)
		} else {
			if err := stripDarwinACL(path); err != nil {
				return nil, err
			}
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, err
			}
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
	if store != nil && store.Save(priv.Seed()) == nil {
		_ = os.Remove(path)
	} else {
		if err := os.WriteFile(path, priv.Seed(), 0o600); err != nil {
			return nil, err
		}
		if err := stripDarwinACL(path); err != nil {
			return nil, err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
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
