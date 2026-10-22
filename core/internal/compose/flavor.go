// Package compose parses and validates flavor definitions ("kind: flavor",
// ADR-0011) and implements the pure flavor composer (S4 story C07, decision
// D6). Composition is data only: it deduplicates software entries by
// "software:<id>@<target>" and configuration operations by
// "config:<id>@<target>:<operation>" and destination/value pair, and reports
// per-identity conflicts when version strings or destination values disagree.
package compose

import (
	"sort"
	"strconv"
	"strings"

	"github.com/qwts/managed-machine/core/internal/definition"
	"github.com/qwts/managed-machine/core/internal/schema"
)

// SchemaVersion is the only flavor definition format version accepted.
const SchemaVersion = 1

// Installation targets a flavor entry may declare, in canonical order
// (ADR-0011: a flavor entry's target is global, user or custom; the custom
// root always comes from the desired configuration that composes it).
var entryTargets = []string{"global", "user", "custom"}

// Flavor is a normalized flavor definition. Field order is the JSON key
// order of the normalized record.
type Flavor struct {
	SchemaVersion int             `json:"schema_version"`
	ID            string          `json:"id"`
	Description   string          `json:"description"`
	Categories    []string        `json:"categories"`
	Software      []SoftwareEntry `json:"software"`
	Config        []ConfigEntry   `json:"config"`
}

// SoftwareEntry is one software item a flavor declares for a target.
type SoftwareEntry struct {
	ID      string `json:"id"`
	Target  string `json:"target"`
	Version string `json:"version,omitempty"`
}

// ConfigEntry is one configuration definition a flavor declares for a
// target, with optional input overrides.
type ConfigEntry struct {
	ID     string            `json:"id"`
	Target string            `json:"target"`
	Inputs map[string]string `json:"inputs,omitempty"`
}

type rawFlavor struct {
	SchemaVersion *int               `yaml:"schema_version"`
	Kind          string             `yaml:"kind"`
	ID            string             `yaml:"id"`
	Description   string             `yaml:"description"`
	Categories    []string           `yaml:"categories"`
	Software      []rawSoftwareEntry `yaml:"software"`
	Config        []rawConfigEntry   `yaml:"config"`
}

type rawSoftwareEntry struct {
	ID      string `yaml:"id"`
	Target  string `yaml:"target"`
	Root    string `yaml:"root"`
	Version string `yaml:"version"`
}

type rawConfigEntry struct {
	ID     string            `yaml:"id"`
	Target string            `yaml:"target"`
	Root   string            `yaml:"root"`
	Inputs map[string]string `yaml:"inputs"`
}

// Parse decodes and validates one flavor definition. doc names the document
// inside its source for error messages.
func Parse(doc string, data []byte) (Flavor, error) {
	var raw rawFlavor
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Flavor{}, err
	}
	if raw.SchemaVersion == nil {
		return Flavor{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "missing")
	}
	if *raw.SchemaVersion != SchemaVersion {
		return Flavor{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "unsupported version %d; supported: %d", *raw.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(raw.Kind) != definition.KindFlavor {
		return Flavor{}, schema.Errorf(schema.Invalid, doc, "kind", "a flavor definition declares kind: %s", definition.KindFlavor)
	}
	if !schema.ValidID(raw.ID) {
		return Flavor{}, schema.Errorf(schema.Invalid, doc, "id", "invalid flavor id %q", raw.ID)
	}
	if strings.TrimSpace(raw.Description) == "" {
		return Flavor{}, schema.Errorf(schema.Invalid, doc, "description", "missing")
	}
	cats, err := uniqueIDs(doc, "categories", raw.Categories)
	if err != nil {
		return Flavor{}, err
	}
	sw, err := parseSoftwareEntries(doc, raw.Software)
	if err != nil {
		return Flavor{}, err
	}
	cfg, err := parseConfigEntries(doc, raw.Config)
	if err != nil {
		return Flavor{}, err
	}
	return Flavor{
		SchemaVersion: SchemaVersion,
		ID:            raw.ID,
		Description:   raw.Description,
		Categories:    cats,
		Software:      sw,
		Config:        cfg,
	}, nil
}

func parseSoftwareEntries(doc string, in []rawSoftwareEntry) ([]SoftwareEntry, error) {
	out := make([]SoftwareEntry, 0, len(in))
	for i, rs := range in {
		if !schema.ValidID(rs.ID) {
			return nil, schema.Errorf(schema.Invalid, doc, indexedField("software", i, "id"), "invalid software id %q", rs.ID)
		}
		if strings.TrimSpace(rs.Root) != "" {
			return nil, schema.Errorf(schema.Invalid, doc, indexedField("software", i, "root"),
				"custom target in a flavor entry must not name a root (%q); the root comes from the desired configuration", rs.Root)
		}
		if err := validateTarget(doc, indexedField("software", i, "target"), rs.Target); err != nil {
			return nil, err
		}
		if rs.Version != "" && strings.TrimSpace(rs.Version) == "" {
			return nil, schema.Errorf(schema.Invalid, doc, indexedField("software", i, "version"), "must not be blank")
		}
		out = append(out, SoftwareEntry{
			ID:      rs.ID,
			Target:  rs.Target,
			Version: strings.TrimSpace(rs.Version),
		})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].ID != out[b].ID {
			return out[a].ID < out[b].ID
		}
		if rA, rB := targetRank(out[a].Target), targetRank(out[b].Target); rA != rB {
			return rA < rB
		}
		return out[a].Version < out[b].Version
	})
	return out, nil
}

