//go:build windows

package secret

import (
	"os"
	"path/filepath"
	"strings"
)

// extensions are the file names Windows resolves an executable by: the
// PATHEXT of the environment, or the default list.
func extensions(env []string) []string {
	if pathext := envValue(env, "PATHEXT"); pathext != "" {
		var out []string
		for _, e := range strings.Split(pathext, ";") {
			if e = strings.TrimSpace(e); e != "" {
				if e[0] != '.' {
					e = "." + e
				}
				out = append(out, strings.ToLower(e))
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{".com", ".exe", ".bat", ".cmd"}
}

// executable reports whether the path is an executable file. Windows has
// no execute bit, so a regular file with a known extension is one.
func executable(path string, exts []string) bool {
	fi, err := os.Stat(path)
	if err != nil || fi.IsDir() {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	for _, e := range exts {
		if ext == e {
			return true
		}
	}
	return false
}
