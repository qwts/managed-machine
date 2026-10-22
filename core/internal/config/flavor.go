package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/catalog"
	"github.com/qwts/managed-machine/core/internal/compose"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/config/secret"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/source"
)

// composePrepared loads and composes every flavor named by
// prep.desired.Flavors for prep.targetKind, resolving each flavored
// configuration entry's destinations and desired operation values so
// cross-definition collisions are detected by --plan and --check. When
// desired.Flavors is empty it does nothing.
func composePrepared(ctx context.Context, d Deps, prep *prepared) *diag.Diagnostic {
	if len(prep.desired.Flavors) == 0 {
		return nil
	}
	fail := func(id, format string, args ...any) *diag.Diagnostic {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return &dg
	}
	entries, err := d.Catalog()
	if err != nil {
		return fail(Diagnostic(err), "%s", catalogMessage(err))
	}
	flavorEntries, err := loadFlavorCatalog(d, entries)
	if err != nil {
		return fail(Diagnostic(err), "%s", catalogMessage(err))
	}
	flavorByID := make(map[string]compose.Entry, len(flavorEntries))
	for _, fe := range flavorEntries {
		flavorByID[fe.Flavor.ID] = fe
	}
	flavors := make([]compose.Flavor, 0, len(prep.desired.Flavors))
	for i, name := range prep.desired.Flavors {
		fe, ok := flavorByID[name]
		if !ok {
			de := &DesiredError{
				Field:   fmt.Sprintf("flavors[%d]", i),
				Message: fmt.Sprintf("unknown flavor %q", name),
			}
			return fail(diag.ConfigDesiredInvalid, "%s", de.Error())
		}
		flavors = append(flavors, fe.Flavor)
	}

	configByID := make(map[string]Entry, len(entries))
	for _, e := range entries {
		configByID[e.Definition.ID] = e
	}

	var allOps []compose.ConfigOperation
	selfOps, err := evalConfigOperations(prep.entry, prep.def, prep.resolved, prep.targetKind)
	if err != nil {
		return fail(diag.ConfigSourceInvalid, "%v", err)
	}
	allOps = append(allOps, selfOps...)

	req := Request{
		Target:   prep.targetKind,
		Root:     prep.customRoot,
		Platform: prep.platform,
	}
	for _, fl := range flavors {
		for _, ce := range fl.Config {
			if ce.Target != prep.targetKind {
				continue
			}
			otherEntry, ok := configByID[ce.ID]
			if !ok {
				return fail(diag.ConfigSourceInvalid, "flavor %q: unknown configuration %q", fl.ID, ce.ID)
			}
			otherDef := withFlavorInputs(otherEntry.Definition, ce.Inputs)
			if d.Compose != nil {
				otherDef, err = d.Compose(otherDef, prep.desired)
				if err != nil {
					return fail(Diagnostic(err), "%s", err.Error())
				}
			}
			otherResolved, err := Resolve(otherDef, prep.desired, req)
			if err != nil {
				return fail(Diagnostic(err), "%s", err.Error())
			}
			if dg := resolveSecretInputs(ctx, d, &otherResolved); dg != nil {
				return dg
			}
			ops, err := evalConfigOperations(otherEntry, otherDef, otherResolved, prep.targetKind)
			if err != nil {
				return fail(diag.ConfigSourceInvalid, "%v", err)
			}
			allOps = append(allOps, ops...)
		}
	}

	prep.composed = compose.Compose(flavors, allOps)
	return nil
}

