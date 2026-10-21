// Package winget is the built-in WinGet provider for the Windows targets
// (S3 story X02). Planning is read-only: it reads the winget version, the
// version the source offers for a package and the installed state, and it
// writes nothing. The machine scope (the global target) is honored only in a
// process that is already elevated, because managed-machine never elevates,
// and custom roots are refused: winget installs into its own scopes. Only the
// winget executable the injected environment names is ever started, with an
// argv and an explicit environment.
package winget

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/provider"
)

// Name is the provider name in recipes.
const Name = "winget"

// The installation scopes winget offers. The recipe target user is the user
// scope; the recipe target global is the machine scope, which needs a
// process that is already elevated.
const (
	scopeUser    = "user"
	scopeMachine = "machine"
)

// commonArgs are added to every invocation: the winget source, its agreement
// and no interactivity, so the provider never waits for a prompt and never
// changes the source configuration of the machine.
var commonArgs = []string{"--source", "winget", "--accept-source-agreements", "--disable-interactivity"}

// Errors the software command maps to diagnostics.
var (
	ErrUnsupported = provider.Kind(provider.ErrUnsupported, "unsupported by the winget provider")
	ErrPrivilege   = provider.Kind(provider.ErrPrivilege, "managed-machine never elevates; run from an elevated shell or use --user")
	ErrMissing     = provider.Kind(provider.ErrMissing, "winget is not installed")
)

// Result and Runner are the shared process seam; this provider only ever
// starts the winget executable it resolved.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// Provider is the WinGet provider.
type Provider struct {
	// Run starts the winget process; nil means Exec.
	Run Runner
	// Environ is the base environment, normally os.Environ.
	Environ func() []string
	// LookPath resolves an executable file; nil means exec.LookPath. The
	// candidates are built from the PATH of the injected environment, so
	// the provider only ever starts a tool that environment names.
	LookPath func(file string) (string, error)
	// Elevated reports whether the current process is elevated; nil means
	// the platform check.
	Elevated func() bool
}

// Request and Resolution are the shared provider contract.
type (
	Request    = provider.Request
	Resolution = provider.Resolution
)

// env is the explicit environment every winget process receives: the
// injected base environment, unchanged. winget has no environment of its own
// to isolate, and this provider edits no shell profile and no PATH
// configuration.
func (p *Provider) env() []string {
	if p.Environ == nil {
		return nil
	}
	base := p.Environ()
	out := make([]string, len(base))
	copy(out, base)
	return out
}

// pathDirs is the PATH of the injected environment.
func (p *Provider) pathDirs() []string {
	for _, kv := range p.env() {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, "PATH") {
			return filepath.SplitList(v)
		}
	}
	return nil
}

