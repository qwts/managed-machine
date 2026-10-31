// Command managed-machine is the Go core entry point (ADR-0016, ADR-0017).
// It exposes the shared boundary, the read-only platform probe and S1
// software discovery and S2 installation; packaging to end users is an S6 step.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/daemon"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/appcopy"
	"github.com/qwts/managed-machine/internal/provider/archive"
	"github.com/qwts/managed-machine/internal/provider/brew"
	"github.com/qwts/managed-machine/internal/provider/cargo"
	"github.com/qwts/managed-machine/internal/provider/golang"
	"github.com/qwts/managed-machine/internal/provider/npm"
	"github.com/qwts/managed-machine/internal/provider/uv"
	"github.com/qwts/managed-machine/internal/provider/winget"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/skill"
	"github.com/qwts/managed-machine/internal/software"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/statedir"
	"github.com/qwts/managed-machine/internal/status"
	"github.com/qwts/managed-machine/internal/target"
)

// version is the release version embedded at build time with
// -ldflags "-s -w -X main.version=<VERSION>" (S6 D2). An unflagged build
// reports cli.DefaultVersion ("0.0.0-dev").
var version = cli.DefaultVersion

// prefixOverride replaces the canonical Homebrew prefix. It is empty in
// production and is never set from a flag or an environment variable: it
// exists so the S2 install gate's tests can build a core that resolves a
// prefix that does not exist, and so no test can reach a real Homebrew.
var prefixOverride string

// brewPrefix is the Homebrew prefix the core installs into.
func brewPrefix(target string) string {
	if prefixOverride != "" {
		return prefixOverride
	}
	return brew.DefaultPrefix(target)
}

