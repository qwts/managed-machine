// Package cargo is the built-in Rust crate provider for S3 (story X03).
// It plans and installs crates through the shared executor and never
// elevates: the cargo root is the user's own cargo root or a custom root
// the current user may write, and a root it may not write fails the plan
// instead of running cargo as another user. Planning is read-only: it
// reads the tool's version and the installed state and resolves the
// crate's version from the crates.io sparse index over HTTPS, but it never
// creates a root, a cache or any other file.
package cargo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
)

// Name is the provider name in recipes.
const Name = "cargo"

// DefaultIndexBase is the crates.io sparse index the provider reads when
// no other base is injected.
const DefaultIndexBase = "https://index.crates.io"

// indexTimeout bounds one index request.
const indexTimeout = 15 * time.Second

// maxIndexBytes bounds one index document.
const maxIndexBytes = 4 << 20

// Result, Runner and Exec are the shared process seam; this provider only
// ever starts the cargo executable it located itself.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// DefaultClient is the HTTP client the index is read with when the
// provider has none: TLS verification on and a bounded timeout.
func DefaultClient() *http.Client { return &http.Client{Timeout: indexTimeout} }

// Provider is the cargo provider. Every field is a seam for tests.
type Provider struct {
	// Run starts the cargo tool.
	Run Runner
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// LookPath locates the cargo executable. When it is nil the provider
	// searches the PATH of the injected environment.
	LookPath func(file string) (string, error)
	// GOOS is the operating system the cargo root layout is derived for.
	// When it is empty the layout follows the injected environment.
	GOOS string
	// IndexBase is the sparse index base URL; empty means
	// DefaultIndexBase.
	IndexBase string
	// Client reads the index; nil means DefaultClient.
	Client *http.Client
	// Writable reports on the cargo root; nil uses the OS checks.
	Writable func(path string) bool
}

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported = provider.Kind(provider.ErrUnsupported, "unsupported by the cargo provider")
	ErrPrivilege   = provider.Kind(provider.ErrPrivilege, "the cargo root is not writable by the current user")
	ErrMissing     = provider.Kind(provider.ErrMissing, "cargo is not installed")
)

// Request and Resolution are the shared provider contract.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

// target is one resolved installation target: the user's own cargo root
// or a custom root.
type target struct {
	kind   string // "user" or "custom"
	root   string // the cargo install root
	custom bool
}

// crateName is a crates.io crate name: lowercase letters, digits, dashes
// and underscores.
var crateName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var versionLine = regexp.MustCompile(`^cargo ([0-9]+\.[0-9]+\.[0-9]+)`)

// installedLine is one line of `cargo install --list`: "<crate> v<version>:"
// followed by the installed binaries.
var installedLine = regexp.MustCompile(`^(\S+) v(\S+):$`)

// Name is the provider name recipes use.
func (p *Provider) Name() string { return Name }

// Version reports the installed cargo version.
func (p *Provider) Version(ctx context.Context) (string, error) {
	tool, err := p.locate()
	if err != nil {
		return "", err
	}
	return p.version(ctx, tool)
}

// version runs the located tool's own version command.
func (p *Provider) version(ctx context.Context, tool string) (string, error) {
	r, err := p.run(ctx, tool, nil, "--version")
	if err != nil {
		return "", err
	}
	m := versionLine.FindSubmatch(r.Stdout)
	if r.Exit != 0 || m == nil {
		return "", fmt.Errorf("cargo --version exited %d: %s", r.Exit, firstLine(r.Stdout, r.Stderr))
	}
	return string(m[1]), nil
}

// locate finds the cargo executable. The absolute path is what the plan
// pins, so a moved tool rejects the plan instead of running somewhere
// else.
func (p *Provider) locate() (string, error) {
	var (
		path string
		err  error
	)
	if p.LookPath != nil {
		path, err = p.LookPath("cargo")
	} else {
		path, err = lookPathIn(p.goos(), "cargo", p.envValue("PATH"))
	}
	if err != nil || path == "" {
		return "", fmt.Errorf("%w: cargo was not found in the PATH", ErrMissing)
	}
	return path, nil
}

