// Package fleet implements the "managed-machine fleet" command (S5 story
// L02; decisions D1, D2, D3, D4 and D7, ADR-0010): key-directory inspection
// (--keys), per-account authorization evaluation (--access), and remote
// gist key-directory caching (--refresh) over the shared policy, directory
// and authorization packages.
package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os/user"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/fleet/authz"
	"github.com/qwts/managed-machine/core/internal/fleet/directory"
	"github.com/qwts/managed-machine/core/internal/fleet/policy"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/provider"
	"github.com/qwts/managed-machine/core/internal/source/remote"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

// GitHubAPIEnv names an explicit https GitHub API root override for gist
// fetches (D3).
const GitHubAPIEnv = remote.GitHubAPIEnv

// PolicyEnv names an explicit policy checkout directory override (D2).
const PolicyEnv = policy.PolicyEnv

// Key states emitted by `fleet --keys` (D4) and `fleet --access` (D4).
const (
	KeyStatePublished = "published"
	KeyStateInvalid   = "invalid"

	AccessStateAuthorized          = "authorized"
	AccessStateApprovedUnpublished = "approved-unpublished"
)

// Deps are the seams of the fleet command.
type Deps struct {
	// Getenv reads the environment for MANAGED_MACHINE_POLICY_DIR,
	// MANAGED_MACHINE_STATE_DIR, MANAGED_MACHINE_GITHUB_API and platform
	// state directory defaults. When nil, no environment variable is set.
	Getenv func(string) string
	// GOOS is the operating system for locating the state directory and
	// matching account names (D1). Empty means the detected host OS or
	// runtime.GOOS.
	GOOS string
	// StateDir locates the platform state directory. When nil,
	// statedir.Path(GOOS, Getenv) is used.
	StateDir func() (string, error)
	// PolicyDir locates the managed-machine-config checkout. When nil,
	// policy.Dir(Getenv, stateDir) is used.
	PolicyDir func() (string, error)
	// Username returns the invoking OS account name (D1). When nil,
	// DefaultUsername is used (os/user.Current with DOMAIN\ stripped on
	// Windows).
	Username func() (string, error)
	// Account is an fallback alias for Username when Username is nil.
	Account func() (string, error)
	// Home returns the current user's home directory when locating the
	// default <HOME>/.ssh/authorized_keys destination (D4). When nil,
	// USERPROFILE (on Windows) or HOME is read from Getenv.
	Home func() (string, error)
	// Machine returns the stable machine identifier for plan bindings.
	// When nil, "local" is used.
	Machine func(ctx context.Context) (string, error)
	// Now is the clock for the "at" timestamp on --keys directory records.
	// When nil, time.Now is used.
	Now func() time.Time
	// NewPlanID builds a plan id for the creation time. When nil,
	// plan.NewID is used.
	NewPlanID func(time.Time) (string, error)
	// Writable reports whether a destination or directory path is
	// writable by the current user. When nil, the OS check is used.
	Writable func(path string) bool
	// Client is the HTTP client --refresh fetches with. When nil,
	// remote.DefaultClient is used.
	Client *http.Client
	// Runner is the process execution seam. The fleet command never
	// spawns git, ssh-keygen, ssh, gh or any external process (D1, D2),
	// so tests can inject a counting Runner and assert zero invocations.
	Runner provider.Runner
}

type flags struct {
	positional string
	ownerFlag  string
	keyPath    string
	label      string
	fromPlan   string
	unpublish  string
	custom     string

	keys         bool
	access       bool
	refresh      bool
	plan         bool
	apply        bool
	check        bool
	publish      bool
	hasUnpublish bool

	user      bool
	global    bool
	hasCustom bool
}

// Command is `managed-machine fleet`: key-directory inspection (--keys),
// authorization evaluation (--access) and remote gist caching (--refresh).
func Command(d Deps) cli.Command {
	return cli.Command{
		Name:    "fleet",
		Summary: "Inspect and manage fleet SSH key directory and account authorization",
		Usage:   usage,
		Run:     func(ctx cli.Context, args []string) cli.Result { return run(d, ctx, args) },
	}
}

const usage = `Usage: managed-machine fleet [owner] [options]

Inspect and manage fleet SSH key directory and account authorization.
Default output is compact JSON Lines on stdout.

Options
-h, --help                 Show usage; never changes the machine
--keys                     List published keys in the key directory (default)
--access                   Evaluate authorized and approved-unpublished keys for the current account
--refresh                  Resolve and cache the configured gist key directory
--plan                     Plan the authorized_keys managed block
--apply                    Apply the saved fleet plan
--check                    Check the authorized_keys managed block for drift
--publish                  Publish a public key to the key directory
--unpublish <fingerprint>  Remove a published key from the key directory
--owner <id>               Select owner id for --publish or --unpublish
--key <public-key-path>    Path to the OpenSSH public key for --publish
--label <text>             Optional key label for --publish
--from-plan <id|path>      Apply the named saved plan
--user                     Select the invoking user target (default and only target)
--json                     Return results as one JSON document
--human                    Render results in a readable table
`

