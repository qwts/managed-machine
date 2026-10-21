package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Query the fixture process, not the runner label or an emulated parent shell.
func nativeArchitecture() (string, bool, error) {
	api := syscall.NewLazyDLL("kernel32.dll").NewProc("IsWow64Process2")
	var process, native uint16
	ok, _, err := api.Call(^uintptr(0), uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native)))
	if ok == 0 {
		return "", false, err
	}
	arch := map[uint16]string{0x8664: "amd64", 0xaa64: "arm64"}[native]
	if arch == "" {
		return "", false, fmt.Errorf("unknown native architecture")
	}
	return arch, process != 0 || runtime.GOARCH != arch, nil
}
