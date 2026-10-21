package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/schema"
)

// indexDocument is the https index format: the files of one source and the
// digest each of them must have.
type indexDocument struct {
	SchemaVersion int         `json:"schema_version"`
	Files         []indexFile `json:"files"`
}

type indexFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
}

// fetched is the content of one source, before it is cached.
type fetched struct {
	// revision is the revision the snapshot is pinned to.
	revision string
	// files are the definition files, sorted by name.
	files []file
}

// file is one definition file of a snapshot.
type file struct {
	name   string
	data   []byte
	digest string
}

// fetchHTTPS reads the index document and every file it lists. Each file
// comes from the index document's directory, on the same scheme, host and
// port, and must match the digest the index records.
func fetchHTTPS(ctx context.Context, src *HTTPSSource, client *http.Client) (*fetched, error) {
	if src.URL == nil {
		return nil, &ConfigError{Setting: URLEnv, Message: "names no index document"}
	}
	f := &fetcher{ctx: ctx, client: client}
	index, err := f.get(src.URL.String(), "", MaxIndexBytes)
	if err != nil {
		return nil, err
	}
	// The index document is the source's revision: the same index always
	// names the same snapshot, and a changed index names a new one.
	revision := "sha256:" + digest(index)
	location := src.identity().Location
	var doc indexDocument
	if err := json.Unmarshal(index, &doc); err != nil {
		return nil, schema.Errorf(schema.Invalid, location, "index", "read the index document: %v", err)
	}
	if doc.SchemaVersion != 1 {
		return nil, schema.Errorf(schema.Invalid, location, "index", "unsupported index schema version %d; supported: 1", doc.SchemaVersion)
	}
	dir := path.Dir(src.URL.Path)
	seen := map[string]bool{}
	files := make([]file, 0, len(doc.Files))
	total := 0
	for _, entry := range doc.Files {
		if err := validFileName(location, entry.Name); err != nil {
			return nil, err
		}
		if seen[entry.Name] {
			return nil, schema.Errorf(schema.Invalid, location, entry.Name, "the index lists the file twice")
		}
		seen[entry.Name] = true
		if !schema.ValidSHA256(entry.SHA256) {
			return nil, schema.Errorf(schema.Invalid, location, entry.Name, "the index lists no valid sha256 digest")
		}
		fileURL := &url.URL{Scheme: src.URL.Scheme, Host: src.URL.Host, Path: path.Join(dir, entry.Name)}
		data, err := f.get(fileURL.String(), "", MaxFileBytes)
		if err != nil {
			return nil, err
		}
		if got := digest(data); got != entry.SHA256 {
			return nil, schema.Errorf(schema.DigestMismatch, location+"/"+entry.Name, "",
				"the content does not match the digest the index records")
		}
		total += len(data)
		if total > MaxSnapshotBytes {
			return nil, &FetchError{Err: errTooLarge(location)}
		}
		files = append(files, file{name: entry.Name, data: data, digest: entry.SHA256})
	}
	sort.Slice(files, func(a, b int) bool { return files[a].name < files[b].name })
	return &fetched{revision: revision, files: files}, nil
}

// validFileName rejects anything but a plain definition file name: a
// source may not name a path, and a definition file always ends in .yaml.
func validFileName(location, name string) error {
	switch {
	case name == "":
		return schema.Errorf(schema.Invalid, location, "", "the source names no file")
	case strings.ContainsAny(name, `/\`) || name == "." || name == ".." || strings.Contains(name, ".."):
		return schema.Errorf(schema.Invalid, location, name, "the source names a path, not a file")
	case !strings.HasSuffix(name, ".yaml"):
		return schema.Errorf(schema.Invalid, location, name, "not a .yaml definition file")
	}
	return nil
}
