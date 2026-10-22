package config

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/catalog"
	"github.com/qwts/managed-machine/core/internal/compose"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/definition"
	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
)

// Entry is one configuration definition with its provenance.
type Entry struct {
	Definition Definition      `json:"config"`
	Source     source.Identity `json:"source"`
	Revision   string          `json:"revision"`
	File       string          `json:"file"`
	Digest     string          `json:"digest"`
	FS         fs.FS           `json:"-"`
	Dir        string          `json:"-"`
}

// Snapshot is the parsed configuration content of one source at one
// revision.
type Snapshot struct {
	Identity source.Identity
	// Revision is the revision of the source the definitions came from.
	Revision string
	Entries  []Entry
}

// Load parses the configuration definitions of one source. Software and
// flavor definitions in the same directory are ignored: a source may hold
// every kind of definition, and each loader picks its own.
func Load(id source.Identity, fsys fs.FS, dir string) (Snapshot, error) {
	c, err := source.Read(id, fsys, dir)
	if err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{Identity: id, Revision: c.Revision}
	for _, f := range c.Files {
		doc := id.Location + "/" + f.Name
		kind, err := definition.KindOf(doc, f.Data)
		if err != nil {
			return Snapshot{}, err
		}
		if kind == definition.KindFlavor {
			fl, err := compose.Parse(doc, f.Data)
			if err != nil {
				return Snapshot{}, err
			}
			if want := strings.TrimSuffix(f.Name, ".yaml"); fl.ID != want {
				return Snapshot{}, schema.Errorf(schema.Invalid, doc, "id", "file %s defines %q; the file name must match the id", f.Name, fl.ID)
			}
			continue
		}
		if kind != definition.KindConfig {
			continue
		}
		def, err := Parse(doc, f.Data)
		if err != nil {
			return Snapshot{}, err
		}
		if want := strings.TrimSuffix(f.Name, ".yaml"); def.ID != want {
			return Snapshot{}, schema.Errorf(schema.Invalid, doc, "id", "file %s defines %q; the file name must match the id", f.Name, def.ID)
		}
		if err := CheckAssets(doc, def, fsys, dir); err != nil {
			return Snapshot{}, err
		}
		snap.Entries = append(snap.Entries, Entry{
			Definition: def,
			Source:     id,
			Revision:   snap.Revision,
			File:       f.Name,
			Digest:     f.Digest,
			FS:         fsys,
			Dir:        dir,
		})
	}
	return snap, nil
}

// CheckAssets validates the assets a definition references. Every file and
// template source must exist in the source, and a template source may use
// only the tokens the definition allows: the inputs it declares, the
// destinations its operations use and the settings of that operation.
func CheckAssets(doc string, def Definition, fsys fs.FS, dir string) error {
	for i, op := range def.Operations {
		if op.Provider != ProviderFile && op.Provider != ProviderTemplate {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, op.Source))
		if err != nil {
			return schema.Errorf(schema.Invalid, doc, field(i, "source"),
				"the %s source %q is not in the source: %v", op.Provider, op.Source, err)
		}
		if op.Provider != ProviderTemplate {
			continue
		}
		settings := map[string]bool{}
		for name := range op.Settings {
			settings[name] = true
		}
		names := render.Names{Inputs: def.InputNames(), Destinations: def.DestinationNames(), Settings: settings}
		if err := render.Check(string(data), names); err != nil {
			return schema.Errorf(schema.Invalid, doc, field(i, "source"), "%v", err)
		}
	}
	return nil
}

// Catalog merges configuration snapshots. An id defined by two sources is a
// conflict: no source silently overrides another, built-in or not.
func Catalog(snaps ...Snapshot) ([]Entry, error) {
	byID := map[string]Entry{}
	seenSource := map[source.Identity]bool{}
	for _, s := range snaps {
		if seenSource[s.Identity] {
			return nil, schema.Errorf(schema.Conflict, s.Identity.Location, "", "source %s/%s is listed twice", s.Identity.Kind, s.Identity.Location)
		}
		seenSource[s.Identity] = true
		for _, e := range s.Entries {
			if prev, ok := byID[e.Definition.ID]; ok {
				return nil, schema.Errorf(schema.Conflict, e.Source.Location+"/"+e.File, "id",
					"configuration %q is already defined by %s/%s", e.Definition.ID, prev.Source.Location, prev.File)
			}
			byID[e.Definition.ID] = e
		}
	}
	out := make([]Entry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Definition.ID < out[b].Definition.ID })
	return out, nil
}

// LoadCatalog returns the configuration definitions of the built-in catalog
// merged with the optional local source and with the cached snapshots of
// the configured remote sources. An id defined in two sources is a
// conflict.
func LoadCatalog(getenv func(string) string) ([]Entry, error) {
	sources, err := catalog.Sources(getenv)
	if err != nil {
		return nil, err
	}
	snaps := make([]Snapshot, 0, len(sources))
	for _, s := range sources {
		snap, err := Load(s.Identity, s.FS, s.Dir)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	return Catalog(snaps...)
}

// Normalize renders entries as compact JSON Lines, one entry per line, in
// id order. Struct field order fixes key order; maps are emitted with
// sorted keys by encoding/json. HTML escaping is off so paths and texts
// keep their bytes.
func Normalize(entries []Entry) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}
