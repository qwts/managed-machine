//go:build !windows

package winget

// elevated reports false on every other platform: the machine scope is a
// Windows concept, and managed-machine never elevates anywhere.
func elevated() bool { return false }
