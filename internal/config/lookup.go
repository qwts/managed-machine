package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// lookPath finds file in extraDirs (for example a custom root's bin
// directory) or in the PATH of env, the way exec.LookPath finds it in the
// process PATH.
func lookPath(file string, extraDirs, env []string) (string, error) {
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
	dirs := append([]string(nil), extraDirs...)
	if pathVal := envValue(env, "PATH"); pathVal != "" {
		dirs = append(dirs, filepath.SplitList(pathVal)...)
	}
	for _, dir := range dirs {
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

// envValue returns the value of the named variable in env, or "" when the
// environment does not carry it.
func envValue(env []string, name string) string {
	prefix := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}
