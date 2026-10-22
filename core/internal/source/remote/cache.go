package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
)

// snapshotFile is one definition file of a cached snapshot.
type snapshotFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// snapshotMeta is the snapshot.json of a cached snapshot. It names the
// source and the revision the snapshot is pinned to, and records the
// digest every file must still have.
type snapshotMeta struct {
	SchemaVersion int            `json:"schema_version"`
	Kind          string         `json:"kind"`
	Location      string         `json:"location"`
	Revision      string         `json:"revision"`
	Files         []snapshotFile `json:"files"`
}

// cache is the snapshot cache under the state directory:
//
//	<state>/sources/<kind>/<sha256 of location@revision>/{files,snapshot.json}
//	<state>/sources/https/<sha256 of location>.current
//
// The snapshot directory is content addressed, so a snapshot is never
// written twice under two names. The pointer file names the revision of
// the most recent refresh of an https location, so a later read finds
// that snapshot without the network.
type cache struct {
	base string
}

// newCache returns the cache under stateDir.
func newCache(stateDir string) cache {
	return cache{base: filepath.Join(stateDir, "sources")}
}

// newDirectoryCache returns the fleet key-directory snapshot cache under
// "<stateDir>/fleet/directory", distinct from the definitions cache under
// "<stateDir>/sources" (D3).
func newDirectoryCache(stateDir string) cache {
	return cache{base: filepath.Join(stateDir, "fleet", "directory")}
}

// snapshotDir returns the directory of one snapshot.
func (c cache) snapshotDir(ident source.Identity, revision string) string {
	return filepath.Join(c.kindDir(ident.Kind), hash(ident.Location+"@"+revision))
}

// pointerPath returns the path of the pointer file of one https location.
func (c cache) pointerPath(location string) string {
	return c.pointerPathFor(source.Identity{Kind: "https", Location: location})
}

// pointerPathFor returns the path of the pointer file for ident.
func (c cache) pointerPathFor(ident source.Identity) string {
	return filepath.Join(c.kindDir(ident.Kind), hash(ident.Location)+".current")
}

// kindDir returns the directory holding the snapshots of one kind.
func (c cache) kindDir(kind string) string {
	return filepath.Join(c.base, kind)
}

// write stores the files as one snapshot and reports whether the snapshot
// was already cached. The snapshot is built in a temporary directory and
// renamed into place, so a reader never sees a half-written snapshot.
func (c cache) write(ident source.Identity, revision string, files []file) (bool, error) {
	dir := c.snapshotDir(ident, revision)
	blob, err := json.Marshal(snapshotMeta{
		SchemaVersion: 1,
		Kind:          ident.Kind,
		Location:      ident.Location,
		Revision:      revision,
		Files:         metaFiles(files),
	})
	if err != nil {
		return false, err
	}
	if unchanged, err := c.intact(dir, blob); err != nil || unchanged {
		return unchanged, err
	}
	if err := os.MkdirAll(c.kindDir(ident.Kind), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.MkdirTemp(c.kindDir(ident.Kind), ".tmp-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(tmp, f.name), f.data, 0o644); err != nil {
			return false, err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, "snapshot.json"), blob, 0o644); err != nil {
		return false, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return false, err
	}
	return false, nil
}

// intact reports whether dir already holds exactly this snapshot: the same
// metadata, and files whose digests still match. A snapshot whose files
// were changed on disk is written again, so a refresh repairs the cache.
func (c cache) intact(dir string, blob []byte) (bool, error) {
	got, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !bytes.Equal(got, blob) {
		return false, nil
	}
	var meta snapshotMeta
	if err := json.Unmarshal(got, &meta); err != nil {
		return false, nil
	}
	for _, f := range meta.Files {
		data, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			return false, nil
		}
		if digest(data) != f.SHA256 {
			return false, nil
		}
	}
	return true, nil
}

// read returns the cached snapshot of one source, verifying every file
// against the digest the snapshot recorded. A file that no longer matches
// is a content failure, never a snapshot that is quietly used.
func (c cache) read(ident source.Identity, revision string) (CachedSource, error) {
	dir := c.snapshotDir(ident, revision)
	blob, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return CachedSource{}, fmt.Errorf("%s: %w", ident.Location, ErrNotCached)
		}
		return CachedSource{}, err
	}
	var meta snapshotMeta
	if err := json.Unmarshal(blob, &meta); err != nil {
		return CachedSource{}, fmt.Errorf("%s: read the cached snapshot: %w", ident.Location, err)
	}
	if meta.Kind != ident.Kind || meta.Location != ident.Location || meta.Revision != revision {
		return CachedSource{}, fmt.Errorf("%s: the cached snapshot does not match the configured source", ident.Location)
	}
	names := map[string]bool{}
	for _, f := range meta.Files {
		data, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			return CachedSource{}, fmt.Errorf("%s: read %s: %w", ident.Location, f.Name, err)
		}
		if digest(data) != f.SHA256 {
			return CachedSource{}, schema.Errorf(schema.DigestMismatch, ident.Location+"/"+f.Name, "",
				"the cached file does not match the digest the snapshot records")
		}
		names[f.Name] = true
	}
	return CachedSource{
		Identity: ident,
		Revision: revision,
		FS:       snapshotFS{dir: dir, files: names},
	}, nil
}

// current returns the revision of the most recent refresh of an https
// location. A location that was never refreshed has no snapshot.
func (c cache) current(location string) (string, error) {
	return c.currentFor(source.Identity{Kind: "https", Location: location})
}

// currentFor returns the revision of the most recent refresh of ident.
func (c cache) currentFor(ident source.Identity) (string, error) {
	data, err := os.ReadFile(c.pointerPathFor(ident))
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%s: %w", ident.Location, ErrNotCached)
		}
		return "", err
	}
	revision := strings.TrimSpace(string(data))
	if revision == "" {
		return "", fmt.Errorf("%s: %w", ident.Location, ErrNotCached)
	}
	return revision, nil
}

// setCurrent records the revision of the most recent refresh of an https
// location.
func (c cache) setCurrent(location, revision string) error {
	return c.setCurrentFor(source.Identity{Kind: "https", Location: location}, revision)
}

// setCurrentFor records the revision of the most recent refresh of ident.
func (c cache) setCurrentFor(ident source.Identity, revision string) error {
	path := c.pointerPathFor(ident)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, []byte(revision))
}

// writeAtomic writes a file through a temporary name, so a reader never
// sees half of it.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// metaFiles renders the files of a snapshot, sorted by name.
func metaFiles(files []file) []snapshotFile {
	out := make([]snapshotFile, 0, len(files))
	for _, f := range files {
		out = append(out, snapshotFile{Name: f.name, SHA256: f.digest})
	}
	return out
}

// snapshotFS exposes only the definition files of a snapshot, so the
// definition loader never sees the cache's own metadata.
type snapshotFS struct {
	dir   string
	files map[string]bool
}

func (f snapshotFS) Open(name string) (fs.File, error) {
	if !f.files[name] {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	return os.DirFS(f.dir).Open(name)
}

func (f snapshotFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	ents, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, 0, len(f.files))
	for _, e := range ents {
		if f.files[e.Name()] {
			out = append(out, e)
		}
	}
	return out, nil
}

// hash is the lowercase hex SHA-256 of s, used to name the content
// addressed directories of the cache.
func hash(s string) string {
	return digest([]byte(s))
}
