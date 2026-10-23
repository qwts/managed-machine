package config

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/source/remote"
)

// Deps are the command's seams. Discovery reads only the catalog; the
// executor seams are consulted only when an action (--status, --check,
// --plan, --apply) runs.
type Deps struct {
	// Catalog returns the configuration definitions of every source.
	Catalog func() ([]Entry, error)
	// Getenv reads the environment for locating the desired
	// configuration, the state directory and PATH. When nil, no
	// environment variable is set.
	Getenv func(string) string
	// Environ returns the environment entries for command lookup. When
	// nil, PATH and PATHEXT are read from Getenv.
	Environ func() []string
	// GOOS is the operating system for locating the desired
	// configuration file. Empty means runtime.GOOS.
	GOOS string
	// StateDir locates the platform state directory.
	StateDir func() (string, error)
	// Account returns the current OS account name.
	Account func() (string, error)
	// Machine returns the stable machine identifier.
	Machine func(ctx context.Context) (string, error)
	// Now is the clock for plan ids, journals and ledger records.
	Now func() time.Time
	// NewPlanID builds a plan id for the creation time. When nil,
	// plan.NewID is used.
	NewPlanID func(time.Time) (string, error)
	// Provider resolves a configuration operation provider by name. When
	// nil, DefaultProvider is used.
	Provider func(name string) Provider
	// Writable reports whether a destination or directory path is
	// writable by the current user. When nil, the OS check is used.
	Writable func(path string) bool
	// Secrets resolves a secret:<name> input value (C05/C06 seam).
	Secrets func(ctx context.Context, name string) (string, error)
	// Compose applies flavor overlays to a definition (C07 seam).
	Compose func(def Definition, d Desired) (Definition, error)
}

type flags struct {
	name, category, custom, fromPlan      string
	available, status, check, plan, apply bool
	global, user, hasCustom               bool
}

// Command is `managed-machine config`: discovery of supported configuration
// definitions, and planning, applying, checking and reporting state through
// the shared plan executor and ownership ledger.
func Command(d Deps) cli.Command {
	return cli.Command{
		Name:    "config",
		Summary: "Discover and apply supported configuration",
		Usage:   usage,
		Help:    func(ctx cli.Context, args []string) string { return help(d, ctx, args) },
		Run:     func(ctx cli.Context, args []string) cli.Result { return run(d, ctx, args) },
	}
}

const usage = `Usage: managed-machine config [name] [options]

Discover and apply supported configuration.
Default output is compact and agent-friendly.
`

const options = `
Options
-h, --help          Show usage and a readable configuration table
--available         List supported configuration definitions (default)
--category <name>   Filter by category
--global            Filter or configure the whole machine
--user              Filter or configure the current user
--custom <path>     Filter or configure within a custom root
--status            Report the applied state of a configuration
--check             Compare the current state with the desired state
--plan              Plan the configuration
--apply             Apply the configuration
--from-plan <id>    Apply the named saved plan
--json              Return results as one JSON document
--human             Render results in a readable form
`

const notes = `
Notes
Filters combine; a configuration can have multiple categories.
Discovery never changes the machine: it never reads the desired
configuration, never opens the plan store and never writes a file.
Applying saves a plan first, then applies that plan under the ownership
ledger; it never replaces an unowned file and never elevates privileges.
`

func usageErr(format string, args ...any) *diag.Diagnostic {
	d := diag.New(diag.Usage, fmt.Sprintf(format, args...))
	return &d
}

