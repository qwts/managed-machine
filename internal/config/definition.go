// Package config parses and validates configuration definitions (the
// "kind: config" documents of the configuration schema), resolves the
// desired configuration of one target and renders it through the
// substitution engine. It is data only: nothing here writes a file, reads
// a secret or starts a program.
//
// A configuration definition declares what a machine should look like, not
// where: the destinations are logical names, and the desired configuration
// maps them to the paths of one target.
package config

import (
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/qwts/managed-machine/internal/config/render"
	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/target"
)

// SchemaVersion is the only configuration definition format version
// accepted.
const SchemaVersion = 1

// The operation providers a configuration definition may declare. Each owns
// the destination in its own way: file and template own the whole file,
// structured owns the keys it declares, managed-block owns its block.
const (
	ProviderFile         = "file"
	ProviderTemplate     = "template"
	ProviderStructured   = "structured"
	ProviderManagedBlock = "managed-block"
)

// Providers returns the operation providers in the order they are listed.
func Providers() []string {
	return []string{ProviderFile, ProviderTemplate, ProviderStructured, ProviderManagedBlock}
}

// The formats a structured operation may declare. The format is never
// inferred from the path of the destination.
const (
	FormatJSON = "json"
	FormatTOML = "toml"
)

// DefaultComment is the marker prefix of a managed block.
const DefaultComment = "#"

// Definition is a normalized configuration definition. Field order is the
// JSON key order of the normalized record.
type Definition struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	Description   string            `json:"description"`
	Categories    []string          `json:"categories"`
	Platforms     []string          `json:"platforms"`
	Targets       []string          `json:"targets"`
	Inputs        map[string]string `json:"inputs"`
	Operations    []Operation       `json:"operations"`
	Activation    map[string]string `json:"activation"`
	Requires      []Requirement     `json:"requires"`
}

// Operation is one change the definition declares.
type Operation struct {
	ID          string            `json:"id"`
	Provider    string            `json:"provider"`
	Destination string            `json:"destination"`
	Source      string            `json:"source,omitempty"`
	Settings    map[string]string `json:"settings,omitempty"`
	Format      string            `json:"format,omitempty"`
	Keys        map[string]any    `json:"keys,omitempty"`
	BlockID     string            `json:"block_id,omitempty"`
	Content     string            `json:"content,omitempty"`
	Comment     string            `json:"comment,omitempty"`
}

// Requirement is one prerequisite of a configuration definition. A
// command is satisfied by the executable on the PATH; a software is
// satisfied by an applied installation receipt.
type Requirement struct {
	Software string `json:"software,omitempty"`
	Command  string `json:"command,omitempty"`
}

type rawDefinition struct {
	SchemaVersion *int              `yaml:"schema_version"`
	Kind          string            `yaml:"kind"`
	ID            string            `yaml:"id"`
	Description   string            `yaml:"description"`
	Categories    []string          `yaml:"categories"`
	Platforms     []string          `yaml:"platforms"`
	Targets       []string          `yaml:"targets"`
	Inputs        map[string]string `yaml:"inputs"`
	Operations    []rawOperation    `yaml:"operations"`
	Activation    map[string]string `yaml:"activation"`
	Requires      []rawRequirement  `yaml:"requires"`
}

type rawOperation struct {
	ID          string               `yaml:"id"`
	Provider    string               `yaml:"provider"`
	Destination string               `yaml:"destination"`
	Source      string               `yaml:"source"`
	Settings    map[string]string    `yaml:"settings"`
	Format      string               `yaml:"format"`
	Keys        map[string]yaml.Node `yaml:"keys"`
	BlockID     string               `yaml:"block_id"`
	Content     string               `yaml:"content"`
	Comment     string               `yaml:"comment"`
}

type rawRequirement struct {
	Software string `yaml:"software"`
	Command  string `yaml:"command"`
}

