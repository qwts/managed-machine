//go:build windows

package daemon

import (
	"net"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestPipePeerMatchesCreatingUser(t *testing.T) {
	dir := t.TempDir()
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	type accepted struct {
		conn net.Conn
		err  error
	}
	ch := make(chan accepted, 1)
	go func() {
		conn, err := ln.Accept()
		ch <- accepted{conn, err}
	}()
	path, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		t.Fatal(err)
	}
	var client windows.Handle
	deadline := time.Now().Add(2 * time.Second)
	for {
		client, err = windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() { windows.CloseHandle(client) })
	got := <-ch
	if got.err != nil {
		t.Fatal(got.err)
	}
	t.Cleanup(func() { got.conn.Close() })
	uid, err := peerCred(got.conn)
	if err != nil {
		t.Fatal(err)
	}
	if uid != ownerPeerID() {
		t.Fatalf("peer %d, owner %d", uid, ownerPeerID())
	}
}

func TestTokenUserIsNotEveryone(t *testing.T) {
	self, err := tokenUserSID(windows.GetCurrentProcessToken())
	if err != nil {
		t.Fatal(err)
	}
	world, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	if self == world.String() {
		t.Fatal("daemon token user is Everyone")
	}
}