func main() {
	getenv := cli.BridgeDarwinEnv(runtime.GOOS, os.Getenv)
	app := &cli.App{
		Name:       "managed-machine",
		Version:    version,
		Getenv:     getenv,
		Environ:    os.Environ,
		Executable: os.Executable,
		Commands: []cli.Command{
			cli.Platform(),
			cli.Version(version),
			software.Command(software.Deps{
				Deps: command.Deps{
					// The environment names the configured remote sources; the
					// default client fetches them with TLS verification on.
					Getenv: getenv,
					Client: nil,
				},
				Catalog: func() ([]source.Entry, *registry.Registry, error) { return software.LoadCatalog(getenv) },
				Install: func(ctx context.Context, r install.Request) (install.Result, error) {
					return install.Run(ctx, install.Env{
						StateDir: func() (string, error) { return statedir.Path(runtime.GOOS, getenv) },
						Account:  install.Account,
						Machine:  install.MachineID,
						Now:      time.Now,
						Provider: providers,
					}, r)
				},
			}),
			skill.Command(func(t target.Target) (skill.Store, error) { return skill.Open(getenv, t) }),
			config.Command(config.Deps{
				Deps: command.Deps{
					Getenv:   getenv,
					Environ:  os.Environ,
					GOOS:     runtime.GOOS,
					StateDir: func() (string, error) { return statedir.Path(runtime.GOOS, getenv) },
					Account:  install.Account,
					Machine:  install.MachineID,
					Now:      time.Now,
				},
				Catalog: func() ([]config.Entry, error) { return config.LoadCatalog(getenv) },
			}),
			status.Command(status.Deps{
				Deps: command.Deps{
					Getenv:   getenv,
					GOOS:     runtime.GOOS,
					StateDir: func() (string, error) { return statedir.Path(runtime.GOOS, getenv) },
					Username: func() (string, error) { return fleet.DefaultUsername(runtime.GOOS) },
					Now:      time.Now,
				},
				SoftwareCatalog: func() ([]source.Entry, *registry.Registry, error) { return software.LoadCatalog(getenv) },
				ConfigCatalog:   func() ([]config.Entry, error) { return config.LoadCatalog(getenv) },
			}),
			fleet.Command(fleet.Deps{
				Deps: command.Deps{
					Getenv:   getenv,
					GOOS:     runtime.GOOS,
					StateDir: func() (string, error) { return statedir.Path(runtime.GOOS, getenv) },
					Username: func() (string, error) { return fleet.DefaultUsername(runtime.GOOS) },
					Now:      time.Now,
					Client:   nil,
				},
			}),
			daemon.Command(daemon.Deps{
				GOOS: runtime.GOOS,
				Caller: func() (daemon.Caller, error) {
					return currentCaller(os.Environ())
				},
				Root: os.UserHomeDir,
				Activate: func(path string) error {
					return daemon.ActivateUserService(runtime.GOOS, path)
				},
				Deactivate: func(path string) error {
					return daemon.DeactivateUserService(runtime.GOOS, path)
				},
			}),
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Detect: target.Detect,
		DaemonUpdate: func() (bool, int) {
			caller, err := currentCaller(os.Environ())
			if err != nil {
				return false, 0
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return false, 0
			}
			base := filepath.Join(home, ".config", "agent-machine")
			endpoint := filepath.Join(base, "daemon.sock")
			if runtime.GOOS == "windows" {
				endpoint = `\\.\pipe\agent-machine`
			}
			handled, res, err := daemon.TryUpdateForCaller(caller, endpoint, filepath.Join(base, "client.key"), "")
			if !handled {
				return false, 0
			}
			if err != nil || !res.OK {
				fmt.Fprintln(os.Stderr, "daemon update failed")
				return true, 1
			}
			fmt.Fprintln(os.Stdout, res.Message)
			return true, 0
		},
	}
	os.Exit(app.Run(os.Args[1:]))
}

func currentCaller(environ []string) (daemon.Caller, error) {
	u, err := user.Current()
	if err != nil {
		return daemon.Caller{}, err
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		uid = -1
	}
	env := make(map[string]string, len(environ))
	for _, item := range environ {
		key, val, ok := strings.Cut(item, "=")
		if ok {
			env[key] = val
		}
	}
	home := env["HOME"]
	if home == "" {
		home = env["USERPROFILE"]
	}
	if home == "" {
		home = u.HomeDir
	}
	return daemon.Caller{
		Username:       u.Username,
		UID:            uid,
		Home:           home,
		RegisteredHome: u.HomeDir,
		Env:            env,
	}, nil
}

// providers is the registry of installation providers with an executor;
// a recipe naming any other provider is refused before anything is
// written. The second argument is the host's canonical target, not the
// installation target: the provider learns the installation target from
// the request it plans.
func providers(name, target string) provider.Provider {
	switch name {
	case brew.Name:
		return &brew.Provider{Prefix: brewPrefix(target), Run: brew.Exec, Environ: os.Environ}
	case uv.Name:
		// The uv provider looks its tool up on the PATH and reads the
		// index over https with the default client.
		return &uv.Provider{Run: uv.Exec, Environ: os.Environ, LookPath: exec.LookPath}
	case winget.Name:
		// The winget executable is looked up in the PATH of the
		// injected environment; the machine scope is honored only in
		// a process that is already elevated.
		return &winget.Provider{Run: winget.Exec, Environ: os.Environ, LookPath: exec.LookPath}
	case npm.Name:
		return &npm.Provider{GOOS: runtime.GOOS, Run: npm.Exec, Environ: os.Environ, LookPath: exec.LookPath}
	case golang.Name:
		return &golang.Provider{GOOS: goos(target), Run: golang.Exec, Environ: os.Environ,
			LookPath: golang.LookPath, Client: golang.DefaultClient()}
	case cargo.Name:
		return &cargo.Provider{Run: cargo.Exec, Environ: os.Environ, LookPath: exec.LookPath,
			GOOS: runtime.GOOS, Client: cargo.DefaultClient()}
	case appcopy.Name:
		return &appcopy.Provider{Run: appcopy.Exec, Environ: os.Environ, Client: appcopy.DefaultClient()}
	case archive.Name:
		return &archive.Provider{GOOS: goos(target), Environ: os.Environ, Client: archive.DefaultClient()}
	}
	return nil
}

// goos is the operating system of the canonical target: the provider's
// paths follow the target, not the running binary.
func goos(platform string) string {
	if i := strings.Index(platform, "/"); i > 0 {
		return platform[:i]
	}
	return platform
}
