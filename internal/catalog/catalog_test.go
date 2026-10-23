package catalog

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/statedir"
)

// The built-in catalog is embedded: discovery never reads a host path.
func TestBuiltinCatalogIsEmbedded(t *testing.T) {
	sources, err := Sources(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("want one source, got %d", len(sources))
	}
	s := sources[0]
	if s.Identity.Kind != "builtin" || s.Identity.Location != "builtin" {
		t.Errorf("identity %+v", s.Identity)
	}
	ents, err := fs.ReadDir(s.FS, s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) == 0 {
		t.Fatal("the built-in catalog is empty")
	}
	configs := 0
	for _, e := range ents {
		data, err := fs.ReadFile(s.FS, filepath.ToSlash(filepath.Join(s.Dir, e.Name())))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "kind: config") {
			configs++
		}
	}
	if configs == 0 {
		t.Error("the built-in catalog holds no configuration definition")
	}
}

// A local source is named by its setting, never by its path, so a record
// and a diagnostic never carry a host path.
func TestLocalSourceIsNamedByItsSetting(t *testing.T) {
	dir := t.TempDir()
	sources, err := Sources(func(k string) string {
		if k == SourceDirEnv {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("want two sources, got %d", len(sources))
	}
	if sources[1].Identity.Kind != "local" || sources[1].Identity.Location != "env:"+SourceDirEnv {
		t.Errorf("identity %+v", sources[1].Identity)
	}
	if strings.Contains(sources[1].Identity.Location, dir) {
		t.Error("the identity carries the host path")
	}
}

// Without a configured remote source the state directory is never even
// computed, so a machine without one never creates it.
func TestNoStateDirectoryWithoutARemoteSource(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if _, err := Sources(func(k string) string {
		if k == statedir.Override {
			return state
		}
		return ""
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Error("the state directory was created")
	}
}
