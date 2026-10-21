// Command managed-machine is the Go core entry point (ADR-0016, ADR-0017).
// Until the command contracts land it exposes the shared boundary and the
// read-only platform probe; packaging to end users is an S6 step.
package main

import (
	"os"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/target"
)

func main() {
	app := &cli.App{
		Name:     "managed-machine",
		Commands: []cli.Command{cli.Platform()},
		Stdout:   os.Stdout,
		Stderr:   os.Stderr,
		Detect:   target.Detect,
	}
	os.Exit(app.Run(os.Args[1:]))
}
