package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ActivateUserService loads the per-user unit. It does not use root.
// Windows only writes the description, so activation is a no-op there.
func ActivateUserService(goos, path string) error {
	switch goos {
	case "darwin":
		uid := os.Getuid()
		err := launchctl("bootstrap", fmt.Sprintf("gui/%d", uid), path)
		if err != nil {
			_ = launchctl("bootout", fmt.Sprintf("user/%d", uid), path)
			if uerr := launchctl("bootstrap", fmt.Sprintf("user/%d", uid), path); uerr == nil {
				return nil
			}
		}
		return err
	case "linux":
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		return systemctl("enable", "--now", "agent-machine.service")
	default:
		return nil
	}
}

// ReloadUserService makes systemd reread user units, so a unit file that
// install restored or removed replaces whatever an earlier daemon-reload
// loaded. launchd reads the plist only at bootstrap, so darwin needs nothing.
func ReloadUserService(goos string) error {
	if goos == "linux" {
		return systemctl("daemon-reload")
	}
	return nil
}

// DeactivateUserService unloads the per-user unit. A unit that is not
// loaded is success.
func DeactivateUserService(goos, path string) error {
	switch goos {
	case "darwin":
		uid := os.Getuid()
		err1 := launchctl("bootout", fmt.Sprintf("gui/%d", uid), path)
		err2 := launchctl("bootout", fmt.Sprintf("user/%d", uid), path)
		if err1 == nil || err2 == nil {
			return nil
		}
		return err1
	case "linux":
		err := systemctl("disable", "--now", "agent-machine.service")
		if err == nil || strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		return err
	default:
		return nil
	}
}

func launchctl(args ...string) error {
	cmd := exec.Command("launchctl", args...)
	cmd.Env = minimalEnv(os.Getenv("HOME"), nil)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.ToLower(string(out))
	if strings.Contains(text, "already") || strings.Contains(text, "no such process") || strings.Contains(text, "not found") {
		return nil
	}
	return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Env = minimalEnv(os.Getenv("HOME"), os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
