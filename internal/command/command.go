// Package command is the shared command kit (S7 story N04; decision D7):
// flag parsing, usage diagnostics, fixed-column table rendering, embedded
// Deps seams and getters, host target detection, NUL-separated SHA-256 input
// digests, normalized plan bindings and the closure-backed execute.Machine
// adapter shared by software, config, fleet, status, skill and install.
package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/statedir"
	"github.com/qwts/managed-machine/internal/target"
)

// Flag describes one value-taking option in a command's flag specification.
type Flag struct {
	// Value receives the option's argument.
	Value *string
	// Set, when non-nil, is set to true when the option is present.
	Set *bool
	// Count, when non-nil, is incremented each time the option is present.
	Count *int
	// Optional allows the option to appear without a value (or with an
	// empty --flag= value) when the caller validates it after parsing.
	Optional bool
}

// Spec describes the options and single positional argument a command
// accepts.
type Spec struct {
	// Positional receives the at-most-one positional argument.
	Positional *string
	// PositionalName names the positional argument in the duplicate
	// argument usage diagnostic (for example "software name").
	PositionalName string
	// Bools maps boolean option flags (such as "--available") to their
	// target booleans.
	Bools map[string]*bool
	// Values maps value-taking option flags (such as "--provider") to
	// their Flag specifications. Both "--flag <val>" and "--flag=<val>"
	// are accepted.
	Values map[string]Flag
}

// UsageErr builds an MM_USAGE diagnostic.
func UsageErr(format string, args ...any) *diag.Diagnostic {
	d := diag.New(diag.Usage, fmt.Sprintf(format, args...))
	return &d
}

// Parse parses args according to spec and returns an MM_USAGE diagnostic on
// unknown options, missing option values or extra positional arguments.
func Parse(args []string, spec Spec) *diag.Diagnostic {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if b, ok := spec.Bools[a]; ok {
			if b != nil {
				*b = true
			}
			continue
		}
		if f, ok := spec.Values[a]; ok {
			if f.Count != nil {
				*f.Count++
			}
			if f.Set != nil {
				*f.Set = true
			}
			if f.Optional {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					i++
					if f.Value != nil {
						*f.Value = args[i]
					}
				}
				continue
			}
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return UsageErr("%s needs a value", a)
			}
			i++
			if f.Value != nil {
				*f.Value = args[i]
			}
			continue
		}
		if strings.HasPrefix(a, "--") {
			if name, val, ok := strings.Cut(a, "="); ok {
				if f, ok := spec.Values[name]; ok {
					if f.Count != nil {
						*f.Count++
					}
					if f.Set != nil {
						*f.Set = true
					}
					if f.Value != nil {
						*f.Value = val
					}
					if !f.Optional && val == "" {
						return UsageErr("%s needs a value", name)
					}
					continue
				}
			}
		}
		if strings.HasPrefix(a, "-") {
			return UsageErr("unknown option %q", a)
		}
		if spec.Positional == nil {
			return UsageErr("unexpected argument %q", a)
		}
		if *spec.Positional != "" {
			return UsageErr("at most one %s; got %q and %q", spec.PositionalName, *spec.Positional, a)
		}
		*spec.Positional = a
	}
	return nil
}

