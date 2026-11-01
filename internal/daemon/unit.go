package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// A system LaunchDaemon, a systemd system unit, and a Windows service
// registration all need a privilege this daemon does not have. The unit is
// a per-user service of the prefix owner: a LaunchAgent, a systemd user
// unit, or a Windows service description that is written and not registered.
func unitPath(goos, root string) (string, error) {
	switch goos {
	case "darwin":
		return filepath.Join(root, "Library", "LaunchAgents", "dev.qwts.agent-machine.plist"), nil
	case "linux":
		return filepath.Join(root, ".config", "systemd", "user", "agent-machine.service"), nil
	case "windows":
		return filepath.Join(root, "AppData", "Local", "agent-machine", "agent-machine-service.xml"), nil
	default:
		return "", fmt.Errorf("unsupported operating system %q", goos)
	}
}

func unitBody(goos, root, executable, username string) (string, error) {
	exec := strings.TrimSpace(executable)
	if exec == "" {
		return "", fmt.Errorf("daemon executable is required")
	}
	switch goos {
	case "darwin":
		return darwinAgent(root, exec), nil
	case "linux":
		return linuxUserUnit(exec, username), nil
	case "windows":
		return windowsDescription(exec, username), nil
	default:
		return "", fmt.Errorf("unsupported operating system %q", goos)
	}
}

func darwinAgent(root, executable string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>dev.qwts.agent-machine</string>
	<key>ProgramArguments</key>
	<array>
		<string>%s</string>
		<string>daemon</string>
		<string>run</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>HOME</key>
		<string>%s</string>
	</dict>
	<key>LimitLoadToSessionType</key>
	<array>
		<string>Aqua</string>
		<string>Background</string>
		<string>StandardIO</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
</dict>
</plist>
`, xmlEscape(executable), xmlEscape(root))
}

func linuxUserUnit(executable, _ string) string {
	return fmt.Sprintf(`[Unit]
Description=agent-machine executor
After=default.target

[Service]
Type=simple
ExecStart=%s daemon run
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=%%h/.local %%h/.config/agent-machine

[Install]
WantedBy=default.target
`, unitEscape(executable))
}

func windowsDescription(executable, username string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!-- Written only. Registering a Windows service is a human action. -->
<service>
	<name>agent-machine</name>
	<account>%s</account>
	<command>%s daemon run</command>
</service>
`, xmlEscape(username), xmlEscape(executable))
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return r.Replace(s)
}

func unitEscape(s string) string {
	return strings.ReplaceAll(s, "\n", "")
}

// Install writes the per-user unit. A second call with the same contents
// leaves the file unchanged. The file mode is 0600.
func Install(goos, root, executable, username string) (string, error) {
	path, err := unitPath(goos, root)
	if err != nil {
		return "", err
	}
	body, err := unitBody(goos, root, executable, username)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	existing, readErr := os.ReadFile(path)
	if readErr == nil && string(existing) == body {
		return path, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

// Uninstall removes the unit. A missing unit is success.
func Uninstall(goos, root string) error {
	path, err := unitPath(goos, root)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Installed reports whether the unit file is present.
func Installed(goos, root string) (bool, error) {
	path, err := unitPath(goos, root)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func hostOS() string { return runtime.GOOS }
