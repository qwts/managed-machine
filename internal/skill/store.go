package skill

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/extension"
	"github.com/qwts/managed-machine/internal/target"
)

//go:embed core
var coreFS embed.FS

// ExtensionsDirEnv names the directory of installed extensions, one per
// subdirectory with a manifest.yaml at its root. It is the S1 stand-in for
// the installed-extension registry; nothing is fetched or executed.
const ExtensionsDirEnv = "MANAGED_MACHINE_EXTENSIONS_DIR"

// Core loads the built-in bundle.
func Core() (Bundle, error) {
	sub, err := fs.Sub(coreFS, "core")
	if err != nil {
		return Bundle{}, err
	}
	return Load(sub, "built-in")
}

// Extensions loads the bundles of every installed extension under dir for
// host target t. An extension is served only when its manifest declares
// the skills capability, its pinned executable for t and every asset match
// their digests, every bundle file is a pinned asset, and the bundle's
// guidance covers only what the extension declares. Discovery reads files;
// the extension's binary is never run.
func Extensions(dir string, t target.Target) ([]Bundle, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read extensions: %v", err)
	}
	var out []Bundle
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		root := os.DirFS(path.Join(dir, e.Name()))
		m, err := extension.Discover(root, "extension "+e.Name())
		if err != nil {
			return nil, err
		}
		if !has(m.Capabilities, "skills") {
			continue
		}
		if _, err := extension.Verify(root, "extension "+m.Name, m, t); err != nil {
			return nil, err
		}
		pinned := map[string]bool{}
		for _, a := range m.Provides.Assets {
			pinned[a.Path] = true
		}
		covered := map[string]bool{}
		for _, c := range m.Capabilities {
			covered[c] = true
		}
		for _, p := range m.Provides.Providers {
			covered[p.Name] = true
		}
		for _, sb := range m.Provides.SkillBundles {
			src := "extension:" + m.Name + "@" + m.Version
			dirName := "bundles/" + sb.ID
			sub, err := fs.Sub(root, dirName)
			if err != nil {
				return nil, err
			}
			err = fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.IsDir() && !pinned[dirName+"/"+p] {
					return fmt.Errorf("%s: bundle file %s is not a pinned asset of the manifest", src, dirName+"/"+p)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			b, err := Load(sub, src)
			if err != nil {
				return nil, err
			}
			if b.Name != sb.ID || b.Version != m.Version {
				return nil, fmt.Errorf("%s: bundle %s@%s does not match the manifest's %s@%s", src, b.Name, b.Version, sb.ID, m.Version)
			}
			for _, c := range sb.Covers {
				if !covered[c] {
					return nil, fmt.Errorf("%s: bundle %s covers %q, which the extension does not declare", src, sb.ID, c)
				}
			}
			for _, s := range b.Skills {
				if !coveredName(s.Name, sb.Covers) {
					return nil, fmt.Errorf("%s: skill %s is outside the bundle's coverage %v", src, s.Name, sb.Covers)
				}
			}
			out = append(out, b)
		}
	}
	return out, nil
}

// coveredName: a skill is named after something its bundle covers: the
// covered item itself, "<item>-<topic>", or a reference "<ns>:<item>".
func coveredName(name string, covers []string) bool {
	for _, c := range covers {
		if name == c || strings.HasPrefix(name, c+"-") || strings.HasSuffix(name, ":"+c) {
			return true
		}
	}
	return false
}

// Store is every served bundle, core first, then extensions by name.
type Store struct{ Bundles []Bundle }

// Open loads the core bundle and, when getenv(ExtensionsDirEnv) is set,
// the installed extensions' bundles. A bundle name served twice is an error.
func Open(getenv func(string) string, t target.Target) (Store, error) {
	core, err := Core()
	if err != nil {
		return Store{}, err
	}
	st := Store{Bundles: []Bundle{core}}
	if dir := getenv(ExtensionsDirEnv); dir != "" {
		ext, err := Extensions(dir, t)
		if err != nil {
			return Store{}, err
		}
		sort.Slice(ext, func(i, j int) bool { return ext[i].Name < ext[j].Name })
		st.Bundles = append(st.Bundles, ext...)
	}
	seen := map[string]bool{}
	for _, b := range st.Bundles {
		if seen[b.Name] {
			return Store{}, fmt.Errorf("bundle %s is served by two sources", b.Name)
		}
		seen[b.Name] = true
	}
	return st, nil
}
