package daemon

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestUpdateRefusesElevationBeforeExec(t *testing.T) {
	var calls int
	p := testPlanner(t, func(Item, string, string) error { calls++; return nil })
	p.Catalog.Flavors = []Flavor{{Name: "default", Items: []string{"ripgrep", "docker"}}}
	ids, diagID := p.Update("")
	if diagID != "MM_DAEMON_PRIVILEGE_REQUIRED" || ids != "" {
		t.Fatalf("got %s %s", ids, diagID)
	}
	if calls != 0 {
		t.Fatal("update started a process")
	}
}

func TestUpdateAppliesDeclaredFlavor(t *testing.T) {
	var calls int
	p := testPlanner(t, func(Item, string, string) error { calls++; return nil })
	p.Catalog.Flavors = []Flavor{{Name: "default", Items: []string{"ripgrep"}}}
	ids, diagID := p.Update("")
	if diagID != "" || ids == "" {
		t.Fatalf("got %s %s", ids, diagID)
	}
	if calls != 1 {
		t.Fatalf("calls %d", calls)
	}
}

func TestTryUpdateFallsBackWhenSocketMissing(t *testing.T) {
	handled, _, err := TryUpdate(filepath.Join(t.TempDir(), "missing.sock"), filepath.Join(t.TempDir(), "client.key"), "")
	if err != nil || handled {
		t.Fatalf("handled %v err %v", handled, err)
	}
}

func TestTryUpdateSpeaksToSocket(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket permissions are not the Windows pipe")
	}
	pub, priv := keyPair(t)
	dpub, dpriv := keyPair(t)
	dir, err := os.MkdirTemp("/tmp", "mm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "client.key"), priv.Seed(), 0o600); err != nil {
		t.Fatal(err)
	}
	p := testPlanner(t, func(Item, string, string) error { return nil })
	p.Now = time.Now
	p.Catalog.Flavors = []Flavor{{Name: "default", Items: []string{"ripgrep"}}}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	g := &Gate{Pairing: Pairing{Live: pub}, PeerUID: os.Getuid()}
	go Serve(t.Context(), ln, g, dpriv, func(net.Conn) (int, error) { return os.Getuid(), nil }, Hooks{Update: p.Update})
	handled, res, err := TryUpdate(filepath.Join(dir, "daemon.sock"), filepath.Join(dir, "client.key"), "")
	if err != nil || !handled || !res.OK {
		t.Fatalf("handled %v res %#v err %v", handled, res, err)
	}
	if !VerifyResponse(dpub, res) {
		t.Fatal("response was not signed")
	}
}
