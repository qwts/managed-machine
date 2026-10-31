//go:build windows

package daemon

// PrefixOwner returns "" on Windows, which has no shared Homebrew prefix.
func PrefixOwner(string) string {
	return ""
}
