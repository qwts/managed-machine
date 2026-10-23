// Package definition parses and validates software definitions (the
// accepted F04 minimal schema: docs cli/software.md "Definition model") and
// produces their normalized JSON form. Parsing is data only: no field is
// evaluated, expanded or executed.
package definition

import (
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/target"
)

// SchemaVersion is the only software definition format version accepted.
const SchemaVersion = 1

// Kinds of definition document one source may hold. A document without a
// kind is a software definition, so a source written before the kinds
// existed keeps its meaning.
const (
	// KindSoftware is a software definition: the accepted F04 schema.
	KindSoftware = "software"
	// KindConfig is a configuration definition.
	KindConfig = "config"
	// KindFlavor is a flavor definition.
	KindFlavor = "flavor"
)

// Kinds returns the known kinds in the order they are listed.
func Kinds() []string { return []string{KindSoftware, KindConfig, KindFlavor} }

// KindOf returns the kind a definition document declares: its top-level
// "kind" field, or KindSoftware when it declares none. A document that is
// not a readable mapping is reported as a software definition, so the
// strict parse names the real error. An unknown kind is refused: a typo
// never turns one kind of definition into another.
func KindOf(doc string, data []byte) (string, error) {
	var probe struct {
		Kind string `yaml:"kind"`
	}
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return KindSoftware, nil
	}
	switch k := strings.TrimSpace(probe.Kind); k {
	case "":
		return KindSoftware, nil
	case KindSoftware, KindConfig, KindFlavor:
		return k, nil
	default:
		return "", schema.Errorf(schema.Invalid, doc, "kind", "unknown kind %q; known kinds: %s", k, strings.Join(Kinds(), ", "))
	}
}

// Installation targets a recipe may declare, in canonical order.
var installTargets = []string{"global", "user", "custom"}

// Software is a normalized software definition. Field order is the JSON
// key order of the normalized record.
type Software struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Description   string   `json:"description"`
	Categories    []string `json:"categories"`
	Recipes       []Recipe `json:"recipes"`
}

// Recipe is one supported way to install the software.
type Recipe struct {
	ID        string         `json:"id"`
	Provider  string         `json:"provider"`
	Package   string         `json:"package"`
	Platforms []string       `json:"platforms"`
	Targets   []string       `json:"targets"`
	Requires  []Requirement  `json:"requires"`
	Options   map[string]any `json:"options"`
}

// Requirement is a prerequisite. v1 knows one kind: other software.
type Requirement struct {
	Software string `json:"software"`
}

type rawSoftware struct {
	SchemaVersion *int        `yaml:"schema_version"`
	Kind          string      `yaml:"kind"`
	ID            string      `yaml:"id"`
	Description   string      `yaml:"description"`
	Categories    []string    `yaml:"categories"`
	Recipes       []rawRecipe `yaml:"recipes"`
}

type rawRecipe struct {
	ID        string               `yaml:"id"`
	Provider  string               `yaml:"provider"`
	Package   string               `yaml:"package"`
	Platforms []string             `yaml:"platforms"`
	Targets   []string             `yaml:"targets"`
	Requires  []Requirement        `yaml:"requires"`
	Options   map[string]yaml.Node `yaml:"options"`
}

// Parse decodes and validates one definition. doc names the document inside
// its source for error messages; reg supplies the provider option schemas.
func Parse(doc string, data []byte, reg *registry.Registry) (Software, error) {
	var raw rawSoftware
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Software{}, err
	}
	if raw.SchemaVersion == nil {
		return Software{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "missing")
	}
	if *raw.SchemaVersion != SchemaVersion {
		return Software{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "unsupported version %d; supported: %d", *raw.SchemaVersion, SchemaVersion)
	}
	if k := strings.TrimSpace(raw.Kind); k != "" && k != KindSoftware {
		return Software{}, schema.Errorf(schema.Invalid, doc, "kind", "%q is not a software definition", k)
	}
	if !schema.ValidID(raw.ID) {
		return Software{}, schema.Errorf(schema.Invalid, doc, "id", "invalid software id %q", raw.ID)
	}
	if strings.TrimSpace(raw.Description) == "" {
		return Software{}, schema.Errorf(schema.Invalid, doc, "description", "missing")
	}
	cats, err := uniqueIDs(doc, "categories", raw.Categories)
	if err != nil {
		return Software{}, err
	}
	if len(raw.Recipes) == 0 {
		return Software{}, schema.Errorf(schema.Invalid, doc, "recipes", "at least one recipe is required")
	}
	sw := Software{SchemaVersion: SchemaVersion, ID: raw.ID, Description: raw.Description, Categories: cats}
	seen := map[string]bool{}
	for i, rr := range raw.Recipes {
		r, err := parseRecipe(doc, i, rr, reg)
		if err != nil {
			return Software{}, err
		}
		if seen[r.ID] {
			return Software{}, schema.Errorf(schema.Conflict, doc, field(i, "id"), "duplicate recipe id %q", r.ID)
		}
		seen[r.ID] = true
		for _, req := range r.Requires {
			if req.Software == sw.ID {
				return Software{}, schema.Errorf(schema.Invalid, doc, field(i, "requires"), "software %q requires itself", sw.ID)
			}
		}
		sw.Recipes = append(sw.Recipes, r)
	}
	return sw, nil
}

func field(i int, name string) string {
	return "recipes[" + strconv.Itoa(i) + "]." + name
}