// toolPath resolves the absolute path of the winget executable in the PATH of
// the injected environment. A tool that is not there is ErrMissing: the
// provider never downloads one and never starts another program.
func (p *Provider) toolPath() (string, error) {
	lookup := p.LookPath
	if lookup == nil {
		lookup = exec.LookPath
	}
	dirs := p.pathDirs()
	if len(dirs) == 0 {
		return "", fmt.Errorf("%w: the environment names no PATH", ErrMissing)
	}
	for _, dir := range dirs {
		for _, name := range toolNames() {
			cand := filepath.Join(dir, name)
			got, err := lookup(cand)
			if err != nil || got == "" {
				continue
			}
			abs, err := filepath.Abs(got)
			if err != nil {
				continue
			}
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: winget was not found in the PATH", ErrMissing)
}

// toolNames are the file names winget is installed under, in the order they
// are tried. On Windows the App Installer provides the alias as winget.exe.
func toolNames() []string {
	if runtime.GOOS == "windows" {
		return []string{"winget.exe", "winget.cmd", "winget.bat", "winget.com", "winget"}
	}
	return []string{"winget"}
}

// argv is the full argument vector of an invocation: the arguments of the
// command and the common ones.
func argv(args ...string) []string {
	full := make([]string, 0, len(args)+len(commonArgs))
	full = append(full, args...)
	return append(full, commonArgs...)
}

// run starts winget with the common arguments and the explicit environment.
func (p *Provider) run(ctx context.Context, path string, args ...string) (Result, error) {
	run := p.Run
	if run == nil {
		run = Exec
	}
	return run(ctx, path, argv(args...), p.env())
}

// Version reports the installed winget version.
func (p *Provider) Version(ctx context.Context) (string, error) {
	path, err := p.toolPath()
	if err != nil {
		return "", err
	}
	return p.version(ctx, path)
}

var versionLine = regexp.MustCompile(`^v?([0-9][0-9A-Za-z.\-+]*)$`)

// version reads the winget version from `winget --version`, which prints it
// as "v1.9.25200"; the leading v is not part of the version.
func (p *Provider) version(ctx context.Context, path string) (string, error) {
	// --version takes no other argument: with the common source arguments
	// winget refuses it as an invalid command line (0x8A150002).
	run := p.Run
	if run == nil {
		run = Exec
	}
	r, err := run(ctx, path, []string{"--version"}, p.env())
	if err != nil {
		return "", err
	}
	if r.Exit != 0 {
		return "", fmt.Errorf("winget --version exited 0x%X: %s", r.Exit, firstMeaningful(r.Stdout, r.Stderr))
	}
	for _, line := range cleanLines(r.Stdout) {
		if m := versionLine.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("winget --version printed no version: %s", firstMeaningful(r.Stdout, r.Stderr))
}

// packageID is a WinGet package identifier: letters, digits, dots, dashes and
// underscores.
var packageID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validPackageID reports whether id is a package identifier: the characters
// winget accepts and at least one dot, which separates the publisher from the
// name.
func validPackageID(id string) bool {
	return packageID.MatchString(id) && strings.Contains(id, ".")
}

// packageIDOf reads the package identifier of the recipe. The recipe declares
// exactly one option, package_id; anything else is refused.
func packageIDOf(opts map[string]string) (string, error) {
	for k := range opts {
		if k != "package_id" {
			return "", fmt.Errorf("%w: option %q (the winget provider takes only package_id)", ErrUnsupported, k)
		}
	}
	id := strings.TrimSpace(opts["package_id"])
	switch {
	case id == "":
		return "", fmt.Errorf("%w: the recipe names no package_id", ErrUnsupported)
	case !validPackageID(id):
		return "", fmt.Errorf("%w: %q is not a package identifier", ErrUnsupported, id)
	}
	return id, nil
}

// scope maps a recipe target to a winget scope. The machine scope is the
// global target and needs a process that is already elevated; managed-machine
// never elevates, so a global target is refused in a process that is not.
// winget has no custom root.
func (p *Provider) scope(target string) (string, error) {
	switch {
	case target == "user":
		return scopeUser, nil
	case target == "global":
		if !p.isElevated() {
			return "", fmt.Errorf("%w: the global target is the machine scope", ErrPrivilege)
		}
		return scopeMachine, nil
	case strings.HasPrefix(target, "custom:"):
		return "", fmt.Errorf("%w: target %s; winget installs into its own scopes, never a custom root", ErrUnsupported, target)
	}
	return "", fmt.Errorf("%w: target %s", ErrUnsupported, target)
}

// isElevated reports whether the current process is elevated.
func (p *Provider) isElevated() bool {
	if p.Elevated != nil {
		return p.Elevated()
	}
	return elevated()
}

// Plan resolves a package without changing the machine.
func (p *Provider) Plan(ctx context.Context, r Request) (*Resolution, error) {
	id, err := packageIDOf(r.Options)
	if err != nil {
		return nil, err
	}
	scope, err := p.scope(r.Target)
	if err != nil {
		return nil, err
	}
	path, err := p.toolPath()
	if err != nil {
		return nil, err
	}
	ver, err := p.version(ctx, path)
	if err != nil {
		return nil, err
	}
	pre := []plan.Precondition{{Kind: "software", Subject: Name, Requirement: "present at " + path, Observed: ver}}
	if scope == scopeMachine {
		pre = append(pre, plan.Precondition{Kind: "privilege", Subject: "winget-scope", Requirement: "elevated process", Observed: "elevated"})
	}
	latest, err := p.show(ctx, path, id)
	if err != nil {
		return nil, err
	}
	installed, err := p.installed(ctx, path, id)
	if err != nil {
		return nil, err
	}
	res := &Resolution{Package: id, Version: latest, ProviderVersion: ver, Preconditions: pre}
	switch {
	case installed == "":
		res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: "create", Provider: Name,
			Params: map[string]string{"package": id, "version": latest, "scope": scope}}}
	case installed == latest:
		res.Installed = true
	default:
		res.Operations = []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: "update", Provider: Name,
			Params: map[string]string{"package": id, "version": latest}}}
	}
	return res, nil
}

// show resolves the version the source offers for a package. A package the
// source does not know is unsupported.
func (p *Provider) show(ctx context.Context, path, id string) (string, error) {
	r, err := p.run(ctx, path, "show", "--id", id, "--exact")
	if err != nil {
		return "", err
	}
	if r.Exit != 0 {
		return "", fmt.Errorf("%w: winget show %s exited 0x%X: %s", ErrUnsupported, id, r.Exit, firstMeaningful(r.Stdout, r.Stderr))
	}
	for _, line := range cleanLines(r.Stdout) {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "Version") {
			continue
		}
		if v := strings.TrimSpace(value); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("%w: winget show %s reported no version: %s", ErrUnsupported, id, firstMeaningful(r.Stdout, r.Stderr))
}

