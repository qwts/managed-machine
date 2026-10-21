// Package cli is the shared command boundary of ADR-0016 F03: it parses the
// presentation flags every command shares, gives -h/--help precedence over
// every action, writes records to stdout in the selected mode and
// diagnostics to stderr, and maps results to the accepted exit codes. It
// knows nothing about installers or observers; commands are plugged in.
package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/target"
)

// Result is what a command hands back. Records are written to stdout in the
// selected mode even when a Diagnostic is set, so a partial report (exit 4)
// or findings (exit 3) stay visible. Exit is taken from the diagnostic's
// registry entry when one is set, otherwise it is 0.
type Result struct {
	Records    []*output.Record
	Diagnostic *diag.Diagnostic
	// Human, when set, replaces the table rendering of Records under
	// --human, for results whose fields do not fit a table (for example
	// multi-line guidance). It never changes what was computed.
	Human string
}

// Context is what a command may consult. Detect supplies host facts so tests
// inject every target without a system call.
type Context struct {
	Mode   output.Mode
	Detect func() (target.Host, error)
}

// Command is one action of the boundary.
type Command struct {
	Name    string
	Summary string
	// Usage is the command's readable help, printed for "<name> -h".
	Usage string
	// Help, when set, renders the command's help from its arguments (for
	// example a filtered table) instead of the static Usage. It must not
	// change the machine.
	Help func(ctx Context, args []string) string
	Run  func(ctx Context, args []string) Result
}

// App is the configured boundary. Stdout receives records and help only;
// Stderr receives diagnostics only.
type App struct {
	Name     string
	Commands []Command
	Stdout   io.Writer
	Stderr   io.Writer
	Detect   func() (target.Host, error)
}

// Run executes one invocation and returns the process exit code. It never
// calls os.Exit so tests can drive it.
func (a *App) Run(args []string) int {
	var (
		jsonFlag, humanFlag, help bool
		name, unknown             string
		rest                      []string
	)
	// The presentation flags and -h are shared by every command and are
	// recognized wherever they appear; everything else after the command name
	// belongs to the command. An unknown option before the command is
	// remembered, not returned, so help still wins and the diagnostic is
	// rendered in the requested presentation.
	for _, arg := range args {
		switch {
		case arg == "-h" || arg == "--help":
			help = true
		case arg == "--json":
			jsonFlag = true
		case arg == "--human":
			humanFlag = true
		case name != "":
			rest = append(rest, arg)
		case strings.HasPrefix(arg, "-"):
			if unknown == "" {
				unknown = arg
			}
		default:
			name = arg
		}
	}
	// -h takes precedence over every action and presentation flag: nothing
	// below this point runs when help was requested, so help never changes
	// the machine, and conflicting presentation flags do not matter.
	if help {
		if name == "" {
			a.writeHelp()
			return diag.ExitOK
		}
		cmd := a.find(name)
		if cmd == nil {
			a.writeHelp()
			return diag.ExitOK
		}
		if cmd.Help != nil {
			io.WriteString(a.Stdout, cmd.Help(Context{Mode: output.Human, Detect: a.Detect}, rest))
			return diag.ExitOK
		}
		io.WriteString(a.Stdout, cmd.Usage)
		return diag.ExitOK
	}
	mode, err := output.SelectMode(jsonFlag, humanFlag)
	if err != nil {
		return a.usage(output.Compact, err.Error())
	}
	if unknown != "" {
		return a.usage(mode, fmt.Sprintf("unknown option %q", unknown))
	}
	if name == "" {
		return a.usage(mode, "a command is required; see -h")
	}
	cmd := a.find(name)
	if cmd == nil {
		return a.usage(mode, fmt.Sprintf("unknown command %q; see -h", name))
	}
	result := cmd.Run(Context{Mode: mode, Detect: a.Detect}, rest)
	var werr error
	if mode == output.Human && result.Human != "" {
		_, werr = io.WriteString(a.Stdout, result.Human)
	} else {
		werr = output.Write(a.Stdout, mode, result.Records)
	}
	if err := werr; err != nil {
		// A broken stdout or a serialization failure is an output failure of
		// the boundary, whatever command produced the records.
		d := diag.New(diag.OutputWriteFailed, "writing records: "+err.Error())
		a.writeDiagnostic(mode, d)
		return d.ExitCode()
	}
	if result.Diagnostic == nil {
		return diag.ExitOK
	}
	a.writeDiagnostic(mode, *result.Diagnostic)
	return result.Diagnostic.ExitCode()
}

func (a *App) find(name string) *Command {
	for i := range a.Commands {
		if a.Commands[i].Name == name {
			return &a.Commands[i]
		}
	}
	return nil
}

func (a *App) usage(mode output.Mode, message string) int {
	d := diag.New(diag.Usage, message)
	a.writeDiagnostic(mode, d)
	return d.ExitCode()
}

// writeDiagnostic emits one stderr line: JSON in compact and --json modes, the
// "MM_<ID>: <message>" form in --human mode.
func (a *App) writeDiagnostic(mode output.Mode, d diag.Diagnostic) {
	if mode == output.Human {
		io.WriteString(a.Stderr, d.Human())
		return
	}
	_ = d.Write(a.Stderr)
}

func (a *App) writeHelp() {
	var b strings.Builder
	fmt.Fprintf(&b, "Usage: %s <command> [options]\n\n", a.Name)
	b.WriteString("Portable command boundary. Default output is compact JSON Lines on stdout;\n")
	b.WriteString("diagnostics are one JSON object per line on stderr.\n\n")
	b.WriteString("Commands\n")
	width := 0
	for _, c := range a.Commands {
		if len(c.Name) > width {
			width = len(c.Name)
		}
	}
	for _, c := range a.Commands {
		fmt.Fprintf(&b, "%-*s    %s\n", width, c.Name, c.Summary)
	}
	b.WriteString("\nOptions\n")
	b.WriteString("-h, --help    Show usage; takes precedence over every action and never changes the machine\n")
	b.WriteString("--json        Return results as one JSON document\n")
	b.WriteString("--human       Render results in a readable form\n")
	b.WriteString("\nExit codes: 0 success, 1 runtime failure, 2 usage or validation error,\n")
	b.WriteString("3 completed evaluation with findings, 4 incomplete evaluation.\n")
	io.WriteString(a.Stdout, b.String())
}
