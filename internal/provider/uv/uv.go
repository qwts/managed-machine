// Package uv is the built-in uv provider for S3 (story X02). It installs
// Python tools with `uv tool install` for the current user or inside a
// custom root, and never elevates: a root the current user may not write
// fails the plan instead of being created with another user's rights.
// Planning is read-only; it reads the uv version, the installed tools and
// the index metadata, and writes nothing at all, not even a custom root
// or a cache.
package uv

import (
	"context"
	"encoding/json"
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
const Name = "uv"

// Tool is the executable this provider drives.
const Tool = "uv"

// DefaultIndexBase is the registry a tool's version is resolved from.
const DefaultIndexBase = "https://pypi.org"

// RequestTimeout bounds one index request.
const RequestTimeout = 15 * time.Second

// MaxIndexBytes bounds the index document this provider reads.
const MaxIndexBytes = 1 << 20

// MaxRedirects bounds the redirects one index request may follow.
const MaxRedirects = 5

// The targets this provider honors: the current user, and a custom root
// named by an absolute path. The global target belongs to a package
// manager that owns a shared prefix, not to uv.
const (
	TargetUser   = "user"
	targetPrefix = "custom:"
)

// The actions a planned operation carries.
const (
	ActionCreate = "create"
	ActionUpdate = "update"
)

// The requirements the preconditions record. The path the requirement is
// about follows it, so the precondition names one concrete location.
const (
	toolRequirement = "present at "
	rootRequirement = "writable by the current user at "
)

// Env is the explicit environment added to every uv invocation, whatever
// the target: uv must not edit the user's shell profile or PATH, and this
// provider writes no shell configuration.
var Env = []string{"UV_NO_MODIFY_PATH=1"}

// RootEnv returns the variables that keep a custom root's tools, its
// Python runtime and its caches inside that root.
func RootEnv(root string) []string {
	return []string{
		"UV_TOOL_DIR=" + filepath.Join(root, "tools"),
		"UV_TOOL_BIN_DIR=" + filepath.Join(root, "bin"),
		"UV_CACHE_DIR=" + filepath.Join(root, "cache"),
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(root, "python"),
		"UV_PYTHON_PREFERENCE=only-managed",
	}
}

// Result, Runner and Exec are the shared process seam; this provider only
// ever starts the uv executable.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// Request and Resolution are the shared provider contract.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported = provider.Kind(provider.ErrUnsupported, "unsupported by the uv provider")
	ErrPrivilege   = provider.Kind(provider.ErrPrivilege, "the custom root is not writable by the current user")
	ErrMissing     = provider.Kind(provider.ErrMissing, "uv is not installed")
)

// Provider is the uv provider.
type Provider struct {
	// Run starts one uv process; it is never a shell string.
	Run Runner
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// LookPath finds the uv executable by name. A nil lookup searches the
	// PATH of Environ, so the tool is found where the provider's own
	// environment points; an injected lookup (exec.LookPath) searches the
	// process PATH, which is that environment in production.
	LookPath func(file string) (string, error)
	// Client fetches the index; a nil client is the default one.
	Client *http.Client
	// IndexBase is the registry a tool's version is resolved from. Empty
	// is https://pypi.org.
	IndexBase string
	// Writable reports whether the current user may create or write a
	// path; a nil report uses the operating system's answer.
	Writable func(path string) bool
}

// DefaultClient is the client the index is fetched with unless a caller
// injects its own: TLS verification on and a total timeout.
func DefaultClient() *http.Client {
	return &http.Client{Timeout: RequestTimeout}
}

// Name is the provider name recipes use.
func (p *Provider) Name() string { return Name }

var _ provider.Provider = (*Provider)(nil)

var versionLine = regexp.MustCompile(`^uv ([0-9][0-9A-Za-z.\-+]*)`)

// Version reports the installed uv version.
func (p *Provider) Version(ctx context.Context) (string, error) {
	path, err := p.tool()
	if err != nil {
		return "", err
	}
	return p.version(ctx, path)
}

// version reads the version of the uv at path.
func (p *Provider) version(ctx context.Context, path string) (string, error) {
	r, err := p.run(ctx, path, "", "--version")
	if err != nil {
		return "", err
	}
	m := versionLine.FindSubmatch(r.Stdout)
	if r.Exit != 0 || m == nil {
		return "", fmt.Errorf("uv --version exited %d: %s", r.Exit, firstLine(r.Stdout, r.Stderr))
	}
	return string(m[1]), nil
}

