package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/source"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

var fixtureID = source.Identity{Kind: "local", Location: "fixtures/local"}

// The configuration definitions of a source normalize to deterministic
// JSON: one record per definition, in id order.
func TestCatalogGolden(t *testing.T) {
	entries := fixtureEntries(t)
	if len(entries) != 2 {
		t.Fatalf("want 2 configuration definitions, got %d", len(entries))
	}
	out, err := Normalize(entries)
	if err != nil {
		t.Fatal(err)
	}
	path := "testdata/catalog.golden.jsonl"
	if *update {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Errorf("normalized catalog differs from the golden (run with -update to inspect)\ngot:\n%s", out)
	}
}

// The same snapshot bytes always normalize to the same bytes.
func TestSameSnapshotSameBytes(t *testing.T) {
	a := fixtureEntries(t)
	b := fixtureEntries(t)
	first, err := Normalize(a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Normalize(b)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Errorf("two normalizations of one snapshot differ:\n%s\n%s", first, second)
	}
}

// A source may hold software definitions too: the configuration loader
// ignores them, and the software loader ignores the configuration
// definitions.
func TestKindsAreIgnored(t *testing.T) {
	entries := fixtureEntries(t)
	for _, e := range entries {
		if e.Definition.ID == "ripgrep" {
			t.Error("a software definition reached the configuration catalog")
		}
	}
	snap, err := source.Load(fixtureID, os.DirFS("testdata/local"), ".", registry.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 1 || snap.Entries[0].Software.ID != "ripgrep" {
		t.Errorf("the software catalog holds %d entries", len(snap.Entries))
	}
}

// An id defined by two sources is a conflict: no source silently
// overrides another.
func TestConfigConflict(t *testing.T) {
	snap, err := Load(fixtureID, os.DirFS("testdata/local"), ".")
	if err != nil {
		t.Fatal(err)
	}
	shadow, err := Load(source.Identity{Kind: "local", Location: "fixtures/shadow"}, fstest.MapFS{
		"shell.yaml": {Data: []byte("kind: config\nschema_version: 1\nid: shell\ndescription: Other.\ntargets: [user]\noperations:\n  - id: rc\n    provider: managed-block\n    destination: shell-rc\n    block_id: managed-machine\n    content: text\n")},
	}, ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Catalog(snap, shadow); !errors.Is(err, schema.Err(schema.Conflict)) ||
		!strings.Contains(err.Error(), `"shell" is already defined by fixtures/local/shell.yaml`) {
		t.Errorf("a configuration id defined by two sources: got %v", err)
	}
}

// A template source is a file in the source, and its tokens are checked
// before the definition is used.
func TestTemplateSourceIsChecked(t *testing.T) {
	const doc = "kind: config\nschema_version: 1\nid: editor\ndescription: An editor.\ntargets: [user]\n%soperations:\n  - id: rc\n    provider: template\n    destination: rc\n    source: templates/rc.tmpl\n"
	fsys := fstest.MapFS{
		"editor.yaml":       {Data: []byte(fmt.Sprintf(doc, ""))},
		"templates/rc.tmpl": {Data: []byte("export EDITOR={{ inputs.nope }}\n")},
	}
	_, err := Load(source.Identity{Kind: "local", Location: "fixtures/local"}, fsys, ".")
	if err == nil || !strings.Contains(err.Error(), `unknown inputs "nope"`) {
		t.Errorf("a template naming an undeclared input: got %v", err)
	}
	fsys["editor.yaml"] = &fstest.MapFile{Data: []byte(fmt.Sprintf(doc, "inputs:\n  editor: vim\n"))}
	fsys["templates/rc.tmpl"] = &fstest.MapFile{Data: []byte("export EDITOR={{ inputs.editor }}\n")}
	if _, err := Load(source.Identity{Kind: "local", Location: "fixtures/local"}, fsys, "."); err != nil {
		t.Errorf("a template naming a declared input: %v", err)
	}
}

// A missing template source is refused: a definition never points at a
// file that is not there.
func TestMissingTemplateSource(t *testing.T) {
	fsys := fstest.MapFS{
		"editor.yaml": {Data: []byte("kind: config\nschema_version: 1\nid: editor\ndescription: An editor.\ntargets: [user]\noperations:\n  - id: rc\n    provider: template\n    destination: rc\n    source: templates/rc.tmpl\n")},
	}
	_, err := Load(source.Identity{Kind: "local", Location: "fixtures/local"}, fsys, ".")
	if err == nil || !strings.Contains(err.Error(), "is not in the source") {
		t.Errorf("a missing template source: got %v", err)
	}
}

// fixtureEntries loads the configuration definitions of the fixture
// source.
func fixtureEntries(t *testing.T) []Entry {
	t.Helper()
	snap, err := Load(fixtureID, os.DirFS("testdata/local"), ".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := Catalog(snap)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}
