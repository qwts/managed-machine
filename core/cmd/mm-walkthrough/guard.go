package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// sentinels are the installer, provider, package-manager and authorization
// programs an S1 command must never start. Each is a copy of this binary on
// an otherwise empty PATH; when started under one of these names it records
// the invocation and fails, so any lookup by name is caught.
var sentinels = []string{
	"brew", "winget", "npm", "npx", "uv", "uvx", "pip", "pip3", "pipx",
	"sudo", "doas", "pkexec", "runas", "osascript", "security", "dscl", "sysadminctl", "installer", "softwareupdate",
	"apt", "apt-get", "dpkg", "snap", "msiexec", "choco", "scoop", "powershell", "pwsh",
	"git", "curl", "wget", "sh", "bash", "zsh", "cmd",
}

const sentinelLogEnv = "MM_WALKTHROUGH_SENTINEL_LOG"

// sentinelMode runs when this binary was started under a sentinel name.
func sentinelMode() bool {
	name := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	for _, s := range sentinels {
		if name == s {
			if p := os.Getenv(sentinelLogEnv); p != "" {
				if f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
					fmt.Fprintf(f, "%s %s\n", s, strings.Join(os.Args[1:], " "))
					f.Close()
				}
			}
			fmt.Fprintf(os.Stderr, "mm-walkthrough sentinel: %s must not be started by an S1 command\n", s)
			os.Exit(97)
		}
	}
	return false
}

func installSentinels(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	src, err := os.ReadFile(self)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, s := range sentinels {
		if runtime.GOOS == "windows" {
			s += ".exe"
		}
		if err := os.WriteFile(filepath.Join(dir, s), src, 0o755); err != nil {
			return err
		}
	}
	return nil
}

// entry is what the mutation guard compares: any created, removed,
// rewritten or re-permissioned path differs.
type entry struct {
	Kind  string
	Mode  fs.FileMode
	Size  int64
	MTime int64
	Hash  string
}

func snapshot(root string) (map[string]entry, error) {
	out := map[string]entry{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		e := entry{Mode: info.Mode(), MTime: info.ModTime().UnixNano()}
		switch {
		case d.IsDir():
			e.Kind = "dir"
		case info.Mode().IsRegular():
			e.Kind, e.Size = "file", info.Size()
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			b, err := io.ReadAll(f)
			f.Close()
			if err != nil {
				return err
			}
			e.Hash = sum(b)
		default:
			e.Kind = "other"
		}
		out[filepath.ToSlash(rel)] = e
		return nil
	})
	return out, err
}

// diff lists changed paths, relative to the harness root, so the evidence
// never carries the runner's absolute paths.
func diff(before, after map[string]entry) []string {
	var d []string
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			d = append(d, "removed "+p)
		case a != b:
			d = append(d, "changed "+p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			d = append(d, "created "+p)
		}
	}
	sort.Strings(d)
	return d
}