func parseRecipe(doc string, i int, rr rawRecipe, reg *registry.Registry) (Recipe, error) {
	p, ok := reg.Lookup(rr.Provider)
	if !ok {
		return Recipe{}, schema.Errorf(schema.UnknownProvider, doc, field(i, "provider"), "unknown provider %q; registered: %s", rr.Provider, strings.Join(reg.Names(), ", "))
	}
	// The accepted convention is <provider> or <provider>-<variant>, but
	// the accepted examples use "homebrew" for provider brew, so only the
	// id grammar is enforced until that contradiction is resolved.
	if !schema.ValidID(rr.ID) {
		return Recipe{}, schema.Errorf(schema.Invalid, doc, field(i, "id"), "invalid recipe id %q", rr.ID)
	}
	if strings.TrimSpace(rr.Package) == "" {
		return Recipe{}, schema.Errorf(schema.Invalid, doc, field(i, "package"), "missing")
	}
	plats, err := platforms(doc, i, rr.Platforms)
	if err != nil {
		return Recipe{}, err
	}
	tgts, err := targets(doc, i, rr.Targets)
	if err != nil {
		return Recipe{}, err
	}
	reqs := []Requirement{}
	seenReq := map[string]bool{}
	for _, q := range rr.Requires {
		if !schema.ValidID(q.Software) {
			return Recipe{}, schema.Errorf(schema.Invalid, doc, field(i, "requires"), "invalid software id %q", q.Software)
		}
		if seenReq[q.Software] {
			return Recipe{}, schema.Errorf(schema.Invalid, doc, field(i, "requires"), "duplicate requirement %q", q.Software)
		}
		seenReq[q.Software] = true
		reqs = append(reqs, q)
	}
	sort.Slice(reqs, func(a, b int) bool { return reqs[a].Software < reqs[b].Software })
	opts, err := options(doc, i, p, rr.Options)
	if err != nil {
		return Recipe{}, err
	}
	return Recipe{ID: rr.ID, Provider: p.Name, Package: rr.Package, Platforms: plats, Targets: tgts, Requires: reqs, Options: opts}, nil
}

// platforms validates exact target spellings and returns them in the
// canonical target order, so declaration order never changes the bytes.
func platforms(doc string, i int, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, schema.Errorf(schema.Invalid, doc, field(i, "platforms"), "at least one platform is required")
	}
	set := map[target.Target]bool{}
	for _, s := range in {
		t, err := target.Parse(s)
		if err != nil {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "platforms"), "%v", err)
		}
		if set[t] {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "platforms"), "duplicate platform %q", s)
		}
		set[t] = true
	}
	out := []string{}
	for _, t := range target.All() {
		if set[t] {
			out = append(out, string(t))
		}
	}
	return out, nil
}

func targets(doc string, i int, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, schema.Errorf(schema.Invalid, doc, field(i, "targets"), "at least one installation target is required")
	}
	set := map[string]bool{}
	for _, s := range in {
		known := false
		for _, t := range installTargets {
			known = known || s == t
		}
		if !known {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "targets"), "unknown installation target %q; supported: %s", s, strings.Join(installTargets, ", "))
		}
		if set[s] {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "targets"), "duplicate installation target %q", s)
		}
		set[s] = true
	}
	out := []string{}
	for _, t := range installTargets {
		if set[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// options checks every option against the provider's schema. Values are
// plain scalars or lists of strings; YAML tags, anchors and aliases are not
// accepted, so an option can never smuggle structure the provider did not
// declare.
func options(doc string, i int, p registry.Provider, in map[string]yaml.Node) (map[string]any, error) {
	out := map[string]any{}
	for name, node := range in {
		f := field(i, "options."+name)
		spec, ok := p.Options[name]
		if !ok {
			return nil, schema.Errorf(schema.UnknownOption, doc, f, "provider %q has no option %q", p.Name, name)
		}
		v, err := optionValue(doc, f, spec.Type, &node)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	for name, spec := range p.Options {
		if _, ok := out[name]; spec.Required && !ok {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "options."+name), "required by provider %q", p.Name)
		}
	}
	return out, nil
}

func optionValue(doc, f string, t registry.OptionType, n *yaml.Node) (any, error) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, schema.Errorf(schema.Invalid, doc, f, "YAML anchors and aliases are not accepted")
	}
	switch t {
	case registry.String:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" || n.Value == "" {
			return nil, schema.Errorf(schema.Invalid, doc, f, "must be a non-empty string")
		}
		return n.Value, nil
	case registry.Bool:
		if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" || (n.Value != "true" && n.Value != "false") {
			return nil, schema.Errorf(schema.Invalid, doc, f, "must be true or false")
		}
		return n.Value == "true", nil
	case registry.Strings:
		if n.Kind != yaml.SequenceNode || len(n.Content) == 0 {
			return nil, schema.Errorf(schema.Invalid, doc, f, "must be a non-empty list of strings")
		}
		out := []string{}
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode || c.Tag != "!!str" || c.Value == "" || c.Anchor != "" {
				return nil, schema.Errorf(schema.Invalid, doc, f, "must be a non-empty list of strings")
			}
			out = append(out, c.Value)
		}
		return out, nil
	}
	return nil, schema.Errorf(schema.Invalid, doc, f, "unknown option type %q", t)
}

func uniqueIDs(doc, f string, in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, s := range in {
		if !schema.ValidID(s) {
			return nil, schema.Errorf(schema.Invalid, doc, f, "invalid identifier %q", s)
		}
		if seen[s] {
			return nil, schema.Errorf(schema.Invalid, doc, f, "duplicate %q", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}
