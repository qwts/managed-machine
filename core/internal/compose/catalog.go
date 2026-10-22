package compose

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/catalog"
	"github.com/qwts/managed-machine/core/internal/definition"
	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
)

// Entry is one flavor definition with its provenance.
type Entry struct {
	Flavor   Flavor          `json:"flavor"`
	Source   source.Identity `json:"source"`
	Revision string          `json:"revision"`
	File     string          `json:"file"`
	Digest   string          `json:"digest"`
}

// Snapshot is the parsed flavor content of one source at one revision.
type Snapshot struct {
	Identity source.Identity
	Revision string
	Entries  []Entry
}

// Load parses the flavor definitions of one source. Software and
// configuration definitions in the same directory are ignored: a source may
// hold every kind of definition, and each loader picks its own.
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
		if kind != definition.KindFlavor {
			continue
		}
		fl, err := Parse(doc, f.Data)
		if err != nil {
			return Snapshot{}, err
		}
		if want := strings.TrimSuffix(f.Name, ".yaml"); fl.ID != want {
			return Snapshot{}, schema.Errorf(schema.Invalid, doc, "id", "file %s defines %q; the file name must match the id", f.Name, fl.ID)
		}
		snap.Entries = append(snap.Entries, Entry{
			Flavor:   fl,
			Source:   id,
			Revision: snap.Revision,
			File:     f.Name,
			Digest:   f.Digest,
		})
	}
	return snap, nil
}

// Catalog merges flavor snapshots. An id defined by two sources is a
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
			if prev, ok := byID[e.Flavor.ID]; ok {
				return nil, schema.Errorf(schema.Conflict, e.Source.Location+"/"+e.File, "id",
					"flavor %q is already defined by %s/%s", e.Flavor.ID, prev.Source.Location, prev.File)
			}
			byID[e.Flavor.ID] = e
		}
	}
	out := make([]Entry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Flavor.ID < out[b].Flavor.ID })
	return out, nil
}

// LoadCatalog returns the flavor definitions of the built-in catalog merged
// with the optional local source and with the cached snapshots of the
// configured remote sources. An id defined in two sources is a conflict.
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
// id order. Struct field order fixes key order; HTML escaping is off so
// version strings and paths keep their bytes.
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