// installed returns the installed version of a package, or "" when the
// package is not installed.
func (p *Provider) installed(ctx context.Context, path, id string) (string, error) {
	r, err := p.run(ctx, path, "list", "--id", id, "--exact")
	if err != nil {
		return "", err
	}
	if r.Exit != 0 {
		if noInstalledPackage(cleanLines(r.Stdout)) || noInstalledPackage(cleanLines(r.Stderr)) {
			return "", nil
		}
		return "", fmt.Errorf("winget list %s exited 0x%X: %s", id, r.Exit, firstMeaningful(r.Stdout, r.Stderr))
	}
	return listVersion(cleanLines(r.Stdout), id), nil
}

// Check re-evaluates one of this provider's preconditions at apply time: the
// tool is still the one that was planned, and the machine scope still has an
// elevated process.
func (p *Provider) Check(ctx context.Context, c plan.Precondition) (string, error) {
	switch {
	case c.Kind == "software" && c.Subject == Name:
		path, err := p.toolPath()
		if err != nil {
			return "absent", err
		}
		if was := strings.TrimPrefix(c.Requirement, "present at "); was != path {
			return path, fmt.Errorf("winget moved from %s to %s", was, path)
		}
		v, err := p.version(ctx, path)
		if err != nil {
			return "absent", err
		}
		if v != c.Observed {
			return v, fmt.Errorf("winget changed from %s to %s", c.Observed, v)
		}
		return v, nil
	case c.Kind == "privilege" && c.Subject == "winget-scope":
		if !p.isElevated() {
			return "not elevated", ErrPrivilege
		}
		return "elevated", nil
	}
	return "", fmt.Errorf("unknown winget precondition %s %s", c.Kind, c.Subject)
}

