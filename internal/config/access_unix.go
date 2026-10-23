//go:build unix

package config

import (
	"os"
	"path/filepath"
	"syscall"
)

// writableDir asks the kernel whether the current user may write the
// directory path: the directory itself when it exists, and otherwise its
// nearest existing ancestor, which is where the directory would be created.
// Nothing is written to find out.
func writableDir(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
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

// writableDestination reports whether the current user may write or replace
// the file at path.
func writableDestination(path string) bool {
	if fi, err := os.Stat(path); err == nil && fi.IsDir() {
		return false
	}
	return writableDir(filepath.Dir(path))
}
