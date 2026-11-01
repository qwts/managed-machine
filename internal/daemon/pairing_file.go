package daemon

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pairingClients are the only live peers. terminal is the Terminal thin
// client. agent-daemon is the local control plane. A later client is not
// accepted by adding a file.
var pairingClients = []string{"terminal", "agent-daemon"}

var pairingSuffixes = []string{".pub", ".next", ".confirm", ".revoked"}

func ensureStateDirAndPeers(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := stripDarwinACL(dir); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	if err := migrateLegacyPairing(dir); err != nil {
		return "", err
	}
	peers := filepath.Join(dir, "peers")
	if err := os.MkdirAll(peers, 0o700); err != nil {
		return "", err
	}
	if err := stripDarwinACL(peers); err != nil {
		return "", err
	}
	if err := os.Chmod(peers, 0o700); err != nil {
		return "", err
	}
	return peers, nil
}

// PairTerminal creates or loads client.key in the daemon state directory and
// stages peers/terminal.next and peers/terminal.confirm so the next request
// makes the terminal key live. It refuses legacy peer.* files beside peers/
// and unknown files in peers/.
func PairTerminal(root string) (string, error) {
	dir := stateDir(root)
	peers, err := ensureStateDirAndPeers(dir)
	if err != nil {
		return "", err
	}
	if err := checkPairingFiles(peers); err != nil {
		return "", err
	}
	if _, err := loadOrCreateKey(filepath.Join(dir, "daemon.key")); err != nil {
		return "", err
	}
	clientKeyPath := filepath.Join(dir, "client.key")
	seed, err := os.ReadFile(clientKeyPath)
	var priv ed25519.PrivateKey
	switch {
	case err == nil && len(seed) == ed25519.SeedSize:
		priv = ed25519.NewKeyFromSeed(seed)
	case errors.Is(err, os.ErrNotExist):
		_, priv, err = ed25519.GenerateKey(nil)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(clientKeyPath, priv.Seed(), 0o600); err != nil {
			return "", err
		}
		if err := stripDarwinACL(clientKeyPath); err != nil {
			return "", err
		}
		if err := os.Chmod(clientKeyPath, 0o600); err != nil {
			return "", err
		}
	case err != nil:
		return "", err
	default:
		return "", errors.New("client key has the wrong length")
	}
	pub := priv.Public().(ed25519.PublicKey)
	if err := os.WriteFile(filepath.Join(peers, "terminal.next"), pub, 0o600); err != nil {
		return "", err
	}
	sum := sha256.Sum256(pub)
	if err := os.WriteFile(filepath.Join(peers, "terminal.confirm"), []byte(hex.EncodeToString(sum[:])+"\n"), 0o600); err != nil {
		return "", err
	}
	if _, err := loadPairing(dir); err != nil {
		return "", err
	}
	return filepath.Join(peers, "terminal.pub"), nil
}

