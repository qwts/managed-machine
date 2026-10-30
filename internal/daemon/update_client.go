package daemon

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// TryUpdate sends one signed update request when the daemon is reachable and
// the client key exists. A missing daemon or key returns handled false so the
// legacy --update path can run. The nonce is single-use. The response is
// accepted only when daemon.pub, beside the client key, verifies it.
func TryUpdate(endpoint, keyPath, flavor string) (bool, Response, error) {
	seed, err := os.ReadFile(keyPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, Response{}, nil
		}
		return false, Response{}, err
	}
	if len(seed) != ed25519.SeedSize {
		return false, Response{}, errors.New("client key has the wrong length")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	conn, err := dialDaemon(endpoint)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, Response{}, nil
		}
		return true, Response{}, err
	}
	defer conn.Close()
	nonce, err := newNonce()
	if err != nil {
		return true, Response{}, err
	}
	req := Request{Verb: "update", Name: flavor, Timestamp: time.Now().Unix(), Nonce: nonce}
	if err := Sign(priv, &req); err != nil {
		return true, Response{}, err
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return true, Response{}, err
	}
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return true, Response{}, err
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return true, Response{}, err
	}
	var res Response
	if err := json.Unmarshal(line, &res); err != nil {
		return true, Response{}, err
	}
	pub, err := loadPublic(filepath.Join(filepath.Dir(keyPath), "daemon.pub"))
	if err != nil {
		return true, Response{}, err
	}
	if !VerifyResponse(pub, res) {
		return true, Response{}, errors.New("daemon response signature does not match")
	}
	return true, res, nil
}