// goos returns the operating system the root layout is derived for. The
// Provider's GOOS field decides it; without one the layout follows the
// injected environment, where Windows names the home USERPROFILE.
func (p *Provider) goos() string {
	if p.GOOS != "" {
		return p.GOOS
	}
	if p.envValue("USERPROFILE") != "" && p.envValue("HOME") == "" {
		return "windows"
	}
	return ""
}

// env returns the explicit environment of one invocation: the injected
// base environment without the variables the provider owns, plus the
// variables the target needs. Nothing else is changed: no shell profile
// is edited and no PATH configuration is written.
func (p *Provider) env(t *target) []string {
	base := []string{}
	if p.Environ != nil {
		base = p.Environ()
	}
	out := make([]string, 0, len(base)+2)
	for _, kv := range base {
		switch name(kv) {
		case "CARGO_INSTALL_ROOT", "CARGO_TARGET_DIR":
			// The provider owns the root: an inherited value must not
			// redirect the install somewhere else.
			continue
		case "CARGO_HOME":
			if t != nil && t.custom {
				// A custom root keeps its own registry cache.
				continue
			}
		}
		out = append(out, kv)
	}
	if t != nil && t.custom {
		out = append(out,
			"CARGO_HOME="+filepath.Join(t.root, "cargo-home"),
			"CARGO_TARGET_DIR="+filepath.Join(t.root, "target"))
	}
	return out
}

// envValue returns one variable of the injected environment.
func (p *Provider) envValue(key string) string {
	base := []string{}
	if p.Environ != nil {
		base = p.Environ()
	}
	prefix := key + "="
	for _, kv := range base {
		if strings.HasPrefix(kv, prefix) {
			return strings.TrimPrefix(kv, prefix)
		}
	}
	return ""
}

// name returns the variable name of an environment entry.
func name(kv string) string {
	if i := strings.IndexByte(kv, '='); i >= 0 {
		return kv[:i]
	}
	return kv
}

// run starts the located tool with an explicit environment.
func (p *Provider) run(ctx context.Context, tool string, t *target, args ...string) (Result, error) {
	return p.Run(ctx, tool, args, p.env(t))
}

func firstLine(bs ...[]byte) string {
	for _, b := range bs {
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.SplitN(s, "\n", 2)[0]
		}
	}
	return ""
}

// Plan resolves a crate without changing the machine.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	crate, err := p.crate(r.Options)
	if err != nil {
		return nil, err
	}
	t, err := p.target(r.Target)
	if err != nil {
		return nil, err
	}
	tool, err := p.locate()
	if err != nil {
		return nil, err
	}
	ver, err := p.version(ctx, tool)
	if err != nil {
		return nil, err
	}
	if err := p.checkRoot(t.root); err != nil {
		return nil, err
	}
	installed, err := p.installed(ctx, tool, t)
	if err != nil {
		return nil, err
	}
	version, err := p.resolve(ctx, crate)
	if err != nil {
		return nil, err
	}
	res := &Resolution{Package: crate, Version: version, ProviderVersion: ver,
		Preconditions: []plan.Precondition{
			{Kind: "software", Subject: Name, Requirement: "present at " + tool, Observed: ver},
			{Kind: "target", Subject: "cargo-root", Requirement: "writable at " + t.root, Observed: "writable"},
		}}
	if v, ok := installed[crate]; ok && v == version {
		// The planned version is installed: there is nothing to do.
		res.Installed = true
		return res, nil
	}
	action := "create"
	if _, ok := installed[crate]; ok {
		action = "update"
	}
	res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: action, Provider: Name,
		Params: map[string]string{"package": crate, "version": version, "root": t.root}}}
	return res, nil
}

// crate reads and validates the recipe's options.
func (p *Provider) crate(options map[string]string) (string, error) {
	for k := range options {
		if k != "crate" {
			return "", fmt.Errorf("%w: option %q; a cargo recipe names one crate", ErrUnsupported, k)
		}
	}
	crate := options["crate"]
	switch {
	case crate == "":
		return "", fmt.Errorf("%w: the recipe names no crate", ErrUnsupported)
	case !crateName.MatchString(crate):
		return "", fmt.Errorf("%w: %q is not a crate name", ErrUnsupported, crate)
	}
	return crate, nil
}

