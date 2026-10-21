//go:build unix

package npm

import (
	"os"
	"syscall"
)

// writable asks the kernel whether the current user may write the
// directory, without writing anything.
func writable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	return syscall.Access(path, 2) == nil // W_OK
}
