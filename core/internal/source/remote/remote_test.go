package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
)

// definition is a valid software definition. A definition file is named
// after the id it defines.
const definition = `schema_version: 1
id: example
description: An example definition served by a fixture.
categories: [tools]
recipes:
  - id: brew
    provider: brew
    package: example
    platforms: [darwin/arm64]
    targets: [global]
`

// anotherDefinition is a second definition, so a snapshot holds more than
// one file.
const anotherDefinition = `schema_version: 1
id: another
description: Another example definition served by a fixture.
categories: [tools]
recipes:
  - id: brew
    provider: brew
    package: another
    platforms: [darwin/arm64]
    targets: [global]
`

// indexJSON renders the index document of the given files.
func indexJSON(files map[string]string) string {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	entries := make([]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, fmt.Sprintf(`{"name":%q,"sha256":%q}`, name, digest([]byte(files[name]))))
	}
	return fmt.Sprintf(`{"schema_version":1,"files":[%s]}`, strings.Join(entries, ","))
}

// server starts a TLS server with the given handler and the client that
// trusts its certificate.
func server(t *testing.T, h http.HandlerFunc) (*httptest.Server, *http.Client) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	return srv, srv.Client()
}

// fileServer serves the given files over TLS, one path per file name.
func fileServer(t *testing.T, files map[string]string) (*httptest.Server, *http.Client) {
	t.Helper()
	return server(t, func(w http.ResponseWriter, r *http.Request) {
		body, ok := files[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	})
}

// indexServer serves an index document and the definition files it lists.
// A file named "index.json" in the map replaces the generated index, so a
// test can serve a malformed one.
func indexServer(t *testing.T, files map[string]string) (*httptest.Server, *http.Client) {
	t.Helper()
	return server(t, func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "index.json" {
			if body, ok := files["index.json"]; ok {
				w.Write([]byte(body))
				return
			}
			w.Write([]byte(indexJSON(files)))
			return
		}
		body, ok := files[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body))
	})
}

// mustURL parses a URL that the test built.
func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// httpsConfig is the configuration of the fixture index server.
func httpsConfig(t *testing.T, srv *httptest.Server) Config {
	t.Helper()
	return Config{HTTPS: &HTTPSSource{URL: mustURL(t, srv.URL+"/index.json")}}
}

// readFile reads one file of a snapshot.
func readFile(fsys fs.FS, name string) (string, error) {
	data, err := fs.ReadFile(fsys, name)
	return string(data), err
}

// A refresh fetches the source, caches it and hands back the identity and
// the revision; a later read finds the same snapshot without the network.
func TestHTTPSRefreshAndCache(t *testing.T) {
	files := map[string]string{"example.yaml": definition}
	index := indexJSON(files)
	srv, client := indexServer(t, files)
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	res, err := Refresh(context.Background(), cfg, dir, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	want := source.Identity{Kind: "https", Location: srv.URL + "/index.json"}
	if res[0].Source != want.Location {
		t.Errorf("source %q, want %q", res[0].Source, want.Location)
	}
	if wantRev := "sha256:" + digest([]byte(index)); res[0].Revision != wantRev {
		t.Errorf("revision %q, want %q", res[0].Revision, wantRev)
	}
	if res[0].Files != 1 || res[0].Outcome != OutcomeRefreshed {
		t.Errorf("result %+v, want one refreshed file", res[0])
	}

	cached, err := Cached(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 {
		t.Fatalf("got %d cached snapshots, want 1", len(cached))
	}
	if cached[0].Identity != want {
		t.Errorf("identity %+v, want %+v", cached[0].Identity, want)
	}
	if cached[0].Revision != res[0].Revision {
		t.Errorf("revision %q, want %q", cached[0].Revision, res[0].Revision)
	}
	got, err := readFile(cached[0].FS, "example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != definition {
		t.Errorf("cached definition differs:\n%s", got)
	}
}

// A second refresh of an unchanged source leaves the cached snapshot
// alone.
func TestHTTPSRefreshUnchanged(t *testing.T) {
	files := map[string]string{"example.yaml": definition}
	srv, client := indexServer(t, files)
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	if _, err := Refresh(context.Background(), cfg, dir, client); err != nil {
		t.Fatal(err)
	}
	res, err := Refresh(context.Background(), cfg, dir, client)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != OutcomeUnchanged {
		t.Errorf("outcome %q, want %q", res[0].Outcome, OutcomeUnchanged)
	}
}

// Content that does not match the digest the index records is refused,
// and nothing is cached.
func TestTamperedFileRejected(t *testing.T) {
	// The index records the digest of an edited file; the server
	// delivers the original.
	tampered := indexJSON(map[string]string{"example.yaml": definition + "# edited\n"})
	srv, client := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/index.json" {
			w.Write([]byte(tampered))
			return
		}
		w.Write([]byte(definition))
	})
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	_, err := Refresh(context.Background(), cfg, dir, client)
	if !errors.Is(err, schema.Err(schema.DigestMismatch)) {
		t.Fatalf("got %v, want a digest mismatch", err)
	}
	if _, err := Cached(cfg, dir); !errors.Is(err, ErrNotCached) {
		t.Errorf("got %v, want no cached snapshot", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sources", "https")); !os.IsNotExist(err) {
		t.Errorf("the cache holds a snapshot of a tampered source")
	}
}

