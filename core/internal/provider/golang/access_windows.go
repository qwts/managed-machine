//go:build windows

package golang

import "os"

// writable reports whether the current user may write path. Windows has
// no permission bits; the read-only attribute of the directory decides.
func writable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir() && info.Mode().Perm()&0o200 != 0
}

// executable reports whether path is a file the current user may run.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
