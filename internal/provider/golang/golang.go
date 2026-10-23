// Package golang is the built-in Go module provider for S3 (story X02).
// It installs Go command-line tools with `go install <package>@<version>`:
// planning is read-only, the version is resolved from the module proxy over
// HTTPS and pinned in the plan, and the installed binary is verified
// independently of the install's own exit status. The provider never
// elevates, never edits a shell profile and never writes outside the
// target it was asked for.
package golang

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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

// Name is the provider name recipes use.
const Name = "go"

// DefaultProxyBase is the module proxy a version is resolved from when no
// other base is configured.
const DefaultProxyBase = "https://proxy.golang.org"

// RequestTimeout is the total time one proxy request may take.
const RequestTimeout = 15 * time.Second

// maxResponseBytes bounds the proxy's answer, so a hostile or broken
// proxy cannot fill the memory of the machine.
const maxResponseBytes = 1 << 20

// Result and Runner are the shared process seam; this provider only ever
// starts the go tool it located itself.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported = provider.Kind(provider.ErrUnsupported, "unsupported by the go provider")
	ErrPrivilege   = provider.Kind(provider.ErrPrivilege, "the target root is not writable by the current user")
	ErrMissing     = provider.Kind(provider.ErrMissing, "the go tool is not installed")
)

// Provider installs Go packages with the go tool of the machine it runs
// on. Every seam is injectable, so a test never reaches the real tool,
// the real PATH or the real network.
type Provider struct {
	// GOOS is the target's operating system: darwin, linux or windows. The
	// provider follows the target, never the running binary, so the paths
	// it plans are the paths the target has.
	GOOS string
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// LookPath finds the go tool on the PATH of the injected environment.
	// It is injectable so a test never searches the machine's PATH; nil
	// uses the package's own search over that PATH.
	LookPath func(file string, env []string) (string, error)
	// Run starts one process.
	Run Runner
	// ProxyBase is the module proxy a version is resolved from; empty
	// means DefaultProxyBase.
	ProxyBase string
	// Client fetches the proxy; nil means DefaultClient.
	Client *http.Client
	// Writable reports on a path; nil uses the OS check.
	Writable func(path string) bool
}

// Request and Resolution are the shared provider contract.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

// owned are the environment variables this provider sets itself. They are
// removed from the injected environment first, so a value left over in a
// shell profile or a developer's export can never reach a process.
var owned = []string{"GOBIN", "GOFLAGS", "GOPATH", "GOMODCACHE", "GOCACHE", "GOENV", "GOTOOLCHAIN", "CGO_ENABLED"}

