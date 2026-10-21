package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/target"
)

// Platform reports the host's canonical platform identifier. It is the one
// read-only action the boundary exposes until the command contracts (F04
// onward) land, and the observable form of the ADR-0014 mapping: native host
// facts map onto one of the five identifiers without changing their spelling.
//
// Record keys, in order: platform, os, arch, emulated.
func Platform() Command {
	return Command{
		Name:    "platform",
		Summary: "Report the host's canonical platform identifier",
		Usage: "Usage: managed-machine platform [--json|--human]\n\n" +
			"Report the platform this process runs on as one record:\n" +
			"platform (one of " + strings.Join(spellings(), ", ") + "),\n" +
			"os, arch (the native machine architecture, not the binary's) and\n" +
			"emulated (true when the binary runs under architecture emulation).\n\n" +
			"Options\n" +
			"-h, --help    Show this usage and perform no probe\n" +
			"--json        Return the record as one JSON document\n" +
			"--human       Render the record in a readable form\n",
		Run: runPlatform,
	}
}

func spellings() []string {
	out := make([]string, 0, 5)
	for _, t := range target.All() {
		out = append(out, string(t))
	}
	return out
}

func runPlatform(ctx Context, args []string) Result {
	if len(args) > 0 {
		d := diag.New(diag.Usage, fmt.Sprintf("platform takes no arguments; got %q", args[0]))
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
	os, arch := split(host.Target)
	rec := output.New().
		Set("platform", string(host.Target)).
		Set("os", os).
		Set("arch", arch).
		Set("emulated", host.Emulated)
	return Result{Records: []*output.Record{rec}}
}

func split(t target.Target) (string, string) {
	parts := strings.SplitN(string(t), "/", 2)
	return parts[0], parts[1]
}