func parseConfigEntries(doc string, in []rawConfigEntry) ([]ConfigEntry, error) {
	out := make([]ConfigEntry, 0, len(in))
	for i, rc := range in {
		if !schema.ValidID(rc.ID) {
			return nil, schema.Errorf(schema.Invalid, doc, indexedField("config", i, "id"), "invalid configuration id %q", rc.ID)
		}
		if strings.TrimSpace(rc.Root) != "" {
			return nil, schema.Errorf(schema.Invalid, doc, indexedField("config", i, "root"),
				"custom target in a flavor entry must not name a root (%q); the root comes from the desired configuration", rc.Root)
		}
		if err := validateTarget(doc, indexedField("config", i, "target"), rc.Target); err != nil {
			return nil, err
		}
		var inputs map[string]string
		if len(rc.Inputs) > 0 {
			inputs = make(map[string]string, len(rc.Inputs))
			for _, k := range sortedMapKeys(rc.Inputs) {
				if !schema.ValidID(k) {
					return nil, schema.Errorf(schema.Invalid, doc, indexedField("config", i, "inputs."+k), "invalid input name %q", k)
				}
				inputs[k] = rc.Inputs[k]
			}
		}
		out = append(out, ConfigEntry{
			ID:     rc.ID,
			Target: rc.Target,
			Inputs: inputs,
		})
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].ID != out[b].ID {
			return out[a].ID < out[b].ID
		}
		return targetRank(out[a].Target) < targetRank(out[b].Target)
	})
	return out, nil
}

// validateTarget enforces ADR-0011: a flavor entry's target must be bare
// "global", "user" or "custom". A custom target with a root
// ("custom:<path>") or any other target spelling is rejected.
func validateTarget(doc, field, t string) error {
	if strings.TrimSpace(t) == "" {
		return schema.Errorf(schema.Invalid, doc, field, "missing")
	}
	if strings.HasPrefix(t, "custom:") {
		return schema.Errorf(schema.Invalid, doc, field,
			"custom target in a flavor entry must not name a root (%q); supported: %s", t, strings.Join(entryTargets, ", "))
	}
	for _, want := range entryTargets {
		if t == want {
			return nil
		}
	}
	return schema.Errorf(schema.Invalid, doc, field,
		"unknown installation target %q; supported: %s", t, strings.Join(entryTargets, ", "))
}

func targetRank(t string) int {
	for i, want := range entryTargets {
		if t == want {
			return i
		}
	}
	return len(entryTargets)
}

func indexedField(section string, i int, name string) string {
	return section + "[" + strconv.Itoa(i) + "]." + name
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

func sortedMapKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
