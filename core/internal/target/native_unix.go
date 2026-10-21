//go:build !windows

package target

import (
	"fmt"
	"os/exec"
	"strings"
)

func nativeArchitecture() (string, error) {
	value, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "", fmt.Errorf("uname -m: %w", err)
	}
	arch := strings.TrimSpace(string(value))
	if arch == "" {
		return "", fmt.Errorf("uname -m returned nothing")
	}
	return arch, nil
}
