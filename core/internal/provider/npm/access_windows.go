//go:build windows

package npm

import "os"

// writable reports whether the directory is usable by the current user.
// Windows has no permission bit to ask the kernel about without writing
// to the directory, so the check is that it exists: the per-user prefix
// under %APPDATA% belongs to the user, and managed-machine never
// elevates to reach a shared one.
func writable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