// LookPath finds an executable on the PATH of env. It is the production
// lookup: the PATH comes from the injected environment, never from the
// process that runs the core, so the tool a plan names is the tool that
// environment would run.
func LookPath(file string, env []string) (string, error) {
	for _, dir := range filepath.SplitList(envValue(env, "PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, file)
		if executable(p) {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not on the PATH", file)
}

// environ is the injected environment.
func (p *Provider) environ() []string {
	if p.Environ == nil {
		return os.Environ()
	}
	return p.Environ()
}

// baseEnv is the injected environment without the variables this provider
// owns.
func (p *Provider) baseEnv() []string {
	base := p.environ()
	out := make([]string, 0, len(base))
	for _, kv := range base {
		if !ownedVar(kv) {
			out = append(out, kv)
		}
	}
	return out
}

// ownedVar reports whether the environment entry names a variable this
// provider sets itself.
func ownedVar(kv string) bool {
	name := kv
	if i := strings.IndexByte(kv, '='); i >= 0 {
		name = kv[:i]
	}
	for _, o := range owned {
		if strings.EqualFold(name, o) {
			return true
		}
	}
	return false
}

// envValue returns the value of name in env.
func envValue(env []string, name string) string {
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i < 0 {
			continue
		}
		if strings.EqualFold(kv[:i], name) {
			return kv[i+1:]
		}
	}
	return ""
}

// layout is where one target keeps its binaries and, for a custom root,
// its module and build caches.
type layout struct {
	root   string // the target root
	bin    string // the directory the binaries are installed into
	custom bool   // a custom root keeps its caches inside itself
}

// layoutFor returns where the target's binaries go. The user target uses
// the go tool's default GOBIN; a custom root keeps everything inside
// itself. Nothing is created here: planning writes nothing.
func (p *Provider) layoutFor(target string) (layout, error) {
	switch {
	case target == "user":
		home, err := p.home()
		if err != nil {
			return layout{}, err
		}
		root := filepath.Join(home, "go")
		return layout{root: root, bin: filepath.Join(root, "bin")}, nil
	case strings.HasPrefix(target, "custom:"):
		root := strings.TrimPrefix(target, "custom:")
		if root == "" || !filepath.IsAbs(root) {
			return layout{}, fmt.Errorf("%w: the custom root %q is not an absolute path", ErrUnsupported, root)
		}
		root = filepath.Clean(root)
		return layout{root: root, bin: filepath.Join(root, "bin"), custom: true}, nil
	case target == "global":
		return layout{}, fmt.Errorf("%w: the go provider installs into a user or a custom root, never a shared global location", ErrUnsupported)
	default:
		return layout{}, fmt.Errorf("%w: target %q; the go provider installs into a user or a custom root", ErrUnsupported, target)
	}
}

// home is the home directory of the injected environment.
func (p *Provider) home() (string, error) {
	name := "HOME"
	if p.GOOS == "windows" {
		name = "USERPROFILE"
	}
	h := envValue(p.environ(), name)
	if h == "" {
		return "", fmt.Errorf("%w: the environment names no home directory (%s)", ErrUnsupported, name)
	}
	return h, nil
}

// env is the base environment plus the variables the target needs: the
// binaries go to the target's bin directory, the toolchain never switches,
// and a custom root keeps the module and build caches inside itself.
func (p *Provider) env(l layout) []string {
	env := append(p.baseEnv(),
		"GOBIN="+l.bin,
		"GOTOOLCHAIN=local",
		"GOFLAGS=-mod=mod",
		"CGO_ENABLED=0",
	)
	if l.custom {
		env = append(env,
			"GOPATH="+filepath.Join(l.root, "gopath"),
			"GOMODCACHE="+filepath.Join(l.root, "gopath", "pkg", "mod"),
			"GOCACHE="+filepath.Join(l.root, "cache"),
			"GOENV=off",
		)
	}
	return env
}

// tool locates the go tool on the PATH of the injected environment and
// returns its absolute path.
func (p *Provider) tool() (string, error) {
	name := "go"
	if p.GOOS == "windows" {
		name = "go.exe"
	}
	lookup := p.LookPath
	if lookup == nil {
		lookup = LookPath
	}
	path, err := lookup(name, p.environ())
	if err != nil || path == "" {
		return "", fmt.Errorf("%w: %s is not on the PATH", ErrMissing, name)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %s is not an absolute path", ErrMissing, path)
	}
	return path, nil
}

// run starts the go tool with the target's explicit environment.
func (p *Provider) run(ctx context.Context, l layout, args ...string) (Result, error) {
	path, err := p.tool()
	if err != nil {
		return Result{}, err
	}
	run := p.Run
	if run == nil {
		run = Exec
	}
	return run(ctx, path, args, p.env(l))
}

var versionLine = regexp.MustCompile(`^go version go([0-9][0-9A-Za-z.\-+]*)`)

// Version reports the installed go tool's version. An absent tool is
// ErrMissing.
func (p *Provider) Version(ctx context.Context) (string, error) {
	path, err := p.tool()
	if err != nil {
		return "", err
	}
	return p.toolVersion(ctx, path)
}

// toolVersion reads the version of the go tool at path. The probe is
// read-only and runs with the explicit base environment: it inspects the
// toolchain, not a target.
func (p *Provider) toolVersion(ctx context.Context, path string) (string, error) {
	run := p.Run
	if run == nil {
		run = Exec
	}
	r, err := run(ctx, path, []string{"version"}, p.baseEnv())
	if err != nil {
		return "", err
	}
	m := versionLine.FindSubmatch(r.Stdout)
	if r.Exit != 0 || m == nil {
		return "", fmt.Errorf("go version exited %d: %s", r.Exit, firstLine(r.Stdout, r.Stderr))
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

// moduleElement is one element of a module or package path.
var moduleElement = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*$`)

// modulePath reports whether s is a module or package path: elements
// separated by single slashes, each starting with a letter or a digit.
func modulePath(s string) bool {
	if s == "" || strings.ContainsAny(s, " \\") {
		return false
	}
	for _, e := range strings.Split(s, "/") {
		if !moduleElement.MatchString(e) {
			return false
		}
	}
	return true
}

// versionPattern is a module version: a v-prefixed semantic version, with
// the pre-release and build suffixes the proxy protocol allows.
var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.\-]+)?$`)

// options are the two settings the go provider accepts: the module the
// package belongs to and the package path itself. Any other option is
// refused rather than ignored.
func options(in map[string]string) (string, string, error) {
	for k := range in {
		if k != "module" && k != "package" {
			return "", "", fmt.Errorf("%w: option %q; the go provider takes only module and package", ErrUnsupported, k)
		}
	}
	module, pkg := in["module"], in["package"]
	switch {
	case module == "":
		return "", "", fmt.Errorf("%w: the recipe names no module", ErrUnsupported)
	case pkg == "":
		return "", "", fmt.Errorf("%w: the recipe names no package", ErrUnsupported)
	case !modulePath(module):
		return "", "", fmt.Errorf("%w: %q is not a module path", ErrUnsupported, module)
	case !modulePath(pkg):
		return "", "", fmt.Errorf("%w: %q is not a package path", ErrUnsupported, pkg)
	case pkg != module && !strings.HasPrefix(pkg, module+"/"):
		return "", "", fmt.Errorf("%w: the package %q is not under the module %q", ErrUnsupported, pkg, module)
	}
	return module, pkg, nil
}

// Plan resolves a package without changing the machine. It reads the go
// tool's version, reads what is installed in the target and asks the
// module proxy for the version to install; it creates nothing.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	module, pkg, err := options(r.Options)
	if err != nil {
		return nil, err
	}
	l, err := p.layoutFor(r.Target)
	if err != nil {
		return nil, err
	}
	tool, err := p.tool()
	if err != nil {
		return nil, err
	}
	ver, err := p.toolVersion(ctx, tool)
	if err != nil {
		return nil, err
	}
	if err := p.checkRoot(l.root); err != nil {
		return nil, err
	}
	pre := []plan.Precondition{
		{Kind: "software", Subject: Name, Requirement: "present at " + tool, Observed: ver},
		{Kind: "target", Subject: "go-root:" + l.root, Requirement: "writable by the current user", Observed: "writable"},
	}
	installed, found, err := p.installedVersion(ctx, l, module, pkg)
	if err != nil {
		return nil, err
	}
	latest, err := p.latest(ctx, module)
	if err != nil {
		return nil, err
	}
	res := &Resolution{Package: pkg, Version: latest, ProviderVersion: ver, Preconditions: pre}
	if found && installed == latest {
		// The planned version is installed: there is nothing to do.
		res.Installed = true
		return res, nil
	}
	action := "create"
	if found {
		action = "update"
	}
	res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: action, Provider: Name,
		Params: map[string]string{"module": module, "package": pkg, "version": latest}}}
	return res, nil
}

