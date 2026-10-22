//go:build unix

package config

import "os"

// extensions are the file names an executable carries on this platform.
func extensions([]string) []string { return []string{""} }

// executable reports whether the path is an executable file.
func executable(path string, _ []string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}
