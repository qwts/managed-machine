package software

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/source/remote"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

// remoteDefinition is a valid software definition, named after the id it
// defines.
const remoteDefinition = `schema_version: 1
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

// remoteServer serves an index document and the definition file it lists
// over TLS, and counts the requests it answered.
func remoteServer(t *testing.T) (*httptest.Server, *http.Client, *int64) {
	t.Helper()
	var hits int64
	sum := sha256.Sum256([]byte(remoteDefinition))
	index := fmt.Sprintf(`{"schema_version":1,"files":[{"name":"example.yaml","sha256":%q}]}`, hex.EncodeToString(sum[:]))
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		switch r.URL.Path {
		case "/index.json":
			w.Write([]byte(index))
		case "/example.yaml":
			w.Write([]byte(remoteDefinition))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, srv.Client(), &hits
}

// remoteHarness is a harness with one configured remote source and an
// isolated state directory.
func remoteHarness(t *testing.T) (*harness, *httptest.Server, *int64) {
	t.Helper()
	srv, client, hits := remoteServer(t)
	h := &harness{
		client: client,
		env: map[string]string{
			remote.URLEnv:     srv.URL + "/index.json",
			statedir.Override: t.TempDir(),
		},
	}
	return h, srv, hits
}

// The refresh action reports one record per source, with the keys in the
// documented order.
func TestRefreshRecords(t *testing.T) {
	h, srv, _ := remoteHarness(t)
	out, errOut, code := h.run(t, target.DarwinARM64, "--refresh")
	if code != 0 || errOut != "" {
		t.Fatalf("refresh: exit %d %q", code, errOut)
	}
	recs, err := output.ReadCompact([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1:\n%s", len(recs), out)
	}
	fields := recs[0].Fields()
	wantKeys := []string{"source", "revision", "files", "outcome"}
	if len(fields) != len(wantKeys) {
		t.Fatalf("got %d fields, want %d", len(fields), len(wantKeys))
	}
	for i, f := range fields {
		if f.Key != wantKeys[i] {
			t.Errorf("field %d is %q, want %q", i, f.Key, wantKeys[i])
		}
	}
	source, _ := recs[0].Get("source")
	if source != srv.URL+"/index.json" {
		t.Errorf("source %v, want the index URL", source)
	}
	revision, _ := recs[0].Get("revision")
	if !strings.HasPrefix(revision.(string), "sha256:") {
		t.Errorf("revision %v, want a sha256 revision", revision)
	}
	files, _ := recs[0].Get("files")
	if fmt.Sprint(files) != "1" {
		t.Errorf("files %v, want 1", files)
	}
	outcome, _ := recs[0].Get("outcome")
	if outcome != remote.OutcomeRefreshed {
		t.Errorf("outcome %v, want %q", outcome, remote.OutcomeRefreshed)
	}
}

// A second refresh of the same source reports the unchanged snapshot.
func TestRefreshUnchanged(t *testing.T) {
	h, _, _ := remoteHarness(t)
	if _, errOut, code := h.run(t, target.DarwinARM64, "--refresh"); code != 0 || errOut != "" {
		t.Fatalf("first refresh: exit %d %q", code, errOut)
	}
	out, errOut, code := h.run(t, target.DarwinARM64, "--refresh")
	if code != 0 || errOut != "" {
		t.Fatalf("second refresh: exit %d %q", code, errOut)
	}
	if !strings.Contains(out, `"outcome":"unchanged"`) {
		t.Errorf("second refresh did not report the unchanged snapshot:\n%s", out)
	}
}

// The refresh records render in every presentation.
func TestRefreshPresentations(t *testing.T) {
	h, _, _ := remoteHarness(t)
	jsonOut, errOut, code := h.run(t, target.DarwinARM64, "--refresh", "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("json refresh: exit %d %q", code, errOut)
	}
	if !strings.Contains(jsonOut, `"source"`) || !strings.Contains(jsonOut, `"outcome"`) {
		t.Errorf("json refresh:\n%s", jsonOut)
	}
	humanOut, errOut, code := h.run(t, target.DarwinARM64, "--refresh", "--human")
	if code != 0 || errOut != "" {
		t.Fatalf("human refresh: exit %d %q", code, errOut)
	}
	if !strings.Contains(humanOut, "SOURCE") || !strings.Contains(humanOut, "OUTCOME") {
		t.Errorf("human refresh:\n%s", humanOut)
	}
}

// After a refresh the discovery lists the remote definition, with the
// identity of the source it came from.
func TestDiscoveryAfterRefresh(t *testing.T) {
	h, srv, _ := remoteHarness(t)
	if _, errOut, code := h.run(t, target.DarwinARM64, "--refresh"); code != 0 || errOut != "" {
		t.Fatalf("refresh: exit %d %q", code, errOut)
	}
	out, errOut, code := h.run(t, target.DarwinARM64, "--available")
	if code != 0 || errOut != "" {
		t.Fatalf("discovery: exit %d %q", code, errOut)
	}
	if !strings.Contains(out, `"software":"example"`) {
		t.Errorf("the remote definition is not listed:\n%s", out)
	}
	entries, _, err := LoadCatalog(func(k string) string { return h.env[k] })
	if err != nil {
		t.Fatal(err)
	}
	want := source.Identity{Kind: "https", Location: srv.URL + "/index.json"}
	found := false
	for _, e := range entries {
		if e.Software.ID == "example" {
			found = true
			if e.Source != want {
				t.Errorf("source identity %+v, want %+v", e.Source, want)
			}
		}
	}
	if !found {
		t.Error("the remote definition is not in the catalog")
	}
}

// A configured source that was never refreshed is an explicit
// diagnostic, never an implicit fetch.
func TestUncachedSourceIsUnavailable(t *testing.T) {
	h, _, hits := remoteHarness(t)
	out, errOut, code := h.run(t, target.DarwinARM64, "--available")
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	if !strings.Contains(errOut, "MM_SOFTWARE_SOURCE_UNAVAILABLE") {
		t.Errorf("diagnostic %q, want MM_SOFTWARE_SOURCE_UNAVAILABLE", errOut)
	}
	if !strings.Contains(errOut, "managed-machine software --refresh") {
		t.Errorf("diagnostic %q, want the refresh command", errOut)
	}
	if out != "" {
		t.Errorf("stdout %q, want no records", out)
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Errorf("discovery made %d requests, want none", n)
	}
}

// Help wins over the refresh: it never fetches.
func TestHelpNeverFetches(t *testing.T) {
	h, _, hits := remoteHarness(t)
	out, errOut, code := h.run(t, target.DarwinARM64, "-h", "--refresh")
	if code != 0 {
		t.Fatalf("exit %d, want 0", code)
	}
	if errOut != "" && !strings.Contains(errOut, "MM_SOFTWARE_SOURCE_UNAVAILABLE") {
		t.Errorf("stderr %q, want no unexpected diagnostic", errOut)
	}
	if !strings.Contains(out, "Options") {
		t.Errorf("help output %q, want the options table", out)
	}
	if !strings.Contains(out, "--refresh") {
		t.Errorf("help output %q, want the refresh option", out)
	}
	if n := atomic.LoadInt64(hits); n != 0 {
		t.Errorf("help made %d requests, want none", n)
	}
}

// The refresh needs a configured source, and it never combines with a
// query.
func TestRefreshUsage(t *testing.T) {
	h := &harness{}
	for _, args := range [][]string{
		{"--refresh"},
		{"--refresh", "--available"},
		{"--refresh", "ruff"},
		{"--refresh", "--install", "ruff"},
	} {
		_, errOut, code := h.run(t, target.DarwinARM64, args...)
		if code != 2 || !strings.Contains(errOut, "MM_USAGE") {
			t.Errorf("%v: exit %d %q, want a usage error", args, code, errOut)
		}
	}
}
