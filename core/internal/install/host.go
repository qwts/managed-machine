package install

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"regexp"
	"runtime"
	"strings"
)

// Account is the current OS account name.
func Account() (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return u.Username, nil
}

var uuidLike = regexp.MustCompile(`"IOPlatformUUID" = "([0-9A-Fa-f-]+)"`)

// MachineID reads the OS's stable machine identifier: IOPlatformUUID on
// macOS, /etc/machine-id on Linux and MachineGuid on Windows. It binds a
// plan to the machine it was made on.
func MachineID(ctx context.Context) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err != nil {
			return "", err
		}
		if m := uuidLike.FindSubmatch(out); m != nil {
			return "darwin:" + strings.ToLower(string(m[1])), nil
		}
	case "linux":
		for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
			if b, err := os.ReadFile(p); err == nil && len(bytes.TrimSpace(b)) > 0 {
				return "linux:" + string(bytes.TrimSpace(b)), nil
			}
		}
	case "windows":
		out, err := exec.CommandContext(ctx, "reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
		if err != nil {
			return "", err
		}
		f := strings.Fields(string(out))
		if len(f) > 0 {
			return "windows:" + strings.ToLower(f[len(f)-1]), nil
		}
	}
	return "", errors.New("the machine identifier could not be read")
}