// target resolves the requested installation target. Cargo installs into
// the user's cargo root or into a custom root; a machine-wide target is
// refused, and so is a relative custom root.
func (p *Provider) target(s string) (*target, error) {
	switch {
	case s == "user":
		root, err := p.userRoot()
		if err != nil {
			return nil, err
		}
		return &target{kind: "user", root: root}, nil
	case s == "global":
		return nil, fmt.Errorf("%w: target global; cargo installs into the user's cargo root or a custom root, never a machine-wide one", ErrUnsupported)
	case strings.HasPrefix(s, "custom:"):
		path := strings.TrimPrefix(s, "custom:")
		if path == "" || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("%w: target %s; a custom root is an absolute path", ErrUnsupported, s)
		}
		return &target{kind: "custom", root: filepath.Clean(path), custom: true}, nil
	default:
		return nil, fmt.Errorf("%w: target %s; supported targets are user and custom:<absolute path>", ErrUnsupported, s)
	}
}

// userRoot is cargo's own default root: $HOME/.cargo on unix and
// %USERPROFILE%\.cargo on Windows. The binaries land in its bin
// directory.
func (p *Provider) userRoot() (string, error) {
	if p.goos() == "windows" {
		home := p.envValue("USERPROFILE")
		if home == "" {
			return "", fmt.Errorf("%w: the environment names no USERPROFILE, so the cargo root is unknown", ErrUnsupported)
		}
		return filepath.Join(home, ".cargo"), nil
	}
	home := p.envValue("HOME")
	if home == "" {
		return "", fmt.Errorf("%w: the environment names no HOME, so the cargo root is unknown", ErrUnsupported)
	}
	return filepath.Join(home, ".cargo"), nil
}

// checkRoot refuses a root the current user may not write. A root that
// does not exist yet is judged by its nearest existing ancestor; a root
// that exists and is not a directory is refused as unsupported.
func (p *Provider) checkRoot(root string) error {
	info, err := os.Stat(root)
	switch {
	case err == nil && !info.IsDir():
		return fmt.Errorf("%w: the root %s exists and is not a directory", ErrUnsupported, root)
	case err == nil:
		if !p.writable(root) {
			return fmt.Errorf("%w (%s); managed-machine never elevates or runs cargo as another user", ErrPrivilege, root)
		}
		return nil
	case !os.IsNotExist(err):
		return fmt.Errorf("the root %s cannot be read: %v", root, err)
	}
	for ancestor := filepath.Dir(root); ; ancestor = filepath.Dir(ancestor) {
		info, err := os.Stat(ancestor)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%w: %s is not a directory", ErrUnsupported, ancestor)
			}
			if !p.writable(ancestor) {
				return fmt.Errorf("%w (%s); managed-machine never elevates or runs cargo as another user", ErrPrivilege, ancestor)
			}
			return nil
		}
		if !os.IsNotExist(err) {
			return fmt.Errorf("the root %s cannot be read: %v", root, err)
		}
		if parent := filepath.Dir(ancestor); parent == ancestor {
			return nil
		}
	}
}

// writable reports whether the current user may write the path.
func (p *Provider) writable(path string) bool {
	if p.Writable != nil {
		return p.Writable(path)
	}
	return writable(path)
}

// installed reads the packages installed in the root. The read-only
// listing never creates the root.
func (p *Provider) installed(ctx context.Context, tool string, t *target) (map[string]string, error) {
	r, err := p.run(ctx, tool, nil, "install", "--list", "--root", t.root)
	if err != nil {
		return nil, err
	}
	if r.Exit != 0 {
		// A root that does not exist yet holds nothing.
		if _, serr := os.Stat(t.root); os.IsNotExist(serr) {
			return map[string]string{}, nil
		}
		return nil, fmt.Errorf("cargo install --list exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return parseInstalled(r.Stdout), nil
}

// parseInstalled reads the "<crate> v<version>:" lines of a listing.
func parseInstalled(out []byte) map[string]string {
	installed := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if m := installedLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			installed[m[1]] = m[2]
		}
	}
	return installed
}