// distributionName is the shape of a Python distribution name (PEP 508).
var distributionName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// Plan resolves a tool without changing the machine: it reads the uv
// version, the installed tools and the index metadata, and writes nothing
// at all, not even a custom root or a cache.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	tool, root, err := p.parse(r)
	if err != nil {
		return nil, err
	}
	pin, python := r.Options["version"], r.Options["python"]
	path, err := p.tool()
	if err != nil {
		return nil, err
	}
	ver, err := p.version(ctx, path)
	if err != nil {
		return nil, err
	}
	pre := []plan.Precondition{{Kind: "software", Subject: Name, Requirement: toolRequirement + path, Observed: ver}}
	if root != "" {
		if err := p.checkRoot(root); err != nil {
			return nil, err
		}
		pre = append(pre, plan.Precondition{Kind: "privilege", Subject: "custom-root", Requirement: rootRequirement + root, Observed: "writable"})
	}
	latest, err := p.resolve(ctx, tool, pin)
	if err != nil {
		return nil, err
	}
	installed, err := p.list(ctx, root, python != "")
	if err != nil {
		return nil, err
	}
	res := &Resolution{Package: tool, Version: latest, ProviderVersion: ver, Preconditions: pre}
	if have, ok := installed[normalize(tool)]; ok && have.version == latest && (python == "" || have.python == python) {
		// The planned version is already installed on the pinned Python:
		// the plan has no operation and the install is unchanged.
		res.Installed = true
		return res, nil
	}
	action := ActionCreate
	if _, ok := installed[normalize(tool)]; ok {
		action = ActionUpdate
	}
	params := map[string]string{"package": tool, "version": latest}
	if root != "" {
		params["root"] = root
	}
	if python != "" {
		params["python"] = python
	}
	res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: action, Provider: Name,
		Params: params}}
	return res, nil
}

// pinnedVersion is the release shape a recipe may pin (X.Y.Z).
var pinnedVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// pythonVersion is the interpreter a recipe may pin: a CPython 3 major.minor,
// never a path.
var pythonVersion = regexp.MustCompile(`^3\.[0-9]{1,2}$`)

// parse validates the request: the recipe names exactly one tool, optionally
// its pinned version and Python, and the target is the current user or a
// custom root named by an absolute path.
func (p *Provider) parse(r Request) (tool, root string, err error) {
	for k, v := range r.Options {
		switch {
		case k == "tool":
		case k == "version" && pinnedVersion.MatchString(v):
		case k == "python" && pythonVersion.MatchString(v):
		case k == "version" || k == "python":
			return "", "", fmt.Errorf("%w: %s %q is not a pinned value", ErrUnsupported, k, v)
		default:
			return "", "", fmt.Errorf("%w: option %q; the uv provider installs exactly one tool", ErrUnsupported, k)
		}
	}
	name := r.Options["tool"]
	switch {
	case name == "":
		return "", "", fmt.Errorf("%w: the recipe names no tool", ErrUnsupported)
	case !distributionName.MatchString(name):
		return "", "", fmt.Errorf("%w: %q is not a Python distribution name", ErrUnsupported, name)
	}
	// The index publishes one distribution under one normalized name, so
	// the plan pins that name.
	tool = strings.ToLower(name)
	switch {
	case r.Target == TargetUser:
	case strings.HasPrefix(r.Target, targetPrefix):
		root = strings.TrimPrefix(r.Target, targetPrefix)
		if root == "" || !filepath.IsAbs(root) {
			return "", "", fmt.Errorf("%w: the custom root %q is not an absolute path", ErrUnsupported, root)
		}
	default:
		return "", "", fmt.Errorf("%w: target %s; uv installs for the current user or into a custom root, never for the whole machine", ErrUnsupported, r.Target)
	}
	return tool, root, nil
}

// tool locates the uv executable and returns its absolute path. An absent
// uv is ErrMissing: this provider never installs uv itself.
func (p *Provider) tool() (string, error) {
	path, err := p.lookPath()
	if err != nil {
		return "", fmt.Errorf("%w: %s is not on the PATH", ErrMissing, Tool)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrMissing, path)
	}
	return abs, nil
}

// lookPath finds the uv executable. The default lookup searches the PATH
// of the environment the provider was given.
func (p *Provider) lookPath() (string, error) {
	if p.LookPath != nil {
		return p.LookPath(Tool)
	}
	return lookPath(Tool, p.base())
}

// base is the injected environment, normally os.Environ.
func (p *Provider) base() []string {
	if p.Environ == nil {
		return nil
	}
	return p.Environ()
}

