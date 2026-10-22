package compose

import (
	"fmt"
	"sort"
	"strings"
)

// Conflict kinds reported by the composer.
const (
	KindSoftware = "software"
	KindConfig   = "config"
)

// SoftwareIdentity returns the canonical resource identity for a software
// entry at target: "software:<id>@<target>".
func SoftwareIdentity(id, target string) string {
	return "software:" + id + "@" + target
}

// ConfigIdentity returns the canonical resource identity for a configuration
// operation at target: "config:<id>@<target>:<operation>".
func ConfigIdentity(id, target, operation string) string {
	return "config:" + id + "@" + target + ":" + operation
}

// ConfigOperation is one resolved configuration operation participating in
// composition: the configuration definition id, the installation target, the
// operation id within the definition, the resolved destination path, and the
// rendered desired value for that destination.
type ConfigOperation struct {
	Config      string `json:"config"`
	Target      string `json:"target"`
	Operation   string `json:"operation"`
	Destination string `json:"destination"`
	Value       string `json:"value"`
}

// Identity returns "config:<id>@<target>:<operation>".
func (o ConfigOperation) Identity() string {
	return ConfigIdentity(o.Config, o.Target, o.Operation)
}

// Conflict is one per-identity composition conflict on a software version or
// a configuration destination.
type Conflict struct {
	Kind        string   `json:"kind"`
	Identity    string   `json:"identity"`
	With        []string `json:"with,omitempty"`
	Identities  []string `json:"identities"`
	Target      string   `json:"target"`
	Destination string   `json:"destination,omitempty"`
	Values      []string `json:"values"`
	Detail      string   `json:"detail"`
}

// Result is the deterministic outcome of composing flavors and resolved
// configuration operations.
type Result struct {
	Software   []SoftwareEntry   `json:"software"`
	Config     []ConfigEntry     `json:"config"`
	Operations []ConfigOperation `json:"operations"`
	Conflicts  []Conflict        `json:"conflicts"`
}

// ConflictFor looks up a conflict by its canonical resource identity
// ("software:<id>@<target>" or "config:<id>@<target>:<operation>").
func (r Result) ConflictFor(identity string) (Conflict, bool) {
	for _, c := range r.Conflicts {
		if c.Identity == identity {
			return c, true
		}
	}
	return Conflict{}, false
}

// Compose deduplicates software entries by "software:<id>@<target>",
// configuration entries by (id, target), and resolved configuration
// operations by (target, destination, value), and reports per-identity
// conflicts when version strings for one "software:<id>@<target>" differ or
// when configuration operations resolve to one destination with different
// values.
func Compose(flavors []Flavor, ops []ConfigOperation) Result {
	sw, swConflicts := composeSoftware(flavors)
	cfg := composeConfigEntries(flavors)
	dedupOps, cfgConflicts := composeOperations(ops)
	conflicts := make([]Conflict, 0, len(swConflicts)+len(cfgConflicts))
	conflicts = append(conflicts, swConflicts...)
	conflicts = append(conflicts, cfgConflicts...)
	return Result{
		Software:   sw,
		Config:     cfg,
		Operations: dedupOps,
		Conflicts:  conflicts,
	}
}

func composeSoftware(flavors []Flavor) ([]SoftwareEntry, []Conflict) {
	type swKey struct {
		id     string
		target string
	}
	type swGroup struct {
		key      swKey
		versions map[string]bool
	}
	groups := map[swKey]*swGroup{}
	var keys []swKey
	for _, fl := range flavors {
		for _, s := range fl.Software {
			k := swKey{id: s.ID, target: s.Target}
			g, ok := groups[k]
			if !ok {
				g = &swGroup{key: k, versions: map[string]bool{}}
				groups[k] = g
				keys = append(keys, k)
			}
			if v := strings.TrimSpace(s.Version); v != "" {
				g.versions[v] = true
			}
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].id != keys[b].id {
			return keys[a].id < keys[b].id
		}
		return targetRank(keys[a].target) < targetRank(keys[b].target)
	})
	out := make([]SoftwareEntry, 0, len(keys))
	var conflicts []Conflict
	for _, k := range keys {
		g := groups[k]
		versions := make([]string, 0, len(g.versions))
		for v := range g.versions {
			versions = append(versions, v)
		}
		sort.Strings(versions)
		ver := ""
		if len(versions) > 0 {
			ver = versions[0]
		}
		out = append(out, SoftwareEntry{
			ID:      k.id,
			Target:  k.target,
			Version: ver,
		})
		if len(versions) > 1 {
			id := SoftwareIdentity(k.id, k.target)
			conflicts = append(conflicts, Conflict{
				Kind:       KindSoftware,
				Identity:   id,
				Identities: []string{id},
				Target:     k.target,
				Values:     versions,
				Detail:     fmt.Sprintf("%s: conflicting versions %s", id, strings.Join(versions, ", ")),
			})
		}
	}
	return out, conflicts
}

