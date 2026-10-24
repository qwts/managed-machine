package main

import "runtime"

// exe appends the native executable suffix to name, so the gates and their
// tests name built binaries the same way on every target.
func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
