// Package brew is the built-in Homebrew formula and cask provider (S2 story
// E02, S7 story N01; decisions D2, D5, D6). It plans and installs formulae
// and app-only signed casks through the shared executor and never elevates:
// when the Homebrew prefix or the Applications directory is not writable by
// the current user the plan fails its precondition instead of running brew as
// the prefix owner or creating /Applications with root privileges, the
// authorization paths the legacy Bash routes take. Custom roots, non-app
// casks and options outside the formula or cask recipe shapes are rejected
// explicitly.
package brew

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/codesign"
	"github.com/qwts/managed-machine/internal/provider/fetch"
)

// Name is the provider name in recipes.
const Name = "brew"

// DefaultPrefix is the canonical Homebrew prefix for a target, or "" when
// the target has no Homebrew.
func DefaultPrefix(target string) string {
	switch target {
	case "darwin/arm64":
		return "/opt/homebrew"
	case "linux/arm64", "linux/amd64":
		return "/home/linuxbrew/.linuxbrew"
	}
	return ""
}

// Env is the explicit Homebrew environment added to every invocation: no
// implicit update, no cleanup, no hints. Brew never edits shell profiles,
// and this provider writes no shell configuration.
var Env = []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_INSTALL_CLEANUP=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1"}

// Result, Runner and Exec are the shared process seam; this provider only
// ever starts the brew executable under its prefix and, for cask signature
// verification, /usr/bin/codesign and /usr/bin/spctl.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// Provider is the Homebrew provider bound to one prefix.
type Provider struct {
	Prefix string
	Run    Runner
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// AppDir overrides the global /Applications directory in tests; empty
	// means "/Applications".
	AppDir string
	// AppName, TeamID and AllowRollingURL supply the cask bundle metadata
	// when Step is called directly in a test without a preceding Plan call.
	AppName         string
	TeamID          string
	AllowRollingURL bool
	// Writable and Owned report on the prefix and appdir; nil uses the OS checks.
	Writable func(path string) bool
	Owned    func(path string) bool

	plannedCask *caskOptions
}

func (p *Provider) brew() string { return filepath.Join(p.Prefix, "bin", "brew") }

func (p *Provider) env() []string {
	base := []string{}
	if p.Environ != nil {
		base = p.Environ()
	}
	env := make([]string, 0, len(base)+len(Env))
	for _, kv := range base {
		if !strings.HasPrefix(kv, "HOMEBREW_NO_") {
			env = append(env, kv)
		}
	}
	return append(env, Env...)
}

func (p *Provider) envValue(key string) string {
	if p.Environ == nil {
		return ""
	}
	prefix := key + "="
	for _, kv := range p.Environ() {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

func (p *Provider) run(ctx context.Context, args ...string) (Result, error) {
	return p.Run(ctx, p.brew(), args, p.env())
}

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported       = provider.Kind(provider.ErrUnsupported, "unsupported by the brew provider")
	ErrPrivilege         = provider.Kind(provider.ErrPrivilege, "the Homebrew prefix is not writable by the current user")
	ErrMissing           = provider.Kind(provider.ErrMissing, "Homebrew is not installed at its canonical prefix")
	ErrCaskRefused       = provider.Kind(provider.ErrCaskRefused, "the cask failed the trust policy")
	ErrOccupied          = provider.Kind(provider.ErrOccupied, "the destination is already occupied")
	ErrSignatureRejected = provider.Kind(provider.ErrSignatureRejected, "the bundle signature or Team ID was rejected")
)

var versionLine = regexp.MustCompile(`^Homebrew ([0-9][0-9A-Za-z.\-+]*)`)

// Version reports the installed Homebrew version.
func (p *Provider) Version(ctx context.Context) (string, error) {
	if p.Prefix == "" {
		return "", fmt.Errorf("%w: no canonical prefix on this platform", ErrMissing)
	}
	if _, err := os.Stat(p.brew()); err != nil {
		return "", fmt.Errorf("%w: %s", ErrMissing, p.brew())
	}
	r, err := p.run(ctx, "--version")
	if err != nil {
		return "", err
	}
	m := versionLine.FindSubmatch(r.Stdout)
	if r.Exit != 0 || m == nil {
		return "", fmt.Errorf("brew --version exited %d: %s", r.Exit, firstLine(r.Stdout, r.Stderr))
	}
	return string(m[1]), nil
}

func firstLine(bs ...[]byte) string {
	for _, b := range bs {
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.SplitN(s, "\n", 2)[0]
		}
	}
	return ""
}

