// Package npm is the built-in npm package provider for S3 (story X03).
// It plans and installs Node packages through the shared executor and
// never elevates: a prefix the current user cannot write fails the plan
// instead of being installed with another user's rights. Planning is
// read-only — it reads the tool's version, the installed state and the
// registry metadata over HTTPS, and writes nothing at all: no prefix, no
// cache, no configuration, no shell profile.
package npm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
)

// Name is the provider name in recipes.
const Name = "npm"

// DefaultRegistry is the public npm registry the provider resolves
// versions from and installs from.
const DefaultRegistry = "https://registry.npmjs.org"

// RequestTimeout bounds one registry request.
const RequestTimeout = 15 * time.Second

// maxRegistryBytes bounds one registry answer.
const maxRegistryBytes = 1 << 20

// Result, Runner and Exec are the shared process seam; this provider
// only ever starts the npm executable it found on the PATH.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// Provider is the npm provider bound to one host.
type Provider struct {
	// GOOS is the operating system whose conventions the target
	// follows: darwin, linux or windows. It is a field and not
	// runtime.GOOS, so one machine can prove every target's prefix.
	GOOS string
	Run  Runner
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// LookPath locates the npm executable; nil searches the PATH of the
	// injected environment.
	LookPath func(file string) (string, error)
	// Client is the HTTP client of the registry requests; nil uses the
	// default client, with TLS verification on and a total timeout.
	Client *http.Client
	// Registry is the registry base URL; nil uses the public registry.
	Registry string
	// Writable reports whether a directory is writable by the current
	// user; nil uses the OS checks.
	Writable func(path string) bool
}

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported = provider.Kind(provider.ErrUnsupported, "unsupported by the npm provider")
	ErrPrivilege   = provider.Kind(provider.ErrPrivilege, "the npm prefix is not writable by the current user")
	ErrMissing     = provider.Kind(provider.ErrMissing, "npm is not installed")
)

// Env is the explicit npm environment every invocation runs with: no
// update notifier, no funding or audit requests. npm never edits a shell
// profile, and this provider writes no npm configuration.
var Env = []string{
	"npm_config_update_notifier=false",
	"npm_config_fund=false",
	"npm_config_audit=false",
}

// defaultClient is the client the registry requests use unless a caller
// injects its own: TLS verification on and a total timeout.
var defaultClient = &http.Client{Timeout: RequestTimeout}

// Request and Resolution are the shared provider contract.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

