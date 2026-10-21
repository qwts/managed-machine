// Package remote fetches definition snapshots from explicitly configured
// remote sources and caches them under the state directory.
//
// A fetch happens only when a caller asks for one. Discovery reads the
// cache, so a configured source is never fetched implicitly and no remote
// content reaches a machine that did not ask for it.
//
// Every source has an identity that names it, never where it was read from
// on this machine: no host path ever reaches a record, a revision or a
// diagnostic.
package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qwts/managed-machine/core/internal/source"
)

// Environment settings naming the configured remote sources. Both are
// explicit: nothing is discovered, and an unset setting means that source
// is not configured.
const (
	// URLEnv names the https index document of one configured source.
	URLEnv = "MANAGED_MACHINE_SOURCE_URL"
	// GistEnv names one configured gist source as "<id>@<revision>".
	GistEnv = "MANAGED_MACHINE_SOURCE_GIST"
)

// DefaultAPIBase is the GitHub API root a gist source is read from.
const DefaultAPIBase = "https://api.github.com"

// Bounds on what one fetch may deliver. A response beyond a bound is
// refused rather than truncated: a source cannot fill the disk, and a
// partial read is never mistaken for the whole content.
const (
	// MaxIndexBytes bounds the https index document.
	MaxIndexBytes = 64 << 10
	// MaxFileBytes bounds one definition file.
	MaxFileBytes = 64 << 10
	// MaxGistBytes bounds a gist response.
	MaxGistBytes = 1 << 20
	// MaxSnapshotBytes bounds the whole snapshot of one source.
	MaxSnapshotBytes = 1 << 20
)

// RequestTimeout is the total time one request of the default client may
// take.
const RequestTimeout = 15 * time.Second

// MaxRedirects is the number of redirects one request may follow.
const MaxRedirects = 3

// Outcomes of a refresh.
const (
	// OutcomeRefreshed means the snapshot was written.
	OutcomeRefreshed = "refreshed"
	// OutcomeUnchanged means the snapshot was already cached and left
	// untouched.
	OutcomeUnchanged = "unchanged"
)

// Config is the parsed remote source configuration. The zero value means
// no remote source is configured, which is the normal case.
type Config struct {
	// HTTPS is the configured https index source, nil when unset.
	HTTPS *HTTPSSource
	// Gist is the configured gist source, nil when unset.
	Gist *GistSource
}

// HTTPSSource is one configured https index source.
type HTTPSSource struct {
	// URL is the index document. Its directory holds the definition files.
	URL *url.URL
}

// GistSource is one configured gist source, pinned to a revision.
type GistSource struct {
	// ID is the gist id.
	ID string
	// Revision is the pinned gist revision. It is the identity's
	// revision, whatever the response says about itself.
	Revision string
	// APIBase is the GitHub API root. It defaults to DefaultAPIBase and
	// is injectable so a test can serve gists from a local server.
	APIBase *url.URL
}

// Configured reports whether any remote source is configured.
func (c Config) Configured() bool { return c.HTTPS != nil || c.Gist != nil }

// ParseConfig reads the two remote source settings and validates them. A
// malformed setting is refused: a configured source is never silently
// replaced by another one.
func ParseConfig(getenv func(string) string) (Config, error) {
	var cfg Config
	if getenv == nil {
		return cfg, nil
	}
	if raw := strings.TrimSpace(getenv(URLEnv)); raw != "" {
		u, err := parseIndexURL(raw)
		if err != nil {
			return Config{}, err
		}
		cfg.HTTPS = &HTTPSSource{URL: u}
	}
	if raw := strings.TrimSpace(getenv(GistEnv)); raw != "" {
		g, err := parseGist(raw)
		if err != nil {
			return Config{}, err
		}
		cfg.Gist = g
	}
	return cfg, nil
}

// parseIndexURL validates the index URL of an https source. Only https is
// accepted, and userinfo is refused: a URL that carries credentials has
// no place in an identity or a diagnostic.
func parseIndexURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, &ConfigError{Setting: URLEnv, Message: "is not a valid URL"}
	}
	switch {
	case u.Scheme != "https":
		return nil, &ConfigError{Setting: URLEnv, Message: "must use https"}
	case u.Host == "":
		return nil, &ConfigError{Setting: URLEnv, Message: "must name a host"}
	case u.User != nil:
		return nil, &ConfigError{Setting: URLEnv, Message: "must not carry userinfo"}
	}
	return u, nil
}

// parseGist validates the "<id>@<revision>" setting of a gist source. Both
// parts are required and both are lowercase hex, so a revision is never a
// branch name that could move.
func parseGist(raw string) (*GistSource, error) {
	id, revision, ok := strings.Cut(raw, "@")
	if !ok || !isHex(id) || !isHex(revision) || strings.Contains(revision, "@") {
		return nil, &ConfigError{Setting: GistEnv, Message: "must be a lowercase hex id and revision as <id>@<revision>"}
	}
	base, err := url.Parse(DefaultAPIBase)
	if err != nil {
		return nil, err
	}
	return &GistSource{ID: id, Revision: revision, APIBase: base}, nil
}

// isHex reports whether s is a non-empty lowercase hex string.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ConfigError is a malformed remote source setting. It never quotes the
// value, which may carry a credential.
type ConfigError struct {
	Setting string
	Message string
}

