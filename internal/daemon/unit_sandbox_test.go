package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLinuxUnitLeavesTheInstallPathsWritable(t *testing.T) {
	body := linuxUserUnit("/home/linuxbrew/.linuxbrew/bin/managed-machine", "")
	for _, want := range []string{
		"NoNewPrivileges=yes\n",
		"ProtectSystem=strict\n",
		"PrivateTmp=yes\n",
		"ReadWritePaths=-/home/linuxbrew/.linuxbrew -%h/.local -%h/.cache -%h/.npm %h/.config/agent-machine\n",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unit lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "%%") {
		t.Errorf("unit carries an unexpanded %%%% specifier:\n%s", body)
	}
}

func TestLinuxInstallCreatesEveryHomePathTheUnitNames(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".local"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Install("linux", root, "/usr/bin/managed-machine", ""); err != nil {
		t.Fatal(err)
	}
	for dir, mode := range map[string]os.FileMode{
		".local":                0o755,
		".cache":                0o700,
		".npm":                  0o700,
		".config/agent-machine": 0o700,
	} {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil || !info.IsDir() || runtime.GOOS != "windows" && info.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", dir, info, err)
		}
	}
	other := t.TempDir()
	if _, err := Install("darwin", other, "/opt/homebrew/bin/managed-machine", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, ".config", "agent-machine")); !os.IsNotExist(err) {
		t.Fatalf("darwin install created the linux state dir: %v", err)
	}
}