// loadFlavorCatalog loads flavor definitions from the sources that supplied
// entries plus any configured catalog sources.
func loadFlavorCatalog(d Deps, entries []Entry) ([]compose.Entry, error) {
	seen := map[source.Identity]bool{}
	var snaps []compose.Snapshot
	for _, e := range entries {
		if e.FS == nil || seen[e.Source] {
			continue
		}
		seen[e.Source] = true
		snap, err := compose.Load(e.Source, e.FS, e.Dir)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	if sources, err := catalog.Sources(d.getenv); err == nil {
		for _, s := range sources {
			if seen[s.Identity] {
				continue
			}
			seen[s.Identity] = true
			snap, err := compose.Load(s.Identity, s.FS, s.Dir)
			if err != nil {
				return nil, err
			}
			snaps = append(snaps, snap)
		}
	}
	return compose.Catalog(snaps...)
}

func withFlavorInputs(def Definition, flavorInputs map[string]string) Definition {
	if len(flavorInputs) == 0 {
		return def
	}
	merged := make(map[string]string, len(def.Inputs)+len(flavorInputs))
	for k, v := range def.Inputs {
		merged[k] = v
	}
	for k, v := range flavorInputs {
		merged[k] = v
	}
	def.Inputs = merged
	return def
}

func resolveSecretInputs(ctx context.Context, d Deps, resolved *Resolved) *diag.Diagnostic {
	for _, k := range sortedKeys(resolved.Inputs) {
		val := resolved.Inputs[k]
		secName, ok := strings.CutPrefix(val, "secret:")
		if !ok {
			continue
		}
		secVal, err := d.resolveSecret(ctx, secName)
		if err != nil || secVal == "" {
			out := diag.New(diag.ConfigSecretUnresolved, fmt.Sprintf("input %q: %s could not be resolved", k, secret.Ref(secName)))
			return &out
		}
		resolved.Inputs[k] = secVal
	}
	return nil
}

// evalConfigOperations resolves every operation of def to its destination
// path and rendered desired value for composition conflict detection.
func evalConfigOperations(entry Entry, def Definition, resolved Resolved, targetKind string) ([]compose.ConfigOperation, error) {
	base := render.Values{
		Inputs:       resolved.Inputs,
		Destinations: resolved.Destinations,
	}
	out := make([]compose.ConfigOperation, 0, len(def.Operations))
	for _, op := range def.Operations {
		destPath := filepath.Clean(resolved.Destinations[op.Destination])
		val, err := evalOperationValue(entry, op, base)
		if err != nil {
			return nil, err
		}
		out = append(out, compose.ConfigOperation{
			Config:      def.ID,
			Target:      targetKind,
			Operation:   op.ID,
			Destination: destPath,
			Value:       val,
		})
	}
	return out, nil
}

func evalOperationValue(entry Entry, op Operation, base render.Values) (string, error) {
	switch op.Provider {
	case ProviderFile:
		if entry.FS == nil {
			return op.Source, nil
		}
		raw, err := fs.ReadFile(entry.FS, path.Join(entry.Dir, op.Source))
		if err != nil {
			return "", fmt.Errorf("source %q: %w", op.Source, err)
		}
		return string(raw), nil
	case ProviderTemplate:
		if entry.FS == nil {
			return op.Source, nil
		}
		raw, err := fs.ReadFile(entry.FS, path.Join(entry.Dir, op.Source))
		if err != nil {
			return "", fmt.Errorf("source %q: %w", op.Source, err)
		}
		settings := make(map[string]string, len(op.Settings))
		for _, k := range sortedKeys(op.Settings) {
			sv, err := render.Render(op.Settings[k], base)
			if err != nil {
				return "", fmt.Errorf("settings.%s: %w", k, err)
			}
			settings[k] = sv
		}
		rendered, err := render.Render(string(raw), render.Values{
			Inputs:       base.Inputs,
			Destinations: base.Destinations,
			Settings:     settings,
		})
		if err != nil {
			return "", fmt.Errorf("source %q: %w", op.Source, err)
		}
		return rendered, nil
	case ProviderManagedBlock:
		rendered, err := render.Render(op.Content, base)
		if err != nil {
			return "", err
		}
		return "managed-block:" + op.Comment + ":" + op.BlockID + "\n" + rendered, nil
	case ProviderStructured:
		renderedKeys := make(map[string]any, len(op.Keys))
		for k, v := range op.Keys {
			switch tv := v.(type) {
			case string:
				rv, err := render.Render(tv, base)
				if err != nil {
					return "", err
				}
				renderedKeys[k] = rv
			case []string:
				rs := make([]string, len(tv))
				for i, item := range tv {
					rv, err := render.Render(item, base)
					if err != nil {
						return "", err
					}
					rs[i] = rv
				}
				renderedKeys[k] = rs
			default:
				renderedKeys[k] = v
			}
		}
		b, err := json.Marshal(renderedKeys)
		if err != nil {
			return "", err
		}
		return "structured:" + op.Format + ":" + string(b), nil
	default:
		return "", nil
	}
}

// conflictPlanOp returns a planned conflict operation when op collides with
// another configuration operation in the composed flavors for the target.
func (p *prepared) conflictPlanOp(op Operation, destPath string) (plan.Operation, bool) {
	id := compose.ConfigIdentity(p.def.ID, p.targetKind, op.ID)
	c, ok := p.composed.ConflictFor(id)
	if !ok {
		return plan.Operation{}, false
	}
	conflict := formatFlavorConflict(c, destPath)
	return plan.Operation{
		ID:        op.ID,
		Operation: op.ID,
		Target:    p.target,
		Action:    "conflict " + conflict,
		Provider:  op.Provider,
		Params: map[string]string{
			"config":   p.def.ID,
			"path":     destPath,
			"conflict": conflict,
		},
	}, true
}

// extraConflictOps returns conflict operations for any cross-definition
// collisions in the composed flavors that did not involve p.def.ID directly.
func (p *prepared) extraConflictOps(seenIDs map[string]bool) []plan.Operation {
	selfPrefix := "config:" + p.def.ID + "@"
	seenDest := map[string]bool{}
	var out []plan.Operation
	for _, c := range p.composed.Conflicts {
		if c.Kind != compose.KindConfig || seenDest[c.Destination] {
			continue
		}
		involvesSelf := false
		for _, id := range c.Identities {
			if strings.HasPrefix(id, selfPrefix) {
				involvesSelf = true
				break
			}
		}
		if involvesSelf {
			continue
		}
		seenDest[c.Destination] = true
		conflict := formatFlavorConflict(c, c.Destination)
		opID := c.Identity
		if seenIDs != nil {
			if seenIDs[opID] {
				opID = "conflict:" + c.Identity
			}
			seenIDs[opID] = true
		}
		out = append(out, plan.Operation{
			ID:        opID,
			Operation: c.Identity,
			Target:    p.target,
			Action:    "conflict " + conflict,
			Provider:  ProviderFile,
			Params: map[string]string{
				"config":   p.def.ID,
				"path":     c.Destination,
				"conflict": conflict,
			},
		})
	}
	return out
}

// checkFlavorConflicts returns conflict records and an MM_CONFIG_CONFLICT
// diagnostic if any operation in the composed flavors for the target has a
// cross-definition collision.
func (p *prepared) checkFlavorConflicts() ([]*output.Record, *diag.Diagnostic) {
	var recs []*output.Record
	for _, op := range p.def.Operations {
		destPath := p.resolved.Destinations[op.Destination]
		if pop, ok := p.conflictPlanOp(op, destPath); ok {
			recs = append(recs, output.New().
				Set("id", p.def.ID).
				Set("operation", pop.Operation).
				Set("target", pop.Target).
				Set("action", pop.Action))
		}
	}
	for _, pop := range p.extraConflictOps(nil) {
		recs = append(recs, output.New().
			Set("id", p.def.ID).
			Set("operation", pop.Operation).
			Set("target", pop.Target).
			Set("action", pop.Action))
	}
	if len(recs) == 0 {
		return nil, nil
	}
	act, _ := recs[0].Get("action")
	dg := diag.New(diag.ConfigConflict, act.(string))
	return recs, &dg
}

func formatFlavorConflict(c compose.Conflict, destPath string) string {
	ids := append([]string(nil), c.Identities...)
	sort.Strings(ids)
	if len(ids) == 0 && c.Identity != "" {
		ids = []string{c.Identity}
	}
	return strings.Join(ids, " ") + " " + destPath
}
