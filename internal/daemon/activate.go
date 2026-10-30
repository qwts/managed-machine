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
		return launchctl("bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), path)
	case "linux":
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
		return systemctl("enable", "--now", "agent-machine.service")
	default:
		return nil
	}
}

// DeactivateUserService unloads the per-user unit. A unit that is not
// loaded is success.
func DeactivateUserService(goos, path string) error {
	switch goos {
	case "darwin":
		return launchctl("bootout", fmt.Sprintf("gui/%d", os.Getuid()), path)
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
