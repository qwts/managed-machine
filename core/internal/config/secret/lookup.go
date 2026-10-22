package secret

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// lookPath finds file in the PATH of env, the way exec.LookPath finds it
// in the process PATH. The resolver looks pass up in the PATH of the
// injected environment, never the host process environment.
func lookPath(file string, env []string) (string, error) {
	if file == "" {
		return "", exec.ErrNotFound
	}
	exts := extensions(env)
	if strings.ContainsRune(file, '/') || strings.ContainsRune(file, os.PathSeparator) {
		if executable(file, exts) {
			return file, nil
		}
		return "", exec.ErrNotFound
	}
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if dir == "" {
			continue
		}
		for _, ext := range exts {
			candidate := filepath.Join(dir, file+ext)
			if executable(candidate, exts) {
				return candidate, nil
			}
		}
	}
	return "", exec.ErrNotFound
}

// envValue returns the last value of the named variable in env (matching
// variable names case-insensitively), or "" when the environment does not
// carry it.
func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
