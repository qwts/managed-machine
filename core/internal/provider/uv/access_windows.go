//go:build windows

package uv

import (
	"os"
	"path/filepath"
)

// writable asks the operating system whether the current user may write
// the path: the path itself when it exists, and otherwise its nearest
// existing ancestor, which is where the directory would be created.
// Windows has no execute bit; a directory that is not read-only is
// reported with its write bit, which is what the operating system answers
// for the current user.
func writable(path string) bool {
	for p := path; ; p = filepath.Dir(p) {
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