// A response beyond the size bound is refused, for the index and for a
// definition file.
func TestOversizedResponsesRejected(t *testing.T) {
	big := strings.Repeat("x", MaxFileBytes+1)
	cases := map[string]map[string]string{
		"index": {"index.json": big},
		"file":  {"example.yaml": big},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			srv, client := indexServer(t, files)
			dir := t.TempDir()
			cfg := httpsConfig(t, srv)
			_, err := Refresh(context.Background(), cfg, dir, client)
			var fe *FetchError
			if !errors.As(err, &fe) {
				t.Fatalf("got %v, want a fetch failure", err)
			}
			if _, err := Cached(cfg, dir); !errors.Is(err, ErrNotCached) {
				t.Errorf("got %v, want no cached snapshot", err)
			}
		})
	}
}

// A redirect to another host is refused: a source cannot hand its
// content to a host the user never configured.
func TestRedirectToAnotherHostRejected(t *testing.T) {
	other, _ := fileServer(t, map[string]string{"index.json": `{"schema_version":1,"files":[]}`})
	srv, client := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/index.json", http.StatusFound)
	})
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	_, err := Refresh(context.Background(), cfg, dir, client)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("got %v, want a fetch failure", err)
	}
	if !strings.Contains(err.Error(), "redirect to host") {
		t.Errorf("got %v, want the redirect refused", err)
	}
}

// A redirect to http is refused: the transport stays on https.
func TestRedirectToHTTPRejected(t *testing.T) {
	srv, client := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.invalid/index.json", http.StatusFound)
	})
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	_, err := Refresh(context.Background(), cfg, dir, client)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("got %v, want a fetch failure", err)
	}
	if !strings.Contains(err.Error(), `redirect to "http"`) {
		t.Errorf("got %v, want the redirect refused", err)
	}
}

// A redirect on the same host is followed: the source may reorganize its
// own directory.
func TestRedirectOnSameHostFollowed(t *testing.T) {
	files := map[string]string{"example.yaml": definition}
	srv, client := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.json":
			http.Redirect(w, r, "/catalog.json", http.StatusFound)
		case "/catalog.json":
			w.Write([]byte(indexJSON(files)))
		default:
			w.Write([]byte(files[strings.TrimPrefix(r.URL.Path, "/")]))
		}
	})
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	res, err := Refresh(context.Background(), cfg, dir, client)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Outcome != OutcomeRefreshed || res[0].Files != 1 {
		t.Errorf("result %+v, want one refreshed file", res[0])
	}
}

// A server that answers too slowly fails the request instead of hanging.
func TestTimeoutRejected(t *testing.T) {
	release := make(chan struct{})
	srv, client := server(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	// The handler is released before the server is closed: the server
	// waits for the requests it still serves.
	t.Cleanup(func() { close(release) })
	client.Timeout = 50 * time.Millisecond
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	_, err := Refresh(context.Background(), cfg, dir, client)
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("got %v, want a fetch failure", err)
	}
}

