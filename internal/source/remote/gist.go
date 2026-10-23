package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/source"
)

// gistDocument is the part of a gist response this package reads. Unknown
// fields are ignored: the API carries more than a definition source
// needs.
type gistDocument struct {
	Files map[string]gistFile `json:"files"`
}

type gistFile struct {
	Filename  string `json:"filename"`
	Size      int    `json:"size"`
	Truncated bool   `json:"truncated"`
	Content   string `json:"content"`
}

type gistHistoryDocument struct {
	History []struct {
		Version string `json:"version"`
	} `json:"history"`
}

// fetchGist reads one pinned gist revision. Only the yaml files of the
// gist are definitions, and a truncated file is refused: the API would
// serve the rest from another host, which this package never follows.
func fetchGist(ctx context.Context, src *GistSource, client *http.Client) (*fetched, error) {
	f := &fetcher{ctx: ctx, client: client}
	endpoint := src.apiBase() + "/gists/" + src.ID + "/" + src.Revision
	body, err := f.get(endpoint, "application/vnd.github+json", MaxGistBytes)
	if err != nil {
		return nil, err
	}
	location := src.identity().Location
	var doc gistDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, schema.Errorf(schema.Invalid, location, "gist", "read the gist response: %v", err)
	}
	files := make([]file, 0, len(doc.Files))
	total := 0
	for name, gf := range doc.Files {
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		if err := validFileName(location, name); err != nil {
			return nil, err
		}
		if gf.Filename != "" && gf.Filename != name {
			return nil, schema.Errorf(schema.Invalid, location, name, "the gist names the file %q", gf.Filename)
		}
		if gf.Truncated {
			return nil, schema.Errorf(schema.Invalid, location, name, "the gist file is truncated; a truncated definition is never used")
		}
		content := []byte(gf.Content)
		if gf.Size > 0 && gf.Size != len(content) {
			return nil, schema.Errorf(schema.Invalid, location, name, "the gist file does not match the size it records")
		}
		if len(content) > MaxFileBytes {
			return nil, &FetchError{Err: fmt.Errorf("%s/%s: the file exceeds %d bytes", location, name, MaxFileBytes)}
		}
		total += len(content)
		if total > MaxSnapshotBytes {
			return nil, &FetchError{Err: errTooLarge(location)}
		}
		files = append(files, file{name: name, data: content, digest: digest(content)})
	}
	sort.Slice(files, func(a, b int) bool { return files[a].name < files[b].name })
	// The configured revision is the identity's revision. What the
	// response says about itself is never trusted.
	return &fetched{revision: src.Revision, files: files}, nil
}

// resolveGistRevision fetches GET <apiBase>/gists/<id> and extracts
// history[0].version (D3).
func resolveGistRevision(ctx context.Context, id string, apiBase *url.URL, client *http.Client) (string, error) {
	if !isHex(id) {
		return "", &FetchError{Err: fmt.Errorf("gist:%s: invalid gist id", id)}
	}
	src := &GistSource{ID: id, APIBase: apiBase}
	f := &fetcher{ctx: ctx, client: client}
	endpoint := src.apiBase() + "/gists/" + id
	body, err := f.get(endpoint, "application/vnd.github+json", MaxGistBytes)
	if err != nil {
		return "", err
	}
	var doc gistHistoryDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", &FetchError{Err: fmt.Errorf("gist:%s: read the gist history: %v", id, err)}
	}
	if len(doc.History) == 0 || !isHex(doc.History[0].Version) {
		return "", &FetchError{Err: fmt.Errorf("gist:%s: gist history has no valid revision", id)}
	}
	return doc.History[0].Version, nil
}

// FetchedGist is a gist revision resolved and downloaded in memory so a
// caller can validate its documents before writing the directory cache.
type FetchedGist struct {
	id       string
	revision string
	files    []file
}

// Identity returns "gist:<id>@<revision>".
func (g *FetchedGist) Identity() string {
	return "gist:" + g.id + "@" + g.revision
}

// Revision returns the resolved gist revision.
func (g *FetchedGist) Revision() string {
	return g.revision
}

// FS returns an in-memory ReadDirFS over the fetched .yaml files.
func (g *FetchedGist) FS() fs.FS {
	return memFilesFS{files: g.files}
}

