//go:build windows

package brew

// Homebrew has no Windows prefix; DefaultPrefix is empty there, so these
// are reached only through an explicit test prefix.
func writable(string) bool { return false }
func owned(string) bool    { return false }