// resolve returns the highest stable version of the crate, read from the
// crates.io sparse index over HTTPS.
func (p *Provider) resolve(ctx context.Context, crate string) (string, error) {
	url := p.indexURL(crate)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("the crate index for %s: %v", crate, err)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("the crate index for %s: %v", crate, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: the crate index knows no crate %q", ErrUnsupported, crate)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the crate index for %s: %s", crate, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBytes+1))
	if err != nil {
		return "", fmt.Errorf("the crate index for %s: %v", crate, err)
	}
	if len(body) > maxIndexBytes {
		return "", fmt.Errorf("the crate index for %s is larger than %d bytes", crate, maxIndexBytes)
	}
	return pickVersion(body, crate)
}

// client is the HTTP client the index is read with.
func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return DefaultClient()
}

// indexURL is the sparse index document of one crate.
func (p *Provider) indexURL(crate string) string {
	base := p.IndexBase
	if base == "" {
		base = DefaultIndexBase
	}
	return strings.TrimSuffix(base, "/") + "/" + indexPath(crate)
}

// indexPath is the sparse index layout: one and two character names sit
// in their own directory, three character names under their first
// character, and longer names under their first two and next two
// characters.
func indexPath(name string) string {
	switch len(name) {
	case 1:
		return "1/" + name
	case 2:
		return "2/" + name
	case 3:
		return "3/" + name[:1] + "/" + name
	default:
		return name[:2] + "/" + name[2:4] + "/" + name
	}
}

// indexEntry is one line of a sparse index document.
type indexEntry struct {
	Name   string `json:"name"`
	Vers   string `json:"vers"`
	Yanked bool   `json:"yanked"`
}

// pickVersion chooses the highest non-yanked version without a
// pre-release part.
func pickVersion(body []byte, crate string) (string, error) {
	best := ""
	var bestv semver
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e indexEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			return "", fmt.Errorf("the crate index for %s has a malformed line: %v", crate, err)
		}
		if e.Yanked {
			continue
		}
		v, ok := parseVersion(e.Vers)
		if !ok || v.pre != "" {
			// A version that is not semver, or a pre-release, is not a
			// candidate.
			continue
		}
		if best == "" || v.compare(bestv) > 0 {
			best, bestv = e.Vers, v
		}
	}
	if best == "" {
		return "", fmt.Errorf("%w: the crate index for %s has no stable version", ErrUnsupported, crate)
	}
	return best, nil
}

// semver is a parsed version: the numeric core and the pre-release part.
type semver struct {
	major, minor, patch int
	pre                 string
}

// parseVersion parses a semantic version.
func parseVersion(s string) (semver, bool) {
	var v semver
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = s[i+1:]
		if v.pre == "" {
			return semver{}, false
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return semver{}, false
	}
	nums := [3]int{}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return semver{}, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, true
}

// compare orders two versions by their numeric core.
func (v semver) compare(o semver) int {
	for _, pair := range [][2]int{{v.major, o.major}, {v.minor, o.minor}, {v.patch, o.patch}} {
		if pair[0] != pair[1] {
			if pair[0] > pair[1] {
				return 1
			}
			return -1
		}
	}
	return 0
}

// Check re-evaluates one of this provider's preconditions at apply time.
func (p *Provider) Check(ctx context.Context, c plan.Precondition) (string, error) {
	switch {
	case c.Kind == "software" && c.Subject == Name:
		path := strings.TrimPrefix(c.Requirement, "present at ")
		tool, err := p.locate()
		if err != nil {
			return "absent", err
		}
		if tool != path {
			return tool, fmt.Errorf("cargo moved from %s to %s", path, tool)
		}
		v, err := p.version(ctx, tool)
		if err != nil {
			return "absent", err
		}
		if v != c.Observed {
			return v, fmt.Errorf("cargo changed from %s to %s", c.Observed, v)
		}
		return v, nil
	case c.Kind == "target" && c.Subject == "cargo-root":
		root := strings.TrimPrefix(c.Requirement, "writable at ")
		if err := p.checkRoot(root); err != nil {
			return "not writable", err
		}
		return "writable", nil
	}
	return "", fmt.Errorf("unknown cargo precondition %s %s", c.Kind, c.Subject)
}

