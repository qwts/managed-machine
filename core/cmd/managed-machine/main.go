// Command managed-machine is the Go core entry point (ADR-0016, ADR-0017).
// It exposes the shared boundary, the read-only platform probe and S1
// software discovery and S2 installation; packaging to end users is an S6 step.
package main

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/install"
	"github.com/qwts/managed-machine/core/internal/provider"
	"github.com/qwts/managed-machine/core/internal/provider/brew"
	"github.com/qwts/managed-machine/core/internal/provider/uv"
	"github.com/qwts/managed-machine/core/internal/provider/winget"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/skill"
	"github.com/qwts/managed-machine/core/internal/software"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

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
	app := &cli.App{
		Name: "managed-machine",
		Commands: []cli.Command{
			cli.Platform(),
			software.Command(software.Deps{
				Catalog: func() ([]source.Entry, *registry.Registry, error) { return software.LoadCatalog(os.Getenv) },
				Install: func(ctx context.Context, r install.Request) (install.Result, error) {
					return install.Run(ctx, install.Env{
						StateDir: func() (string, error) { return statedir.Path(runtime.GOOS, os.Getenv) },
						Account:  install.Account,
						Machine:  install.MachineID,
						Now:      time.Now,
						Provider: providers,
					}, r)
				},
				// The environment names the configured remote sources; the
				// default client fetches them with TLS verification on.
				Getenv: os.Getenv,
				Client: nil,
			}),
			skill.Command(func(t target.Target) (skill.Store, error) { return skill.Open(os.Getenv, t) }),
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Detect: target.Detect,
	}
	os.Exit(app.Run(os.Args[1:]))
}

// providers is the registry of installation providers with an executor;
// a recipe naming any other provider is refused before anything is
// written.
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
	}
	return nil
}
