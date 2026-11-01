package daemon

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallIdempotent(t *testing.T) {
	root := t.TempDir()
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			path, err := Install(goos, root, "/opt/managed-machine/bin/managed-machine", "admin")
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
				t.Fatalf("mode %o", info.Mode().Perm())
			}
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), "sudo") || strings.Contains(string(body), "UserName") {
				t.Fatalf("unit elevates or names a system account field:\n%s", body)
			}
			if _, err := Install(goos, root, "/opt/managed-machine/bin/managed-machine", "admin"); err != nil {
				t.Fatal(err)
			}
			on, err := Installed(goos, root)
			if err != nil || !on {
				t.Fatalf("installed %v %v", on, err)
			}
			if err := Uninstall(goos, root); err != nil {
				t.Fatal(err)
			}
			if err := Uninstall(goos, root); err != nil {
				t.Fatal(err)
			}
			on, err = Installed(goos, root)
			if err != nil || on {
				t.Fatalf("still installed %v %v", on, err)
			}
		})
	}
}

func TestUnitPath(t *testing.T) {
	path, err := unitPath("darwin", "/Users/admin")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, filepath.Join("Library", "LaunchAgents", "dev.qwts.agent-machine.plist")) {
		t.Fatal(path)
	}
}

func TestDarwinAgentIncludesBackgroundSessionAndHome(t *testing.T) {
	body := darwinAgent("/Users/admin", "/opt/homebrew/bin/agent-machine")
	for _, want := range []string{"<string>Background</string>", "<string>Aqua</string>", "<key>HOME</key>", "<string>/Users/admin</string>"} {
		if !strings.Contains(body, want) {
			t.Fatalf("darwinAgent missing %s:\n%s", want, body)
		}
	}
}

type memKeyStore struct {
	seed    []byte
	loadErr error
	saveErr error
	saves   int
	deletes int
}

func (m *memKeyStore) Load() ([]byte, bool, error) {
	if m.loadErr != nil {
		return nil, false, m.loadErr
	}
	if len(m.seed) == 0 {
		return nil, false, nil
	}
	return append([]byte(nil), m.seed...), true, nil
}

func (m *memKeyStore) Save(seed []byte) error {
	m.saves++
	if m.saveErr != nil {
		return m.saveErr
	}
	m.seed = append([]byte(nil), seed...)
	return nil
}

func (m *memKeyStore) Delete() error {
	m.deletes++
	m.seed = nil
	return nil
}

func TestPrivateStateDirPermissionsAndNoUserAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	base, err := os.MkdirTemp("/tmp", "mm-priv")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	dir := StateDir(base)
	if want := filepath.Join(base, ".config", "agent-machine"); dir != want {
		t.Fatalf("StateDir(%q) = %q, want %q", base, dir, want)
	}
	if _, err := PairTerminal(base); err != nil {
		t.Fatal(err)
	}
	ln, err := Listen(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	priv, err := loadOrCreateKey(filepath.Join(dir, "daemon.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDigest(filepath.Join(dir, "status-digest.json"), []byte(`{"ok":true}`), priv); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{dir, filepath.Join(dir, "peers")} {
		info, err := os.Stat(d)
		if err != nil || info.Mode().Perm() != 0o700 || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("dir %s mode %v %v (must be 0700 with zero group/other access)", d, info.Mode(), err)
		}
		assertNoDarwinACL(t, d)
	}
	for _, f := range []string{
		filepath.Join(dir, "daemon.sock"),
		filepath.Join(dir, "daemon.pub"),
		filepath.Join(dir, "daemon.key"),
		filepath.Join(dir, "status-digest.json"),
		filepath.Join(dir, "client.key"),
		filepath.Join(dir, "peers", "terminal.pub"),
	} {
		info, err := os.Stat(f)
		if err != nil || info.Mode().Perm() != 0o600 || info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("file %s mode %v %v (must be 0600 with zero group/other access)", f, info.Mode(), err)
		}
		assertNoDarwinACL(t, f)
	}
}

