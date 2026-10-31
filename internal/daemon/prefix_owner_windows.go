//go:build windows

package daemon

// PrefixOwner returns ("", nil) on Windows, which has no shared Homebrew prefix.
func PrefixOwner(string) (string, error) {
	return "", nil
}
