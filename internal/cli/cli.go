// Package cli is the shared command boundary of ADR-0016 F03: it parses the
// presentation flags every command shares, gives -h/--help precedence over
// every action, writes records to stdout in the selected mode and
// diagnostics to stderr, and maps results to the accepted exit codes. It
// knows nothing about installers or observers; commands are plugged in.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/target"
)

// Environment variable names consulted by the darwin legacy exec seam and
// the darwin policy/source directory bridge (S8 D3).
const (
	LegacyRootEnv     = "MANAGED_MACHINE_LEGACY_ROOT"
	PolicyDirEnv      = "MANAGED_MACHINE_POLICY_DIR"
	SourceDirEnv      = "MANAGED_MACHINE_SOURCE_DIR"
	ConfigRepoRootEnv = "CONFIG_REPO_ROOT"
	XDGDataHomeEnv    = "XDG_DATA_HOME"
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
	Mode       output.Mode
	Detect     func() (target.Host, error)
	Getenv     func(string) string
	Executable func() (string, error)
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

// DefaultVersion is the version reported by an unflagged build (S6 D2).
const DefaultVersion = "0.0.0-dev"

// App is the configured boundary. Stdout receives records and help only;
// Stderr receives diagnostics only.
type App struct {
	Name       string
	Version    string
	GOOS       string
	Commands   []Command
	Stdout     io.Writer
	Stderr     io.Writer
	Detect     func() (target.Host, error)
	Getenv     func(string) string
	Environ    func() []string
	Executable func() (string, error)
	// Exec replaces the current process with argv0 (receiving argv, with
	// argv[0] == argv0, and envv). When nil, syscall.Exec is used.
	Exec func(argv0 string, argv []string, envv []string) error
	// DaemonUpdate, when set, may handle --update by talking to the
	// per-user daemon. Returning false leaves the legacy route unchanged.
	DaemonUpdate func() (handled bool, code int)
	// BoundaryHelp, when set, renders the minimal command-table help and
	// requires a command name for empty argv (used by boundary unit tests).
	BoundaryHelp bool
}

// DarwinCheckoutDir returns the candidate managed-machine-config checkout
// directory for the D3(c) darwin policy/source directory bridge:
// $CONFIG_REPO_ROOT when set, else
// ${XDG_DATA_HOME:-$HOME/.local/share}/managed-machine/managed-machine-config.
// It returns "" when none of CONFIG_REPO_ROOT, XDG_DATA_HOME, or HOME is set.
func DarwinCheckoutDir(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	if cfg := strings.TrimSpace(getenv(ConfigRepoRootEnv)); cfg != "" {
		return filepath.Clean(cfg)
	}
	if xdg := strings.TrimSpace(getenv(XDGDataHomeEnv)); xdg != "" {
		return filepath.Join(filepath.Clean(xdg), "managed-machine", "managed-machine-config")
	}
	if home := strings.TrimSpace(getenv("HOME")); home != "" {
		return filepath.Join(filepath.Clean(home), ".local", "share", "managed-machine", "managed-machine-config")
	}
	return ""
}

// BridgeDarwinEnv wraps getenv with the S8 D3(c) darwin policy/source
// directory bridge. On darwin only, when MANAGED_MACHINE_POLICY_DIR is unset
// and the candidate checkout directory exists on disk,
// MANAGED_MACHINE_POLICY_DIR resolves to that directory; when
// MANAGED_MACHINE_SOURCE_DIR is unset and <checkout>/software exists on disk,
// MANAGED_MACHINE_SOURCE_DIR resolves to <checkout>/software. On non-darwin
// targets getenv is returned unchanged. No directory is ever created.
func BridgeDarwinEnv(goos string, getenv func(string) string) func(string) string {
	if getenv == nil {
		return func(string) string { return "" }
	}
	if i := strings.IndexByte(goos, '/'); i > 0 {
		goos = goos[:i]
	}
	if goos != "darwin" {
		return getenv
	}
	return func(k string) string {
		if v := getenv(k); v != "" {
			return v
		}
		switch k {
		case PolicyDirEnv:
			if dir := DarwinCheckoutDir(getenv); dir != "" && existingDir(dir) {
				return dir
			}
		case SourceDirEnv:
			if dir := DarwinCheckoutDir(getenv); dir != "" {
				soft := filepath.Join(dir, "software")
				if existingDir(soft) {
					return soft
				}
			}
		}
		return ""
	}
}

func existingDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

func (a *App) goos() string {
	if a.GOOS != "" {
		if i := strings.IndexByte(a.GOOS, '/'); i > 0 {
			return a.GOOS[:i]
		}
		return a.GOOS
	}
	return runtime.GOOS
}

func (a *App) bridgedGetenv() func(string) string {
	base := a.Getenv
	if base == nil {
		return func(string) string { return "" }
	}
	return func(k string) string {
		if v := base(k); v != "" {
			return v
		}
		if k != PolicyDirEnv && k != SourceDirEnv {
			return ""
		}
		return BridgeDarwinEnv(a.goos(), base)(k)
	}
}

func (a *App) version() string {
	if a.Version == "" {
		return DefaultVersion
	}
	return a.Version
}

func hasTopLevelVersion(args []string) bool {
	for _, arg := range args {
		if arg == "--version" {
			return true
		}
		if !strings.HasPrefix(arg, "-") {
			// Everything after the command name belongs to the command
			// (for example `skill --version <ver>`).
			return false
		}
	}
	return false
}

// Run executes one invocation and returns the process exit code. It never
// calls os.Exit so tests can drive it.
func (a *App) Run(args []string) int {
	// S6 D2: managed-machine --version (the single argument, handled before
	// option parsing) prints <version> and a newline on stdout and exits 0
	// without running any probe; --version with any other argument is MM_USAGE.
	if hasTopLevelVersion(args) {
		if len(args) == 1 {
			if _, err := io.WriteString(a.Stdout, a.version()+"\n"); err != nil {
				d := diag.New(diag.OutputWriteFailed, "writing version: "+err.Error())
				a.writeDiagnostic(output.Compact, d)
				return d.ExitCode()
			}
			return diag.ExitOK
		}
		return a.usage(output.Compact, "--version takes no other arguments; use 'managed-machine version' for structured output")
	}

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
	getenv := a.bridgedGetenv()
	if !a.BoundaryHelp && (len(args) == 0 || (name == "help" && len(rest) == 0 && unknown == "" && !jsonFlag && !humanFlag)) {
		a.writeHelp()
		return diag.ExitOK
	}
	if help {
		if name == "" || name == "setup" || name == "help" {
			a.writeHelp()
			return diag.ExitOK
		}
		cmd := a.find(name)
		if cmd == nil {
			if r, ok := legacy.Match(args); ok && r.RemovedIn == "" && r.Command != "skill" && a.goos() == "darwin" {
				if legacyRoot := a.legacyRoot(getenv); legacyRoot != "" {
					return a.execLegacy(output.Compact, legacyRoot, args)
				}
			}
			a.writeHelp()
			return diag.ExitOK
		}
		if cmd.Help != nil {
			io.WriteString(a.Stdout, cmd.Help(Context{Mode: output.Human, Detect: a.Detect, Getenv: getenv, Executable: a.Executable}, rest))
			return diag.ExitOK
		}
		io.WriteString(a.Stdout, cmd.Usage)
		return diag.ExitOK
	}
	mode, err := output.SelectMode(jsonFlag, humanFlag)
	if err != nil {
		return a.usage(output.Compact, err.Error())
	}
	var cutover *legacy.SetupRoute
	if sr, ok := legacy.MatchSetupCutover(args); ok && legacy.HasSetupRecipe(getenv, sr.Software) {
		name = "software"
		rest = []string{sr.Software, "--install", "--" + sr.Target}
		cutover = &sr
	} else if r, ok := legacy.Match(args); ok {
		if r.Command == "--update" && a.DaemonUpdate != nil {
			if handled, code := a.DaemonUpdate(); handled {
				return code
			}
		}
		if r.RemovedIn == "" && r.Command != "skill" && a.goos() == "darwin" {
			if legacyRoot := a.legacyRoot(getenv); legacyRoot != "" {
				return a.execLegacy(mode, legacyRoot, args)
			}
		}
		d := legacy.Diagnostic(r)
		a.writeDiagnostic(mode, d)
		return d.ExitCode()
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
	result := cmd.Run(Context{Mode: mode, Detect: a.Detect, Getenv: getenv, Executable: a.Executable}, rest)
	if cutover != nil && result.Diagnostic != nil && result.Diagnostic.ID == diag.SoftwarePrivilegeRequired && a.goos() == "darwin" && mode != output.JSON {
		// The core never elevates, and on the supported macOS layout the
		// Homebrew prefix belongs to an admin-group owner, not the invoking
		// user. The legacy setup route owns the administrator authorization
		// dialog for exactly this case, so hand it the install before
		// anything is written: the refused plan left no record and no state.
		// Legacy cmd_setup installs catalog software only as the bare
		// `setup <name>`: it treats every other token, --json and --human
		// included, as setup arguments and refuses them, so the delegated
		// argv is rebuilt from the canonical route name and the core's
		// presentation flags are dropped. In --json mode no legacy output
		// could honour the requested structure, so the diagnostic stands and
		// names the `setup <name>` route to run instead.
		if legacyRoot := a.legacyRoot(getenv); legacyRoot != "" {
			return a.execLegacy(mode, legacyRoot, []string{"setup", cutover.Name})
		}
	}
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

// ResolveLegacyRoot resolves the legacy tree root on darwin per S8 D3(c):
// MANAGED_MACHINE_LEGACY_ROOT when set, else
// <directory of symlink-resolved executable>/../libexec/legacy when that
// directory (or a sibling ../libexec in a brew-style prefix) exists on disk,
// else <directory of symlink-resolved executable>/../legacy.
func ResolveLegacyRoot(getenv func(string) string, executable func() (string, error)) string {
	if getenv != nil {
		if v := strings.TrimSpace(getenv(LegacyRootEnv)); v != "" {
			return filepath.Clean(v)
		}
	}
	exeFn := executable
	if exeFn == nil {
		exeFn = os.Executable
	}
	exePath, err := exeFn()
	if err != nil || strings.TrimSpace(exePath) == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exePath); err == nil {
		exePath = resolved
	}
	exeDir := filepath.Dir(BundleHostExecutable(exePath))
	libexecDir := filepath.Clean(filepath.Join(exeDir, "..", "libexec"))
	libexecLegacy := filepath.Join(libexecDir, "legacy")
	if existingDir(libexecLegacy) || existingDir(libexecDir) {
		return libexecLegacy
	}
	return filepath.Clean(filepath.Join(exeDir, "..", "legacy"))
}

// BundleHostExecutable maps the main executable of an app bundle,
// <prefix>/<Name>.app/Contents/MacOS/<exe>, to <prefix>/bin/<exe>, the path
// the binary would have outside the bundle, so libexec resolves to
// <prefix>/libexec either way. Other paths are returned unchanged.
func BundleHostExecutable(exePath string) string {
	macOSDir := filepath.Dir(exePath)
	contentsDir := filepath.Dir(macOSDir)
	appDir := filepath.Dir(contentsDir)
	if filepath.Base(macOSDir) != "MacOS" || filepath.Base(contentsDir) != "Contents" || filepath.Ext(appDir) != ".app" {
		return exePath
	}
	return filepath.Join(filepath.Dir(appDir), "bin", filepath.Base(exePath))
}

func (a *App) legacyRoot(getenv func(string) string) string {
	if getenv != nil {
		if v := strings.TrimSpace(getenv(LegacyRootEnv)); v != "" {
			return filepath.Clean(v)
		}
	}
	if a.Executable != nil {
		return ResolveLegacyRoot(getenv, a.Executable)
	}
	return ""
}

func (a *App) execLegacy(mode output.Mode, legacyRoot string, args []string) int {
	entry := filepath.Join(filepath.Clean(legacyRoot), "bin", "managed-machine")
	info, err := os.Stat(entry)
	// The host filesystem, not the emulated target, decides whether permission
	// bits exist: Windows files carry no execute bit.
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode()&0o111 == 0) {
		d := diag.New(
			diag.LegacyTreeMissing,
			fmt.Sprintf("the managed-machine legacy tree is missing at %s; run brew reinstall managed-machine", entry),
		)
		a.writeDiagnostic(mode, d)
		return d.ExitCode()
	}
	argv := make([]string, 0, 1+len(args))
	argv = append(argv, entry)
	argv = append(argv, args...)
	envv := a.legacyEnviron()
	execFn := a.Exec
	if execFn == nil {
		execFn = syscall.Exec
	}
	if err := execFn(entry, argv, envv); err != nil {
		d := diag.New(
			diag.LegacyTreeMissing,
			fmt.Sprintf("the managed-machine legacy tree is missing at %s: %v; run brew reinstall managed-machine", entry, err),
		)
		a.writeDiagnostic(mode, d)
		return d.ExitCode()
	}
	return diag.ExitOK
}

func (a *App) legacyEnviron() []string {
	if a.Environ != nil {
		return append([]string(nil), a.Environ()...)
	}
	return os.Environ()
}

func (a *App) find(name string) *Command {
	for i := range a.Commands {
		if a.Commands[i].Name == name {
			return &a.Commands[i]
		}
	}
	if name == "version" {
		c := Version(a.version())
		return &c
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

func legacyRouteHelpLine(r legacy.Route) string {
	switch r.Name {
	case "setup":
		return "  [legacy] setup <name> [args] | setup --list [--json]"
	case "catalog":
		return "  [legacy] catalog list|resolve [--json] [--no-refresh]"
	case "adopt":
		return "  [legacy] adopt [<name>]"
	case "add-agent":
		return "  [legacy] add-agent <slug>|--all [--full-name <persona>] [--with-harness]"
	case "account":
		return "  [legacy] account setup|doctor <account> [--json]"
	case "ssh":
		return "  [legacy] ssh enroll|status [--authentication|--signing|--fleet|--json]"
	case "--bootstrap":
		return "  [legacy] --bootstrap [--interactive|--non-interactive]"
	case "--update":
		return "  [legacy] --update"
	case "fleet remove":
		return "  [legacy] fleet remove <machine-id> [--yes] [--revoke-github] [--dry-run] [--json]"
	default:
		return "  [legacy] " + r.Name
	}
}

func (a *App) writeHelp() {
	if a.BoundaryHelp {
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
		return
	}

	var b strings.Builder
	b.WriteString("managed-machine - machine setup and orchestration\n\n")
	b.WriteString("Portable core commands (all five targets):\n")
	b.WriteString("  managed-machine software [name] [--available|--install|--refresh] [--category <name>] [--provider <name>|--recipe <id>] [--user|--global|--custom <path>] [--json|--human]\n")
	b.WriteString("  managed-machine config [name] [--available|--status|--check|--plan|--apply] [--from-plan <id>] [--category <name>] [--global|--user|--custom <path>] [--json|--human]\n")
	b.WriteString("  managed-machine status [--section software|config|flavor|fleet [name]] [--global|--user|--custom <path>] [--json|--human]\n")
	b.WriteString("  managed-machine fleet [owner] [--keys|--access|--refresh|--plan|--apply|--check|--publish|--unpublish <fp>] [--owner <id>] [--key <path>] [--label <text>] [--from-plan <id|path>] [--user] [--json|--human]\n")
	b.WriteString("  managed-machine skill [name] [--kind <kind>] [--bundle <bundle>] [--search <term>] [--version <v>] [--next <h>] [--json|--human] | skill path [--json|--human]\n")
	b.WriteString("  managed-machine platform [--json|--human]\n")
	b.WriteString("  managed-machine version [--json|--human]\n")
	b.WriteString("  managed-machine --version    print the release version\n")
	b.WriteString("  managed-machine --help       show this message (also -h, help, or no command)\n\n")
	b.WriteString("Legacy commands (macOS only; Homebrew install):\n")
	statusAdded := false
	for _, r := range legacy.Routes {
		if r.RemovedIn != "" {
			continue
		}
		if r.Section == "" {
			b.WriteString(legacyRouteHelpLine(r) + "\n")
			continue
		}
		if r.Command == "status" && !statusAdded {
			fmt.Fprintf(&b, "  [legacy] status --section %s [--json]\n", strings.Join(legacy.LegacyStatusSections, "|"))
			statusAdded = true
		}
	}
	b.WriteString("\nCompatibility (ADR-0016, ADR-0017):\n")
	b.WriteString("  Legacy routes removed in v0.11.0 (status --section <removed>, fleet list) report\n")
	b.WriteString("  MM_LEGACY_ROUTE_REMOVED with their replacement; surviving legacy routes stay on macOS.\n\n")
	b.WriteString("Run 'managed-machine setup --list' for the setup names and their install state.\n\n")
	b.WriteString("Exit codes: 0 success, 1 runtime failure, 2 usage or validation error,\n")
	b.WriteString("3 completed evaluation with findings, 4 incomplete evaluation.\n")
	io.WriteString(a.Stdout, b.String())
}