func parse(args []string) (flags, *diag.Diagnostic) {
	var f flags
	value := func(i *int, name string) (string, *diag.Diagnostic) {
		if *i+1 >= len(args) || strings.HasPrefix(args[*i+1], "-") {
			return "", usageErr("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		var d *diag.Diagnostic
		switch a := args[i]; a {
		case "--category":
			f.category, d = value(&i, a)
		case "--custom":
			f.custom, d = value(&i, a)
			f.hasCustom = true
		case "--from-plan":
			f.fromPlan, d = value(&i, a)
		case "--available":
			f.available = true
		case "--status":
			f.status = true
		case "--check":
			f.check = true
		case "--plan":
			f.plan = true
		case "--apply":
			f.apply = true
		case "--global":
			f.global = true
		case "--user":
			f.user = true
		default:
			if strings.HasPrefix(a, "-") {
				return f, usageErr("unknown option %q", a)
			}
			if f.name != "" {
				return f, usageErr("at most one configuration name; got %q and %q", f.name, a)
			}
			f.name = a
		}
		if d != nil {
			return f, d
		}
	}
	targets := 0
	for _, b := range []bool{f.global, f.user, f.hasCustom} {
		if b {
			targets++
		}
	}
	actions := 0
	for _, b := range []bool{f.status, f.check, f.plan, f.apply} {
		if b {
			actions++
		}
	}
	switch {
	case targets > 1:
		return f, usageErr("--global, --user and --custom are mutually exclusive")
	case actions > 1:
		return f, usageErr("--status, --check, --plan and --apply are mutually exclusive")
	case f.available && actions > 0:
		return f, usageErr("--available and %s are mutually exclusive", f.action())
	case f.fromPlan != "" && !f.apply:
		return f, usageErr("--from-plan is used with --apply")
	case actions > 0 && f.name == "":
		return f, usageErr("%s requires a configuration name", f.action())
	case actions > 0 && targets != 1:
		return f, usageErr("%s requires exactly one target: --global, --user or --custom <path>", f.action())
	}
	return f, nil
}

// target returns the installation target the flags name, or the empty
// string when they name none.
func (f flags) target() string {
	switch {
	case f.global:
		return TargetGlobal
	case f.user:
		return TargetUser
	case f.hasCustom:
		return TargetCustom
	}
	return ""
}

// action returns the action the flags name, or the empty string when they
// name none.
func (f flags) action() string {
	switch {
	case f.status:
		return "--status"
	case f.check:
		return "--check"
	case f.plan:
		return "--plan"
	case f.apply:
		return "--apply"
	}
	return ""
}

func run(d Deps, ctx cli.Context, args []string) cli.Result {
	f, bad := parse(args)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	switch {
	case f.plan:
		return planRun(d, ctx, f)
	case f.apply:
		return applyRun(d, ctx, f)
	case f.check:
		return checkRun(d, ctx, f)
	case f.status:
		return statusRun(d, ctx, f)
	}
	recs, dg := list(d, f)
	return cli.Result{Records: recs, Diagnostic: dg}
}

// list returns one record per matching configuration definition. Every
// filter intersects.
func list(d Deps, f flags) ([]*output.Record, *diag.Diagnostic) {
	entries, err := d.Catalog()
	if err != nil {
		dg := diag.New(Diagnostic(err), catalogMessage(err))
		return nil, &dg
	}
	if f.category != "" && !anyCategory(entries, f.category) {
		dg := diag.New(diag.Usage, fmt.Sprintf("no configuration carries category %q", f.category))
		return nil, &dg
	}
	if f.name != "" && !anyName(entries, f.name) {
		dg := diag.New(diag.ConfigUnknownName, fmt.Sprintf("%q is not in the catalog", f.name))
		return nil, &dg
	}
	var recs []*output.Record
	for _, e := range entries {
		def := e.Definition
		if f.name != "" && def.ID != f.name ||
			f.category != "" && !contains(def.Categories, f.category) ||
			f.target() != "" && !contains(def.Targets, f.target()) {
			continue
		}
		recs = append(recs, output.New().Set("id", def.ID).
			Set("categories", def.Categories).Set("targets", def.Targets))
	}
	return recs, nil
}

// catalogMessage names the reason a source could not be read. A
// configured remote source that was never refreshed says so, because
// fetching stays an explicit action.
func catalogMessage(err error) string {
	if errors.Is(err, remote.ErrNotCached) {
		return err.Error() + `; run "managed-machine software --refresh" to fetch it`
	}
	return err.Error()
}

// Diagnostic returns the diagnostic identifier an error of this package
// belongs to. A desired configuration that cannot be used is
// MM_CONFIG_DESIRED_INVALID, a definition that does not declare the
// requested target or platform is MM_CONFIG_UNSUPPORTED_COMBINATION, and
// anything else is a source that failed validation.
func Diagnostic(err error) string {
	var de *DesiredError
	if errors.As(err, &de) {
		return diag.ConfigDesiredInvalid
	}
	var ue *UnsupportedError
	if errors.As(err, &ue) {
		return diag.ConfigUnsupportedCombo
	}
	return diag.ConfigSourceInvalid
}

func help(d Deps, ctx cli.Context, args []string) string {
	var b strings.Builder
	b.WriteString(usage)
	f, bad := parse(args)
	if bad == nil {
		f.status, f.check, f.plan, f.apply = false, false, false, false
		recs, dg := list(d, f)
		if dg == nil {
			b.WriteString("\nAvailable configuration\n")
			rows := [][]string{{"ID", "CATEGORIES", "TARGETS"}}
			for _, r := range recs {
				id, _ := r.Get("id")
				cats, _ := r.Get("categories")
				targets, _ := r.Get("targets")
				rows = append(rows, []string{id.(string), strings.Join(cats.([]string), ", "), strings.Join(targets.([]string), ", ")})
			}
			b.WriteString(table(rows))
		} else {
			fmt.Fprintf(&b, "\n%s\n", dg.Human())
		}
	}
	b.WriteString(options)
	b.WriteString(notes)
	return b.String()
}

// table renders rows in fixed columns.
func table(rows [][]string) string {
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	var b strings.Builder
	for _, r := range rows {
		var line strings.Builder
		for i, c := range r {
			if i < len(r)-1 {
				fmt.Fprintf(&line, "%-*s  ", widths[i], c)
			} else {
				line.WriteString(c)
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
	}
	return b.String()
}

func anyName(es []Entry, name string) bool {
	for _, e := range es {
		if e.Definition.ID == name {
			return true
		}
	}
	return false
}

func anyCategory(es []Entry, c string) bool {
	for _, e := range es {
		if contains(e.Definition.Categories, c) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
