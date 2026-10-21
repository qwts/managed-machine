//go:build windows

package winget

import (
	"syscall"
	"unsafe"
)

// elevated reports whether the current process runs with an elevated token.
// It reads TOKEN_ELEVATION (class 20) from the token of the current process
// with the standard library's OpenProcessToken and GetTokenInformation, so
// the provider needs no new module dependency. A process that cannot be
// asked is not elevated.
func elevated() bool {
	proc, err := syscall.GetCurrentProcess()
	if err != nil {
		return false
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(proc, syscall.TOKEN_QUERY, &token); err != nil {
		return false
	}
	defer token.Close()
	var elevation uint32
	var returned uint32
	if err := syscall.GetTokenInformation(token, syscall.TokenElevation, (*byte)(unsafe.Pointer(&elevation)),
		uint32(unsafe.Sizeof(elevation)), &returned); err != nil {
		return false
	}
	return elevation != 0
}
