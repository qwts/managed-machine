// Package secret resolves secret:<name> references for configuration inputs
// (S4 story C05, D5) and records resolved secrets as digest-only plan assets.
//
// Resolution checks MM_SECRET_<NAME> first through an injected getenv seam
// (never os.Environ directly), where <NAME> is the reference name uppercased
// with every character outside [A-Z0-9] replaced by '_'. When the environment
// does not hold the secret, the resolver consults its optional Fallback seam
// (left for the C06 pass source). Neither source resolving is an
// UnresolvedError naming only the reference.
//
// Plans record each secret in bindings.assets as
// {name: "secret:<name>", sha256: sha256(name || 0x00 || value)} so secret
// values never enter the plan document, journal, output or diagnostics.
package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/qwts/managed-machine/core/internal/plan"
)

// Prefix marks a desired input value as a secret reference ("secret:<name>").
const Prefix = "secret:"

// EnvPrefix begins every environment variable that supplies a secret (D5).
// The prefix prevents a reference such as secret:PATH from reading an
// arbitrary process environment variable.
const EnvPrefix = "MM_SECRET_"

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

// UnresolvedError reports a secret:<name> reference that resolved from
// neither source. Its message names only the reference, never any value.
type UnresolvedError struct {
	Name string
}

func (e *UnresolvedError) Error() string {
	return Ref(e.Name) + ": could not be resolved"
}

// Resolver resolves secret:<name> references from the injected getenv seam
// first (MM_SECRET_<NAME>), then from the optional Fallback source (the C06
// pass seam).
type Resolver struct {
	// Getenv reads one environment variable by name. When nil, no
	// environment variable is set.
	Getenv func(string) string
	// Fallback is the optional secondary source consulted when
	// MM_SECRET_<NAME> yields nothing (C06 pass seam).
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
	if r.Fallback != nil {
		if v, ok, err := r.Fallback(ctx, name); err == nil && ok && v != "" {
			return v, nil
		}
	}
	return "", &UnresolvedError{Name: name}
}
