package source

import (
	"bytes"
	"errors"
	"flag"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/core/internal/extension"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/schema"
)

var update = flag.Bool("update", false, "rewrite testdata/catalog.golden.jsonl")

var (
	builtinID = Identity{Kind: "builtin", Location: "builtin"}
	localID   = Identity{Kind: "local", Location: "fixtures/local"}
)

// withExtension is the builtin registry plus the cargo-tools fixture
// extension's providers, discovered from its manifest only.
func withExtension(t *testing.T) *registry.Registry {
	t.Helper()
	reg := registry.Builtin()
	m, err := extension.Discover(os.DirFS("../extension/testdata/cargo-tools"), "cargo-tools")
	if err != nil {
		t.Fatal(err)
	}
	if err := extension.RegisterProviders(reg, m); err != nil {
		t.Fatal(err)
	}
	return reg
}

func normalizedDirs(t *testing.T, builtin, local fs.FS) []byte {
	t.Helper()
	reg := withExtension(t)
	b, err := Load(builtinID, builtin, "builtin", reg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := Load(localID, local, "local", reg)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := Catalog(b, l)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Normalize(cat)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCatalogGolden(t *testing.T) {
	out := normalizedDirs(t, os.DirFS("testdata"), os.DirFS("testdata"))
	if *update {
		if err := os.WriteFile("testdata/catalog.golden.jsonl", out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile("testdata/catalog.golden.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, want) {
		t.Errorf("normalized catalog differs from golden (run with -update to inspect)\ngot:\n%s", out)
	}
	if n := bytes.Count(out, []byte("\n")); n != 4 {
		t.Errorf("want 4 records (3 builtin, 1 local), got %d", n)
	}
	if !bytes.Contains(out, []byte(`"source":{"kind":"local","location":"fixtures/local"}`)) {
		t.Error("local definition lost its source identity")
	}
}

// The same snapshot bytes always normalize to the same bytes, whatever
// the file system or read order; one changed byte changes the revision.
func TestSameSnapshotSameBytes(t *testing.T) {
	a := normalizedDirs(t, os.DirFS("testdata"), os.DirFS("testdata"))
	mem := fstest.MapFS{}
	for _, dir := range []string{"builtin", "local"} {
		ents, _ := os.ReadDir("testdata/" + dir)
		for i := len(ents) - 1; i >= 0; i-- {
			data, err := os.ReadFile("testdata/" + dir + "/" + ents[i].Name())
			if err != nil {
				t.Fatal(err)
			}
			mem[dir+"/"+ents[i].Name()] = &fstest.MapFile{Data: data}
		}
	}
	b := normalizedDirs(t, mem, mem)
	if !bytes.Equal(a, b) {
		t.Fatalf("two normalizations of one snapshot differ:\n%s\n%s", a, b)
	}

	reg := withExtension(t)
	s1, err := Load(builtinID, mem, "builtin", reg)
	if err != nil {
		t.Fatal(err)
	}
	mem["builtin/uv.yaml"] = &fstest.MapFile{Data: append([]byte("# edited\n"), mem["builtin/uv.yaml"].Data...)}
	s2, err := Load(builtinID, mem, "builtin", reg)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Revision == s2.Revision {
		t.Error("revision did not change when a file changed")
	}
}

func TestLocalDefinitionNeedsItsProvider(t *testing.T) {
	_, err := Load(localID, os.DirFS("testdata"), "local", registry.Builtin())
	if !errors.Is(err, schema.Err(schema.UnknownProvider)) {
		t.Fatalf("got %v, want unknown provider without the extension", err)
	}
}

func TestConflicts(t *testing.T) {
	reg := withExtension(t)
	b, err := Load(builtinID, os.DirFS("testdata"), "builtin", reg)
	if err != nil {
		t.Fatal(err)
	}
	ruff, _ := os.ReadFile("testdata/builtin/ruff.yaml")
	shadow, err := Load(localID, fstest.MapFS{"d/ruff.yaml": {Data: ruff}}, "d", reg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Catalog(b, shadow); !errors.Is(err, schema.Err(schema.Conflict)) || !strings.Contains(err.Error(), `"ruff" is already defined by builtin/ruff.yaml`) {
		t.Errorf("a local definition shadowing a builtin id: got %v", err)
	}
	if _, err := Catalog(b, b); !errors.Is(err, schema.Err(schema.Conflict)) {
		t.Errorf("same source twice: got %v", err)
	}
	if err := reg.Register("extension:other@1.0.0", registry.Provider{Name: "cargo"}); !errors.Is(err, schema.Err(schema.Conflict)) {
		t.Errorf("second provider named cargo: got %v", err)
	}
	if err := reg.Register("extension:other@1.0.0", registry.Provider{Name: "brew"}); !errors.Is(err, schema.Err(schema.Conflict)) {
		t.Errorf("extension provider shadowing builtin brew: got %v", err)
	}
}

func TestLoadRejects(t *testing.T) {
	ruff, _ := os.ReadFile("testdata/builtin/ruff.yaml")
	reg := registry.Builtin()
	for name, fsys := range map[string]fstest.MapFS{
		"file name differs from id": {"d/linter.yaml": {Data: ruff}},
		"non-yaml file":             {"d/ruff.yaml": {Data: ruff}, "d/install.sh": {Data: []byte("exit 0\n")}},
		"yml extension":             {"d/ruff.yml": {Data: ruff}},
	} {
		if _, err := Load(localID, fsys, "d", reg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(Identity{Kind: "ftp", Location: "x"}, fstest.MapFS{}, ".", reg); err == nil {
		t.Error("unknown source kind accepted")
	}
}
