package target

import (
	"fmt"
	"syscall"
	"unsafe"
)

// nativeArchitecture asks the kernel about this process: the native machine
// architecture, not the architecture of an emulated parent shell.
func nativeArchitecture() (string, error) {
	api := syscall.NewLazyDLL("kernel32.dll").NewProc("IsWow64Process2")
	var process, native uint16
	ok, _, err := api.Call(^uintptr(0), uintptr(unsafe.Pointer(&process)), uintptr(unsafe.Pointer(&native)))
	if ok == 0 {
		return "", fmt.Errorf("IsWow64Process2: %w", err)
	}
	switch native {
	case 0x8664:
		return "amd64", nil
	case 0xaa64:
		return "arm64", nil
	}
	return "", fmt.Errorf("unknown native machine 0x%x", native)
}