// binary is the file the package installs.
func (p *Provider) binary(l layout, pkg string) string {
	name := filepath.Base(pkg)
	if p.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(l.bin, name)
}

// installedVersion reports the module version of the binary installed for
// the package, if there is one. A binary of another module is refused:
// the provider never overwrites what it did not install.
func (p *Provider) installedVersion(ctx context.Context, l layout, module, pkg string) (string, bool, error) {
	bin := p.binary(l, pkg)
	if _, err := os.Stat(bin); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%w: %s: %v", ErrUnsupported, bin, err)
	}
	r, err := p.run(ctx, l, "version", "-m", bin)
	if err != nil {
		return "", false, err
	}
	mod, ver, ok := moduleVersion(r.Stdout)
	if r.Exit != 0 {
		return "", false, fmt.Errorf("%w: go version -m %s exited %d; managed-machine will not overwrite it", ErrUnsupported, bin, r.Exit)
	}
	if !ok {
		return "", false, fmt.Errorf("%w: %s carries no module; managed-machine will not overwrite it", ErrUnsupported, bin)
	}
	if mod != module {
		return "", false, fmt.Errorf("%w: %s was built from %s, not %s", ErrUnsupported, bin, mod, module)
	}
	return ver, true, nil
}

// moduleVersion reads the module and the version out of the output of
// `go version -m`. Every line of that output is tab indented, and the
// module line is "mod<TAB><module><TAB><version>".
func moduleVersion(out []byte) (string, string, bool) {
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(strings.TrimLeft(strings.TrimRight(line, "\r"), " \t"), "\t")
		if len(f) >= 3 && f[0] == "mod" {
			return f[1], f[2], true
		}
	}
	return "", "", false
}

