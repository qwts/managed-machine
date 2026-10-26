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
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/fleet/authz"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/source/remote"
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
	command.Deps
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
	return command.DefaultUsername(goos)
}

func (d Deps) policyDir(goos string) (string, error) {
	if d.PolicyDir != nil {
		return d.PolicyDir()
	}
	d.GOOS = d.OS(goos)
	if p := strings.TrimSpace(d.Env(policy.PolicyEnv)); p != "" {
		return policy.Dir(d.Env, "")
	}
	stateDir, err := d.ResolveStateDir(goos)
	if err != nil {
		return "", &policy.UnavailableError{Message: err.Error()}
	}
	return policy.Dir(d.Env, stateDir)
}

func parse(args []string) (flags, *diag.Diagnostic) {
	var f flags
	bad := command.Parse(args, command.Spec{
		Positional:     &f.positional,
		PositionalName: "owner argument",
		Bools: map[string]*bool{
			"--keys":    &f.keys,
			"--access":  &f.access,
			"--refresh": &f.refresh,
			"--plan":    &f.plan,
			"--apply":   &f.apply,
			"--check":   &f.check,
			"--publish": &f.publish,
			"--user":    &f.user,
			"--global":  &f.global,
		},
		Values: map[string]command.Flag{
			"--unpublish": {Value: &f.unpublish, Set: &f.hasUnpublish},
			"--owner":     {Value: &f.ownerFlag},
			"--key":       {Value: &f.keyPath},
			"--label":     {Value: &f.label},
			"--from-plan": {Value: &f.fromPlan},
			"--custom":    {Value: &f.custom, Set: &f.hasCustom, Optional: true},
		},
	})
	if bad != nil {
		return f, bad
	}

	if f.global || f.hasCustom {
		return f, command.UsageErr("--user is the only target: fleet authorizes OS accounts, Souls have none (ADR-0009; --custom is not supported) and machine-wide sshd files are outside v1 (--global is not supported)")
	}

	actions := 0
	for _, b := range []bool{f.keys, f.access, f.refresh, f.plan, f.apply, f.check, f.publish, f.hasUnpublish} {
		if b {
			actions++
		}
	}
	if actions > 1 {
		return f, command.UsageErr("--keys, --access, --refresh, --plan, --apply, --check, --publish and --unpublish are mutually exclusive")
	}
	if actions == 0 {
		f.keys = true
	}

	if f.positional != "" {
		if !f.keys {
			return f, command.UsageErr("positional owner %q is only valid with --keys; cannot combine with %s", f.positional, f.action())
		}
		if f.positional == "list" || f.positional == "remove" {
			return f, command.UsageErr("legacy verb %q is not a new-core fleet action; use --keys", f.positional)
		}
		if !policy.ValidOwnerID(f.positional) {
			return f, command.UsageErr("invalid owner id %q", f.positional)
		}
	}

	if f.fromPlan != "" && !f.apply {
		return f, command.UsageErr("--from-plan is used with --apply")
	}
	if (f.keyPath != "" || f.label != "") && !f.publish {
		return f, command.UsageErr("--key and --label are used with --publish")
	}
	if f.ownerFlag != "" && !f.publish && !f.hasUnpublish {
		return f, command.UsageErr("--owner is used with --publish or --unpublish")
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
	if r, ok := legacy.Match(append([]string{"fleet"}, args...)); ok {
		dg := legacy.Diagnostic(r)
		return cli.Result{Diagnostic: &dg}
	}
	f, bad := parse(args)
	if bad != nil {
		return cli.Result{Diagnostic: bad}
	}
	var detectedOS string
	if ctx.Detect != nil {
		host, dg := command.DetectHost(ctx)
		if dg != nil {
			return cli.Result{Diagnostic: dg}
		}
		detectedOS = string(host.Target)
	}
	goos := d.OS(detectedOS)
	d.GOOS = goos

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
		return cli.Result{Diagnostic: command.UsageErr("%s is not available in this slice", f.action())}
	}
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
	stateDir, err := d.ResolveStateDir(goos)
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
		return cli.Result{Diagnostic: command.UsageErr("key directory is a local path source; --refresh applies only to a gist key directory")}
	}
	apiBase, err := remote.ParseAPIBase(d.Env)
	if err != nil {
		invalid := diag.New(diag.FleetPolicyInvalid, err.Error())
		return cli.Result{Diagnostic: &invalid}
	}
	stateDir, err := d.ResolveStateDir(goos)
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
	outcome, err := fg.CacheAt(stateDir, d.NowRFC3339())
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
		Set("at", d.NowRFC3339()))
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
	account, err := d.ResolveUsername(goos)
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
