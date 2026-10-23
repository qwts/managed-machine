package uv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// lookPath finds file in the PATH of env, the way exec.LookPath finds it
// in the process PATH. The provider looks its tool up in the PATH of the
// environment it was given, so a plan binds to the environment it runs
// in and not to whatever the process happens to carry.
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
	for _, dir := range filepath.SplitList(value(env, "PATH")) {
		if dir == "" {
			dir = "."
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

// value returns the value of the named variable in env, or "" when the
// environment does not carry it.
func value(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}
