package software

import (
	"github.com/qwts/managed-machine/core/internal/catalog"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/source"
)

// LoadCatalog returns the built-in catalog merged with the optional local
// source named by getenv(catalog.SourceDirEnv) and with the cached
// snapshots of the configured remote sources. An id defined in two sources
// is a conflict.
func LoadCatalog(getenv func(string) string) ([]source.Entry, *registry.Registry, error) {
	reg := registry.Builtin()
	sources, err := catalog.Sources(getenv)
	if err != nil {
		return nil, nil, err
	}
	snaps := make([]source.Snapshot, 0, len(sources))
	for _, s := range sources {
		snap, err := source.Load(s.Identity, s.FS, s.Dir, reg)
		if err != nil {
			return nil, nil, err
		}
		snaps = append(snaps, snap)
	}
	entries, err := source.Catalog(snaps...)
	return entries, reg, err
}
