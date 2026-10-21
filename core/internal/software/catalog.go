package software

import (
	"embed"
	"os"
	"runtime"

	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/source/remote"
	"github.com/qwts/managed-machine/core/internal/statedir"
)

// The built-in catalog is a small illustrative subset for S1 discovery
// (migration plan F06). Its platforms are declared support, not tested
// provider support; the real catalog arrives through the F05 conversion.
//
//go:embed catalog/*.yaml
var builtin embed.FS

// SourceDirEnv names one explicitly configured local definition source.
// It is read only when set; a local source is read where it is, and never
// fetched.
const SourceDirEnv = "MANAGED_MACHINE_SOURCE_DIR"

// LoadCatalog returns the built-in catalog merged with the optional local
// source named by getenv(SourceDirEnv) and with the cached snapshots of
// the configured remote sources. An id defined in two sources is a
// conflict.
func LoadCatalog(getenv func(string) string) ([]source.Entry, *registry.Registry, error) {
	reg := registry.Builtin()
	snaps := []source.Snapshot{}
	b, err := source.Load(source.Identity{Kind: "builtin", Location: "builtin"}, builtin, "catalog", reg)
	if err != nil {
		return nil, nil, err
	}
	snaps = append(snaps, b)
	if dir := getenv(SourceDirEnv); dir != "" {
		// The identity names the setting, not the path, so records and
		// diagnostics never carry a host path.
		l, err := source.Load(source.Identity{Kind: "local", Location: "env:" + SourceDirEnv}, os.DirFS(dir), ".", reg)
		if err != nil {
			return nil, nil, err
		}
		snaps = append(snaps, l)
	}
	if err := appendRemote(getenv, reg, &snaps); err != nil {
		return nil, nil, err
	}
	entries, err := source.Catalog(snaps...)
	return entries, reg, err
}

// appendRemote merges the cached snapshots of the configured remote
// sources. The state directory is computed only when a remote source is
// configured, so a machine without one never creates it, and a source
// that was never refreshed is an explicit error, never an implicit fetch.
func appendRemote(getenv func(string) string, reg *registry.Registry, snaps *[]source.Snapshot) error {
	cfg, err := remote.ParseConfig(getenv)
	if err != nil {
		return err
	}
	if !cfg.Configured() {
		return nil
	}
	dir, err := statedir.Path(runtime.GOOS, getenv)
	if err != nil {
		return err
	}
	cached, err := remote.Cached(cfg, dir)
	if err != nil {
		return err
	}
	for _, c := range cached {
		s, err := source.Load(c.Identity, c.FS, ".", reg)
		if err != nil {
			return err
		}
		*snaps = append(*snaps, s)
	}
	return nil
}