// Table renders rows in fixed-width columns separated by two spaces, with
// trailing spaces trimmed from each line.
func Table(rows [][]string) string {
	if len(rows) == 0 {
		return ""
	}
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

// DetectHost runs ctx.Detect (falling back to target.Detect when nil) and
// maps probe failures to MM_PLATFORM_UNSUPPORTED or MM_PLATFORM_PROBE_FAILED.
func DetectHost(ctx cli.Context) (target.Host, *diag.Diagnostic) {
	detect := ctx.Detect
	if detect == nil {
		detect = target.Detect
	}
	host, err := detect()
	if err != nil {
		id := diag.PlatformProbeFailed
		var unsupported *target.UnsupportedError
		if errors.As(err, &unsupported) {
			id = diag.PlatformUnsupported
		}
		dg := diag.New(id, err.Error())
		return target.Host{}, &dg
	}
	return host, nil
}

// Deps holds the shared environment, identity, clock, plan-id, filesystem
// writability and HTTP client seams embedded by command Deps structs.
type Deps struct {
	// Getenv reads the environment. When nil, no environment variable is set.
	Getenv func(string) string
	// Environ returns the environment entries for command lookup. When nil,
	// PATH and PATHEXT are read from Getenv.
	Environ func() []string
	// GOOS is the operating system for locating state and configuration
	// paths and matching account names. Empty means the detected host OS or
	// runtime.GOOS.
	GOOS string
	// StateDir locates the platform state directory. When nil,
	// statedir.Path(GOOS, Getenv) is used.
	StateDir func() (string, error)
	// PolicyDir locates the fleet policy checkout.
	PolicyDir func() (string, error)
	// Account returns the current OS account name.
	Account func() (string, error)
	// Username returns the invoking OS account name (with DOMAIN\ stripped
	// on Windows). When nil, Account or DefaultUsername is used.
	Username func() (string, error)
	// Home returns the current user's home directory. When nil, USERPROFILE
	// (on Windows) or HOME is read from Getenv.
	Home func() (string, error)
	// Machine returns the stable machine identifier for plan bindings.
	// When nil, "local" is used.
	Machine func(ctx context.Context) (string, error)
	// Now is the clock for plan ids, journals, ledger records and status
	// timestamps. When nil, time.Now is used.
	Now func() time.Time
	// NewPlanID builds a plan id for the creation time. When nil,
	// plan.NewID is used.
	NewPlanID func(time.Time) (string, error)
	// Writable reports whether a destination or directory path is writable
	// by the current user. When nil, the OS check is used.
	Writable func(path string) bool
	// Client is the HTTP client --refresh fetches with. A nil client means
	// the default one.
	Client *http.Client
}

// DefaultUsername reads the current OS user via os/user.Current and strips
// any Windows DOMAIN\ prefix when goos is "windows".
func DefaultUsername(goos string) (string, error) {
	u, err := user.Current()
	if err != nil {
		return "", err
	}
	return NormalizeUsername(u.Username, goos), nil
}

// NormalizeUsername strips any Windows DOMAIN\ prefix when goos is "windows".
func NormalizeUsername(name, goos string) string {
	if goos == "windows" {
		if i := strings.LastIndexByte(name, '\\'); i >= 0 {
			return name[i+1:]
		}
	}
	return name
}

// NewPlanID builds a plan id for t using plan.NewID.
func NewPlanID(t time.Time) (string, error) {
	return plan.NewID(t)
}

// Env reads k from Getenv, returning "" when Getenv is nil.
func (d Deps) Env(k string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(k)
}

// EnvironVars returns the environment entries for command lookup, falling
// back to PATH and PATHEXT from Getenv when Environ is nil.
func (d Deps) EnvironVars() []string {
	if d.Environ != nil {
		return d.Environ()
	}
	var env []string
	for _, k := range []string{"PATH", "PATHEXT"} {
		if v := d.Env(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// OS returns GOOS when set, the first non-empty detected OS when given, or
// runtime.GOOS.
func (d Deps) OS(detected ...string) string {
	if d.GOOS != "" {
		return d.GOOS
	}
	for _, s := range detected {
		if i := strings.IndexByte(s, '/'); i > 0 {
			s = s[:i]
		}
		if s != "" {
			return s
		}
	}
	return runtime.GOOS
}

func (d Deps) resolveOS(goos []string) string {
	for _, s := range goos {
		if i := strings.IndexByte(s, '/'); i > 0 {
			s = s[:i]
		}
		if s != "" {
			return s
		}
	}
	return d.OS()
}

// ResolveStateDir returns the platform state directory from StateDir or
// statedir.Path(d.OS(goos...), d.Env).
func (d Deps) ResolveStateDir(goos ...string) (string, error) {
	if d.StateDir != nil {
		return d.StateDir()
	}
	return statedir.Path(d.resolveOS(goos), d.Env)
}

// ResolveAccount returns the current OS account name from Account, or
// "default" when Account is nil.
func (d Deps) ResolveAccount() (string, error) {
	if d.Account != nil {
		return d.Account()
	}
	return "default", nil
}

// ResolveUsername returns the invoking OS account name from Username,
// Account, or DefaultUsername, normalized for goos.
func (d Deps) ResolveUsername(goos ...string) (string, error) {
	osName := d.resolveOS(goos)
	if d.Username != nil {
		u, err := d.Username()
		if err != nil {
			return "", err
		}
		return NormalizeUsername(u, osName), nil
	}
	if d.Account != nil {
		u, err := d.Account()
		if err != nil {
			return "", err
		}
		return NormalizeUsername(u, osName), nil
	}
	return DefaultUsername(osName)
}

// ResolveHome returns the current user's home directory from Home or from
// USERPROFILE (on Windows) / HOME in Getenv.
func (d Deps) ResolveHome(goos ...string) (string, error) {
	if d.Home != nil {
		return d.Home()
	}
	if d.resolveOS(goos) == "windows" {
		if u := strings.TrimSpace(d.Env("USERPROFILE")); u != "" {
			return filepath.Clean(u), nil
		}
		if h := strings.TrimSpace(d.Env("HOME")); h != "" {
			return filepath.Clean(h), nil
		}
		return "", errors.New("the environment names no home directory (USERPROFILE)")
	}
	if h := strings.TrimSpace(d.Env("HOME")); h != "" {
		return filepath.Clean(h), nil
	}
	return "", errors.New("the environment names no home directory (HOME)")
}

// ResolveMachine returns the stable machine identifier from Machine, or
// "local" when Machine is nil.
func (d Deps) ResolveMachine(ctx context.Context) (string, error) {
	if d.Machine != nil {
		return d.Machine(ctx)
	}
	return "local", nil
}

// Time returns the current time from Now, or time.Now when Now is nil.
func (d Deps) Time() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// NowRFC3339 returns the current UTC time truncated to seconds in RFC 3339
// format.
func (d Deps) NowRFC3339() string {
	return d.Time().UTC().Truncate(time.Second).Format(time.RFC3339)
}

// PlanID builds a plan id for t using NewPlanID or plan.NewID.
func (d Deps) PlanID(t time.Time) (string, error) {
	if d.NewPlanID != nil {
		return d.NewPlanID(t)
	}
	return NewPlanID(t)
}

// IsWritable reports whether destPath may be written or replaced by the
// current user, consulting Writable when set.
func (d Deps) IsWritable(destPath string) bool {
	if d.Writable != nil {
		return d.Writable(destPath)
	}
	return IsWritable(destPath)
}

// IsWritableDir reports whether dirPath (or its nearest existing ancestor)
// may be written by the current user, consulting Writable when set.
func (d Deps) IsWritableDir(dirPath string) bool {
	if d.Writable != nil {
		return d.Writable(dirPath)
	}
	return IsWritableDir(dirPath)
}

// Digest returns the "sha256:<hex>" digest over NUL-joined parts.
func Digest(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:])
}

// Bindings builds a normalized plan.Bindings with assets and providers
// deduplicated and sorted by name and "sha256:" prefixes stripped from
// asset digests.
func Bindings(machine, platform, digest string, assets []plan.Asset, providers []plan.Provider) plan.Bindings {
	seenAssets := make(map[string]string, len(assets))
	for _, a := range assets {
		seenAssets[a.Name] = strings.TrimPrefix(a.SHA256, "sha256:")
	}
	assetNames := make([]string, 0, len(seenAssets))
	for name := range seenAssets {
		assetNames = append(assetNames, name)
	}
	sort.Strings(assetNames)
	normAssets := make([]plan.Asset, 0, len(assetNames))
	for _, name := range assetNames {
		normAssets = append(normAssets, plan.Asset{Name: name, SHA256: seenAssets[name]})
	}

	seenProvs := make(map[string]string, len(providers))
	for _, p := range providers {
		seenProvs[p.Name] = p.Version
	}
	provNames := make([]string, 0, len(seenProvs))
	for name := range seenProvs {
		provNames = append(provNames, name)
	}
	sort.Strings(provNames)
	normProvs := make([]plan.Provider, 0, len(provNames))
	for _, name := range provNames {
		normProvs = append(normProvs, plan.Provider{Name: name, Version: seenProvs[name]})
	}

	return plan.Bindings{
		Machine:      machine,
		Platform:     platform,
		InputsDigest: digest,
		Assets:       normAssets,
		Providers:    normProvs,
	}
}

// Machine adapts closures to execute.Machine.
type Machine struct {
	BindingsFunc func(ctx context.Context, d *plan.Document) (plan.Bindings, error)
	CheckFunc    func(ctx context.Context, p plan.Precondition) (string, error)
	StepFunc     func(op plan.Operation) (execute.Step, error)
}

var (
	_ execute.Machine = Machine{}
	_ execute.Machine = (*Machine)(nil)
)

// Bindings implements execute.Machine.
func (m Machine) Bindings(ctx context.Context, d *plan.Document) (plan.Bindings, error) {
	if m.BindingsFunc == nil {
		return plan.Bindings{}, nil
	}
	return m.BindingsFunc(ctx, d)
}

// Check implements execute.Machine.
func (m Machine) Check(ctx context.Context, p plan.Precondition) (string, error) {
	if m.CheckFunc == nil {
		return p.Observed, nil
	}
	return m.CheckFunc(ctx, p)
}

// Step implements execute.Machine.
func (m Machine) Step(op plan.Operation) (execute.Step, error) {
	if m.StepFunc == nil {
		return nil, fmt.Errorf("unknown provider %q", op.Provider)
	}
	return m.StepFunc(op)
}