// Parse decodes and validates one configuration definition. doc names the
// document inside its source for error messages.
func Parse(doc string, data []byte) (Definition, error) {
	var raw rawDefinition
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Definition{}, err
	}
	if raw.SchemaVersion == nil {
		return Definition{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "missing")
	}
	if *raw.SchemaVersion != SchemaVersion {
		return Definition{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "unsupported version %d; supported: %d", *raw.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(raw.Kind) != definition.KindConfig {
		return Definition{}, schema.Errorf(schema.Invalid, doc, "kind", "a configuration definition declares kind: %s", definition.KindConfig)
	}
	if !schema.ValidID(raw.ID) {
		return Definition{}, schema.Errorf(schema.Invalid, doc, "id", "invalid configuration id %q", raw.ID)
	}
	if strings.TrimSpace(raw.Description) == "" {
		return Definition{}, schema.Errorf(schema.Invalid, doc, "description", "missing")
	}
	cats, err := uniqueIDs(doc, "categories", raw.Categories)
	if err != nil {
		return Definition{}, err
	}
	plats, err := platforms(doc, raw.Platforms)
	if err != nil {
		return Definition{}, err
	}
	tgts, err := targets(doc, raw.Targets)
	if err != nil {
		return Definition{}, err
	}
	inputs, err := names(doc, "inputs", raw.Inputs)
	if err != nil {
		return Definition{}, err
	}
	activation, err := names(doc, "activation", raw.Activation)
	if err != nil {
		return Definition{}, err
	}
	ops, err := operations(doc, raw.Operations)
	if err != nil {
		return Definition{}, err
	}
	reqs, err := requirements(doc, raw.Requires)
	if err != nil {
		return Definition{}, err
	}
	def := Definition{
		SchemaVersion: SchemaVersion, ID: raw.ID, Description: raw.Description, Categories: cats,
		Platforms: plats, Targets: tgts, Inputs: inputs, Operations: ops, Activation: activation, Requires: reqs,
	}
	if err := def.checkTemplates(doc); err != nil {
		return Definition{}, err
	}
	return def, nil
}

// InputNames returns the names of the definition's inputs.
func (d Definition) InputNames() map[string]bool {
	out := map[string]bool{}
	for name := range d.Inputs {
		out[name] = true
	}
	return out
}

// DestinationNames returns the logical names the definition's operations
// use. They are the only destinations a template may name.
func (d Definition) DestinationNames() map[string]bool {
	out := map[string]bool{}
	for _, op := range d.Operations {
		out[op.Destination] = true
	}
	return out
}

// Supports reports whether the definition declares the installation target
// and the host platform. A definition without platforms declares every
// target of the matrix.
func (d Definition) Supports(installationTarget string, platform target.Target) bool {
	found := false
	for _, t := range d.Targets {
		if t == installationTarget {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	if len(d.Platforms) == 0 {
		return true
	}
	for _, p := range d.Platforms {
		if p == string(platform) {
			return true
		}
	}
	return false
}

// checkTemplates validates the templates a definition carries: its
// activation values and its managed blocks. A template may only name an
// input or a destination the definition declares; a settings name belongs
// to one operation and is checked with that operation's source.
func (d Definition) checkTemplates(doc string) error {
	names := render.Names{Inputs: d.InputNames(), Destinations: d.DestinationNames(), Settings: map[string]bool{}}
	for _, key := range sortedKeys(d.Activation) {
		if err := render.Check(d.Activation[key], names); err != nil {
			return schema.Errorf(schema.Invalid, doc, "activation."+key, "%v", err)
		}
	}
	for i, op := range d.Operations {
		switch op.Provider {
		case ProviderTemplate:
			for _, key := range sortedKeys(op.Settings) {
				if err := render.Check(op.Settings[key], names); err != nil {
					return schema.Errorf(schema.Invalid, doc, field(i, "settings")+"."+key, "%v", err)
				}
			}
		case ProviderManagedBlock:
			if err := render.Check(op.Content, names); err != nil {
				return schema.Errorf(schema.Invalid, doc, field(i, "content"), "%v", err)
			}
		}
	}
	return nil
}

// operations validates the operations of a definition. An operation id is
// unique in the definition, so a plan can name one operation.
func operations(doc string, in []rawOperation) ([]Operation, error) {
	if len(in) == 0 {
		return nil, schema.Errorf(schema.Invalid, doc, "operations", "at least one operation is required")
	}
	out := []Operation{}
	seen := map[string]bool{}
	for i, ro := range in {
		op, err := operation(doc, i, ro)
		if err != nil {
			return nil, err
		}
		if seen[op.ID] {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "id"), "duplicate operation id %q", op.ID)
		}
		seen[op.ID] = true
		out = append(out, op)
	}
	return out, nil
}

func operation(doc string, i int, ro rawOperation) (Operation, error) {
	if !schema.ValidID(ro.ID) {
		return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "id"), "invalid operation id %q", ro.ID)
	}
	if !knownProvider(ro.Provider) {
		return Operation{}, schema.Errorf(schema.UnknownProvider, doc, field(i, "provider"),
			"unknown provider %q; known providers: %s", ro.Provider, strings.Join(Providers(), ", "))
	}
	if !schema.ValidID(ro.Destination) {
		return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "destination"), "invalid destination name %q", ro.Destination)
	}
	op := Operation{ID: ro.ID, Provider: ro.Provider, Destination: ro.Destination}
	switch ro.Provider {
	case ProviderFile:
		if err := only(doc, i, ro, "source"); err != nil {
			return Operation{}, err
		}
		if err := asset(doc, i, ro.Source); err != nil {
			return Operation{}, err
		}
		op.Source = ro.Source
	case ProviderTemplate:
		if err := only(doc, i, ro, "source", "settings"); err != nil {
			return Operation{}, err
		}
		if err := asset(doc, i, ro.Source); err != nil {
			return Operation{}, err
		}
		op.Source = ro.Source
		settings, err := names(doc, field(i, "settings"), ro.Settings)
		if err != nil {
			return Operation{}, err
		}
		if len(settings) > 0 {
			op.Settings = settings
		}
	case ProviderStructured:
		if err := only(doc, i, ro, "format", "keys"); err != nil {
			return Operation{}, err
		}
		if strings.TrimSpace(ro.Format) == "" {
			return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "format"),
				"missing; a structured operation declares format: %s or %s", FormatJSON, FormatTOML)
		}
		if ro.Format != FormatJSON && ro.Format != FormatTOML {
			return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "format"),
				"unknown format %q; supported: %s, %s", ro.Format, FormatJSON, FormatTOML)
		}
		op.Format = ro.Format
		keys, err := keyValues(doc, field(i, "keys"), ro.Keys)
		if err != nil {
			return Operation{}, err
		}
		if len(keys) > 0 {
			op.Keys = keys
		}
	case ProviderManagedBlock:
		if err := only(doc, i, ro, "block_id", "content", "comment"); err != nil {
			return Operation{}, err
		}
		if !schema.ValidID(ro.BlockID) {
			return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "block_id"), "invalid block id %q", ro.BlockID)
		}
		if strings.TrimSpace(ro.Content) == "" {
			return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "content"), "missing")
		}
		op.BlockID = ro.BlockID
		op.Content = ro.Content
		if ro.Comment == "" {
			op.Comment = DefaultComment
		} else if strings.ContainsAny(ro.Comment, "\r\n") {
			return Operation{}, schema.Errorf(schema.Invalid, doc, field(i, "comment"), "must be one line")
		} else {
			op.Comment = ro.Comment
		}
	}
	return op, nil
}

