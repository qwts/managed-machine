package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunChecksRealPeer(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "mm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := stateDir(root)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	pub, priv := keyPair(t)
	if err := os.WriteFile(filepath.Join(dir, "peer.pub"), pub, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, root) }()

	sock := filepath.Join(dir, "daemon.sock")
	var conn net.Conn
	for i := 0; i < 50; i++ {
		conn, err = net.Dial("unix", sock)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	now := time.Now()
	raw := signed(t, priv, Request{Verb: "status", Timestamp: now.Unix(), Nonce: "peer-1"})
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
	if !res.OK || res.Diagnostic != "" {
		t.Fatalf("response %#v", res)
	}
	cancel()
	<-done
}
