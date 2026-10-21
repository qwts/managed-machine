// Package legacy reads the Bash-era catalog (managed-machine-config
// apps.json) as data and classifies every row against the F04 software
// model. It is a read-only migration report: it never fetches, refreshes
// or writes the catalog, and it never decides that a row is portable on
// its own. Anything it cannot carry is a named item, never a silent drop.
package legacy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"go.yaml.in/yaml/v3"

	"github.com/qwts/managed-machine/core/internal/definition"
	"github.com/qwts/managed-machine/core/internal/registry"
)

// Verdicts, from best to worst.
const (
	Mappable   = "mappable"   // carried in full by a candidate recipe
	Manual     = "manual"     // a candidate recipe exists; named items need a person
	Unmappable = "unmappable" // no candidate recipe; named items say why
)

// Item actions.
const (
	Carried  = "carried"  // copied into the candidate recipe
	Desired  = "desired"  // moved to desired input, never a recipe default
	Deferred = "deferred" // owned by a later story; recorded, not converted
	Policy   = "policy"   // carried under the proposed conversion policy
	Blocked  = "blocked"  // cannot be carried; needs a person or a schema delta
	Retained = "retained" // no candidate recipe yet; the value must survive into one
)

// Item records what happened to one legacy field.
type Item struct {
	Field  string `json:"field"`
	Action string `json:"action"`
	Reason string `json:"reason"`
}

// Row is the classification of one legacy catalog row.
type Row struct {
	Name     string               `json:"name"`
	Kind     string               `json:"kind"`
	Verdict  string               `json:"verdict"`
	Software *definition.Software `json:"software,omitempty"`
	Aliases  []string             `json:"aliases"`
	Auto     string               `json:"auto"`
	Desired  map[string]string    `json:"desired"`
	Items    []Item               `json:"items"`
}

// Collision records an alias or name claimed by more than one row.
type Collision struct {
	Alias string   `json:"alias"`
	Rows  []string `json:"rows"`
}

// Report is the full classification.
type Report struct {
	SchemaVersion int            `json:"schema_version"`
	Rows          []Row          `json:"rows"`
	Collisions    []Collision    `json:"collisions"`
	Counts        map[string]int `json:"counts"`
}

// The legacy catalog has only ever been exercised on macOS on Apple
// silicon. Candidates claim that pair and nothing else: provider
// availability elsewhere is not tested support (F04 risk).
const legacyPlatform = "darwin/arm64"

// knownFields are the fields each legacy kind is read for. A field outside
// its kind's set is a blocked item, so a new catalog field cannot pass
// through unreviewed.
var knownFields = map[string][]string{
	"signed-cask":  {"token", "app_name", "team_id", "url_hosts", "homepage_hosts", "allow_rolling_url"},
	"brew-formula": {"formula"},
	"npm":          {"package", "command", "display", "version"},
	"uv-tool":      {"package", "command", "display", "version"},
	"official-cli": {"url", "command", "display", "args", "env", "bin_dir"},
	"vendor-dmg":   {"app_name", "team_id", "url", "url_hosts", "sha256", "allow_rolling_url", "sparkle", "retire_cask", "url_arm64", "url_x86_64"},
	"opencode":     {},
	"devin":        {},
}

// common fields every kind may carry; handled before the kind mapping.
var common = []string{"name", "kind", "aliases", "auto"}