// opFields are the fields an operation may carry, in a fixed order, so a
// diagnostic names the same field every time.
var opFields = []string{"source", "settings", "format", "keys", "block_id", "content", "comment"}

// only rejects the fields the provider of an operation does not accept, so
// a definition never carries a field that would be ignored.
func only(doc string, i int, ro rawOperation, accepted ...string) error {
	allowed := map[string]bool{}
	for _, name := range accepted {
		allowed[name] = true
	}
	for _, name := range opFields {
		if allowed[name] {
			continue
		}
		if opSet(ro, name) {
			return schema.Errorf(schema.Invalid, doc, field(i, name),
				"the %s provider does not accept %q", ro.Provider, name)
		}
	}
	return nil
}

// opSet reports whether an operation carries a field.
func opSet(ro rawOperation, name string) bool {
	switch name {
	case "source":
		return ro.Source != ""
	case "settings":
		return len(ro.Settings) > 0
	case "format":
		return ro.Format != ""
	case "keys":
		return len(ro.Keys) > 0
	case "block_id":
		return ro.BlockID != ""
	case "content":
		return ro.Content != ""
	case "comment":
		return ro.Comment != ""
	}
	return false
}

// asset validates the path of an asset inside the source: it is relative
// and never leaves the source.
func asset(doc string, i int, p string) error {
	if strings.TrimSpace(p) == "" {
		return schema.Errorf(schema.Invalid, doc, field(i, "source"), "missing")
	}
	if !fs.ValidPath(p) {
		return schema.Errorf(schema.Invalid, doc, field(i, "source"),
			"must be a relative path inside the source; got %q", p)
	}
	if path.IsAbs(p) {
		return schema.Errorf(schema.Invalid, doc, field(i, "source"), "must be relative to the source; got %q", p)
	}
	return nil
}

// keyValues validates the keys of a structured operation: dot-separated
// key paths whose segments are never empty, and scalar or string-list
// values.
func keyValues(doc, f string, in map[string]yaml.Node) (map[string]any, error) {
	out := map[string]any{}
	names := make([]string, 0, len(in))
	for name := range in {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validKeyPath(name); err != nil {
			return nil, schema.Errorf(schema.Invalid, doc, f+"."+name, "%v", err)
		}
		node := in[name]
		v, err := keyValue(doc, f+"."+name, &node)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

// validKeyPath reports whether a key path is a dot-separated path whose
// segments are never empty.
func validKeyPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("the key path is empty")
	}
	for _, segment := range strings.Split(p, ".") {
		if segment == "" {
			return fmt.Errorf("the key path %q has an empty segment", p)
		}
	}
	return nil
}

