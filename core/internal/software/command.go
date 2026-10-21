package software

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/install"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/source/remote"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

// Deps are the command's seams. Install plans, saves and applies one
// selected recipe; discovery and help never call it, and tests count calls.
type Deps struct {
	Catalog func() ([]source.Entry, *registry.Registry, error)
	Install func(ctx context.Context, r install.Request) (install.Result, error)
	// Getenv reads the environment for the remote source settings. It is
	// nil when the command runs without an environment, which means no
	// remote source is configured.
	Getenv func(string) string
	// Client is the HTTP client --refresh fetches with. A nil client
	// means the default one: TLS verification, a timeout and redirects
	// limited to the host the request started with.
	Client *http.Client
}

type flags struct {
	name, provider, recipe, category, custom    string
	available, install, global, user, hasCustom bool
	refresh                                     bool
}

// Command is `managed-machine software`: discovery, and installation through
// the saved-plan executor for the providers that ship in S2 (Homebrew
// formulae).
func Command(d Deps) cli.Command {
	return cli.Command{
		Name:    "software",
		Summary: "Discover and install supported software (Homebrew formulae in S2)",
		Usage:   usage,
		Help:    func(ctx cli.Context, args []string) string { return help(d, ctx, args) },
		Run:     func(ctx cli.Context, args []string) cli.Result { return run(d, ctx, args) },
	}
}

const usage = `Usage: managed-machine software [name] [options]

Discover and install supported software.
Default output is compact and agent-friendly.
`

const options = `
Options
-h, --help          Show usage and a readable software table
--available         List supported software in compact form
--category <name>   Filter by category
--provider <name>   Select or filter by installation mechanism
--recipe <id>       Select a specific recipe
--user              Filter or install for the current user
--global            Filter or install for the whole machine
--install           Install the named software (Homebrew formulae in S2)
--custom <path>     Filter or install within a custom root
--json              Return results as one JSON document
--human             Render results in a readable form
`

// refreshOption is listed only where it applies: the action fetches a
// remote source, so it is offered only when one is configured.
const refreshOption = "--refresh  Fetch the configured remote sources' snapshots\n"

