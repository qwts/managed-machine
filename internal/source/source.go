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

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/schema"
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

// File is one definition file of a source.
type File struct {
	Name string
	Data []byte
	// Digest is the digest of the file's bytes.
	Digest string
}

// Content is the raw content of one source: its definition files in name
// order and the revision computed over them. The revision covers every
// file of the source, so one kind of definition cannot change behind the
// other's back.
type Content struct {
	Identity Identity
	Revision string
	Files    []File
}

// Read reads every definition file of one source: the *.yaml files directly
// under dir, in name order. A file that is not a definition file is
// rejected rather than skipped, so a typo in an extension is not lost; a
// subdirectory is left to the assets the definitions may reference. The
// loader that reads the content checks that every file is named after the
// id it defines.
func Read(id Identity, fsys fs.FS, dir string) (Content, error) {
	if !kinds[id.Kind] || id.Location == "" {
		return Content{}, schema.Errorf(schema.Invalid, id.Location, "", "invalid source identity %q/%q", id.Kind, id.Location)
	}
	ents, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return Content{}, schema.Errorf(schema.Invalid, id.Location, "", "read source: %v", err)
	}
	type file struct {
		name string
		data []byte
	}
	var files []file
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() {
			// A subdirectory holds the assets of the definitions, not
			// definitions of its own.
			continue
		}
		if !e.Type().IsRegular() || path.Ext(name) != ".yaml" {
			return Content{}, schema.Errorf(schema.Invalid, id.Location, name, "not a .yaml definition file")
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, name))
		if err != nil {
			return Content{}, schema.Errorf(schema.Invalid, id.Location, name, "read: %v", err)
		}
		files = append(files, file{name, data})
	}
	sort.Slice(files, func(a, b int) bool { return files[a].name < files[b].name })
	h := sha256.New()
	out := Content{Identity: id}
	for _, f := range files {
		// Length-prefixed so a name/content boundary cannot be shifted.
		h.Write([]byte(f.name))
		h.Write([]byte{0})
		writeLen(h, len(f.data))
		h.Write(f.data)
		sum := sha256.Sum256(f.data)
		out.Files = append(out.Files, File{Name: f.name, Data: f.data, Digest: "sha256:" + hex.EncodeToString(sum[:])})
	}
	out.Revision = "sha256:" + hex.EncodeToString(h.Sum(nil))
	return out, nil
}

// Load reads every *.yaml file directly under dir in fsys as one software
// definition. A file must be named after the id it defines, so two files
// can never silently define one id inside a source. Other files are
// rejected rather than skipped, so a typo in an extension is not lost.
// Configuration and flavor definitions are ignored: software discovery
// never sees them.
func Load(id Identity, fsys fs.FS, dir string, reg *registry.Registry) (Snapshot, error) {
	c, err := Read(id, fsys, dir)
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
		if kind != definition.KindSoftware {
			continue
		}
		sw, err := definition.Parse(doc, f.Data, reg)
		if err != nil {
			return Snapshot{}, err
		}
		if want := strings.TrimSuffix(f.Name, ".yaml"); sw.ID != want {
			return Snapshot{}, schema.Errorf(schema.Invalid, doc, "id", "file %s defines %q; the file name must match the id", f.Name, sw.ID)
		}
		snap.Entries = append(snap.Entries, Entry{Software: sw, Source: id, File: f.Name, Digest: f.Digest})
	}
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