// keyValue reads one value of a structured operation.
func keyValue(doc, f string, n *yaml.Node) (any, error) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		return nil, schema.Errorf(schema.Invalid, doc, f, "YAML anchors and aliases are not accepted")
	}
	switch n.Kind {
	case yaml.ScalarNode:
		switch n.Tag {
		case "!!str":
			return n.Value, nil
		case "!!bool":
			return n.Value == "true", nil
		case "!!int":
			v, err := strconv.ParseInt(n.Value, 10, 64)
			if err != nil {
				return nil, schema.Errorf(schema.Invalid, doc, f, "%v", err)
			}
			return v, nil
		case "!!float":
			v, err := strconv.ParseFloat(n.Value, 64)
			if err != nil {
				return nil, schema.Errorf(schema.Invalid, doc, f, "%v", err)
			}
			return v, nil
		}
	case yaml.SequenceNode:
		out := []string{}
		for _, c := range n.Content {
			if c.Kind != yaml.ScalarNode || c.Tag != "!!str" {
				return nil, schema.Errorf(schema.Invalid, doc, f, "must be a list of strings")
			}
			out = append(out, c.Value)
		}
		return out, nil
	}
	return nil, schema.Errorf(schema.Invalid, doc, f, "must be a scalar or a list of strings")
}

// requirements validates the prerequisites of a definition.
func requirements(doc string, in []rawRequirement) ([]Requirement, error) {
	out := []Requirement{}
	for i, r := range in {
		if r.Software == "" && r.Command == "" {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "software"),
				"a requirement names a software or a command")
		}
		if r.Software != "" && !schema.ValidID(r.Software) {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "software"), "invalid software id %q", r.Software)
		}
		if r.Command != "" && !schema.ValidID(r.Command) {
			return nil, schema.Errorf(schema.Invalid, doc, field(i, "command"), "invalid command name %q", r.Command)
		}
		out = append(out, Requirement{Software: r.Software, Command: r.Command})
	}
	return out, nil
}

// platforms validates the platforms a definition declares and returns them
// in the canonical target order, so declaration order never changes the
// bytes. A definition without platforms declares every target.
func platforms(doc string, in []string) ([]string, error) {
	if len(in) == 0 {
		out := []string{}
		for _, t := range target.All() {
			out = append(out, string(t))
		}
		return out, nil
	}
	set := map[target.Target]bool{}
	for _, s := range in {
		t, err := target.Parse(s)
		if err != nil {
			return nil, schema.Errorf(schema.Invalid, doc, "platforms", "%v", err)
		}
		if set[t] {
			return nil, schema.Errorf(schema.Invalid, doc, "platforms", "duplicate platform %q", s)
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

// targets validates the installation targets a definition declares and
// returns them in the canonical order.
func targets(doc string, in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, schema.Errorf(schema.Invalid, doc, "targets", "at least one installation target is required")
	}
	known := map[string]bool{"global": true, "user": true, "custom": true}
	set := map[string]bool{}
	for _, s := range in {
		if !known[s] {
			return nil, schema.Errorf(schema.Invalid, doc, "targets",
				"unknown installation target %q; supported: global, user, custom", s)
		}
		if set[s] {
			return nil, schema.Errorf(schema.Invalid, doc, "targets", "duplicate installation target %q", s)
		}
		set[s] = true
	}
	out := []string{}
	for _, t := range []string{"global", "user", "custom"} {
		if set[t] {
			out = append(out, t)
		}
	}
	return out, nil
}

// names validates a string map whose keys are identifiers.
func names(doc, f string, in map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range sortedKeys(in) {
		if !schema.ValidID(name) {
			return nil, schema.Errorf(schema.Invalid, doc, f+"."+name, "invalid name %q", name)
		}
		out[name] = in[name]
	}
	return out, nil
}

func knownProvider(name string) bool {
	for _, p := range Providers() {
		if p == name {
			return true
		}
	}
	return false
}

// field names a field of one operation.
func field(i int, name string) string {
	return "operations[" + strconv.Itoa(i) + "]." + name
}

// uniqueIDs validates a list of identifiers and returns them in canonical
// order.
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

// sortedKeys returns the keys of a string map in a fixed order.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