const notes = `
Notes
Filters combine; software can have multiple categories.
Discovery never changes the machine and never refreshes a source.
Installation saves a plan, then applies exactly that plan; it never
switches target or provider and never elevates privileges.
Supported software and targets depend on OS and recipes; catalog
rows are declared support, not tested provider support.
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
		case "--provider":
			f.provider, d = value(&i, a)
		case "--recipe":
			f.recipe, d = value(&i, a)
		case "--category":
			f.category, d = value(&i, a)
		case "--custom":
			f.custom, d = value(&i, a)
			f.hasCustom = true
		case "--available":
			f.available = true
		case "--install":
			f.install = true
		case "--refresh":
			f.refresh = true
		case "--global":
			f.global = true
		case "--user":
			f.user = true
		default:
			if strings.HasPrefix(a, "-") {
				return f, usageErr("unknown option %q", a)
			}
			if f.name != "" {
				return f, usageErr("at most one software name; got %q and %q", f.name, a)
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
	switch {
	case targets > 1:
		return f, usageErr("--global, --user and --custom are mutually exclusive")
	case f.available && f.install:
		return f, usageErr("--available and --install are mutually exclusive")
	case f.refresh && (f.available || f.install || f.name != ""):
		return f, usageErr("--refresh cannot be combined with --available, --install or a software name")
	case f.recipe != "" && f.provider != "":
		return f, usageErr("--recipe and --provider are mutually exclusive")
	case f.install && f.name == "":
		return f, usageErr("--install requires a software name")
	case f.install && targets != 1:
		return f, usageErr("--install requires exactly one target: --global, --user or --custom <path>")
	}
	return f, nil
}

func (f flags) target() string {
	switch {
	case f.global:
		return "global"
	case f.user:
		return "user"
	case f.hasCustom:
		return "custom"
	}
	return ""
}

func run(d Deps, ctx cli.Context, args []string) cli.Result {
	f, bad := parse(args)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	if f.refresh {
		// The refresh is the only action that touches the network.
		return refresh(d)
	}
	if f.install {
		return installRun(d, ctx, f)
	}
	recs, dg := discover(d, ctx, f)
	return cli.Result{Records: recs, Diagnostic: dg}
}

// refresh fetches the configured remote sources into the snapshot cache
// and reports one record per source. Nothing else in the command fetches:
// discovery reads the cache only.
func refresh(d Deps) cli.Result {
	cfg, err := remote.ParseConfig(d.Getenv)
	if err != nil {
		dg := diag.New(diag.SoftwareSourceInvalid, err.Error())
		return cli.Result{Diagnostic: &dg}
	}
	if !cfg.Configured() {
		u := diag.New(diag.Usage, "no remote source is configured")
		return cli.Result{Diagnostic: &u}
	}
	dir, err := statedir.Path(runtime.GOOS, d.Getenv)
	if err != nil {
		dg := diag.New(diag.SoftwareSourceInvalid, err.Error())
		return cli.Result{Diagnostic: &dg}
	}
	results, err := remote.Refresh(context.Background(), cfg, dir, d.Client)
	if err != nil {
		// A transport failure is a failed fetch; a malformed source or a
		// digest that does not match is an invalid source.
		id := diag.SoftwareSourceInvalid
		var fe *remote.FetchError
		if errors.As(err, &fe) {
			id = diag.SoftwareSourceFetchFailed
		}
		dg := diag.New(id, err.Error())
		return cli.Result{Diagnostic: &dg}
	}
	recs := make([]*output.Record, 0, len(results))
	for _, r := range results {
		recs = append(recs, output.New().Set("source", r.Source).Set("revision", r.Revision).
			Set("files", r.Files).Set("outcome", r.Outcome))
	}
	return cli.Result{Records: recs}
}

// discover returns one record per matching recipe. Every filter
// intersects; nothing is selected, so no preference can hide a recipe.
func discover(d Deps, ctx cli.Context, f flags) ([]*output.Record, *diag.Diagnostic) {
	host, err := ctx.Detect()
	if err != nil {
		id := diag.PlatformProbeFailed
		var unsupported *target.UnsupportedError
		if errors.As(err, &unsupported) {
			id = diag.PlatformUnsupported
		}
		dg := diag.New(id, err.Error())
		return nil, &dg
	}
	entries, reg, err := d.Catalog()
	if err != nil {
		id := diag.SoftwareSourceInvalid
		msg := sourceMessage(err)
		if errors.Is(err, remote.ErrNotCached) {
			// A configured source that was never refreshed is an
			// explicit diagnostic: fetching stays an explicit action.
			id = diag.SoftwareSourceUnavailable
			msg = msg + `; run "managed-machine software --refresh" to fetch it`
		}
		dg := diag.New(id, msg)
		return nil, &dg
	}
	if f.provider != "" {
		if _, ok := reg.Lookup(f.provider); !ok {
			dg := diag.New(diag.SoftwareUnknownProvider, fmt.Sprintf("unknown provider %q; known: %s", f.provider, strings.Join(reg.Names(), ", ")))
			return nil, &dg
		}
	}
	if f.category != "" && !anyCategory(entries, f.category) {
		dg := diag.New(diag.SoftwareUnknownCategory, fmt.Sprintf("no software carries category %q", f.category))
		return nil, &dg
	}
	if f.name != "" && !anyName(entries, f.name) {
		dg := diag.New(diag.SoftwareUnknownName, fmt.Sprintf("%q is not in the catalog", f.name))
		return nil, &dg
	}
	if f.recipe != "" && !anyRecipe(entries, f.name, f.recipe) {
		where := "any software"
		if f.name != "" {
			where = f.name
		}
		dg := diag.New(diag.SoftwareUnknownRecipe, fmt.Sprintf("%s has no recipe %q", where, f.recipe))
		return nil, &dg
	}
	var recs []*output.Record
	for _, e := range entries {
		sw := e.Software
		if f.name != "" && sw.ID != f.name || f.category != "" && !contains(sw.Categories, f.category) {
			continue
		}
		for _, r := range sw.Recipes {
			if !contains(r.Platforms, string(host.Target)) ||
				f.provider != "" && r.Provider != f.provider ||
				f.recipe != "" && r.ID != f.recipe ||
				f.target() != "" && !contains(r.Targets, f.target()) {
				continue
			}
			recs = append(recs, output.New().Set("software", sw.ID).Set("recipe", r.ID).
				Set("provider", r.Provider).Set("package", r.Package).Set("targets", r.Targets))
		}
	}
	return recs, nil
}

func sourceMessage(err error) string {
	var se *schema.Error
	if errors.As(err, &se) {
		return se.Error()
	}
	return err.Error()
}

func help(d Deps, ctx cli.Context, args []string) string {
	var b strings.Builder
	b.WriteString(usage)
	f, bad := parse(args)
	// The refresh is listed where it applies: when a remote source is
	// configured, or when the caller asked about it.
	asked := bad == nil && f.refresh
	if bad == nil {
		f.install = false
		// Help never fetches: the refresh is an action, and help is
		// not an action.
		f.refresh = false
		recs, dg := discover(d, ctx, f)
		if dg == nil {
			b.WriteString("\nAvailable software\n")
			rows := [][]string{{"SOFTWARE", "PROVIDER", "PACKAGE", "TARGETS"}}
			for _, r := range recs {
				sw, _ := r.Get("software")
				p, _ := r.Get("provider")
				pkg, _ := r.Get("package")
				t, _ := r.Get("targets")
				rows = append(rows, []string{sw.(string), p.(string), pkg.(string), strings.Join(t.([]string), ", ")})
			}
			b.WriteString(table(rows))
		} else {
			fmt.Fprintf(&b, "\n%s\n", dg.Human())
		}
	}
	b.WriteString(options)
	if asked || d.remoteConfigured() {
		b.WriteString(refreshOption)
	}
	b.WriteString(notes)
	return b.String()
}

// remoteConfigured reports whether a remote source is configured, so the
// help lists the refresh action only where it applies.
func (d Deps) remoteConfigured() bool {
	if d.Getenv == nil {
		return false
	}
	cfg, err := remote.ParseConfig(d.Getenv)
	return err == nil && cfg.Configured()
}

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

func anyName(es []source.Entry, name string) bool {
	for _, e := range es {
		if e.Software.ID == name {
			return true
		}
	}
	return false
}

func anyCategory(es []source.Entry, c string) bool {
	for _, e := range es {
		if contains(e.Software.Categories, c) {
			return true
		}
	}
	return false
}

func anyRecipe(es []source.Entry, name, id string) bool {
	for _, e := range es {
		if name != "" && e.Software.ID != name {
			continue
		}
		for _, r := range e.Software.Recipes {
			if r.ID == id {
				return true
			}
		}
	}
	return false
}