// DefaultUsername reads the current OS user via os/user.Current and strips
// any Windows DOMAIN\ prefix when goos is "windows" (D1).
func DefaultUsername(goos string) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return normalizeUsername(u.Username, goos), nil
}

func normalizeUsername(name, goos string) string {
	if goos == "windows" {
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			return name[i+1:]
		}
	}
	return name
}

func (d Deps) getenv(k string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(k)
}

func (d Deps) goos(detectedOS string) string {
	if d.GOOS != "" {
		return d.GOOS
	}
	if detectedOS != "" {
		return detectedOS
	}
	return runtime.GOOS
}

func (d Deps) stateDir(goos string) (string, error) {
	if d.StateDir != nil {
		return d.StateDir()
	}
	return statedir.Path(goos, d.getenv)
}

func (d Deps) policyDir(goos string) (string, error) {
	if d.PolicyDir != nil {
		return d.PolicyDir()
	}
	if p := strings.TrimSpace(d.getenv(policy.PolicyEnv)); p != "" {
		return policy.Dir(d.getenv, "")
	}
	stateDir, err := d.stateDir(goos)
	if err != nil {
		return "", &policy.UnavailableError{Message: err.Error()}
	}
	return policy.Dir(d.getenv, stateDir)
}

func (d Deps) username(goos string) (string, error) {
	if d.Username != nil {
		u, err := d.Username()
		if err != nil {
			return "", err
		}
		return normalizeUsername(u, goos), nil
	}
	if d.Account != nil {
		u, err := d.Account()
		if err != nil {
			return "", err
		}
		return normalizeUsername(u, goos), nil
	}
	return DefaultUsername(goos)
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) nowRFC3339() string {
	return d.now().UTC().Truncate(time.Second).Format(time.RFC3339)
}

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
		a := args[i]
		switch {
		case a == "--keys":
			f.keys = true
		case a == "--access":
			f.access = true
		case a == "--refresh":
			f.refresh = true
		case a == "--plan":
			f.plan = true
		case a == "--apply":
			f.apply = true
		case a == "--check":
			f.check = true
		case a == "--publish":
			f.publish = true
		case a == "--unpublish":
			f.hasUnpublish = true
			f.unpublish, d = value(&i, a)
		case strings.HasPrefix(a, "--unpublish="):
			f.hasUnpublish = true
			f.unpublish = strings.TrimPrefix(a, "--unpublish=")
			if f.unpublish == "" {
				d = usageErr("--unpublish needs a value")
			}
		case a == "--owner":
			f.ownerFlag, d = value(&i, a)
		case strings.HasPrefix(a, "--owner="):
			f.ownerFlag = strings.TrimPrefix(a, "--owner=")
			if f.ownerFlag == "" {
				d = usageErr("--owner needs a value")
			}
		case a == "--key":
			f.keyPath, d = value(&i, a)
		case strings.HasPrefix(a, "--key="):
			f.keyPath = strings.TrimPrefix(a, "--key=")
			if f.keyPath == "" {
				d = usageErr("--key needs a value")
			}
		case a == "--label":
			f.label, d = value(&i, a)
		case strings.HasPrefix(a, "--label="):
			f.label = strings.TrimPrefix(a, "--label=")
			if f.label == "" {
				d = usageErr("--label needs a value")
			}
		case a == "--from-plan":
			f.fromPlan, d = value(&i, a)
		case strings.HasPrefix(a, "--from-plan="):
			f.fromPlan = strings.TrimPrefix(a, "--from-plan=")
			if f.fromPlan == "" {
				d = usageErr("--from-plan needs a value")
			}
		case a == "--user":
			f.user = true
		case a == "--global":
			f.global = true
		case a == "--custom":
			f.hasCustom = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				f.custom = args[i]
			}
		case strings.HasPrefix(a, "--custom="):
			f.hasCustom = true
			f.custom = strings.TrimPrefix(a, "--custom=")
		default:
			if strings.HasPrefix(a, "-") {
				return f, usageErr("unknown option %q", a)
			}
			if f.positional != "" {
				return f, usageErr("at most one owner argument; got %q and %q", f.positional, a)
			}
			f.positional = a
		}
		if d != nil {
			return f, d
		}
	}

	if f.global || f.hasCustom {
		return f, usageErr("--user is the only target: fleet authorizes OS accounts, Souls have none (ADR-0009; --custom is not supported) and machine-wide sshd files are outside v1 (--global is not supported)")
	}

	actions := 0
	for _, b := range []bool{f.keys, f.access, f.refresh, f.plan, f.apply, f.check, f.publish, f.hasUnpublish} {
		if b {
			actions++
		}
	}
	if actions > 1 {
		return f, usageErr("--keys, --access, --refresh, --plan, --apply, --check, --publish and --unpublish are mutually exclusive")
	}
	if actions == 0 {
		f.keys = true
	}

	if f.positional != "" {
		if !f.keys {
			return f, usageErr("positional owner %q is only valid with --keys; cannot combine with %s", f.positional, f.action())
		}
		if f.positional == "list" || f.positional == "remove" {
			return f, usageErr("legacy verb %q is not a new-core fleet action; use --keys", f.positional)
		}
		if !policy.ValidOwnerID(f.positional) {
			return f, usageErr("invalid owner id %q", f.positional)
		}
	}

	if f.fromPlan != "" && !f.apply {
		return f, usageErr("--from-plan is used with --apply")
	}
	if (f.keyPath != "" || f.label != "") && !f.publish {
		return f, usageErr("--key and --label are used with --publish")
	}
	if f.ownerFlag != "" && !f.publish && !f.hasUnpublish {
		return f, usageErr("--owner is used with --publish or --unpublish")
	}

	return f, nil
}

