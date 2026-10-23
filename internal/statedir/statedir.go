// Package statedir locates the platform state directory of ADR-0012, where
// saved plans, journals and source snapshots live. It only computes the
// path; callers create what they need when they first write, so read-only
// commands never create it.
package statedir

import (
	"errors"
	"path/filepath"
)

// Name is the directory name under each platform's base.
const Name = "managed-machine"

// Override names an explicit state directory, for isolated harnesses and
// tests; it takes precedence over the platform default.
const Override = "MANAGED_MACHINE_STATE_DIR"

// ErrNoHome means the environment names no base directory for the OS.
var ErrNoHome = errors.New("no home directory in the environment to locate the state directory")

// Path returns the state directory for goos from getenv: the override when
// set, otherwise ~/Library/Application Support/managed-machine on macOS,
// $XDG_DATA_HOME/managed-machine or ~/.local/share/managed-machine on
// Linux, and %LOCALAPPDATA%\managed-machine on Windows.
func Path(goos string, getenv func(string) string) (string, error) {
	if p := getenv(Override); p != "" {
		return filepath.Clean(p), nil
	}
	switch goos {
	case "darwin":
		if h := getenv("HOME"); h != "" {
			return filepath.Join(h, "Library", "Application Support", Name), nil
		}
	case "windows":
		if l := getenv("LOCALAPPDATA"); l != "" {
			return filepath.Join(l, Name), nil
		}
	default:
		if x := getenv("XDG_DATA_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, Name), nil
		}
		if h := getenv("HOME"); h != "" {
			return filepath.Join(h, ".local", "share", Name), nil
		}
	}
	return "", ErrNoHome
}