// escapeModule escapes a module path for the proxy protocol: an
// uppercase letter becomes "!" followed by its lowercase form.
func escapeModule(module string) string {
	var b strings.Builder
	for _, r := range module {
		if r >= 'A' && r <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(r + ('a' - 'A'))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// latest resolves the module's latest version from the module proxy. The
// proxy is asked over HTTPS, never through the go tool, so planning reads
// one version and pins it.
func (p *Provider) latest(ctx context.Context, module string) (string, error) {
	base := p.ProxyBase
	if base == "" {
		base = DefaultProxyBase
	}
	if err := checkProxyBase(base); err != nil {
		return "", err
	}
	u := strings.TrimSuffix(base, "/") + "/" + escapeModule(module) + "/@latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("the module proxy %s: %w", base, err)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("the module proxy %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the module proxy %s answered %s for %s", base, resp.Status, module)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", fmt.Errorf("the module proxy %s: %w", base, err)
	}
	var doc struct {
		Version string `json:"Version"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("the module proxy %s answered %v for %s", base, err, module)
	}
	if !versionPattern.MatchString(doc.Version) {
		return "", fmt.Errorf("%w: the module proxy reported the version %q for %s", ErrUnsupported, doc.Version, module)
	}
	return doc.Version, nil
}

// client is the HTTP client the proxy is fetched with.
func (p *Provider) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	return DefaultClient()
}

// DefaultClient returns the client the module proxy is fetched with
// unless a caller injects its own: TLS verification on, a total timeout
// and redirects limited to the host the request started with.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: RequestTimeout, CheckRedirect: sameHost}
}

// sameHost refuses a redirect that leaves the host the request started
// with.
func sameHost(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("the module proxy redirected to %s", req.URL.Host)
	}
	if len(via) >= 3 {
		return errors.New("the module proxy redirected too many times")
	}
	return nil
}

// checkProxyBase refuses a proxy that is not served over TLS. The
// loopback interface is the one exception, so a test can serve the
// registry locally.
func checkProxyBase(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("%w: the module proxy %q is not a URL", ErrUnsupported, base)
	}
	switch {
	case u.Scheme == "https":
		return nil
	case u.Scheme == "http" && loopback(u.Hostname()):
		return nil
	default:
		return fmt.Errorf("%w: the module proxy %q is not served over https", ErrUnsupported, base)
	}
}

// loopback reports whether host is the loopback interface.
func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkRoot refuses a target root the current user cannot write. The
// nearest existing ancestor decides, because the root itself may still be
// absent: managed-machine never elevates and never creates a root
// somewhere else.
func (p *Provider) checkRoot(root string) error {
	info, err := os.Stat(root)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%w: the target root %s is not a directory", ErrUnsupported, root)
		}
	case os.IsNotExist(err):
		anc := nearestExisting(root)
		if anc == "" {
			return fmt.Errorf("%w: the target root %s has no existing ancestor", ErrUnsupported, root)
		}
		if !p.writable(anc) {
			return fmt.Errorf("%w (%s); managed-machine never elevates", ErrPrivilege, anc)
		}
		return nil
	default:
		return fmt.Errorf("%w: the target root %s: %v", ErrUnsupported, root, err)
	}
	if !p.writable(root) {
		return fmt.Errorf("%w (%s); managed-machine never elevates", ErrPrivilege, root)
	}
	return nil
}

// writable reports whether the current user may write the path.
func (p *Provider) writable(path string) bool {
	if p.Writable != nil {
		return p.Writable(path)
	}
	return writable(path)
}

// nearestExisting returns the closest existing ancestor of a path that
// does not exist, or "" when there is none.
func nearestExisting(path string) string {
	dir := filepath.Dir(path)
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// Check re-evaluates one of this provider's preconditions at apply time.
func (p *Provider) Check(ctx context.Context, c plan.Precondition) (string, error) {
	switch {
	case c.Kind == "software" && c.Subject == Name:
		tool, err := p.tool()
		if err != nil {
			return "absent", err
		}
		if want := strings.TrimPrefix(c.Requirement, "present at "); want != tool {
			return tool, fmt.Errorf("the go tool moved from %s to %s", want, tool)
		}
		v, err := p.toolVersion(ctx, tool)
		if err != nil {
			return "absent", err
		}
		if v != c.Observed {
			return v, fmt.Errorf("the go tool changed from %s to %s", c.Observed, v)
		}
		return v, nil
	case c.Kind == "target" && strings.HasPrefix(c.Subject, "go-root:"):
		root := strings.TrimPrefix(c.Subject, "go-root:")
		if err := p.checkRoot(root); err != nil {
			return "not writable", err
		}
		return "writable", nil
	}
	return "", fmt.Errorf("unknown go precondition %s %s", c.Kind, c.Subject)
}

// Step returns the performer of one saved go operation. Only the pinned
// package and version are accepted: the operation carries no room for
// anything else.
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	module, pkg, version := op.Params["module"], op.Params["package"], op.Params["version"]
	if op.Provider != Name || len(op.Params) != 3 ||
		(op.Action != "create" && op.Action != "update") ||
		!modulePath(module) || !modulePath(pkg) ||
		(pkg != module && !strings.HasPrefix(pkg, module+"/")) ||
		!versionPattern.MatchString(version) {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	l, err := p.layoutFor(op.Target)
	if err != nil {
		return nil, err
	}
	return &step{p: p, l: l, module: module, pkg: pkg, version: version}, nil
}

// step performs one planned installation.
type step struct {
	p           *Provider
	l           layout
	module, pkg string
	version     string
}

// Apply installs the pinned version. The target's directories are created
// here, at apply time: planning creates nothing.
func (s *step) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := os.MkdirAll(s.l.bin, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", s.l.bin, err)
	}
	r, err := s.p.run(ctx, s.l, "install", s.pkg+"@"+s.version)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "go install " + s.pkg + "@" + s.version, "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("go install exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return ev, nil
}

// Verify asks the go tool what the installed binary is, independently of
// the install's own exit status, and requires the planned module version.
func (s *step) Verify(ctx context.Context) (execute.Evidence, error) {
	bin := s.p.binary(s.l, s.pkg)
	r, err := s.p.run(ctx, s.l, "version", "-m", bin)
	if err != nil {
		return nil, err
	}
	module, version, ok := moduleVersion(r.Stdout)
	ev := execute.Evidence{"command": "go version -m " + bin, "exit": r.Exit, "module": module, "version": version}
	if r.Exit != 0 || !ok {
		return ev, fmt.Errorf("%s is not installed after go install", bin)
	}
	if module != s.module {
		return ev, fmt.Errorf("%s was built from %s, not %s", bin, module, s.module)
	}
	if version != s.version {
		return ev, fmt.Errorf("%s %s was planned but %s is installed", s.pkg, s.version, version)
	}
	return ev, nil
}

// Name is the provider name recipes use.
func (p *Provider) Name() string { return Name }

var _ provider.Provider = (*Provider)(nil)
