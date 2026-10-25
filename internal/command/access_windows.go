//go:build windows

package command

import (
	"os"
	"path/filepath"
)

// IsWritableDir asks the operating system whether the current user may write
// the directory path: the directory itself when it exists, and otherwise
// its nearest existing ancestor, which is where the directory would be
// created.
func IsWritableDir(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		fi, err := os.Stat(p)
		if err == nil {
			return fi.IsDir() && fi.Mode().Perm()&0o200 != 0
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

// IsWritable reports whether the current user may write or replace the file
// at path.
func IsWritable(path string) bool {
	if fi, err := os.Stat(path); err == nil {
		if fi.IsDir() || fi.Mode().Perm()&0o200 == 0 {
			return false
		}
	}
	return IsWritableDir(filepath.Dir(path))
}
