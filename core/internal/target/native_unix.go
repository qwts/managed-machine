//go:build !windows

package target

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// unamePaths are the system locations of uname. The utility is invoked by
// absolute path with an empty environment, so a caller who controls PATH
// cannot substitute it and spoof the reported architecture.
var unamePaths = []string{"/usr/bin/uname", "/bin/uname"}

func nativeArchitecture() (string, error) {
	for _, path := range unamePaths {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		cmd := exec.Command(path, "-m")
		cmd.Env = []string{}
		value, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("%s -m: %w", path, err)
		}
		arch := strings.TrimSpace(string(value))
		if arch == "" {
			return "", fmt.Errorf("%s -m returned nothing", path)
		}
		return arch, nil
	}
	return "", errors.New("uname not found at " + strings.Join(unamePaths, " or "))
}