// Cache writes the fetched gist into the fleet key-directory cache under
// "<stateDir>/fleet/directory" and returns OutcomeRefreshed or
// OutcomeUnchanged.
func (g *FetchedGist) Cache(stateDir string) (string, error) {
	return g.CacheAt(stateDir, time.Now().UTC().Truncate(time.Second).Format(time.RFC3339))
}

// CacheAt writes the fetched gist into the fleet key-directory cache under
// "<stateDir>/fleet/directory", recording at as the snapshot fetch time when
// the snapshot is written, and returns OutcomeRefreshed or OutcomeUnchanged.
func (g *FetchedGist) CacheAt(stateDir, at string) (string, error) {
	c := newDirectoryCache(stateDir)
	ident := source.Identity{Kind: "gist", Location: "gist:" + g.id}
	prevRev, prevErr := c.currentFor(ident)
	unchanged, err := c.writeAt(ident, g.revision, at, g.files)
	if err != nil {
		return "", &FetchError{Err: err}
	}
	if err := c.setCurrentFor(ident, g.revision); err != nil {
		return "", &FetchError{Err: err}
	}
	return outcomeOf(unchanged && prevErr == nil && prevRev == g.revision), nil
}

// FetchLatestGist resolves the current revision of gist id through
// GET <api>/gists/<id> (history[0].version) and fetches that pinned
// revision through fetchGist (D3). Any transport, size-bound or
// truncated-file failure is returned as *FetchError.
func FetchLatestGist(ctx context.Context, id string, apiBase *url.URL, client *http.Client) (*FetchedGist, error) {
	rev, err := resolveGistRevision(ctx, id, apiBase, client)
	if err != nil {
		return nil, asFetchError(err)
	}
	src := &GistSource{ID: id, Revision: rev, APIBase: apiBase}
	fetched, err := fetchGist(ctx, src, client)
	if err != nil {
		return nil, asFetchError(err)
	}
	return &FetchedGist{
		id:       id,
		revision: fetched.revision,
		files:    fetched.files,
	}, nil
}

// CachedGistDirectory returns the cached snapshot of gist id from the
// fleet key-directory cache "<stateDir>/fleet/directory". It never touches
// the network and never writes to disk (D3).
func CachedGistDirectory(id, stateDir string) (CachedSource, error) {
	c := newDirectoryCache(stateDir)
	ident := source.Identity{Kind: "gist", Location: "gist:" + id}
	revision, err := c.currentFor(ident)
	if err != nil {
		return CachedSource{}, err
	}
	return c.read(ident, revision)
}

func asFetchError(err error) error {
	if err == nil {
		return nil
	}
	var fe *FetchError
	if errors.As(err, &fe) {
		return err
	}
	return &FetchError{Err: err}
}

type memFilesFS struct {
	files []file
}

func (m memFilesFS) Open(name string) (fs.File, error) {
	for _, f := range m.files {
		if f.name == name {
			return &memFile{name: f.name, data: f.data, r: bytes.NewReader(f.data)}, nil
		}
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (m memFilesFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name != "." {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]fs.DirEntry, len(m.files))
	for i, f := range m.files {
		out[i] = memDirEntry{name: f.name, size: int64(len(f.data))}
	}
	return out, nil
}

type memFile struct {
	name string
	data []byte
	r    *bytes.Reader
}

func (f *memFile) Stat() (fs.FileInfo, error) {
	return memDirEntry{name: f.name, size: int64(len(f.data))}, nil
}

func (f *memFile) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *memFile) Close() error               { return nil }
func (f *memFile) Seek(offset int64, whence int) (int64, error) {
	return f.r.Seek(offset, whence)
}
func (f *memFile) ReadAt(b []byte, off int64) (int, error) {
	return f.r.ReadAt(b, off)
}

var _ io.ReaderAt = (*memFile)(nil)

type memDirEntry struct {
	name string
	size int64
}

func (e memDirEntry) Name() string               { return e.name }
func (e memDirEntry) Size() int64                { return e.size }
func (e memDirEntry) Mode() fs.FileMode          { return 0o444 }
func (e memDirEntry) ModTime() time.Time         { return time.Time{} }
func (e memDirEntry) IsDir() bool                { return false }
func (e memDirEntry) Sys() any                   { return nil }
func (e memDirEntry) Type() fs.FileMode          { return 0 }
func (e memDirEntry) Info() (fs.FileInfo, error) { return e, nil }