// Request and Resolution are the shared provider contract.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

var (
	formulaName   = regexp.MustCompile(`^[a-z0-9][a-z0-9@+._\-]*$`)
	teamIDPattern = regexp.MustCompile(`^[A-Z0-9]{10}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Plan resolves a formula or cask without changing the machine.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	formula := r.Options["formula"]
	cask := r.Options["cask"]
	if formula != "" && cask != "" {
		return nil, fmt.Errorf("%w: recipe cannot name both formula and cask", ErrUnsupported)
	}
	if cask != "" {
		return p.planCask(ctx, r)
	}
	for k := range r.Options {
		if k != "formula" {
			return nil, fmt.Errorf("%w: option %q is not valid for a formula recipe", ErrUnsupported, k)
		}
	}
	switch {
	case formula == "":
		return nil, fmt.Errorf("%w: the recipe names no formula or cask", ErrUnsupported)
	case !formulaName.MatchString(formula):
		return nil, fmt.Errorf("%w: %q is not a formula name", ErrUnsupported, formula)
	case r.Target != "global" && r.Target != "user":
		return nil, fmt.Errorf("%w: target %s; Homebrew installs into its shared prefix, never a custom root", ErrUnsupported, r.Target)
	}
	ver, err := p.Version(ctx)
	if err != nil {
		return nil, err
	}
	pre := []plan.Precondition{{Kind: "software", Subject: Name, Requirement: "present at " + p.Prefix, Observed: ver}}
	writable := p.writable()
	if !writable {
		return nil, fmt.Errorf("%w (%s); managed-machine never elevates or runs brew as another user", ErrPrivilege, p.Prefix)
	}
	pre = append(pre, plan.Precondition{Kind: "privilege", Subject: "homebrew-prefix", Requirement: "writable by the current user", Observed: "writable"})
	if r.Target == "user" {
		if !p.owned() {
			return nil, fmt.Errorf("%w: target user needs a prefix owned by the current user; %s is shared", ErrUnsupported, p.Prefix)
		}
		pre = append(pre, plan.Precondition{Kind: "target", Subject: "homebrew-prefix", Requirement: "owned by the current user", Observed: "owned"})
	}
	info, err := p.info(ctx, formula)
	if err != nil {
		return nil, err
	}
	res := &Resolution{Package: formula, ProviderVersion: ver, Preconditions: pre}
	if len(info.Installed) > 0 {
		res.Installed, res.Version = true, info.Installed[len(info.Installed)-1].Version
		return res, nil
	}
	if info.Versions.Stable == "" {
		return nil, fmt.Errorf("%w: formula %s has no stable version", ErrUnsupported, formula)
	}
	res.Version = info.Versions.Stable
	if info.Revision > 0 {
		res.Version = fmt.Sprintf("%s_%d", info.Versions.Stable, info.Revision)
	}
	res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: "create", Provider: Name,
		Params: map[string]string{"formula": formula, "version": res.Version}}}
	return res, nil
}

type caskOptions struct {
	cask            string
	appName         string
	teamID          string
	urlHosts        []string
	homepageHosts   []string
	allowRollingURL bool
	appDir          string
}

var knownCaskOptions = map[string]bool{
	"cask":              true,
	"app_name":          true,
	"team_id":           true,
	"url_hosts":         true,
	"homepage_hosts":    true,
	"allow_rolling_url": true,
}

func parseCaskOptions(raw map[string]string) (*caskOptions, error) {
	for k := range raw {
		if !knownCaskOptions[k] {
			return nil, fmt.Errorf("%w: unknown option %q for brew cask", ErrUnsupported, k)
		}
	}
	cask := strings.TrimSpace(raw["cask"])
	if !formulaName.MatchString(cask) {
		return nil, fmt.Errorf("%w: %q is not a cask token", ErrUnsupported, raw["cask"])
	}
	appName := strings.TrimSpace(raw["app_name"])
	if !validAppName(appName) {
		return nil, fmt.Errorf("%w: option \"app_name\" %q must be a single .app bundle name", ErrUnsupported, raw["app_name"])
	}
	teamID := strings.TrimSpace(raw["team_id"])
	if !teamIDPattern.MatchString(teamID) {
		return nil, fmt.Errorf("%w: option \"team_id\" %q must match [A-Z0-9]{10}", ErrUnsupported, raw["team_id"])
	}
	urlHosts := fetch.SplitHosts(raw["url_hosts"])
	if len(urlHosts) == 0 {
		return nil, fmt.Errorf("%w: option \"url_hosts\" is required for a cask", ErrUnsupported)
	}
	for _, h := range urlHosts {
		if strings.ContainsAny(h, "/:\\") {
			return nil, fmt.Errorf("%w: invalid url_hosts entry %q", ErrUnsupported, h)
		}
	}
	homepageHosts := fetch.SplitHosts(raw["homepage_hosts"])
	if len(homepageHosts) == 0 {
		return nil, fmt.Errorf("%w: option \"homepage_hosts\" is required for a cask", ErrUnsupported)
	}
	for _, h := range homepageHosts {
		if strings.ContainsAny(h, "/:\\") {
			return nil, fmt.Errorf("%w: invalid homepage_hosts entry %q", ErrUnsupported, h)
		}
	}
	allowRolling := false
	if v, ok := raw["allow_rolling_url"]; ok && strings.TrimSpace(v) != "" {
		switch strings.TrimSpace(v) {
		case "true":
			allowRolling = true
		case "false":
			allowRolling = false
		default:
			return nil, fmt.Errorf("%w: option \"allow_rolling_url\" must be true or false, got %q", ErrUnsupported, v)
		}
	}
	return &caskOptions{
		cask:            cask,
		appName:         appName,
		teamID:          teamID,
		urlHosts:        urlHosts,
		homepageHosts:   homepageHosts,
		allowRollingURL: allowRolling,
	}, nil
}

func validAppName(s string) bool {
	if len(s) <= len(".app") || !strings.HasSuffix(s, ".app") {
		return false
	}
	if strings.ContainsAny(s, "/\\\x00\r\n") || s == ".app" || strings.HasPrefix(s, "..") {
		return false
	}
	return true
}

func isAbsPath(p string) bool {
	return filepath.IsAbs(p) || strings.HasPrefix(p, "/")
}

func (p *Provider) resolveAppDir(target string) (string, error) {
	switch target {
	case "global":
		if p.AppDir != "" {
			return filepath.Clean(p.AppDir), nil
		}
		return "/Applications", nil
	case "user":
		home := strings.TrimSpace(p.envValue("HOME"))
		if home == "" {
			return "", fmt.Errorf("%w: the environment names no HOME, so the user Applications directory is unknown", ErrUnsupported)
		}
		return filepath.Join(filepath.Clean(home), "Applications"), nil
	default:
		return "", fmt.Errorf("%w: target %s; Homebrew installs into its shared prefix, never a custom root", ErrUnsupported, target)
	}
}

func (p *Provider) planCask(ctx context.Context, r Request) (*Resolution, error) {
	opts, err := parseCaskOptions(r.Options)
	if err != nil {
		return nil, err
	}
	appdir, err := p.resolveAppDir(r.Target)
	if err != nil {
		return nil, err
	}
	ver, err := p.Version(ctx)
	if err != nil {
		return nil, err
	}
	pre := []plan.Precondition{{Kind: "software", Subject: Name, Requirement: "present at " + p.Prefix, Observed: ver}}
	if !p.writable() {
		return nil, fmt.Errorf("%w (%s); managed-machine never elevates or runs brew as another user", ErrPrivilege, p.Prefix)
	}
	pre = append(pre, plan.Precondition{Kind: "privilege", Subject: "homebrew-prefix", Requirement: "writable by the current user", Observed: "writable"})
	if !p.appdirWritable(appdir) {
		return nil, fmt.Errorf("%w (appdir %s is absent or not writable by the current user); managed-machine never elevates", ErrPrivilege, appdir)
	}
	pre = append(pre, plan.Precondition{Kind: "privilege", Subject: "appdir", Requirement: "writable by the current user", Observed: "writable"})
	if r.Target == "user" {
		if !p.owned() {
			return nil, fmt.Errorf("%w: target user needs a prefix owned by the current user; %s is shared", ErrUnsupported, p.Prefix)
		}
		pre = append(pre, plan.Precondition{Kind: "target", Subject: "homebrew-prefix", Requirement: "owned by the current user", Observed: "owned"})
	}

	info, err := p.caskInfo(ctx, opts)
	if err != nil {
		return nil, err
	}
	opts.appDir = appdir
	p.plannedCask = opts
	res := &Resolution{Package: opts.cask, ProviderVersion: ver, Preconditions: pre}

	bundle := filepath.Join(appdir, opts.appName)
	if fi, err := os.Lstat(bundle); err == nil {
		if !fi.IsDir() {
			return nil, fmt.Errorf("%w: %s already exists and is not an application bundle", ErrOccupied, bundle)
		}
		installedVer, ok := p.hasCaskReceipt(opts.cask, info)
		if !ok {
			return nil, fmt.Errorf("%w: %s already exists at %s without a Caskroom receipt for %s", ErrOccupied, opts.appName, bundle, opts.cask)
		}
		res.Installed, res.Version = true, installedVer
		return res, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	res.Version = info.Version
	res.Operations = []plan.Operation{{
		ID:        "1",
		Operation: "software:" + r.Software,
		Target:    r.Target,
		Action:    "create",
		Provider:  Name,
		Params: map[string]string{
			"cask":    opts.cask,
			"version": res.Version,
			"appdir":  appdir,
		},
	}}
	return res, nil
}

type caskInfo struct {
	Token     string                       `json:"token"`
	Tap       string                       `json:"tap"`
	URL       string                       `json:"url"`
	Homepage  string                       `json:"homepage"`
	Version   string                       `json:"version"`
	SHA256    string                       `json:"sha256"`
	Installed json.RawMessage              `json:"installed"`
	Artifacts []map[string]json.RawMessage `json:"artifacts"`
}

func (c *caskInfo) installedVersion() string {
	if len(c.Installed) == 0 || string(c.Installed) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(c.Installed, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var xs []struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(c.Installed, &xs); err == nil && len(xs) > 0 {
		return strings.TrimSpace(xs[len(xs)-1].Version)
	}
	return ""
}

func (p *Provider) caskInfo(ctx context.Context, opts *caskOptions) (*caskInfo, error) {
	r, err := p.run(ctx, "info", "--json=v2", "--cask", opts.cask)
	if err != nil {
		return nil, err
	}
	if r.Exit != 0 {
		return nil, fmt.Errorf("%w: brew info %s exited %d: %s", ErrCaskRefused, opts.cask, r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	var doc struct {
		Casks []caskInfo `json:"casks"`
	}
	if err := json.Unmarshal(r.Stdout, &doc); err != nil {
		return nil, fmt.Errorf("brew info %s: %v", opts.cask, err)
	}
	if len(doc.Casks) != 1 || doc.Casks[0].Token != opts.cask {
		return nil, fmt.Errorf("%w: brew info %s did not describe exactly that cask", ErrCaskRefused, opts.cask)
	}
	info := &doc.Casks[0]
	if err := verifyCaskSource(opts, info); err != nil {
		return nil, err
	}
	return info, nil
}

func verifyCaskSource(opts *caskOptions, info *caskInfo) error {
	if info.Tap != "homebrew/cask" {
		return fmt.Errorf("%w: refusing cask %s from tap %q; only homebrew/cask is allowed", ErrCaskRefused, opts.cask, info.Tap)
	}
	digest := strings.ToLower(strings.TrimSpace(info.SHA256))
	switch {
	case digest == "no_check" && opts.allowRollingURL:
	case digest == "no_check":
		return fmt.Errorf("%w: refusing cask %s: sha256 is no_check without allow_rolling_url", ErrCaskRefused, opts.cask)
	case !sha256Pattern.MatchString(digest):
		return fmt.Errorf("%w: refusing cask %s: invalid or missing sha256 %q", ErrCaskRefused, opts.cask, info.SHA256)
	}
	u, err := url.Parse(strings.TrimSpace(info.URL))
	if err != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return fmt.Errorf("%w: refusing cask %s: invalid download URL %q", ErrCaskRefused, opts.cask, info.URL)
	}
	if !fetch.HostAllowed(u.Hostname(), opts.urlHosts) {
		return fmt.Errorf("%w: refusing cask %s: download host %q is not in url_hosts", ErrCaskRefused, opts.cask, strings.ToLower(u.Hostname()))
	}
	h, err := url.Parse(strings.TrimSpace(info.Homepage))
	if err != nil || h.Hostname() == "" || (h.Scheme != "https" && h.Scheme != "http") {
		return fmt.Errorf("%w: refusing cask %s: invalid homepage URL %q", ErrCaskRefused, opts.cask, info.Homepage)
	}
	if !fetch.HostAllowed(h.Hostname(), opts.homepageHosts) {
		return fmt.Errorf("%w: refusing cask %s: homepage host %q is not in homepage_hosts", ErrCaskRefused, opts.cask, strings.ToLower(h.Hostname()))
	}
	appCount := 0
	for _, art := range info.Artifacts {
		if len(art) == 0 {
			return fmt.Errorf("%w: refusing cask %s: empty artifact stanza", ErrCaskRefused, opts.cask)
		}
		for kind, raw := range art {
			switch kind {
			case "app":
				var entries []json.RawMessage
				if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
					return fmt.Errorf("%w: refusing cask %s: malformed app artifact", ErrCaskRefused, opts.cask)
				}
				appCount += len(entries)
			case "target":
				// brew info --json=v2 records the default install path
				// beside the app stanza; only /Applications/<app> is plain.
				if err := checkAppTarget(art, raw); err != nil {
					return fmt.Errorf("%w: refusing cask %s: %v", ErrCaskRefused, opts.cask, err)
				}
			case "zap", "uninstall":
				// Uninstall and zap stanzas run at uninstall, not install.
			default:
				return fmt.Errorf("%w: refusing cask %s: artifact %q requires administrator or non-app handling", ErrCaskRefused, opts.cask, kind)
			}
		}
	}
	if appCount == 0 {
		return fmt.Errorf("%w: refusing cask %s: no app artifact declared", ErrCaskRefused, opts.cask)
	}
	if strings.TrimSpace(info.Version) == "" {
		return fmt.Errorf("%w: refusing cask %s: cask has no version", ErrCaskRefused, opts.cask)
	}
	return nil
}

// checkAppTarget accepts the "target" key of an app stanza only when it is
// the default /Applications/<app> path of that stanza's single app; a renamed
// or relocated target needs handling the app-only policy does not provide.
func checkAppTarget(art map[string]json.RawMessage, raw json.RawMessage) error {
	appRaw, ok := art["app"]
	if !ok {
		return errors.New(`artifact "target" outside an app stanza`)
	}
	var apps []string
	if err := json.Unmarshal(appRaw, &apps); err != nil || len(apps) != 1 {
		return errors.New(`artifact "target" beside a malformed or multi-app stanza`)
	}
	var target string
	if err := json.Unmarshal(raw, &target); err != nil {
		return errors.New(`malformed artifact "target"`)
	}
	if target != "/Applications/"+apps[0] {
		return fmt.Errorf("app target %q is not /Applications/%s", target, apps[0])
	}
	return nil
}

func (p *Provider) hasCaskReceipt(token string, info *caskInfo) (string, bool) {
	if v := info.installedVersion(); v != "" {
		return v, true
	}
	room := filepath.Join(p.Prefix, "Caskroom", token)
	entries, err := os.ReadDir(room)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			return e.Name(), true
		}
	}
	return info.Version, true
}

type formulaInfo struct {
	Name     string `json:"name"`
	Versions struct {
		Stable string `json:"stable"`
	} `json:"versions"`
	// Revision is the formula rebuild number; brew lists an installed
	// version as <stable>_<revision> when it is not zero.
	Revision  int `json:"revision"`
	Installed []struct {
		Version string `json:"version"`
	} `json:"installed"`
}

func (p *Provider) info(ctx context.Context, formula string) (*formulaInfo, error) {
	r, err := p.run(ctx, "info", "--json=v2", "--formula", formula)
	if err != nil {
		return nil, err
	}
	if r.Exit != 0 {
		return nil, fmt.Errorf("%w: brew info %s exited %d: %s", ErrUnsupported, formula, r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	var doc struct {
		Formulae []formulaInfo `json:"formulae"`
	}
	if err := json.Unmarshal(r.Stdout, &doc); err != nil {
		return nil, fmt.Errorf("brew info %s: %v", formula, err)
	}
	if len(doc.Formulae) != 1 || doc.Formulae[0].Name != formula {
		return nil, fmt.Errorf("%w: brew info %s did not describe exactly that formula", ErrUnsupported, formula)
	}
	return &doc.Formulae[0], nil
}

func (p *Provider) writable() bool {
	return p.writablePath(p.Prefix)
}

func (p *Provider) writablePath(path string) bool {
	if p.Writable != nil {
		return p.Writable(path)
	}
	return writable(path)
}

func (p *Provider) appdirWritable(appdir string) bool {
	fi, err := os.Stat(appdir)
	if err != nil || !fi.IsDir() {
		return false
	}
	return p.writablePath(appdir)
}

func (p *Provider) owned() bool {
	if p.Owned != nil {
		return p.Owned(p.Prefix)
	}
	return owned(p.Prefix)
}

// Check re-evaluates one of this provider's preconditions at apply time.
func (p *Provider) Check(ctx context.Context, c plan.Precondition) (string, error) {
	switch {
	case c.Kind == "software" && c.Subject == Name:
		v, err := p.Version(ctx)
		if err != nil {
			return "absent", err
		}
		if v != c.Observed {
			return v, fmt.Errorf("Homebrew changed from %s to %s", c.Observed, v)
		}
		return v, nil
	case c.Kind == "privilege" && c.Subject == "homebrew-prefix":
		if !p.writable() {
			return "not writable", ErrPrivilege
		}
		return "writable", nil
	case c.Kind == "privilege" && c.Subject == "appdir":
		dir := p.AppDir
		if p.plannedCask != nil && p.plannedCask.appDir != "" {
			dir = p.plannedCask.appDir
		}
		if dir != "" {
			if !p.appdirWritable(dir) {
				return "not writable", ErrPrivilege
			}
		} else if !p.writablePath("/Applications") {
			return "not writable", ErrPrivilege
		}
		return "writable", nil
	case c.Kind == "target" && c.Subject == "homebrew-prefix":
		if !p.owned() {
			return "not owned", errors.New("the prefix is no longer owned by the current user")
		}
		return "owned", nil
	}
	return "", fmt.Errorf("unknown brew precondition %s %s", c.Kind, c.Subject)
}

// Step returns the performer of one saved brew operation.
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	if op.Provider != Name || op.Action != "create" {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	if c := op.Params["cask"]; c != "" {
		v, appdir := op.Params["version"], op.Params["appdir"]
		if !formulaName.MatchString(c) || v == "" || appdir == "" || !isAbsPath(appdir) || len(op.Params) != 3 {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
		appName, teamID, allowRolling := p.AppName, p.TeamID, p.AllowRollingURL
		if p.plannedCask != nil && p.plannedCask.cask == c {
			appName = p.plannedCask.appName
			teamID = p.plannedCask.teamID
			allowRolling = p.plannedCask.allowRollingURL
		}
		if !validAppName(appName) || !teamIDPattern.MatchString(teamID) {
			return nil, fmt.Errorf("%w: operation %s missing cask bundle metadata", ErrUnsupported, op.ID)
		}
		return &caskStep{
			p:            p,
			cask:         c,
			version:      v,
			appdir:       appdir,
			appName:      appName,
			teamID:       teamID,
			allowRolling: allowRolling,
		}, nil
	}
	f, v := op.Params["formula"], op.Params["version"]
	if !formulaName.MatchString(f) || v == "" || len(op.Params) != 2 {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	return &step{p: p, formula: f, version: v}, nil
}

type step struct {
	p                *Provider
	formula, version string
}

func (s *step) Apply(ctx context.Context) (execute.Evidence, error) {
	r, err := s.p.run(ctx, "install", "--formula", s.formula)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "brew install --formula " + s.formula, "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("brew install exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return ev, nil
}

// Verify asks brew for the installed versions independently of the
// install's own exit status and requires the planned version.
func (s *step) Verify(ctx context.Context) (execute.Evidence, error) {
	r, err := s.p.run(ctx, "list", "--versions", "--formula", s.formula)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(r.Stdout))
	ev := execute.Evidence{"command": "brew list --versions --formula " + s.formula, "exit": r.Exit, "installed": strings.Join(fields[min(1, len(fields)):], " ")}
	if r.Exit != 0 || len(fields) < 2 || fields[0] != s.formula {
		return ev, fmt.Errorf("%s is not installed after brew install", s.formula)
	}
	for _, v := range fields[1:] {
		if v == s.version {
			ev["version"] = v
			return ev, nil
		}
	}
	return ev, fmt.Errorf("%s %s was planned but %v is installed", s.formula, s.version, fields[1:])
}

type caskStep struct {
	p            *Provider
	cask         string
	version      string
	appdir       string
	appName      string
	teamID       string
	allowRolling bool
}

func (s *caskStep) Apply(ctx context.Context) (execute.Evidence, error) {
	r, err := s.p.run(ctx, "install", "--cask", s.cask, "--appdir="+s.appdir)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "brew install --cask " + s.cask + " --appdir=" + s.appdir, "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("brew install exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return ev, nil
}

// Verify requires brew list --cask --versions <token> to name the planned
// version, the bundle at <appdir>/<app_name>, and the D5 signature check to
// report team_id.
func (s *caskStep) Verify(ctx context.Context) (execute.Evidence, error) {
	r, err := s.p.run(ctx, "list", "--cask", "--versions", s.cask)
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(string(r.Stdout))
	ev := execute.Evidence{
		"command":   "brew list --cask --versions " + s.cask,
		"exit":      r.Exit,
		"installed": strings.Join(fields[min(1, len(fields)):], " "),
	}
	if r.Exit != 0 || len(fields) < 2 || fields[0] != s.cask {
		return ev, fmt.Errorf("%s is not installed after brew install", s.cask)
	}
	matched := false
	for _, v := range fields[1:] {
		if v == s.version {
			matched = true
			break
		}
	}
	if !matched {
		return ev, fmt.Errorf("%s %s was planned but %v is installed", s.cask, s.version, fields[1:])
	}
	bundle := filepath.Join(s.appdir, s.appName)
	fi, err := os.Stat(bundle)
	if err != nil || !fi.IsDir() {
		return ev, fmt.Errorf("expected app bundle %s missing at %s", s.appName, bundle)
	}
	if err := codesign.VerifyWithEnv(ctx, s.p.Run, s.p.env(), bundle, s.teamID, s.allowRolling); err != nil {
		return ev, fmt.Errorf("%w: %v", ErrSignatureRejected, err)
	}
	ev["version"] = s.version
	ev["bundle"] = bundle
	ev["team_id"] = s.teamID
	return ev, nil
}

// Name is the provider name recipes use.
func (p *Provider) Name() string { return Name }

var _ provider.Provider = (*Provider)(nil)
