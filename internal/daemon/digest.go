package daemon

import (
	"crypto/ed25519"
	"os"
	"time"
)

// digestEvery is the status push cadence in addition to a digest after every receipt.
const digestEvery = 15 * time.Minute

// writeDigest stores one signed status snapshot. The body is the status
// report; command output is not added here.
func writeDigest(path string, body []byte, priv ed25519.PrivateKey) error {
	nonce, err := newNonce()
	if err != nil {
		return err
	}
	res := Response{
		OK:        true,
		State:     "digest",
		Body:      body,
		Timestamp: time.Now().Unix(),
		Nonce:     nonce,
	}
	signResponse(priv, &res)
	line, err := formatLine(res)
	if err != nil {
		return err
	}
	_ = os.Remove(path)
	if err := os.WriteFile(path, line, 0o600); err != nil {
		return err
	}
	if err := stripDarwinACL(path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}
