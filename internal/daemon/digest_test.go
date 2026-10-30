package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDigestEvery15Minutes(t *testing.T) {
	if digestEvery != 15*time.Minute {
		t.Fatalf("cadence %s", digestEvery)
	}
}

func TestWriteDigestIsSignedAndMode0600(t *testing.T) {
	pub, priv := keyPair(t)
	path := filepath.Join(t.TempDir(), "status-digest.json")
	body := []byte(`{"section":"software","state":"absent"}`)
	if err := writeDigest(path, body, priv); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var res Response
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || string(res.Body) != string(body) || !VerifyResponse(pub, res) {
		t.Fatalf("digest %#v", res)
	}
}
