package daemon

import (
	"os"
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

func TestSharedPrefixDirPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	base, err := os.MkdirTemp("/tmp", "mm-shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	shared := filepath.Join(base, "var", "agent-machine")
	peers, err := ensureStateDirAndPeers(shared)
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(shared)
	if err != nil || dirInfo.Mode().Perm() != 0o750 {
		t.Fatalf("shared dir mode %v %v", dirInfo, err)
	}
	peersInfo, err := os.Stat(peers)
	if err != nil || peersInfo.Mode().Perm() != 0o770 || peersInfo.Mode()&os.ModeSticky == 0 {
		t.Fatalf("shared peers mode %v %v", peersInfo.Mode(), err)
	}
	ln, err := Listen(shared)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sockInfo, err := os.Stat(filepath.Join(shared, "daemon.sock"))
	if err != nil || sockInfo.Mode().Perm() != 0o660 {
		t.Fatalf("shared sock mode %v %v", sockInfo, err)
	}
	if _, err := loadOrCreateKey(filepath.Join(shared, "daemon.key")); err != nil {
		t.Fatal(err)
	}
	pubInfo, err := os.Stat(filepath.Join(shared, "daemon.pub"))
	if err != nil || pubInfo.Mode().Perm() != 0o640 {
		t.Fatalf("shared daemon.pub mode %v %v", pubInfo, err)
	}
	keyInfo, err := os.Stat(filepath.Join(shared, "daemon.key"))
	if err != nil || keyInfo.Mode().Perm() != 0o600 {
		t.Fatalf("shared daemon.key mode %v %v", keyInfo, err)
	}
}