// env is the explicit environment of every uv process: the injected base
// environment without any UV_ variable, plus the variables the target
// needs. Nothing here edits a shell profile or a PATH configuration.
func (p *Provider) env(root string) []string {
	base := p.base()
	env := make([]string, 0, len(base)+len(Env)+len(RootEnv(root)))
	for _, kv := range base {
		if strings.HasPrefix(kv, "UV_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, Env...)
	if root != "" {
		env = append(env, RootEnv(root)...)
	}
	return env
}

// run starts one uv process with an explicit environment.
func (p *Provider) run(ctx context.Context, path, root string, args ...string) (Result, error) {
	return p.Run(ctx, path, args, p.env(root))
}

// toolLine is one installed tool: "ruff v0.7.4", or with --show-python
// "ruff v0.7.4 [CPython 3.12.3]".
var toolLine = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*) v([0-9][0-9A-Za-z.\-+]*)(?: \[CPython ([0-9]+\.[0-9]+)\.[0-9A-Za-z+]*\])?$`)

// installedTool is one tool uv lists: its version and, when the list was
// asked for it, the major.minor of its CPython.
type installedTool struct {
	version, python string
}

// listArgs is the argv of the tool list; showPython adds each tool's
// interpreter.
func listArgs(showPython bool) []string {
	if showPython {
		return []string{"tool", "list", "--show-python"}
	}
	return []string{"tool", "list"}
}

// list reads the installed tools of the target with the target's
// environment. An empty list, or "No tools installed", means no tool is
// installed.
func (p *Provider) list(ctx context.Context, root string, showPython bool) (map[string]installedTool, error) {
	path, err := p.tool()
	if err != nil {
		return nil, err
	}
	r, err := p.run(ctx, path, root, listArgs(showPython)...)
	if err != nil {
		return nil, err
	}
	if r.Exit != 0 {
		return nil, fmt.Errorf("uv tool list exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return parseTools(string(r.Stdout)), nil
}

// parseTools reads the tool list uv prints: one "<name> v<version>" line
// per installed tool, each followed by its executable lines.
func parseTools(out string) map[string]installedTool {
	tools := map[string]installedTool{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "-") {
			continue
		}
		m := toolLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		tools[normalize(m[1])] = installedTool{version: m[2], python: m[3]}
	}
	return tools
}

// normalize is the name a distribution is compared under: the index and
// uv both name it in lower case with the separators folded together.
func normalize(name string) string {
	return strings.ToLower(strings.NewReplacer("_", "-", ".", "-").Replace(name))
}

// resolve reads the version the index publishes for the tool: the latest
// release, or exactly the pinned one, which the index must publish.
func (p *Provider) resolve(ctx context.Context, tool, pin string) (string, error) {
	base := p.IndexBase
	if base == "" {
		base = DefaultIndexBase
	}
	u := strings.TrimSuffix(base, "/") + "/pypi/" + url.PathEscape(tool) + "/json"
	if pin != "" {
		u = strings.TrimSuffix(base, "/") + "/pypi/" + url.PathEscape(tool) + "/" + url.PathEscape(pin) + "/json"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("the index URL of %s is malformed: %v", tool, err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("the index of %s could not be read: %v", tool, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxIndexBytes))
	if err != nil {
		return "", fmt.Errorf("the index of %s could not be read: %v", tool, err)
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: the index has no distribution named %q", ErrUnsupported, tool)
	default:
		return "", fmt.Errorf("the index of %s answered %d", tool, resp.StatusCode)
	}
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("the index of %s is not readable: %v", tool, err)
	}
	if doc.Info.Version == "" {
		return "", fmt.Errorf("%w: the index publishes no version for %q", ErrUnsupported, tool)
	}
	if pin != "" && doc.Info.Version != pin {
		return "", fmt.Errorf("%w: the index answered %s for the pinned %s==%s", ErrUnsupported, doc.Info.Version, tool, pin)
	}
	return doc.Info.Version, nil
}

// client is the HTTP client the index is fetched with. An injected client
// keeps its own transport and timeout, but not its redirect policy: the
// policy is a property of this provider, so the index cannot hand its
// content to another host.
func (p *Provider) client() *http.Client {
	if p.Client == nil {
		return DefaultClient()
	}
	c := *p.Client
	c.CheckRedirect = redirectPolicy
	return &c
}

// redirectPolicy allows a few redirects and only to https on the host the
// request started with.
func redirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return fmt.Errorf("more than %d redirects", MaxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to %q is not allowed", req.URL.Scheme)
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("redirect to host %q is not allowed", req.URL.Host)
	}
	return nil
}

// checkRoot refuses a root the current user may not write, and a root
// that is not a directory. The provider never elevates.
func (p *Provider) checkRoot(root string) error {
	fi, err := os.Stat(root)
	switch {
	case err == nil && !fi.IsDir():
		return fmt.Errorf("%w: the custom root %s is not a directory", ErrUnsupported, root)
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("%w: the custom root %s could not be read: %v", ErrUnsupported, root, err)
	}
	if !p.writable(root) {
		return fmt.Errorf("%w (%s); managed-machine never elevates to create a root", ErrPrivilege, root)
	}
	return nil
}

func (p *Provider) writable(path string) bool {
	if p.Writable != nil {
		return p.Writable(path)
	}
	return writable(path)
}

// Check re-evaluates one of this provider's preconditions at apply time.
// The tool is observed again, so a moved or changed uv rejects the plan.
func (p *Provider) Check(ctx context.Context, c plan.Precondition) (string, error) {
	switch {
	case c.Kind == "software" && c.Subject == Name:
		path, err := p.tool()
		if err != nil {
			return "absent", err
		}
		if was := strings.TrimPrefix(c.Requirement, toolRequirement); was != path {
			return path, fmt.Errorf("uv moved from %s to %s", was, path)
		}
		v, err := p.Version(ctx)
		if err != nil {
			return "absent", err
		}
		if v != c.Observed {
			return v, fmt.Errorf("uv changed from %s to %s", c.Observed, v)
		}
		return v, nil
	case c.Kind == "privilege" && c.Subject == "custom-root":
		root := strings.TrimPrefix(c.Requirement, rootRequirement)
		if err := p.checkRoot(root); err != nil {
			return "not writable", err
		}
		return "writable", nil
	}
	return "", fmt.Errorf("unknown uv precondition %s %s", c.Kind, c.Subject)
}

// Step returns the performer of one saved uv operation.
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	tool, version := op.Params["package"], op.Params["version"]
	if op.Provider != Name || !distributionName.MatchString(tool) || tool != strings.ToLower(tool) || version == "" {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	switch op.Action {
	case ActionCreate, ActionUpdate:
	default:
		return nil, fmt.Errorf("%w: operation %s has the action %q", ErrUnsupported, op.ID, op.Action)
	}
	params := 2
	python, pinned := op.Params["python"]
	if pinned {
		if !pythonVersion.MatchString(python) {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
		params++
	}
	var root string
	switch {
	case op.Target == TargetUser:
		if len(op.Params) != params {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
	case strings.HasPrefix(op.Target, targetPrefix):
		root = strings.TrimPrefix(op.Target, targetPrefix)
		if root == "" || !filepath.IsAbs(root) || op.Params["root"] != root || len(op.Params) != params+1 {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
	default:
		return nil, fmt.Errorf("%w: operation %s targets %s", ErrUnsupported, op.ID, op.Target)
	}
	return &step{p: p, tool: tool, version: version, python: python, root: root, action: op.Action}, nil
}

type step struct {
	p             *Provider
	tool, version string
	python        string
	root          string
	action        string
}

// Apply installs exactly the pinned version. The custom root is created
// here, at apply time, and never during planning.
func (s *step) Apply(ctx context.Context) (execute.Evidence, error) {
	if s.root != "" {
		if err := os.MkdirAll(s.root, 0o755); err != nil {
			return nil, fmt.Errorf("create the custom root %s: %v", s.root, err)
		}
	}
	path, err := s.p.tool()
	if err != nil {
		return nil, err
	}
	args := []string{"tool", "install"}
	if s.action == ActionUpdate {
		args = append(args, "--force")
	}
	if s.python != "" {
		args = append(args, "--python", s.python)
	}
	args = append(args, s.tool+"=="+s.version)
	r, err := s.p.run(ctx, path, s.root, args...)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "uv " + strings.Join(args, " "), "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("uv %s exited %d: %s", strings.Join(args, " "), r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	return ev, nil
}

// Verify asks uv what is installed, independently of the install's own
// exit status, and requires the planned version and Python.
func (s *step) Verify(ctx context.Context) (execute.Evidence, error) {
	path, err := s.p.tool()
	if err != nil {
		return nil, err
	}
	args := listArgs(s.python != "")
	r, err := s.p.run(ctx, path, s.root, args...)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "uv " + strings.Join(args, " "), "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("uv tool list exited %d: %s", r.Exit, firstLine(r.Stderr, r.Stdout))
	}
	have, ok := parseTools(string(r.Stdout))[normalize(s.tool)]
	if !ok {
		return ev, fmt.Errorf("%s is not installed after uv tool install", s.tool)
	}
	ev["installed"] = have.version
	if have.version != s.version {
		return ev, fmt.Errorf("%s %s was planned but %s is installed", s.tool, s.version, have.version)
	}
	if s.python != "" && have.python != s.python {
		return ev, fmt.Errorf("%s was planned on Python %s but runs on %q", s.tool, s.python, have.python)
	}
	ev["version"] = have.version
	return ev, nil
}

func firstLine(bs ...[]byte) string {
	for _, b := range bs {
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.SplitN(s, "\n", 2)[0]
		}
	}
	return ""
}
