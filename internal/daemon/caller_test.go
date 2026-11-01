package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCallerRefuse(t *testing.T) {
	ok := Caller{Username: "admin", UID: 501, Home: "/Users/admin", RegisteredHome: "/Users/admin"}
	if msg := ok.Refuse(); msg != "" {
		t.Fatalf("owner refused: %s", msg)
	}
	okUIDFallback := Caller{Username: "admin", UID: 501, Home: "/Users/admin", RegisteredHome: "/Users/admin", PrefixOwner: "501"}
	if msg := okUIDFallback.Refuse(); msg != "" {
		t.Fatalf("uid fallback refused: %s", msg)
	}
	cases := []struct {
		name string
		c    Caller
	}{
		{"root name", Caller{Username: "root", UID: 501, Home: "/var/root", RegisteredHome: "/var/root"}},
		{"uid 0", Caller{Username: "admin", UID: 0, Home: "/Users/admin", RegisteredHome: "/Users/admin"}},
		{"system", Caller{Username: "SYSTEM", UID: 18, Home: `C:\Windows`, RegisteredHome: `C:\Windows`}},
		{"agent account", Caller{Username: "qwts-cursor-agent", UID: 502, Home: "/Users/qwts-cursor-agent", RegisteredHome: "/Users/qwts-cursor-agent"}},
		{"agent env", Caller{Username: "admin", UID: 501, Home: "/Users/admin", RegisteredHome: "/Users/admin", Env: map[string]string{"CURSOR_AGENT": "1"}}},
		{"foreign home", Caller{Username: "admin", UID: 501, Home: "/tmp/elsewhere", RegisteredHome: "/Users/admin"}},
		{"unknown home", Caller{Username: "admin", UID: 501, Home: "/Users/admin"}},
		{"not prefix owner", Caller{Username: "user", UID: 502, Home: "/Users/user", RegisteredHome: "/Users/user", PrefixOwner: "admin"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.c.Refuse() == "" {
				t.Fatal("expected a refusal")
			}
		})
	}
	userClient := Caller{Username: "user", UID: 502, Home: "/Users/user", RegisteredHome: "/Users/user", PrefixOwner: "admin"}
	if msg := userClient.RefuseClient(); msg != "" {
		t.Fatalf("expected user client to be allowed on admin-owned prefix, got: %s", msg)
	}
	otherClient := Caller{Username: "guest", UID: 503, Home: "/Users/guest", RegisteredHome: "/Users/guest", PrefixOwner: "admin"}
	if otherClient.RefuseClient() == "" {
		t.Fatal("expected guest client to be refused")
	}
}

func TestPrefixOwnerDistinguishesAbsentFromUnreadable(t *testing.T) {
	owner, err := PrefixOwner(filepath.Join(t.TempDir(), "absent"))
	if err != nil || owner != "" {
		t.Fatalf("absent prefix got %q %v", owner, err)
	}
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		return
	}
	ownedDir := t.TempDir()
	gotOwner, err := PrefixOwner(ownedDir)
	if err != nil || gotOwner == "" {
		t.Fatalf("owned prefix got %q %v", gotOwner, err)
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if _, err := PrefixOwner(filepath.Join(locked, "prefix")); err == nil {
		t.Fatal("expected an error when prefix parent denies traversal")
	}
}
