// Package catalog loads the definition sources the core reads: the
// built-in catalog, the optional local source named by
// MANAGED_MACHINE_SOURCE_DIR and the cached snapshots of the configured
// remote sources. The built-in catalog is embedded, so discovery never
// reads a host path, and every source is named by an identity that never
// carries one.
//
// The catalog holds every kind of definition the core reads: software
// definitions and configuration definitions live side by side, and each
// loader picks the documents of its own kind.
package catalog

import (
	"embed"
	"io/fs"
	"os"
	"runtime"

	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/source/remote"
	"github.com/qwts/managed-machine/core/internal/statedir"
)

// The built-in catalog is a small illustrative subset of software and
// configuration definitions (migration plan F06). Its platforms are
// declared support, not tested provider support; the real catalog arrives
// through the F05 conversion.
//
//go:embed *.yaml
var builtin embed.FS

// SourceDirEnv names one explicitly configured local definition source.
// It is read only when set; a local source is read where it is, and never
// fetched.
const SourceDirEnv = "MANAGED_MACHINE_SOURCE_DIR"

// Source is one configured source: its identity and the file system and
// directory its files live in.
type Source struct {
	Identity source.Identity
	FS       fs.FS
	Dir      string
}

// Sources returns every configured source in a stable order: the built-in
// catalog, the local source when one is configured, then the cached
// snapshots of the configured remote sources. A configured remote source
// that was never refreshed is an error: fetching stays an explicit action,
// never an implicit one.
func Sources(getenv func(string) string) ([]Source, error) {
	out := []Source{{Identity: source.Identity{Kind: "builtin", Location: "builtin"}, FS: builtin, Dir: "."}}
	if dir := getenv(SourceDirEnv); dir != "" {
		// The identity names the setting, not the path, so records and
		// diagnostics never carry a host path.
		out = append(out, Source{Identity: source.Identity{Kind: "local", Location: "env:" + SourceDirEnv}, FS: os.DirFS(dir), Dir: "."})
	}
	cached, err := cachedSources(getenv)
	if err != nil {
		return nil, err
	}
	return append(out, cached...), nil
}

// cachedSources returns the cached snapshots of the configured remote
// sources. The state directory is computed only when a remote source is
// configured, so a machine without one never creates it.
func cachedSources(getenv func(string) string) ([]Source, error) {
	cfg, err := remote.ParseConfig(getenv)
	if err != nil {
		return nil, err
	}
	if !cfg.Configured() {
		return nil, nil
	}
	dir, err := statedir.Path(runtime.GOOS, getenv)
	if err != nil {
		return nil, err
	}
	cached, err := remote.Cached(cfg, dir)
	if err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(cached))
	for _, c := range cached {
		out = append(out, Source{Identity: c.Identity, FS: c.FS, Dir: "."})
	}
	return out, nil
}
