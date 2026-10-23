package cli

import (
	"errors"
	"fmt"
	"runtime"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/target"
)

// Version reports the release version, the host's canonical target identifier
// and the Go toolchain version as one record (S6 D2).
//
// Record keys, in order: version, target, toolchain.
func Version(version string) Command {
	if version == "" {
		version = DefaultVersion
	}
	return Command{
		Name:    "version",
		Summary: "Report the release version, target and Go toolchain",
		Usage: "Usage: managed-machine version [--json|--human]\n\n" +
			"Report the release version, the host's canonical platform identifier and\n" +
			"the Go toolchain version as one record: version, target, toolchain.\n\n" +
			"Options\n" +
			"-h, --help    Show this usage and perform no probe\n" +
			"--json        Return the record as one JSON document\n" +
			"--human       Render the record in a readable form\n",
		Run: func(ctx Context, args []string) Result {
			return runVersion(version, ctx, args)
		},
	}
}

func runVersion(version string, ctx Context, args []string) Result {
	if len(args) > 0 {
		d := diag.New(diag.Usage, fmt.Sprintf("version takes no arguments; got %q", args[0]))
		return Result{Diagnostic: &d}
	}
	host, err := ctx.Detect()
	if err != nil {
		var unsupported *target.UnsupportedError
		id := diag.PlatformProbeFailed
		if errors.As(err, &unsupported) {
			id = diag.PlatformUnsupported
		}
		d := diag.New(id, err.Error())
		return Result{Diagnostic: &d}
	}
	rec := output.New().
		Set("version", version).
		Set("target", string(host.Target)).
		Set("toolchain", runtime.Version())
	return Result{Records: []*output.Record{rec}}
}
