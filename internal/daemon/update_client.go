package daemon

import (
	"bufio"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net"
	"os"
	"time"
)

// TryUpdate sends one signed update request when the daemon socket and the
// client key both exist. A missing socket or key returns handled false so
// the legacy --update path can run. It does not modify that path.
func TryUpdate(socketPath, keyPath, flavor string) (bool, Response, error) {
	if _, err := os.Stat(socketPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, Response{}, nil
		}
		return false, Response{}, err
	}
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
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return true, Response{}, err
	}
	defer conn.Close()
	req := Request{Verb: "update", Name: flavor, Timestamp: time.Now().Unix(), Nonce: "update"}
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
	return true, res, nil
}
