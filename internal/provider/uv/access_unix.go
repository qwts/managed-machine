//go:build unix

package uv

import (
	"os"
	"path/filepath"
	"syscall"
)

// writable asks the kernel whether the current user may write the path:
// the path itself when it exists, and otherwise its nearest existing
// ancestor, which is where the directory would be created. Nothing is
// written to find out.
func writable(path string) bool {
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err == nil {
			return fi.IsDir() && syscall.Access(p, 2) == nil // W_OK
		}
		if !os.IsNotExist(err) {
			return false
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false
		}
	}
}
