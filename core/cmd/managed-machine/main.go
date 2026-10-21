// Command managed-machine is the Go core entry point (ADR-0016, ADR-0017).
// It exposes the shared boundary, the read-only platform probe and S1
// software discovery and S2 installation; packaging to end users is an S6 step.
package main

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/install"
	"github.com/qwts/managed-machine/core/internal/provider/brew"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/skill"
	"github.com/qwts/managed-machine/core/internal/software"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

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
						Brew: func(t string) *brew.Provider {
							return &brew.Provider{Prefix: brew.DefaultPrefix(t), Run: brew.Exec, Environ: os.Environ}
						},
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
