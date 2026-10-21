// Package brew is the built-in Homebrew formula provider for S2 (story
// E02). It plans and installs formulae through the shared executor and
// never elevates: when the Homebrew prefix is not writable by the current
// user the plan fails its precondition instead of running brew as the
// prefix owner, the authorization path the legacy Bash routes take. Casks,
// custom roots and options other than the formula are rejected explicitly.
package brew

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/plan"
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

// Result is one finished process.
type Result struct {
	Stdout, Stderr []byte
	Exit           int
}

// Runner starts one process. The provider only ever passes the brew
// executable under its prefix, with an argv, never a shell string.
type Runner func(ctx context.Context, path string, args, env []string) (Result, error)

// Exec is the real Runner.
func Exec(ctx context.Context, path string, args, env []string) (Result, error) {
	c := exec.CommandContext(ctx, path, args...)
	c.Env = env
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	r := Result{Stdout: o.Bytes(), Stderr: e.Bytes()}
	var x *exec.ExitError
	if errors.As(err, &x) {
		r.Exit = x.ExitCode()
		return r, nil
	}
	return r, err
}

// Provider is the Homebrew provider bound to one prefix.
type Provider struct {
	Prefix string
	Run    Runner
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// Writable and Owned report on the prefix; nil uses the OS checks.
	Writable func(path string) bool
	Owned    func(path string) bool
}

func (p *Provider) brew() string { return filepath.Join(p.Prefix, "bin", "brew") }

func (p *Provider) run(ctx context.Context, args ...string) (Result, error) {
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
	return p.Run(ctx, p.brew(), args, append(env, Env...))
}

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported = errors.New("unsupported by the brew provider")
	ErrPrivilege   = errors.New("the Homebrew prefix is not writable by the current user")
	ErrMissing     = errors.New("Homebrew is not installed at its canonical prefix")
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

// Request is one formula installation to plan.
type Request struct {
	Software string
	Target   string // global or user; custom roots are rejected
	Options  map[string]string
}

// Resolution is what planning found, ready to save.
type Resolution struct {
	Formula         string
	Version         string // the version that will be (or already is) installed
	Installed       bool   // already installed: the plan has no operations
	ProviderVersion string
	Preconditions   []plan.Precondition
	Operations      []plan.Operation
}

var formulaName = regexp.MustCompile(`^[a-z0-9][a-z0-9@+._\-]*$`)

// Plan resolves a formula without changing the machine.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	formula := r.Options["formula"]
	for k := range r.Options {
		if k != "formula" {
			return nil, fmt.Errorf("%w: option %q (casks and app options need authorization handling and are deferred)", ErrUnsupported, k)
		}
	}
	switch {
	case formula == "":
		return nil, fmt.Errorf("%w: the recipe names no formula", ErrUnsupported)
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
	res := &Resolution{Formula: formula, ProviderVersion: ver, Preconditions: pre}
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
	if p.Writable != nil {
		return p.Writable(p.Prefix)
	}
	return writable(p.Prefix)
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
	f, v := op.Params["formula"], op.Params["version"]
	if op.Provider != Name || op.Action != "create" || !formulaName.MatchString(f) || v == "" || len(op.Params) != 2 {
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