// A gist is fetched from its pinned revision, and only its yaml files are
// definitions.
func TestGistRefreshAndCache(t *testing.T) {
	id := "abc123"
	revision := "0123456789abcdef0123456789abcdef01234567"
	body := fmt.Sprintf(`{"files":{"example.yaml":{"filename":"example.yaml","size":%d,"truncated":false,"content":%q},"notes.txt":{"filename":"notes.txt","size":2,"truncated":false,"content":"hi"}}}`, len(definition), definition)
	srv, client := fileServer(t, map[string]string{"gists/" + id + "/" + revision: body})
	dir := t.TempDir()
	cfg := Config{Gist: &GistSource{ID: id, Revision: revision, APIBase: mustURL(t, srv.URL)}}

	res, err := Refresh(context.Background(), cfg, dir, client)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("got %d results, want 1", len(res))
	}
	if res[0].Source != "gist:"+id {
		t.Errorf("source %q, want %q", res[0].Source, "gist:"+id)
	}
	if res[0].Revision != revision {
		t.Errorf("revision %q, want the configured revision %q", res[0].Revision, revision)
	}
	if res[0].Files != 1 || res[0].Outcome != OutcomeRefreshed {
		t.Errorf("result %+v, want one refreshed file", res[0])
	}

	cached, err := Cached(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cached) != 1 {
		t.Fatalf("got %d cached snapshots, want 1", len(cached))
	}
	if cached[0].Identity != (source.Identity{Kind: "gist", Location: "gist:" + id}) {
		t.Errorf("identity %+v, want the gist identity", cached[0].Identity)
	}
	got, err := readFile(cached[0].FS, "example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != definition {
		t.Errorf("cached definition differs:\n%s", got)
	}
}

// A truncated gist file is refused: its rest would come from another
// host, which this package never follows.
func TestGistTruncatedFileRejected(t *testing.T) {
	id := "abc123"
	revision := "0123456789abcdef0123456789abcdef01234567"
	body := fmt.Sprintf(`{"files":{"example.yaml":{"filename":"example.yaml","size":9000,"truncated":true,"content":%q}}}`, definition)
	srv, client := fileServer(t, map[string]string{"gists/" + id + "/" + revision: body})
	dir := t.TempDir()
	cfg := Config{Gist: &GistSource{ID: id, Revision: revision, APIBase: mustURL(t, srv.URL)}}

	_, err := Refresh(context.Background(), cfg, dir, client)
	if err == nil {
		t.Fatal("a truncated gist file was accepted")
	}
	if _, err := Cached(cfg, dir); !errors.Is(err, ErrNotCached) {
		t.Errorf("got %v, want no cached snapshot", err)
	}
}

// A malformed setting is a configuration error, never a silent fallback.
func TestMalformedConfigRejected(t *testing.T) {
	cases := map[string]map[string]string{
		"http url":         {URLEnv: "http://example.com/index.json"},
		"url with user":    {URLEnv: "https://user@example.com/index.json"},
		"url without host": {URLEnv: "https:///index.json"},
		"gist without at":  {GistEnv: "abc123"},
		"gist without id":  {GistEnv: "@0123456789abcdef"},
		"gist without rev": {GistEnv: "abc123@"},
		"gist not hex":     {GistEnv: "abc123@ZZZZ"},
		"gist upper hex":   {GistEnv: "ABC123@0123456789abcdef"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig(func(k string) string { return env[k] }); err == nil {
				t.Error("a malformed setting was accepted")
			}
		})
	}
}

