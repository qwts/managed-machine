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