// Classify reads apps.json bytes and classifies every row. reg must be the
// built-in registry: a legacy row never names an extension provider.
func Classify(data []byte, reg *registry.Registry) (Report, error) {
	var doc struct {
		SchemaVersion *int                         `json:"schema_version"`
		Apps          []map[string]json.RawMessage `json:"apps"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return Report{}, fmt.Errorf("legacy catalog: %v", err)
	}
	if doc.SchemaVersion == nil || *doc.SchemaVersion != 1 {
		return Report{}, fmt.Errorf("legacy catalog: schema_version must be 1")
	}
	rep := Report{SchemaVersion: 1, Rows: []Row{}, Collisions: []Collision{}, Counts: map[string]int{}}
	claims := map[string][]string{}
	seen := map[string]bool{}
	for i, raw := range doc.Apps {
		row, err := classifyRow(raw, reg)
		if err != nil {
			return Report{}, fmt.Errorf("legacy catalog: apps[%d]: %v", i, err)
		}
		if seen[row.Name] {
			return Report{}, fmt.Errorf("legacy catalog: duplicate row name %q", row.Name)
		}
		seen[row.Name] = true
		claims[row.Name] = append(claims[row.Name], row.Name)
		for _, a := range row.Aliases {
			claims[a] = append(claims[a], row.Name)
		}
		rep.Rows = append(rep.Rows, row)
		rep.Counts[row.Verdict]++
	}
	for alias, rows := range claims {
		if len(rows) > 1 {
			sort.Strings(rows)
			rep.Collisions = append(rep.Collisions, Collision{Alias: alias, Rows: rows})
		}
	}
	sort.Slice(rep.Collisions, func(a, b int) bool { return rep.Collisions[a].Alias < rep.Collisions[b].Alias })
	sort.Slice(rep.Rows, func(a, b int) bool { return rep.Rows[a].Name < rep.Rows[b].Name })
	rep.Counts["rows"] = len(rep.Rows)
	return rep, nil
}

type rowReader struct {
	raw   map[string]json.RawMessage
	used  map[string]bool
	items []Item
}

func (r *rowReader) str(k string) (string, bool, error) {
	v, ok := r.raw[k]
	if !ok {
		return "", false, nil
	}
	r.used[k] = true
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return "", true, fmt.Errorf("%s must be a string", k)
	}
	return s, true, nil
}

func (r *rowReader) strs(k string) ([]string, bool, error) {
	v, ok := r.raw[k]
	if !ok {
		return nil, false, nil
	}
	r.used[k] = true
	var s []string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, true, fmt.Errorf("%s must be a list of strings", k)
	}
	return s, true, nil
}

func (r *rowReader) boolean(k string) (bool, bool, error) {
	v, ok := r.raw[k]
	if !ok {
		return false, false, nil
	}
	r.used[k] = true
	var b bool
	if err := json.Unmarshal(v, &b); err != nil {
		return false, true, fmt.Errorf("%s must be true or false", k)
	}
	return b, true, nil
}

func (r *rowReader) note(field, action, reason string) {
	r.used[field] = true
	r.items = append(r.items, Item{Field: field, Action: action, Reason: reason})
}

func classifyRow(raw map[string]json.RawMessage, reg *registry.Registry) (Row, error) {
	r := &rowReader{raw: raw, used: map[string]bool{}}
	name, ok, err := r.str("name")
	if err != nil || !ok || name == "" {
		return Row{}, fmt.Errorf("every row needs a string name")
	}
	kind, _, err := r.str("kind")
	if err != nil {
		return Row{}, err
	}
	row := Row{Name: name, Kind: kind, Aliases: []string{}, Desired: map[string]string{}}
	aliases, has, err := r.strs("aliases")
	if err != nil {
		return Row{}, err
	}
	if has {
		row.Aliases = append(row.Aliases, aliases...)
		sort.Strings(row.Aliases)
		r.note("aliases", Policy, "discovery aliases have no F04 field; kept in the report and checked for collisions (proposed policy on #252)")
	}
	// auto: omitted or true installs during bootstrap, false is setup-only.
	// It becomes membership of the desired bootstrap input, never a flavor.
	auto, has, err := r.boolean("auto")
	if err != nil {
		return Row{}, err
	}
	row.Auto = "bootstrap"
	if has && !auto {
		row.Auto = "setup-only"
	}
	if has {
		r.note("auto", Desired, "bootstrap membership is desired input, not part of the recipe")
	}

	fields, known := knownFields[kind]
	if !known {
		r.note("kind", Blocked, fmt.Sprintf("unknown legacy kind %q", kind))
		row.Verdict = Unmappable
		row.Items = r.items
		return finish(row, r), nil
	}
	sw, verdict, err := mapKind(r, name, kind, row.Desired)
	if err != nil {
		return Row{}, err
	}
	for k := range raw {
		if !r.used[k] && !contains(common, k) {
			if contains(fields, k) {
				return Row{}, fmt.Errorf("field %s of kind %s was read for nothing (adapter bug)", k, kind)
			}
			r.note(k, Blocked, fmt.Sprintf("field not known for kind %s; review before converting", kind))
		}
	}
	if sw == nil {
		for i, it := range r.items {
			if it.Action == Carried {
				r.items[i] = Item{Field: it.Field, Action: Retained, Reason: "no candidate recipe; the value stays in the source catalog and must be carried by the eventual recipe"}
			}
		}
	}
	if sw != nil {
		valid, err := validate(sw, reg)
		if err != nil {
			return Row{}, fmt.Errorf("candidate for %s is not a valid F04 definition: %v", name, err)
		}
		row.Software = &valid
	}
	row.Verdict = verdict
	for _, it := range r.items {
		if it.Action == Blocked && row.Verdict == Mappable {
			row.Verdict = Manual
		}
	}
	return finish(row, r), nil
}

func finish(row Row, r *rowReader) Row {
	row.Items = append([]Item{}, r.items...)
	sort.SliceStable(row.Items, func(a, b int) bool { return row.Items[a].Field < row.Items[b].Field })
	return row
}

// mapKind builds the candidate definition for one row. Each legacy field
// either lands in an option, moves to desired input, or becomes an item.
func mapKind(r *rowReader, name, kind string, desired map[string]string) (*definition.Software, string, error) {
	opts := map[string]any{}
	carry := func(field, opt string, isList bool) error {
		if isList {
			v, ok, err := r.strs(field)
			if err != nil || !ok {
				return err
			}
			opts[opt] = v
		} else {
			v, ok, err := r.str(field)
			if err != nil || !ok {
				return err
			}
			opts[opt] = v
		}
		r.note(field, Carried, "options."+opt)
		return nil
	}
	carryBool := func(field string) error {
		v, ok, err := r.boolean(field)
		if err != nil || !ok {
			return err
		}
		opts[field] = v
		r.note(field, Carried, "options."+field)
		return nil
	}
	version := func() error {
		v, ok, err := r.str("version")
		if err != nil || !ok {
			return err
		}
		desired["version"] = v
		r.note("version", Desired, "a pinned version is desired input, never a recipe default")
		return nil
	}
	describe := func() string {
		if d, ok, _ := r.str("display"); ok && d != "" {
			r.note("display", Carried, "description (the display name; a real description is manual work)")
			return d
		}
		r.items = append(r.items, Item{Field: "description", Action: Blocked, Reason: "no legacy source for the required description; the name is a placeholder"})
		return name
	}
	command := func() {
		if _, ok, _ := r.str("command"); ok {
			r.note("command", Deferred, "installed-command probe belongs to the S2 executor's verification, not the definition")
		}
	}
	var (
		provider, pkg string
		targets       []string
		verdict       = Mappable
	)
	switch kind {
	case "signed-cask":
		provider, targets = "brew", []string{"global"}
		tok, _, err := r.str("token")
		if err != nil {
			return nil, "", err
		}
		pkg = tok
		opts["cask"] = tok
		r.note("token", Carried, "package and options.cask")
		for _, f := range []string{"app_name", "team_id"} {
			if err := carry(f, f, false); err != nil {
				return nil, "", err
			}
		}
		for _, f := range []string{"url_hosts", "homepage_hosts"} {
			if err := carry(f, f, true); err != nil {
				return nil, "", err
			}
		}
		if err := carryBool("allow_rolling_url"); err != nil {
			return nil, "", err
		}
	case "brew-formula":
		provider, targets = "brew", []string{"global"}
		f, _, err := r.str("formula")
		if err != nil {
			return nil, "", err
		}
		pkg = f
		opts["formula"] = f
		r.note("formula", Carried, "package and options.formula")
	case "npm", "uv-tool":
		provider, targets = map[string]string{"npm": "npm", "uv-tool": "uv"}[kind], []string{"user"}
		p, _, err := r.str("package")
		if err != nil {
			return nil, "", err
		}
		pkg = p
		opts[map[string]string{"npm": "package", "uv-tool": "tool"}[kind]] = p
		r.note("package", Carried, "package and provider option")
		if err := version(); err != nil {
			return nil, "", err
		}
		command()
	case "vendor-dmg":
		provider, targets = "download", []string{"global"}
		pkg = name
		for _, f := range []string{"app_name", "team_id", "url", "sha256"} {
			if err := carry(f, f, false); err != nil {
				return nil, "", err
			}
		}
		if err := carry("url_hosts", "url_hosts", true); err != nil {
			return nil, "", err
		}
		if err := carryBool("allow_rolling_url"); err != nil {
			return nil, "", err
		}
		for _, f := range []string{"url_arm64", "url_x86_64"} {
			if _, ok := r.raw[f]; ok {
				r.note(f, Blocked, "per-architecture URLs have no F04 field; needs one recipe per platform or a schema delta")
			}
		}
		if _, ok, _ := r.boolean("sparkle"); ok {
			r.note("sparkle", Blocked, "self-updating app (Sparkle); update ownership needs a decision before S2")
		}
		if _, ok, _ := r.str("retire_cask"); ok {
			r.note("retire_cask", Blocked, "retiring a cask install is an executable migration step, not definition data")
		}
		if _, ok := opts["url"]; !ok {
			// The download provider needs one URL; without it there is no
			// candidate, and the per-architecture items say why.
			r.items = append(r.items, Item{Field: "url", Action: Blocked, Reason: "no single download URL for the candidate recipe"})
			return nil, Unmappable, nil
		}
	case "official-cli":
		for _, f := range []string{"url", "args", "env", "bin_dir"} {
			if _, ok := r.raw[f]; ok {
				r.note(f, Blocked, "runs a vendor install script; executable installation logic is not data (F04 risk)")
			}
		}
		describe()
		command()
		return nil, Unmappable, nil
	case "opencode", "devin":
		r.items = append(r.items, Item{Field: "kind", Action: Blocked, Reason: "bespoke Bash installer with no declarative fields; needs a provider or an extension"})
		return nil, Unmappable, nil
	}
	sw := &definition.Software{SchemaVersion: definition.SchemaVersion, ID: name, Description: describe(), Categories: []string{}, Recipes: []definition.Recipe{{
		ID: provider, Provider: provider, Package: pkg, Platforms: []string{legacyPlatform}, Targets: targets, Requires: []definition.Requirement{}, Options: opts,
	}}}
	if verdict == Mappable {
		for _, it := range r.items {
			if it.Action == Blocked {
				verdict = Manual
			}
		}
	}
	return sw, verdict, nil
}

// validate round-trips the candidate through the F04 parser, so a
// candidate the real loader would reject can never appear in the report.
func validate(sw *definition.Software, reg *registry.Registry) (definition.Software, error) {
	b, err := CandidateYAML(*sw)
	if err != nil {
		return definition.Software{}, err
	}
	return definition.Parse(sw.ID+".yaml", b, reg)
}

// CandidateYAML renders a candidate as an F04 definition file.
func CandidateYAML(sw definition.Software) ([]byte, error) {
	type recipe struct {
		ID        string                   `yaml:"id"`
		Provider  string                   `yaml:"provider"`
		Package   string                   `yaml:"package"`
		Platforms []string                 `yaml:"platforms"`
		Targets   []string                 `yaml:"targets"`
		Requires  []definition.Requirement `yaml:"requires"`
		Options   map[string]any           `yaml:"options"`
	}
	type doc struct {
		SchemaVersion int      `yaml:"schema_version"`
		ID            string   `yaml:"id"`
		Description   string   `yaml:"description"`
		Categories    []string `yaml:"categories"`
		Recipes       []recipe `yaml:"recipes"`
	}
	d := doc{SchemaVersion: sw.SchemaVersion, ID: sw.ID, Description: sw.Description, Categories: sw.Categories}
	for _, r := range sw.Recipes {
		d.Recipes = append(d.Recipes, recipe{r.ID, r.Provider, r.Package, r.Platforms, r.Targets, r.Requires, r.Options})
	}
	return yaml.Marshal(d)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Summary is a one-line human summary used by the report command.
func (rep Report) Summary() string {
	return fmt.Sprintf("%d rows: %d mappable, %d manual, %d unmappable; %d alias collisions",
		rep.Counts["rows"], rep.Counts[Mappable], rep.Counts[Manual], rep.Counts[Unmappable], len(rep.Collisions))
}
