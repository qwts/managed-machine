//go:build unix

package cargo

import (
	"os"
	"syscall"
)

// writable asks the kernel whether the current user may write the path,
// without writing anything.
func writable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	return syscall.Access(path, 2) == nil // W_OK
}