func composeConfigEntries(flavors []Flavor) []ConfigEntry {
	type cfgKey struct {
		id     string
		target string
	}
	byKey := map[cfgKey]ConfigEntry{}
	var keys []cfgKey
	for _, fl := range flavors {
		for _, c := range fl.Config {
			k := cfgKey{id: c.ID, target: c.Target}
			cur, ok := byKey[k]
			if !ok {
				cur = ConfigEntry{ID: c.ID, Target: c.Target}
				keys = append(keys, k)
			}
			if len(c.Inputs) > 0 {
				if cur.Inputs == nil {
					cur.Inputs = map[string]string{}
				}
				for _, inKey := range sortedMapKeys(c.Inputs) {
					cur.Inputs[inKey] = c.Inputs[inKey]
				}
			}
			byKey[k] = cur
		}
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].id != keys[b].id {
			return keys[a].id < keys[b].id
		}
		return targetRank(keys[a].target) < targetRank(keys[b].target)
	})
	out := make([]ConfigEntry, 0, len(keys))
	for _, k := range keys {
		out = append(out, byKey[k])
	}
	return out
}

func composeOperations(ops []ConfigOperation) ([]ConfigOperation, []Conflict) {
	type destKey struct {
		target      string
		destination string
	}
	type destGroup struct {
		key    destKey
		ops    []ConfigOperation
		values map[string]bool
	}
	groups := map[destKey]*destGroup{}
	var keys []destKey
	for _, op := range ops {
		k := destKey{target: op.Target, destination: op.Destination}
		g, ok := groups[k]
		if !ok {
			g = &destGroup{key: k, values: map[string]bool{}}
			groups[k] = g
			keys = append(keys, k)
		}
		g.ops = append(g.ops, op)
		g.values[op.Value] = true
	}
	sort.Slice(keys, func(a, b int) bool {
		if rA, rB := targetRank(keys[a].target), targetRank(keys[b].target); rA != rB {
			return rA < rB
		}
		if keys[a].target != keys[b].target {
			return keys[a].target < keys[b].target
		}
		return keys[a].destination < keys[b].destination
	})

	out := []ConfigOperation{}
	var conflicts []Conflict
	for _, k := range keys {
		g := groups[k]
		// Sort operations in canonical identity order so deduplication
		// and conflict reporting are independent of input order.
		sortedOps := append([]ConfigOperation(nil), g.ops...)
		sort.SliceStable(sortedOps, func(a, b int) bool {
			idA, idB := sortedOps[a].Identity(), sortedOps[b].Identity()
			if idA != idB {
				return idA < idB
			}
			return sortedOps[a].Value < sortedOps[b].Value
		})

		if len(g.values) <= 1 {
			// Identical destination-and-value pairs dedupe.
			if len(sortedOps) > 0 {
				out = append(out, sortedOps[0])
			}
			continue
		}

		// Two or more distinct values resolve to the same destination:
		// deduplicate identical (identity, value) pairs and report one
		// conflict per colliding config:<id>@<target>:<operation>.
		seenOpVal := map[string]bool{}
		seenID := map[string]bool{}
		var allIDs []string
		for _, op := range sortedOps {
			id := op.Identity()
			ovKey := id + "\x00" + op.Value
			if !seenOpVal[ovKey] {
				seenOpVal[ovKey] = true
				out = append(out, op)
			}
			if !seenID[id] {
				seenID[id] = true
				allIDs = append(allIDs, id)
			}
		}
		sort.Strings(allIDs)
		values := make([]string, 0, len(g.values))
		for v := range g.values {
			values = append(values, v)
		}
		sort.Strings(values)

		for _, id := range allIDs {
			var with []string
			for _, other := range allIDs {
				if other != id {
					with = append(with, other)
				}
			}
			conflicts = append(conflicts, Conflict{
				Kind:        KindConfig,
				Identity:    id,
				With:        with,
				Identities:  allIDs,
				Target:      k.target,
				Destination: k.destination,
				Values:      values,
				Detail:      fmt.Sprintf("%s conflicts on %s", strings.Join(allIDs, " vs "), k.destination),
			})
		}
	}
	return out, conflicts
}
