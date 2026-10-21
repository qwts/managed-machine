// Command managed-machine is the Go core entry point (ADR-0016, ADR-0017).
// It exposes the shared boundary, the read-only platform probe and S1
// software discovery; packaging to end users is an S6 step.
package main

import (
	"os"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/skill"
	"github.com/qwts/managed-machine/core/internal/software"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/target"
)

func main() {
	app := &cli.App{
		Name: "managed-machine",
		Commands: []cli.Command{
			cli.Platform(),
			software.Command(software.Deps{
				Catalog: func() ([]source.Entry, *registry.Registry, error) { return software.LoadCatalog(os.Getenv) },
				// No installer exists before S2; the command never calls this.
				Install: nil,
			}),
			skill.Command(func(t target.Target) (skill.Store, error) { return skill.Open(os.Getenv, t) }),
		},
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Detect: target.Detect,
	}
	os.Exit(app.Run(os.Args[1:]))
}
