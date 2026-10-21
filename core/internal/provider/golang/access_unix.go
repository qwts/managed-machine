//go:build unix

package golang

import (
	"os"
	"syscall"
)

// writable asks the kernel whether the current user may write path,
// without writing anything.
func writable(path string) bool {
	return syscall.Access(path, 2) == nil // W_OK
}

// executable reports whether path is a file the current user may run.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode().Perm()&0o111 != 0
}
