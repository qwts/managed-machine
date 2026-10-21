package software

import (
	"embed"
	"os"

	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/source"
)

// The built-in catalog is a small illustrative subset for S1 discovery
// (migration plan F06). Its platforms are declared support, not tested
// provider support; the real catalog arrives through the F05 conversion.
//
//go:embed catalog/*.yaml
var builtin embed.FS

// SourceDirEnv names one explicitly configured local definition source.
// It is read only when set; there is no implicit refresh and no remote
// transport in S1.
const SourceDirEnv = "MANAGED_MACHINE_SOURCE_DIR"

// LoadCatalog returns the built-in catalog merged with the optional local
// source named by getenv(SourceDirEnv). An id defined in both is a conflict.
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
	entries, err := source.Catalog(snaps...)
	return entries, reg, err
}
