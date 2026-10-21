package cargo

import (
	"fmt"
	"os"
	"path/filepath"
)

// lookPathIn searches a PATH for an executable, the way exec.LookPath
// does, so the provider never consults the process environment behind the
// caller's back: the PATH it searches is the one of the injected
// environment.
func lookPathIn(goos, file, pathEnv string) (string, error) {
	if file == "" {
		return "", fmt.Errorf("no file to look for")
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		path := filepath.Join(dir, file)
		if goos == "windows" {
			path += ".exe"
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if goos != "windows" && info.Mode()&0o111 == 0 {
			continue
		}
		return path, nil
	}
	return "", fmt.Errorf("%s: executable file not found in $PATH", file)
}
