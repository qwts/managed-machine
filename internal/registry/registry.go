// Package registry is the versioned provider registry. Built-in providers
// and providers declared by extension manifests are registered the same
// way, so nothing in the loader switches on a provider name.
package registry

import (
	"fmt"
	"sort"

	"github.com/qwts/managed-machine/internal/schema"
)

// Version is the registry format version recorded with normalized output.
// It changes when an option type or the provider record shape changes.
const Version = 1

// OptionType is the value type an option accepts.
type OptionType string

const (
	String  OptionType = "string"
	Strings OptionType = "strings"
	Bool    OptionType = "bool"
)

// Option is one provider-specific setting a recipe may declare.
type Option struct {
	Type     OptionType `json:"type" yaml:"type"`
	Required bool       `json:"required,omitempty" yaml:"required"`
}

// Provider describes an installer adapter by its option schema only. The
// registry never says which platforms a provider works on: support is
// declared per recipe and proven by tests, not inferred from availability.
type Provider struct {
	Name    string            `json:"name"`
	Origin  string            `json:"origin"`
	Options map[string]Option `json:"options"`
}

// Registry maps provider names to providers.
type Registry struct {
	providers map[string]Provider
}

// New returns an empty registry.
func New() *Registry { return &Registry{providers: map[string]Provider{}} }

// Builtin returns a registry holding the built-in providers. The option
// sets cover the trust fields the legacy catalog rows carry (formula or
// cask token, signing team, allowed download and homepage hosts, pinned
// digest, rolling-URL exception) so the catalog
// adapter can represent every row without inventing fields.
func Builtin() *Registry {
	r := New()
	for _, p := range []Provider{
		{Name: "brew", Options: map[string]Option{
			"formula": {Type: String}, "cask": {Type: String},
			"app_name": {Type: String}, "team_id": {Type: String},
			"url_hosts": {Type: Strings}, "homepage_hosts": {Type: Strings},
			"allow_rolling_url": {Type: Bool},
		}},
		{Name: "uv", Options: map[string]Option{"tool": {Type: String, Required: true}}},
		{Name: "npm", Options: map[string]Option{"package": {Type: String, Required: true}}},
		{Name: "cargo", Options: map[string]Option{"crate": {Type: String, Required: true}}},
		{Name: "winget", Options: map[string]Option{"package_id": {Type: String, Required: true}}},
		{Name: "go", Options: map[string]Option{
			"module": {Type: String, Required: true}, "package": {Type: String, Required: true},
		}},
		{Name: "download", Options: map[string]Option{
			"url": {Type: String, Required: true}, "url_hosts": {Type: Strings, Required: true},
			"homepage_hosts": {Type: Strings}, "team_id": {Type: String}, "app_name": {Type: String},
			"sha256": {Type: String}, "allow_rolling_url": {Type: Bool},
		}},
	} {
		if err := r.Register("builtin", p); err != nil {
			panic(err)
		}
	}
	return r
}

// Register adds a provider. A name already registered is a conflict, never
// an override, whichever origin registered it first.
func (r *Registry) Register(origin string, p Provider) error {
	doc := origin
	if !schema.ValidID(p.Name) {
		return schema.Errorf(schema.Invalid, doc, "provider", "invalid provider name %q", p.Name)
	}
	if prev, ok := r.providers[p.Name]; ok {
		return schema.Errorf(schema.Conflict, doc, "provider", "provider %q is already registered by %s", p.Name, prev.Origin)
	}
	for name, o := range p.Options {
		if !validOptionName(name) {
			return schema.Errorf(schema.Invalid, doc, "provider."+p.Name, "invalid option name %q", name)
		}
		switch o.Type {
		case String, Strings, Bool:
		default:
			return schema.Errorf(schema.Invalid, doc, "provider."+p.Name+"."+name, "unknown option type %q", o.Type)
		}
	}
	opts := make(map[string]Option, len(p.Options))
	for k, v := range p.Options {
		opts[k] = v
	}
	r.providers[p.Name] = Provider{Name: p.Name, Origin: origin, Options: opts}
	return nil
}

// Lookup returns the named provider.
func (r *Registry) Lookup(name string) (Provider, bool) {
	p, ok := r.providers[name]
	return p, ok
}

// Names returns the registered provider names in sorted order.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.providers))
	for n := range r.providers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func validOptionName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		if !(c >= 'a' && c <= 'z' || c == '_' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// String is for diagnostics only.
func (p Provider) String() string { return fmt.Sprintf("%s (%s)", p.Name, p.Origin) }