func (f flags) action() string {
	switch {
	case f.keys:
		return "--keys"
	case f.access:
		return "--access"
	case f.refresh:
		return "--refresh"
	case f.plan:
		return "--plan"
	case f.apply:
		return "--apply"
	case f.check:
		return "--check"
	case f.publish:
		return "--publish"
	case f.hasUnpublish:
		return "--unpublish"
	}
	return ""
}

func run(d Deps, ctx cli.Context, args []string) cli.Result {
	f, bad := parse(args)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	detectedOS, dg := detectOS(ctx)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	goos := d.goos(detectedOS)

	switch {
	case f.refresh:
		return refreshRun(d, goos)
	case f.access:
		return accessRun(d, goos)
	case f.keys:
		return keysRun(d, goos, f)
	case f.publish:
		return publishRun(d, goos, f)
	case f.hasUnpublish:
		return unpublishRun(d, goos, f)
	case f.plan:
		return planRun(d, ctx, goos)
	case f.apply:
		return applyRun(d, ctx, goos, f)
	case f.check:
		return checkRun(d, ctx, goos)
	default:
		return cli.Result{Diagnostic: usageErr("%s is not available in this slice", f.action())}
	}
}

func detectOS(ctx cli.Context) (string, *diag.Diagnostic) {
	if ctx.Detect == nil {
		return "", nil
	}
	host, err := ctx.Detect()
	if err != nil {
		id := diag.PlatformProbeFailed
		var unsupported *target.UnsupportedError
		if errors.As(err, &unsupported) {
			id = diag.PlatformUnsupported
		}
		dg := diag.New(id, err.Error())
		return "", &dg
	}
	if i := strings.IndexByte(string(host.Target), '/'); i > 0 {
		return string(host.Target[:i]), nil
	}
	return "", nil
}

func (d Deps) loadPolicy(goos string) (*policy.Policy, *diag.Diagnostic) {
	dir, err := d.policyDir(goos)
	if err != nil {
		dg := policy.ToDiagnostic(err)
		return nil, &dg
	}
	pol, err := policy.Load(dir)
	if err != nil {
		dg := policy.ToDiagnostic(err)
		return nil, &dg
	}
	return pol, nil
}

func (d Deps) loadDirectory(goos string, kd policy.KeyDirectorySource) (directory.Directory, *diag.Diagnostic) {
	if kd.Path != "" {
		dir, err := directory.LoadPath(kd.Path)
		if err != nil {
			dg := directory.ToDiagnostic(err)
			return directory.Directory{}, &dg
		}
		return dir, nil
	}
	stateDir, err := d.stateDir(goos)
	if err != nil {
		dg := diag.New(diag.FleetDirectoryUnavailable, err.Error())
		return directory.Directory{}, &dg
	}
	cached, err := remote.CachedGistDirectory(kd.Gist, stateDir)
	if err != nil {
		if errors.Is(err, remote.ErrNotCached) {
			dg := diag.New(diag.FleetDirectoryUnavailable, fmt.Sprintf("gist:%s has no cached snapshot; run \"managed-machine fleet --refresh\" to fetch it", kd.Gist))
			return directory.Directory{}, &dg
		}
		dg := diag.New(diag.FleetDirectoryInvalid, err.Error())
		return directory.Directory{}, &dg
	}
	dir, err := directory.LoadGistFS("gist:"+kd.Gist+"@"+cached.Revision, cached.FS)
	if err != nil {
		dg := directory.ToDiagnostic(err)
		return directory.Directory{}, &dg
	}
	return dir, nil
}

