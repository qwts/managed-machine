// Package secret resolves secret:<name> references for configuration inputs
// (S4 stories C05 and C06, D5) and records resolved secrets as digest-only
// plan assets.
//
// Resolution checks MM_SECRET_<NAME> first through an injected getenv seam
// (never os.Environ directly), where <NAME> is the reference name uppercased
// with every character outside [A-Z0-9] replaced by '_'. When the environment
// does not hold the secret, the resolver invokes `pass show <name>` through
// the internal/provider.Runner seam (argv only, no shell, inherited
// environment) and takes the first line of stdout; `pass` missing from PATH
// or exiting nonzero yields nothing, and stderr from `pass` is never echoed.
// Neither source resolving is an UnresolvedError naming only the reference.
//
// Plans record each secret in bindings.assets as
// {name: "secret:<name>", sha256: sha256(name || 0x00 || value)} so secret
// values never enter the plan document, journal, output or diagnostics, and
// the asset digest is identical whether the value came from the environment
// or Pass.
package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/provider"
)

// Prefix marks a desired input value as a secret reference ("secret:<name>").
const Prefix = "secret:"

// EnvPrefix begins every environment variable that supplies a secret (D5).
// The prefix prevents a reference such as secret:PATH from reading an
// arbitrary process environment variable.
const EnvPrefix = "MM_SECRET_"

// Tool is the Pass executable name resolved on PATH when MM_SECRET_<NAME>
// is unset (S4 story C06, D5).
const Tool = "pass"

// Result, Runner and Exec are the shared process seam from internal/provider;
// the secret resolver only ever starts the pass executable with argv
// ["show", <name>] and never a shell string.
type (
	Result = provider.Result
	Runner = provider.Runner
)

// Exec is the real Runner.
var Exec = provider.Exec

// ParseRef reports whether raw is a secret:<name> reference and returns its
// reference name.
func ParseRef(raw string) (name string, ok bool) {
	return strings.CutPrefix(raw, Prefix)
}

// Ref returns the canonical secret:<name> reference string for name.
func Ref(name string) string {
	return Prefix + name
}

// EnvVar maps a secret reference name to its D5 environment variable name
// MM_SECRET_<NAME>: the reference name is uppercased and every character
// outside [A-Z0-9] is replaced by '_'.
func EnvVar(name string) string {
	var b strings.Builder
	b.Grow(len(EnvPrefix) + len(name))
	b.WriteString(EnvPrefix)
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteByte(byte(r - 'a' + 'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteByte(byte(r))
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// Digest returns the lowercase hexadecimal SHA-256 digest of a resolved
// secret: sha256(name || 0x00 || value) (D5).
func Digest(name, value string) string {
	h := sha256.New()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(value))
	return hex.EncodeToString(h.Sum(nil))
}

// Asset returns the digest-only plan.Asset for a resolved secret:
// {Name: "secret:<name>", SHA256: sha256(name || 0x00 || value)}.
func Asset(name, value string) plan.Asset {
	return plan.Asset{
		Name:   Ref(name),
		SHA256: Digest(name, value),
	}
}

// Source resolves a secret by reference name when an earlier source did not.
// A source that does not hold the secret returns ("", false, nil) so the
// resolver can continue or report UnresolvedError.
type Source func(ctx context.Context, name string) (value string, ok bool, err error)

// Pass returns a Source that runs argv `pass show <name>` through run
// (defaulting to Exec) with the inherited environment from environ, taking
// the first line of stdout and never echoing stderr. When `pass` is not on
// the PATH of environ or exits nonzero, the source yields nothing.
func Pass(run Runner, environ func() []string, lookPath ...func(string) (string, error)) Source {
	var lp func(string) (string, error)
	if len(lookPath) > 0 {
		lp = lookPath[0]
	}
	return func(ctx context.Context, name string) (string, bool, error) {
		return runPass(ctx, name, run, environ, lp)
	}
}

func runPass(ctx context.Context, name string, run Runner, environ func() []string, lp func(string) (string, error)) (string, bool, error) {
	var env []string
	if environ != nil {
		base := environ()
		env = make([]string, len(base))
		copy(env, base)
	}
	var (
		exe string
		err error
	)
	switch {
	case lp != nil:
		exe, err = lp(Tool)
	case envValue(env, "PATH") == "" && run != nil:
		exe = Tool
	default:
		exe, err = lookPath(Tool, env)
	}
	if err != nil || exe == "" {
		return "", false, nil
	}
	if run == nil {
		run = Exec
	}
	res, err := run(ctx, exe, []string{"show", name}, env)
	if err != nil || res.Exit != 0 {
		return "", false, nil
	}
	line, _, _ := strings.Cut(string(res.Stdout), "\n")
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return "", false, nil
	}
	return line, true, nil
}

// UnresolvedError reports a secret:<name> reference that resolved from
// neither source. Its message names only the reference, never any value.
type UnresolvedError struct {
	Name string
}

func (e *UnresolvedError) Error() string {
	return Ref(e.Name) + ": could not be resolved"
}

// Resolver resolves secret:<name> references from the injected getenv seam
// first (MM_SECRET_<NAME>), then from Fallback (or Pass when Run, Environ
// or LookPath is configured).
type Resolver struct {
	// Getenv reads one environment variable by name. When nil, no
	// environment variable is set.
	Getenv func(string) string
	// Run starts one pass process (argv ["show", name], never a shell
	// string) when MM_SECRET_<NAME> is unset. When nil, Exec is used.
	Run Runner
	// Environ returns the inherited environment passed to pass and
	// searched for its executable on PATH.
	Environ func() []string
	// LookPath locates the pass executable by name. When nil, the PATH
	// of Environ is searched.
	LookPath func(file string) (string, error)
	// Fallback is the optional secondary source consulted when
	// MM_SECRET_<NAME> yields nothing. When nil and Run, Environ or
	// LookPath is set, Pass is consulted.
	Fallback Source
}

// NewResolver constructs a Resolver that reads MM_SECRET_<NAME> through
// getenv and falls back to fallback when unset.
func NewResolver(getenv func(string) string, fallback ...Source) Resolver {
	var fb Source
	if len(fallback) > 0 {
		fb = fallback[0]
	}
	return Resolver{Getenv: getenv, Fallback: fb}
}

// Resolve resolves name across the configured sources. Neither source
// yielding a non-empty value returns *UnresolvedError naming only the
// reference.
func (r Resolver) Resolve(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", &UnresolvedError{Name: name}
	}
	if r.Getenv != nil {
		if v := r.Getenv(EnvVar(name)); v != "" {
			return v, nil
		}
	}
	fb := r.Fallback
	if fb == nil && (r.Run != nil || r.Environ != nil || r.LookPath != nil) {
		fb = Pass(r.Run, r.Environ, r.LookPath)
	}
	if fb != nil {
		if v, ok, err := fb(ctx, name); err == nil && ok && v != "" {
			return v, nil
		}
	}
	return "", &UnresolvedError{Name: name}
}
