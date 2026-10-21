package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/schema"
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