// Step returns the performer of one saved cargo operation.
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	crate, version, root := op.Params["package"], op.Params["version"], op.Params["root"]
	if op.Provider != Name || (op.Action != "create" && op.Action != "update") ||
		len(op.Params) != 3 || !crateName.MatchString(crate) || !validVersion(version) ||
		root == "" || !filepath.IsAbs(root) {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	t, err := p.stepTarget(op, root)
	if err != nil {
		return nil, err
	}
	return &step{p: p, crate: crate, version: version, t: t, force: op.Action == "update"}, nil
}

// stepTarget decides how the operation's environment is built: a custom
// root keeps the registry cache and the build output inside the root, the
// user's root is cargo's own default.
func (p *Provider) stepTarget(op plan.Operation, root string) (*target, error) {
	user, err := p.userRoot()
	if err != nil {
		user = ""
	}
	t := &target{kind: "custom", root: root, custom: user == "" || filepath.Clean(root) != filepath.Clean(user)}
	switch {
	case op.Target == "":
		// The pinned root alone decides.
	case op.Target == "user":
		if t.custom {
			return nil, fmt.Errorf("%w: operation %s names the user target but pins the root %s", ErrUnsupported, op.ID, root)
		}
		t.kind = "user"
	case strings.HasPrefix(op.Target, "custom:"):
		path := strings.TrimPrefix(op.Target, "custom:")
		if filepath.Clean(path) != filepath.Clean(root) {
			return nil, fmt.Errorf("%w: operation %s names the custom root %s but pins %s", ErrUnsupported, op.ID, path, root)
		}
		t.kind, t.custom = "custom", true
	default:
		return nil, fmt.Errorf("%w: operation %s names the target %s", ErrUnsupported, op.ID, op.Target)
	}
	return t, nil
}

// validVersion reports whether s is a semantic version.
func validVersion(s string) bool {
	v, ok := parseVersion(s)
	return ok && v.pre == ""
}

// step performs one saved cargo operation.
type step struct {
	p       *Provider
	crate   string
	version string
	t       *target
	force   bool
}

// Apply installs exactly the pinned version.
func (s *step) Apply(ctx context.Context) (execute.Evidence, error) {
	tool, err := s.p.locate()
	if err != nil {
		return nil, err
	}
	// The root is created here, at apply time; planning never creates it.
	if err := os.MkdirAll(s.t.root, 0o755); err != nil {
		return nil, fmt.Errorf("the cargo root %s could not be created: %v", s.t.root, err)
	}
	args := []string{"install", s.crate, "--version", "=" + s.version, "--locked", "--root", s.t.root}
	if s.force {
		args = append(args, "--force")
	}
	r, err := s.p.run(ctx, tool, s.t, args...)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "cargo " + strings.Join(args, " "), "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("cargo install exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return ev, nil
}

// Verify asks cargo for the installed state independently of the
// install's own exit status and requires the planned version.
func (s *step) Verify(ctx context.Context) (execute.Evidence, error) {
	tool, err := s.p.locate()
	if err != nil {
		return nil, err
	}
	r, err := s.p.run(ctx, tool, s.t, "install", "--list", "--root", s.t.root)
	if err != nil {
		return nil, err
	}
	installed := parseInstalled(r.Stdout)
	ev := execute.Evidence{"command": "cargo install --list --root " + s.t.root, "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("cargo install --list exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	v, ok := installed[s.crate]
	if !ok {
		return ev, fmt.Errorf("%s is not installed after cargo install", s.crate)
	}
	ev["installed"] = v
	if v != s.version {
		return ev, fmt.Errorf("%s %s was planned but %s is installed", s.crate, s.version, v)
	}
	ev["version"] = v
	return ev, nil
}

var _ provider.Provider = (*Provider)(nil)