// Step returns the performer of one saved winget operation. Only the
// operations this provider plans are accepted: the package, the pinned
// version and, for an install, the scope.
func (p *Provider) Step(op plan.Operation) (execute.Step, error) {
	id, version := op.Params["package"], op.Params["version"]
	if op.Provider != Name || !validPackageID(id) || version == "" {
		return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
	}
	switch op.Action {
	case "create":
		if len(op.Params) != 3 {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
		scope := op.Params["scope"]
		if scope != scopeUser && scope != scopeMachine {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
		return &step{p: p, action: "create", id: id, version: version, scope: scope}, nil
	case "update":
		if len(op.Params) != 2 {
			return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
		}
		return &step{p: p, action: "update", id: id, version: version}, nil
	}
	return nil, fmt.Errorf("%w: operation %s", ErrUnsupported, op.ID)
}

// step applies one planned winget operation and verifies it independently.
type step struct {
	p           *Provider
	action      string
	id, version string
	scope       string
}

// Apply runs the pinned version: the version the plan carries, never the
// latest one the source may offer later.
func (s *step) Apply(ctx context.Context) (execute.Evidence, error) {
	path, err := s.p.toolPath()
	if err != nil {
		return nil, err
	}
	var args []string
	switch s.action {
	case "create":
		args = []string{"install", "--id", s.id, "--exact", "--version", s.version, "--scope", s.scope, "--silent", "--accept-package-agreements"}
	case "update":
		args = []string{"upgrade", "--id", s.id, "--exact", "--version", s.version, "--silent", "--accept-package-agreements"}
	}
	full := argv(args...)
	r, err := s.p.run(ctx, path, args...)
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "winget " + strings.Join(full, " "), "exit": r.Exit}
	if r.Exit != 0 {
		return ev, fmt.Errorf("winget %s exited 0x%X: %s", s.action, r.Exit, firstMeaningful(r.Stdout, r.Stderr))
	}
	return ev, nil
}

// Verify re-reads the installed state independently of the install's own
// exit status and requires the planned version.
func (s *step) Verify(ctx context.Context) (execute.Evidence, error) {
	path, err := s.p.toolPath()
	if err != nil {
		return nil, err
	}
	r, err := s.p.run(ctx, path, "list", "--id", s.id, "--exact")
	if err != nil {
		return nil, err
	}
	ev := execute.Evidence{"command": "winget list --id " + s.id + " --exact", "exit": r.Exit}
	if r.Exit != 0 {
		if noInstalledPackage(cleanLines(r.Stdout)) || noInstalledPackage(cleanLines(r.Stderr)) {
			return ev, fmt.Errorf("%s is not installed after winget %s", s.id, s.action)
		}
		return ev, fmt.Errorf("winget list %s exited 0x%X: %s", s.id, r.Exit, firstMeaningful(r.Stdout, r.Stderr))
	}
	installed := listVersion(cleanLines(r.Stdout), s.id)
	ev["installed"] = installed
	switch {
	case installed == "":
		return ev, fmt.Errorf("%s is not installed after winget %s", s.id, s.action)
	case installed != s.version:
		return ev, fmt.Errorf("%s %s was planned but %s is installed", s.id, s.version, installed)
	}
	ev["version"] = installed
	return ev, nil
}

// cleanLines returns the meaningful lines of a winget output: the progress
// noise is removed (everything up to the last carriage return of a line, and
// the lines that carry only a progress bar or a spinner) and every line is
// trimmed. A carriage return that ends a line is a line ending, not a
// progress overwrite.
func cleanLines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if i := strings.LastIndex(line, "\r"); i >= 0 {
			line = line[i+1:]
		}
		line = strings.TrimSpace(line)
		if line == "" || isNoise(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// isNoise reports whether a line carries only a progress bar or a spinner.
func isNoise(line string) bool {
	for _, r := range line {
		switch r {
		case '-', '\\', '|', '/', '█', '▒', ' ':
		default:
			return false
		}
	}
	return true
}

// firstMeaningful is the first line of the output that says something.
func firstMeaningful(out, errOut []byte) string {
	for _, b := range [][]byte{out, errOut} {
		if lines := cleanLines(b); len(lines) > 0 {
			return lines[0]
		}
	}
	return ""
}

// noInstalledPackage reports the message winget prints when nothing matches
// the request.
func noInstalledPackage(lines []string) bool {
	for _, line := range lines {
		if strings.Contains(strings.ToLower(line), "no installed package found") {
			return true
		}
	}
	return false
}

// column is one column of a table: its name and the half-open range of the
// header line it covers.
type column struct {
	name       string
	start, end int
}

// columns locates the columns of a table by the token positions of its
// header; a column runs until the next token starts.
func columns(header string) []column {
	type token struct{ start, end int }
	var toks []token
	for i := 0; i < len(header); {
		if header[i] == ' ' || header[i] == '\t' {
			i++
			continue
		}
		j := i
		for j < len(header) && header[j] != ' ' && header[j] != '\t' {
			j++
		}
		toks = append(toks, token{i, j})
		i = j
	}
	out := make([]column, 0, len(toks))
	for k, t := range toks {
		end := len(header)
		if k+1 < len(toks) {
			end = toks[k+1].start
		}
		out = append(out, column{name: strings.ToLower(header[t.start:t.end]), start: t.start, end: end})
	}
	return out
}

// indexOf returns the position of a column in the header.
func indexOf(cols []column, name string) int {
	for i, c := range cols {
		if c.name == name {
			return i
		}
	}
	return -1
}

// field reads one column of a row.
func field(line string, c column) string {
	if c.start >= len(line) {
		return ""
	}
	end := c.end
	if end > len(line) {
		end = len(line)
	}
	return strings.TrimSpace(line[c.start:end])
}

// isListHeader reports whether a line is the header of the table `winget
// list` prints.
func isListHeader(line string) bool {
	cols := columns(line)
	for _, want := range []string{"name", "id", "version"} {
		if indexOf(cols, want) < 0 {
			return false
		}
	}
	return true
}

// listVersion reads the installed version of a package from the table `winget
// list` prints. The columns are located by the header, so a table with more
// or fewer columns still reads correctly, and the rows of other packages are
// ignored.
func listVersion(lines []string, id string) string {
	head := -1
	for i, line := range lines {
		if isListHeader(line) {
			head = i
			break
		}
	}
	if head < 0 {
		return ""
	}
	cols := columns(lines[head])
	idCol, versionCol := indexOf(cols, "id"), indexOf(cols, "version")
	if idCol < 0 || versionCol < 0 {
		return ""
	}
	for _, line := range lines[head+1:] {
		if strings.TrimSpace(line) == "" || isNoise(line) {
			continue
		}
		got, version := field(line, cols[idCol]), field(line, cols[versionCol])
		if !strings.EqualFold(got, id) {
			// A row whose columns are not padded to the positions of
			// the header is read by its fields instead.
			fields := strings.Fields(line)
			if len(fields) > max(idCol, versionCol) && strings.EqualFold(fields[idCol], id) {
				got, version = fields[idCol], fields[versionCol]
			}
		}
		if strings.EqualFold(got, id) {
			return version
		}
	}
	return ""
}

// Name is the provider name recipes use.
func (p *Provider) Name() string { return Name }

var _ provider.Provider = (*Provider)(nil)
