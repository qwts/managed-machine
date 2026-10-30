package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestListenModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket permissions are not the Windows pipe")
	}
	dir := filepath.Join(t.TempDir(), "state")
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", info.Mode().Perm())
	}
	sock, err := os.Stat(filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	if sock.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o", sock.Mode().Perm())
	}
}

func TestServeStatusSigned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket permissions are not the Windows pipe")
	}
	pub, priv := keyPair(t)
	dpub, dpriv := keyPair(t)
	now := time.Now()
	g := &Gate{Pairing: Pairing{Live: pub}, PeerUID: 501, Now: func() time.Time { return now }}
	dir := filepath.Join(t.TempDir(), "state")
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, ln, g, dpriv, func(net.Conn) (int, error) { return 501, nil }, Hooks{}) }()

	conn, err := net.Dial("unix", filepath.Join(dir, "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw := signed(t, priv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "sock-1", Soul: "iron", Harness: "cursor"})
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var res Response
	if err := json.Unmarshal(line, &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.State != "idle" {
		t.Fatalf("response %#v", res)
	}
	if res.RequestNonce != "sock-1" {
		t.Fatalf("request nonce %q", res.RequestNonce)
	}
	if !VerifyResponse(dpub, res) {
		t.Fatal("response signature did not verify")
	}
	res.RequestNonce = "other"
	if VerifyResponse(dpub, res) {
		t.Fatal("a response for another request verified")
	}
	cancel()
	<-done
}