func TestKeyStoreCustodyRemovesPlaintextKeyFile(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "daemon.key")
	store := &memKeyStore{}
	priv1, err := loadOrCreateKeyWithStore(keyPath, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("expected daemon.key to be absent on disk when stored in keyStore, got err=%v", err)
	}
	if len(store.seed) == 0 {
		t.Fatal("expected seed in keyStore")
	}
	priv2, err := loadOrCreateKeyWithStore(keyPath, store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(priv1.Seed(), priv2.Seed()) {
		t.Fatal("reloaded key seed mismatch")
	}
}

func TestKeyStoreLockedFailsClosedWithoutRotatingKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "daemon.key")
	store := &memKeyStore{}
	priv, err := loadOrCreateKeyWithStore(keyPath, store)
	if err != nil {
		t.Fatal(err)
	}
	wantSeed := append([]byte(nil), priv.Seed()...)
	store.loadErr = os.ErrPermission
	savesBefore := store.saves
	if _, err := loadOrCreateKeyWithStore(keyPath, store); err == nil {
		t.Fatal("expected locked/denied keyStore to fail closed")
	}
	if store.saves != savesBefore {
		t.Fatal("locked keyStore rotated the key")
	}
	if !bytes.Equal(store.seed, wantSeed) {
		t.Fatal("stored seed was modified on load error")
	}
	if _, err := os.Stat(keyPath); !os.IsNotExist(err) {
		t.Fatalf("expected no fallback daemon.key file on disk, got err=%v", err)
	}
}

func TestPurgeLegacySharedStateAndRefuseSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix symlinks only")
	}
	base, err := os.MkdirTemp("/tmp", "mm-legacy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })

	// 1. Refuse an arbitrary symlinked state directory.
	foreign := filepath.Join(base, "foreign")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(base, "home1", ".config")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	symlinkDir := filepath.Join(cfgDir, "agent-machine")
	if err := os.Symlink(foreign, symlinkDir); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureStateDirAndPeers(symlinkDir); err == nil {
		t.Fatal("expected symlinked state directory to be refused")
	}
	if _, err := Listen(symlinkDir); err == nil {
		t.Fatal("expected Listen on symlinked state directory to be refused")
	}

	// 2. Purge legacy <prefix>/var/agent-machine, unlink symlink to it, and rotate keys.
	legacyShared := filepath.Join(base, "var", "agent-machine")
	if err := os.MkdirAll(filepath.Join(legacyShared, "peers"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyShared, "daemon.key"), make([]byte, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	home2Cfg := filepath.Join(base, "home2", ".config")
	if err := os.MkdirAll(home2Cfg, 0o700); err != nil {
		t.Fatal(err)
	}
	dir2 := filepath.Join(home2Cfg, "agent-machine")
	if err := os.Symlink(legacyShared, dir2); err != nil {
		t.Fatal(err)
	}
	store := &memKeyStore{seed: bytes.Repeat([]byte{0x42}, 32)}
	if err := purgeLegacySharedState(dir2, legacyShared, store); err != nil {
		t.Fatalf("purgeLegacySharedState: %v", err)
	}
	if _, err := os.Lstat(legacyShared); !os.IsNotExist(err) {
		t.Fatalf("expected legacyShared to be removed, got err=%v", err)
	}
	if _, err := os.Lstat(dir2); !os.IsNotExist(err) {
		t.Fatalf("expected legacy symlink to be removed, got err=%v", err)
	}
	if store.deletes != 1 || len(store.seed) != 0 {
		t.Fatalf("expected keyStore seed to be rotated/deleted, got deletes=%d len=%d", store.deletes, len(store.seed))
	}
}

func assertNoDarwinACL(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		return
	}
	out, err := exec.Command("/bin/ls", "-lde", path).Output()
	if err != nil {
		t.Fatalf("ls -lde %s: %v", path, err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 1 {
		t.Fatalf("expected no ACL entries on %s, got:\n%s", path, string(out))
	}
}