// A well formed setting is accepted, and neither setting means no remote
// source at all.
func TestConfigAccepted(t *testing.T) {
	env := map[string]string{
		URLEnv:  "https://example.com/catalog/index.json",
		GistEnv: "abc123@0123456789abcdef0123456789abcdef01234567",
	}
	cfg, err := ParseConfig(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Configured() || cfg.HTTPS == nil || cfg.Gist == nil {
		t.Fatalf("configuration %+v, want both sources", cfg)
	}
	if got := cfg.HTTPS.identity().Location; got != "https://example.com/catalog/index.json" {
		t.Errorf("location %q, want the index URL without query", got)
	}
	if got := cfg.Gist.identity().Location; got != "gist:abc123" {
		t.Errorf("location %q, want the gist id", got)
	}
	empty, err := ParseConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if empty.Configured() {
		t.Error("an empty environment configured a remote source")
	}
}

// A source that was never refreshed has no snapshot, and reading it is
// never an implicit fetch.
func TestCachedWithoutRefresh(t *testing.T) {
	srv, _ := indexServer(t, map[string]string{"example.yaml": definition})
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	if _, err := Cached(cfg, dir); !errors.Is(err, ErrNotCached) {
		t.Fatalf("got %v, want no cached snapshot", err)
	}
	if _, err := Cached(cfg, dir); !errors.Is(err, ErrNotCached) {
		t.Errorf("got %v, want no cached snapshot", err)
	}
}

// A cached file that was changed on disk fails the verification: a
// snapshot is only the content it recorded.
func TestCachedFileModifiedOnDisk(t *testing.T) {
	files := map[string]string{"example.yaml": definition}
	srv, client := indexServer(t, files)
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	if _, err := Refresh(context.Background(), cfg, dir, client); err != nil {
		t.Fatal(err)
	}
	editCachedFile(t, dir, "example.yaml", definition+"# edited\n")
	if _, err := Cached(cfg, dir); !errors.Is(err, schema.Err(schema.DigestMismatch)) {
		t.Fatalf("got %v, want a digest mismatch", err)
	}
}

// No identity, revision or record carries the path of the machine the
// snapshot was cached on.
func TestIdentityCarriesNoHostPath(t *testing.T) {
	files := map[string]string{"example.yaml": definition}
	srv, client := indexServer(t, files)
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	res, err := Refresh(context.Background(), cfg, dir, client)
	if err != nil {
		t.Fatal(err)
	}
	cached, err := Cached(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		res[0].Source, res[0].Revision, res[0].Outcome,
		cached[0].Identity.Location, cached[0].Revision,
	} {
		if strings.Contains(s, dir) || strings.Contains(s, os.TempDir()) {
			t.Errorf("%q carries a host path", s)
		}
	}
}

// editCachedFile changes one definition file of the cached snapshot.
func editCachedFile(t *testing.T, stateDir, name, content string) {
	t.Helper()
	err := filepath.Walk(filepath.Join(stateDir, "sources"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && info.Name() == name {
			return os.WriteFile(path, []byte(content), 0o644)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The cached snapshot names the source, the revision and the digest of
// every file it holds, and the pointer file names the revision of the
// most recent refresh.
func TestSnapshotMetadata(t *testing.T) {
	files := map[string]string{"example.yaml": definition, "another.yaml": anotherDefinition}
	srv, client := indexServer(t, files)
	dir := t.TempDir()
	cfg := httpsConfig(t, srv)

	res, err := Refresh(context.Background(), cfg, dir, client)
	if err != nil {
		t.Fatal(err)
	}
	location := srv.URL + "/index.json"
	sum := sha256.Sum256([]byte(location + "@" + res[0].Revision))
	snapshot := filepath.Join(dir, "sources", "https", hex.EncodeToString(sum[:]))
	blob, err := os.ReadFile(filepath.Join(snapshot, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"schema_version":1,"kind":"https","location":%q,"revision":%q,"files":[`+
		`{"name":"another.yaml","sha256":%q},{"name":"example.yaml","sha256":%q}]}`,
		location, res[0].Revision, digest([]byte(anotherDefinition)), digest([]byte(definition)))
	if string(blob) != want {
		t.Errorf("snapshot.json =\n%s\nwant\n%s", blob, want)
	}
	pointer := filepath.Join(dir, "sources", "https", digest([]byte(location))+".current")
	got, err := os.ReadFile(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != res[0].Revision {
		t.Errorf("the pointer file holds %q, want %q", got, res[0].Revision)
	}
}
