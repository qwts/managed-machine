// Package source turns a set of definition files into a snapshot with an
// explicit identity and revision, and merges snapshots into one catalog.
// Built-in and external sources go through the same path; the only
// difference is the identity they are given.
package source

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/definition"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/schema"
)

// Identity names a source independently of where it was read from on this
// machine, so normalized output never carries a host path.
type Identity struct {
	Kind     string `json:"kind"`     // builtin, local, https, gist
	Location string `json:"location"` // stable name: "builtin", a repo-relative path, a URL
}

var kinds = map[string]bool{"builtin": true, "local": true, "https": true, "gist": true}

// Entry is one normalized definition with its provenance.
type Entry struct {
	Software definition.Software `json:"software"`
	Source   Identity            `json:"source"`
	Revision string              `json:"revision"`
	File     string              `json:"file"`
	Digest   string              `json:"digest"`
}

// Snapshot is the parsed content of one source at one revision.
type Snapshot struct {
	Identity Identity
	// Revision is "sha256:<hex>" over the sorted file names and contents,
	// so the same files always produce the same revision.
	Revision string
	Entries  []Entry
}

// Load reads every *.yaml file directly under dir in fsys as one software
// definition. A file must be named after the id it defines, so two files
// can never silently define one id inside a source. Other files are
// rejected rather than skipped, so a typo in an extension is not lost.
func Load(id Identity, fsys fs.FS, dir string, reg *registry.Registry) (Snapshot, error) {
	if !kinds[id.Kind] || id.Location == "" {
		return Snapshot{}, schema.Errorf(schema.Invalid, id.Location, "", "invalid source identity %q/%q", id.Kind, id.Location)
	}
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Snapshot{}, schema.Errorf(schema.Invalid, id.Location, "", "read source: %v", err)
	}
	type file struct {
		name string
		data []byte
	}
	var files []file
	for _, e := range ents {
		name := e.Name()
		if !e.Type().IsRegular() || path.Ext(name) != ".yaml" {
			return Snapshot{}, schema.Errorf(schema.Invalid, id.Location, name, "not a .yaml definition file")
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return Snapshot{}, schema.Errorf(schema.Invalid, id.Location, name, "read: %v", err)
		}
		files = append(files, file{name, data})
	}
	sort.Slice(files, func(a, b int) bool { return files[a].name < files[b].name })
	h := sha256.New()
	snap := Snapshot{Identity: id}
	for _, f := range files {
		// Length-prefixed so a name/content boundary cannot be shifted.
		h.Write([]byte(f.name))
		h.Write([]byte{0})
		writeLen(h, len(f.data))
		h.Write(f.data)
		doc := id.Location + "/" + f.name
		sw, err := definition.Parse(doc, f.data, reg)
		if err != nil {
			return Snapshot{}, err
		}
		if want := strings.TrimSuffix(f.name, ".yaml"); sw.ID != want {
			return Snapshot{}, schema.Errorf(schema.Invalid, doc, "id", "file %s defines %q; the file name must match the id", f.name, sw.ID)
		}
		sum := sha256.Sum256(f.data)
		snap.Entries = append(snap.Entries, Entry{Software: sw, Source: id, File: f.name, Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	snap.Revision = "sha256:" + hex.EncodeToString(h.Sum(nil))
	for i := range snap.Entries {
		snap.Entries[i].Revision = snap.Revision
	}
	return snap, nil
}

func writeLen(w interface{ Write([]byte) (int, error) }, n int) {
	var b [8]byte
	for i := 7; i >= 0; i-- {
		b[i] = byte(n)
		n >>= 8
	}
	w.Write(b[:])
}

// Catalog merges snapshots. An id defined by two sources is a conflict:
// no source silently overrides another, built-in or not.
func Catalog(snaps ...Snapshot) ([]Entry, error) {
	byID := map[string]Entry{}
	seenSource := map[Identity]bool{}
	for _, s := range snaps {
		if seenSource[s.Identity] {
			return nil, schema.Errorf(schema.Conflict, s.Identity.Location, "", "source %s/%s is listed twice", s.Identity.Kind, s.Identity.Location)
		}
		seenSource[s.Identity] = true
		for _, e := range s.Entries {
			if prev, ok := byID[e.Software.ID]; ok {
				return nil, schema.Errorf(schema.Conflict, e.Source.Location+"/"+e.File, "id",
					"software %q is already defined by %s/%s", e.Software.ID, prev.Source.Location, prev.File)
			}
			byID[e.Software.ID] = e
		}
	}
	out := make([]Entry, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Software.ID < out[b].Software.ID })
	return out, nil
}

// Normalize renders entries as compact JSON Lines, one entry per line, in
// id order. Struct field order fixes key order; option maps are emitted
// with sorted keys by encoding/json. HTML escaping is off so package names
// and URLs keep their bytes.
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
