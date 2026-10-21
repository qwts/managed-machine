//go:build !windows

package main

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func nativeArchitecture() (string, bool, error) {
	value, err := exec.Command("uname", "-m").Output()
	if err != nil {
		return "", false, err
	}
	arch := map[string]string{"aarch64": "arm64", "arm64": "arm64", "x86_64": "amd64"}[strings.TrimSpace(string(value))]
	if arch == "" {
		return "", false, fmt.Errorf("unknown native architecture")
	}
	return arch, arch != runtime.GOARCH, nil
}