var versionLine = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.\-]+)?$`)

// Version reports the installed npm version.
func (p *Provider) Version(ctx context.Context) (string, error) {
	return p.version(ctx)
}

// version reads the tool's own version.
func (p *Provider) version(ctx context.Context) (string, error) {
	path, err := p.tool()
	if err != nil {
		return "", err
	}
	r, err := p.Run(ctx, path, []string{"--version"}, p.env("", false))
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(string(r.Stdout))
	if r.Exit != 0 || !versionLine.MatchString(out) {
		return "", fmt.Errorf("npm --version exited %d: %s", r.Exit, firstLine(r.Stdout, r.Stderr))
	}
	return out, nil
}

// tool locates the npm executable on the PATH of the injected
// environment. Its absolute path is recorded in the plan, so a tool that
// moved or changed rejects the plan.
func (p *Provider) tool() (string, error) {
	path, err := p.lookPath("npm")
	if err != nil || path == "" {
		return "", fmt.Errorf("%w: npm is not on the PATH", ErrMissing)
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	return path, nil
}

// lookPath finds an executable. The injected LookPath decides where it
// is; without one the provider searches the PATH of the injected
// environment, so the tool is never found outside it.
func (p *Provider) lookPath(file string) (string, error) {
	if p.LookPath != nil {
		return p.LookPath(file)
	}
	return p.lookPathIn(file)
}

// lookPathIn searches the PATH of the injected environment.
func (p *Provider) lookPathIn(file string) (string, error) {
	names := []string{file}
	if p.GOOS == "windows" {
		// npm ships a .cmd shim on Windows; the bare name is not there.
		names = []string{file + ".cmd", file + ".bat", file + ".ps1", file + ".exe"}
	}
	for _, dir := range filepath.SplitList(envValue(p.baseEnv(), "PATH")) {
		if dir == "" {
			dir = "."
		}
		for _, name := range names {
			path := filepath.Join(dir, name)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				continue
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH", file)
}

// baseEnv is the injected environment.
func (p *Provider) baseEnv() []string {
	if p.Environ == nil {
		return nil
	}
	return p.Environ()
}

// env builds the explicit environment of one invocation: the injected
// base environment without any npm configuration, plus the provider's
// own variables and, for a custom root, the cache inside that root. A
// machine's npm configuration can therefore never redirect the registry
// or the cache this provider uses.
func (p *Provider) env(prefix string, custom bool) []string {
	base := p.baseEnv()
	out := make([]string, 0, len(base)+len(Env)+1)
	for _, kv := range base {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "NPM_CONFIG_") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, Env...)
	if custom {
		out = append(out, "npm_config_cache="+filepath.Join(prefix, ".npm-cache"))
	}
	return out
}

// prefix is the installation prefix of one target: the per-user prefix
// for the user target, the root itself for a custom target. The system
// global prefix is never used.
func (p *Provider) prefix(target string) (string, bool, error) {
	switch {
	case target == "user":
		switch p.GOOS {
		case "windows":
			app := envValue(p.baseEnv(), "APPDATA")
			if app == "" {
				return "", false, fmt.Errorf("%w: the user target on Windows needs APPDATA", ErrUnsupported)
			}
			return filepath.Join(app, "npm"), false, nil
		case "darwin", "linux":
			home := envValue(p.baseEnv(), "HOME")
			if home == "" {
				return "", false, fmt.Errorf("%w: the user target needs HOME", ErrUnsupported)
			}
			return filepath.Join(home, ".local"), false, nil
		}
		return "", false, fmt.Errorf("%w: unknown operating system %q", ErrUnsupported, p.GOOS)
	case strings.HasPrefix(target, "custom:"):
		root := strings.TrimPrefix(target, "custom:")
		if root == "" || !filepath.IsAbs(root) {
			return "", false, fmt.Errorf("%w: custom root %q is not an absolute path", ErrUnsupported, root)
		}
		return filepath.Clean(root), true, nil
	}
	return "", false, fmt.Errorf("%w: target %s; npm never installs into the system global prefix", ErrUnsupported, target)
}

// checkRoot refuses a prefix the current user cannot write. The nearest
// existing ancestor must be writable, so a root that does not exist yet
// is created inside a directory the user owns. managed-machine never
// elevates and never installs as the prefix owner.
func (p *Provider) checkRoot(prefix string) error {
	info, err := os.Stat(prefix)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("%w: %s is not a directory", ErrUnsupported, prefix)
	case err == nil:
		if !p.writable(prefix) {
			return fmt.Errorf("%w (%s); managed-machine never elevates", ErrPrivilege, prefix)
		}
	case os.IsNotExist(err):
		ancestor := nearestExisting(prefix)
		if ancestor == "" {
			return fmt.Errorf("%w: %s has no existing ancestor", ErrUnsupported, prefix)
		}
		if !p.writable(ancestor) {
			return fmt.Errorf("%w (%s); managed-machine never elevates", ErrPrivilege, ancestor)
		}
	default:
		return err
	}
	return nil
}

// nearestExisting is the closest existing ancestor of a path that does
// not exist yet.
func nearestExisting(path string) string {
	for p := filepath.Dir(path); ; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		if parent := filepath.Dir(p); parent == p {
			return ""
		}
	}
}

func (p *Provider) writable(path string) bool {
	if p.Writable != nil {
		return p.Writable(path)
	}
	return writable(path)
}

// registryBase is the registry the provider talks to.
func (p *Provider) registryBase() string {
	base := p.Registry
	if base == "" {
		base = DefaultRegistry
	}
	return strings.TrimSuffix(base, "/")
}

// registryURL is the registry the provider installs from.
func (p *Provider) registryURL() string { return p.registryBase() + "/" }

// client is the HTTP client of the registry requests.
func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return defaultClient
}

// latestVersion reads the registry's metadata for a package over HTTPS
// and returns the version it publishes as latest. The registry is asked
// directly and never through npm, so no machine configuration can
// redirect the lookup.
func (p *Provider) latestVersion(ctx context.Context, pkg string) (string, error) {
	base := p.registryBase()
	u := base + "/" + url.PathEscape(pkg) + "/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("the registry at %s: %v", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the registry at %s answered %s for %s", base, resp.Status, pkg)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRegistryBytes))
	if err != nil {
		return "", fmt.Errorf("the registry at %s: %v", base, err)
	}
	var doc struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("the registry at %s: %v", base, err)
	}
	if doc.Version == "" {
		return "", fmt.Errorf("the registry at %s names no version for %s", base, pkg)
	}
	return doc.Version, nil
}

// installed reads the installed state of one package in a prefix. npm
// exits 1 when the package is not there, which means it is not
// installed; a malformed answer is an error.
func (p *Provider) installed(ctx context.Context, prefix, pkg string, custom bool) (string, bool, error) {
	path, err := p.tool()
	if err != nil {
		return "", false, err
	}
	args := []string{"ls", "--global", "--prefix", prefix, "--json", "--depth=0", pkg}
	r, err := p.Run(ctx, path, args, p.env(prefix, custom))
	if err != nil {
		return "", false, err
	}
	out := strings.TrimSpace(string(r.Stdout))
	if out == "" {
		return "", false, nil
	}
	var doc struct {
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return "", false, fmt.Errorf("npm ls %s: %v", pkg, err)
	}
	d, ok := doc.Dependencies[pkg]
	if !ok || d.Version == "" {
		return "", false, nil
	}
	return d.Version, true, nil
}

// Plan resolves a package without changing the machine.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	pkg := r.Options["package"]
	for k := range r.Options {
		if k != "package" {
			return nil, fmt.Errorf("%w: option %q (the npm provider installs exactly one package)", ErrUnsupported, k)
		}
	}
	if pkg == "" {
		return nil, fmt.Errorf("%w: the recipe names no package", ErrUnsupported)
	}
	if err := validPackage(pkg); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupported, err)
	}
	prefix, custom, err := p.prefix(r.Target)
	if err != nil {
		return nil, err
	}
	tool, err := p.tool()
	if err != nil {
		return nil, err
	}
	if err := p.checkRoot(prefix); err != nil {
		return nil, err
	}
	ver, err := p.version(ctx)
	if err != nil {
		return nil, err
	}
	latest, err := p.latestVersion(ctx, pkg)
	if err != nil {
		return nil, err
	}
	installed, ok, err := p.installed(ctx, prefix, pkg, custom)
	if err != nil {
		return nil, err
	}
	res := &Resolution{Package: pkg, Version: latest, ProviderVersion: ver, Preconditions: []plan.Precondition{
		{Kind: "software", Subject: Name, Requirement: "present at " + tool, Observed: ver},
		{Kind: "target", Subject: "npm-prefix", Requirement: "writable by the current user", Observed: prefix},
	}}
	if ok && installed == latest {
		res.Installed = true
		return res, nil
	}
	action := "create"
	if ok {
		action = "update"
	}
	res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: action, Provider: Name,
		Params: map[string]string{"package": pkg, "version": latest, "prefix": prefix}}}
	return res, nil
}

// Check re-evaluates one of this provider's preconditions at apply time.
func (p *Provider) Check(ctx context.Context, c plan.Precondition) (string, error) {
	switch {
	case c.Kind == "software" && c.Subject == Name:
		path, err := p.tool()
		if err != nil {
			return "absent", err
		}
		if planned := strings.TrimPrefix(c.Requirement, "present at "); planned != path {
			return path, fmt.Errorf("npm moved from %s to %s", planned, path)
		}
		v, err := p.version(ctx)
		if err != nil {
			return "absent", err
		}
		if v != c.Observed {
			return v, fmt.Errorf("npm changed from %s to %s", c.Observed, v)
		}
		return v, nil
	case c.Kind == "target" && c.Subject == "npm-prefix":
		if err := p.checkRoot(c.Observed); err != nil {
			return "not writable", err
		}
		return "writable", nil
	}
	return "", fmt.Errorf("unknown npm precondition %s %s", c.Kind, c.Subject)
}

// Step returns the performer of one saved npm operation.
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	pkg, ver, prefix := op.Params["package"], op.Params["version"], op.Params["prefix"]
	if op.Provider != Name || (op.Action != "create" && op.Action != "update") || len(op.Params) != 3 ||
		pkg == "" || ver == "" || prefix == "" || validPackage(pkg) != nil {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	return &step{p: p, pkg: pkg, version: ver, prefix: prefix, custom: strings.HasPrefix(op.Target, "custom:")}, nil
}

type step struct {
	p            *Provider
	pkg, version string
	prefix       string
	custom       bool
}

// Apply installs exactly the planned version. Lifecycle scripts never
// run, and the custom root is created here, at apply time: planning
// wrote nothing.
func (s *step) Apply(ctx context.Context) (execute.Evidence, error) {
	if s.custom {
		if err := os.MkdirAll(s.prefix, 0o755); err != nil {
			return nil, err
		}
	}
	path, err := s.p.tool()
	if err != nil {
		return nil, err
	}
	args := []string{"install", "--global", "--prefix", s.prefix, "--ignore-scripts", "--no-fund", "--no-audit",
		"--registry=" + s.p.registryURL(), s.pkg + "@" + s.version}
	r, err := s.p.Run(ctx, path, args, s.p.env(s.prefix, s.custom))
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "npm " + strings.Join(args, " "), "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("npm install exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return ev, nil
}

// Verify asks npm for the installed state independently of the install's
// own exit status and requires the planned version.
func (s *step) Verify(ctx context.Context) (execute.Evidence, error) {
	ev := execute.Evidence{"command": "npm ls --global --prefix " + s.prefix + " --json --depth=0 " + s.pkg}
	v, ok, err := s.p.installed(ctx, s.prefix, s.pkg, s.custom)
	if err != nil {
		return ev, err
	}
	if !ok {
		return ev, fmt.Errorf("%s is not installed after npm install", s.pkg)
	}
	ev["installed"] = v
	if v != s.version {
		return ev, fmt.Errorf("%s %s was planned but %s is installed", s.pkg, s.version, v)
	}
	ev["version"] = v
	return ev, nil
}

// Name is the provider name recipes use.
func (p *Provider) Name() string { return Name }

// validPackage checks an npm package name: an optional scope, a name,
// and nothing else.
func validPackage(name string) error {
	if name == "" {
		return errors.New("the package name is empty")
	}
	if len(name) > 214 {
		return fmt.Errorf("the package name %q is longer than 214 characters", name)
	}
	parts := strings.Split(name, "/")
	switch len(parts) {
	case 1:
		return validName(parts[0])
	case 2:
		if !strings.HasPrefix(parts[0], "@") || parts[0] == "@" {
			return fmt.Errorf("%q is not a scoped package name", name)
		}
		if err := validName(strings.TrimPrefix(parts[0], "@")); err != nil {
			return err
		}
		return validName(parts[1])
	}
	return fmt.Errorf("%q is not an npm package name", name)
}

// validName checks one part of a package name: no leading dot or
// underscore, and only characters that are safe in a URL.
func validName(s string) error {
	if s == "" {
		return errors.New("the package name has an empty part")
	}
	if s[0] == '.' || s[0] == '_' {
		return fmt.Errorf("the package name part %q starts with %q", s, string(s[0]))
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~':
		default:
			return fmt.Errorf("the package name part %q has an unexpected character %q", s, string(r))
		}
	}
	return nil
}

// envValue is the value of one variable of an environment.
func envValue(env []string, name string) string {
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func firstLine(bs ...[]byte) string {
	for _, b := range bs {
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.SplitN(s, "\n", 2)[0]
		}
	}
	return ""
}

var _ provider.Provider = (*Provider)(nil)