// UnpairTerminal revokes the terminal pairing and removes client.key.
func UnpairTerminal(root string) error {
	dir := stateDir(root)
	peers, err := ensureStateDirAndPeers(dir)
	if err != nil {
		return err
	}
	if err := checkPairingFiles(peers); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(peers, "terminal.revoked"), []byte("revoked\n"), 0o600); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(peers, "terminal.next"))
	_ = os.Remove(filepath.Join(peers, "terminal.confirm"))
	if err := os.Remove(filepath.Join(dir, "client.key")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// loadPairing reads each client's live key from peers/<client>.pub.
// peers/<client>.next does not replace that client's live key unless
// peers/<client>.confirm contains the SHA-256 of the staged key. A wrong
// confirm file leaves the live key in place. peers/<client>.revoked drops
// that client until a confirmed re-registration replaces it. The other
// client stays live. A legacy peer.pub is moved to peers/terminal.pub.
// Old peer.* files beside an existing peers directory are refused.
func loadPairing(dir string) (Pairing, error) {
	if err := migrateLegacyPairing(dir); err != nil {
		return Pairing{}, err
	}
	peers := filepath.Join(dir, "peers")
	if _, err := os.Stat(peers); errors.Is(err, os.ErrNotExist) {
		return Pairing{}, nil
	} else if err != nil {
		return Pairing{}, err
	}
	if err := checkPairingFiles(peers); err != nil {
		return Pairing{}, err
	}
	var keys []ed25519.PublicKey
	var terminal ed25519.PublicKey
	for _, client := range pairingClients {
		key, err := loadClientKey(peers, client)
		if err != nil {
			return Pairing{}, err
		}
		if key != nil {
			keys = append(keys, key)
			if client == "terminal" {
				terminal = append(ed25519.PublicKey(nil), key...)
			}
		}
	}
	pairing, err := pairingFromKeys(keys)
	if err != nil {
		return Pairing{}, err
	}
	pairing.Terminal = terminal
	return pairing, nil
}

func migrateLegacyPairing(dir string) error {
	legacy := []string{"peer.pub", "peer.next", "peer.confirm", "peer.revoked"}
	var present []string
	for _, name := range legacy {
		_, err := os.Stat(filepath.Join(dir, name))
		if err == nil {
			present = append(present, name)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if len(present) == 0 {
		return nil
	}
	peers := filepath.Join(dir, "peers")
	if _, err := os.Stat(peers); err == nil {
		return errors.New("pairing state is mixed")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Mkdir(peers, 0o700); err != nil {
		return err
	}
	for _, name := range present {
		suffix := strings.TrimPrefix(name, "peer")
		if err := os.Rename(filepath.Join(dir, name), filepath.Join(peers, "terminal"+suffix)); err != nil {
			return err
		}
	}
	return nil
}

func checkPairingFiles(peers string) error {
	entries, err := os.ReadDir(peers)
	if err != nil {
		return err
	}
	allowed := map[string]struct{}{}
	for _, client := range pairingClients {
		for _, suffix := range pairingSuffixes {
			allowed[client+suffix] = struct{}{}
		}
	}
	for _, entry := range entries {
		if _, ok := allowed[entry.Name()]; !ok || entry.IsDir() {
			return fmt.Errorf("pairing file %s is not a client key", entry.Name())
		}
	}
	return nil
}

func loadClientKey(peers, client string) (ed25519.PublicKey, error) {
	revokedPath := filepath.Join(peers, client+".revoked")
	revoked := false
	if _, err := os.Stat(revokedPath); err == nil {
		revoked = true
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var live ed25519.PublicKey
	if !revoked {
		pub, err := loadPublic(filepath.Join(peers, client+".pub"))
		if err == nil {
			live = pub
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	next, err := loadPublic(filepath.Join(peers, client+".next"))
	if errors.Is(err, os.ErrNotExist) {
		return live, nil
	}
	if err != nil {
		return nil, err
	}
	confirm, err := os.ReadFile(filepath.Join(peers, client+".confirm"))
	if errors.Is(err, os.ErrNotExist) {
		return live, nil
	}
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(next)
	if strings.TrimSpace(string(confirm)) != hex.EncodeToString(sum[:]) {
		return live, nil
	}
	pubPath := filepath.Join(peers, client+".pub")
	if err := os.WriteFile(pubPath, next, 0o600); err != nil {
		return nil, err
	}
	if err := stripDarwinACL(pubPath); err != nil {
		return nil, err
	}
	if err := os.Chmod(pubPath, 0o600); err != nil {
		return nil, err
	}
	_ = os.Remove(filepath.Join(peers, client+".next"))
	_ = os.Remove(filepath.Join(peers, client+".confirm"))
	_ = os.Remove(revokedPath)
	return append(ed25519.PublicKey(nil), next...), nil
}

func pairingFromKeys(keys []ed25519.PublicKey) (Pairing, error) {
	var pairing Pairing
	seen := map[string]struct{}{}
	var live []ed25519.PublicKey
	for _, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			continue
		}
		if _, ok := seen[string(key)]; ok {
			return Pairing{}, errors.New("pairing keys are not distinct")
		}
		seen[string(key)] = struct{}{}
		live = append(live, append(ed25519.PublicKey(nil), key...))
	}
	if len(live) == 0 {
		return pairing, nil
	}
	pairing.Live = live[0]
	pairing.Extra = live[1:]
	return pairing, nil
}