func refreshRun(d Deps, goos string) cli.Result {
	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if pol.KeyDirectory.Gist == "" {
		return cli.Result{Diagnostic: usageErr("key directory is a local path source; --refresh applies only to a gist key directory")}
	}
	apiBase, err := remote.ParseAPIBase(d.getenv)
	if err != nil {
		invalid := diag.New(diag.FleetPolicyInvalid, err.Error())
		return cli.Result{Diagnostic: &invalid}
	}
	stateDir, err := d.stateDir(goos)
	if err != nil {
		unavail := diag.New(diag.FleetPolicyUnavailable, err.Error())
		return cli.Result{Diagnostic: &unavail}
	}
	fg, err := remote.FetchLatestGist(context.Background(), pol.KeyDirectory.Gist, apiBase, d.Client)
	if err != nil {
		fetchDiag := diag.New(diag.FleetDirectoryFetchFailed, err.Error())
		return cli.Result{Diagnostic: &fetchDiag}
	}
	dir, err := directory.LoadGistFS(fg.Identity(), fg.FS())
	if err != nil {
		invalid := directory.ToDiagnostic(err)
		return cli.Result{Diagnostic: &invalid}
	}
	outcome, err := fg.CacheAt(stateDir, d.nowRFC3339())
	if err != nil {
		fetchDiag := diag.New(diag.FleetDirectoryFetchFailed, err.Error())
		return cli.Result{Diagnostic: &fetchDiag}
	}
	rec := output.New().
		Set("directory", dir.Identity).
		Set("revision", fg.Revision()).
		Set("keys", dir.KeyCount()).
		Set("outcome", outcome)
	return cli.Result{Records: []*output.Record{rec}}
}

func keysRun(d Deps, goos string, f flags) cli.Result {
	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}

	type keyRow struct {
		owner string
		index int
		key   directory.Key
	}
	ownersCount := 0
	var rows []keyRow
	for _, entry := range dir.Entries {
		if f.positional != "" && entry.ID != f.positional {
			continue
		}
		ownersCount++
		for idx, k := range entry.Keys {
			rows = append(rows, keyRow{
				owner: entry.ID,
				index: idx,
				key:   k,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].owner != rows[j].owner {
			return rows[i].owner < rows[j].owner
		}
		if rows[i].key.Fingerprint != rows[j].key.Fingerprint {
			return rows[i].key.Fingerprint < rows[j].key.Fingerprint
		}
		return rows[i].index < rows[j].index
	})

	recs := make([]*output.Record, 0, 1+len(rows))
	recs = append(recs, output.New().
		Set("directory", dir.Identity).
		Set("owners", ownersCount).
		Set("keys", len(rows)).
		Set("at", d.nowRFC3339()))
	for _, row := range rows {
		state := KeyStateInvalid
		if row.key.Valid() {
			state = KeyStatePublished
		}
		recs = append(recs, output.New().
			Set("owner", row.owner).
			Set("fingerprint", row.key.Fingerprint).
			SetOptional("key_type", row.key.KeyType).
			SetOptional("label", row.key.Label).
			Set("published_at", row.key.PublishedAt).
			Set("revision", row.key.Revision).
			Set("state", state).
			SetOptional("detail", row.key.Detail))
	}
	return cli.Result{Records: recs}
}

func accessRun(d Deps, goos string) cli.Result {
	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	account, err := d.username(goos)
	if err != nil {
		fail := diag.New(diag.FleetApplyFailed, err.Error())
		return cli.Result{Diagnostic: &fail}
	}
	res := authz.Evaluate(pol.Groups, dir, account, goos)
	recs := make([]*output.Record, 0, len(res.Authorized)+len(res.ApprovedUnpublished))
	for _, a := range res.Authorized {
		recs = append(recs, output.New().
			Set("account", account).
			Set("fingerprint", a.Fingerprint).
			Set("state", AccessStateAuthorized).
			SetOptional("owner", a.Owner).
			Set("group", a.Group))
	}
	for _, u := range res.ApprovedUnpublished {
		recs = append(recs, output.New().
			Set("account", account).
			Set("fingerprint", u.Fingerprint).
			Set("state", AccessStateApprovedUnpublished).
			SetOptional("owner", "").
			Set("group", u.Group))
	}
	return cli.Result{Records: recs}
}