func (e *ConfigError) Error() string { return e.Setting + " " + e.Message }

// FetchError marks a transport failure: the request never delivered the
// content, because the network, TLS, a redirect or a size bound stopped
// it. It is distinct from a content failure, which means the content
// arrived and was wrong.
type FetchError struct {
	Err error
}

func (e *FetchError) Error() string { return e.Err.Error() }

func (e *FetchError) Unwrap() error { return e.Err }

// ErrNotCached means a configured source has no cached snapshot. It is
// wrapped with the source's location, so a caller can name the source.
var ErrNotCached = errors.New("no cached snapshot")

// Result is the outcome of refreshing one configured source.
type Result struct {
	// Source is the identity location of the source.
	Source string `json:"source"`
	// Revision is the revision the snapshot is pinned to.
	Revision string `json:"revision"`
	// Files is the number of definition files in the snapshot.
	Files int `json:"files"`
	// Outcome is OutcomeRefreshed or OutcomeUnchanged.
	Outcome string `json:"outcome"`
}

// CachedSource is one cached snapshot: its identity, the revision it is
// pinned to and the definition files it holds.
type CachedSource struct {
	// Identity names the source the snapshot came from.
	Identity source.Identity
	// Revision is the revision the snapshot is pinned to.
	Revision string
	// FS holds the definition files of the snapshot.
	FS fs.FS
}

// Refresh fetches every configured source and caches its snapshot. It is
// the only function in this package that touches the network.
func Refresh(ctx context.Context, cfg Config, stateDir string, client *http.Client) ([]Result, error) {
	if !cfg.Configured() {
		return nil, nil
	}
	c := newCache(stateDir)
	var out []Result
	if src := cfg.HTTPS; src != nil {
		ident := src.identity()
		fetched, err := fetchHTTPS(ctx, src, client)
		if err != nil {
			return nil, err
		}
		unchanged, err := c.write(ident, fetched.revision, fetched.files)
		if err != nil {
			return nil, err
		}
		if err := c.setCurrent(ident.Location, fetched.revision); err != nil {
			return nil, err
		}
		out = append(out, Result{
			Source:   ident.Location,
			Revision: fetched.revision,
			Files:    len(fetched.files),
			Outcome:  outcomeOf(unchanged),
		})
	}
	if src := cfg.Gist; src != nil {
		ident := src.identity()
		fetched, err := fetchGist(ctx, src, client)
		if err != nil {
			return nil, err
		}
		unchanged, err := c.write(ident, fetched.revision, fetched.files)
		if err != nil {
			return nil, err
		}
		out = append(out, Result{
			Source:   ident.Location,
			Revision: fetched.revision,
			Files:    len(fetched.files),
			Outcome:  outcomeOf(unchanged),
		})
	}
	return out, nil
}

// Cached returns the cached snapshot of every configured source. It never
// touches the network and never writes: a source that was never refreshed
// is ErrNotCached, never an implicit fetch.
func Cached(cfg Config, stateDir string) ([]CachedSource, error) {
	if !cfg.Configured() {
		return nil, nil
	}
	c := newCache(stateDir)
	var out []CachedSource
	if src := cfg.HTTPS; src != nil {
		ident := src.identity()
		revision, err := c.current(ident.Location)
		if err != nil {
			return nil, err
		}
		entry, err := c.read(ident, revision)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	if src := cfg.Gist; src != nil {
		ident := src.identity()
		entry, err := c.read(ident, src.Revision)
		if err != nil {
			return nil, err
		}
		out = append(out, entry)
	}
	return out, nil
}

// outcomeOf names the outcome of a write that may have found the snapshot
// already cached.
func outcomeOf(unchanged bool) string {
	if unchanged {
		return OutcomeUnchanged
	}
	return OutcomeRefreshed
}

// identity is the identity of an https source: the index document's host
// and path, without query, fragment or userinfo.
func (s *HTTPSSource) identity() source.Identity {
	return source.Identity{Kind: "https", Location: "https://" + s.URL.Host + s.URL.EscapedPath()}
}

// identity is the identity of a gist source: the gist id, never the API
// root it was read from.
func (s *GistSource) identity() source.Identity {
	return source.Identity{Kind: "gist", Location: "gist:" + s.ID}
}

// apiBase is the API root of a gist source, defaulting to the public
// GitHub API.
func (s *GistSource) apiBase() string {
	if s.APIBase == nil {
		return DefaultAPIBase
	}
	return strings.TrimSuffix(s.APIBase.String(), "/")
}

// digest is the lowercase hex SHA-256 of some bytes.
func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// DefaultClient returns the client every fetch uses unless a caller
// injects its own: TLS verification on, a total timeout, and redirects
// limited to the host the request started with.
func DefaultClient() *http.Client {
	return &http.Client{
		Timeout:       RequestTimeout,
		CheckRedirect: redirectPolicy,
	}
}

// redirectPolicy allows a few redirects and only to https on the host the
// request started with: a source cannot hand its content to another host.
func redirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return fmt.Errorf("more than %d redirects", MaxRedirects)
	}
	if req.URL.Scheme != "https" {
		return fmt.Errorf("redirect to %q is not allowed", req.URL.Scheme)
	}
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("redirect to host %q is not allowed", req.URL.Host)
	}
	return nil
}
