// Package skill serves bounded, structured, versioned skill guidance
// (docs cli/skill.md, ADR-0015). Guidance is data: reading it never runs
// anything, and nothing in this package starts a process.
package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qwts/managed-machine/internal/schema"
)

// Bounds accepted as the F07 initial defaults (2026-10-21, delegate;
// ADR-0015 amended). Changed only by recorded delta.
const (
	MaxResponseBytes = 4096     // every response, metadata included
	MaxSearchRecords = 50       // hits per search response
	MaxItemBytes     = 64 << 10 // largest servable content item
	BundleSchema     = 1        // bundle.yaml schema_version
	BundleFile       = "bundle.yaml"
)

// Kinds in their fixed order.
var Kinds = []string{"summary", "contract", "procedure", "reference", "example"}

// Skill is one validated skill of a bundle.
type Skill struct {
	Name       string
	Kinds      []string
	Caveats    []string
	References []string
	Content    map[string]string // kind -> Markdown body
}

// Bundle is a validated bundle.
type Bundle struct {
	Name     string
	Version  string
	Revision string // first 12 hex of SHA-256 over every bundle file
	Source   string // "built-in" or "extension:<name>@<version>"
	Skills   []Skill
}

type rawBundle struct {
	SchemaVersion *int       `yaml:"schema_version"`
	Name          string     `yaml:"name"`
	Version       string     `yaml:"version"`
	Skills        []rawSkill `yaml:"skills"`
}

type rawSkill struct {
	Name       string   `yaml:"name"`
	Kinds      []string `yaml:"kinds"`
	Caveats    []string `yaml:"caveats"`
	References []string `yaml:"references"`
}

// A skill name is an id, or a reference name "<namespace>:<id>" such as
// provider:brew. The colon maps to a directory level on disk, so names stay
// valid file paths on every target.
var skillName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*(:[a-z0-9]+(-[a-z0-9]+)*)?$`)

// Reserved names stay routed to the legacy CLI (`skill path`, F01).
var reserved = map[string]bool{"path": true}

func contentPath(name, kind string) string {
	return "skills/" + strings.ReplaceAll(name, ":", "/") + "/" + kind + ".md"
}

// Load reads and validates a bundle rooted at fsys. Every file under the
// root must be bundle.yaml or a declared content file, so nothing ships in
// a bundle unreviewed.
func Load(fsys fs.FS, source string) (Bundle, error) {
	doc := source
	data, err := fs.ReadFile(fsys, BundleFile)
	if err != nil {
		return Bundle{}, schema.Errorf(schema.Invalid, doc, "", "read %s: %v", BundleFile, err)
	}
	var raw rawBundle
	if err := schema.Decode(doc+"/"+BundleFile, data, &raw); err != nil {
		return Bundle{}, err
	}
	if raw.SchemaVersion == nil || *raw.SchemaVersion != BundleSchema {
		return Bundle{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "supported: %d", BundleSchema)
	}
	if !schema.ValidID(raw.Name) {
		return Bundle{}, schema.Errorf(schema.Invalid, doc, "name", "invalid bundle name %q", raw.Name)
	}
	if !schema.ValidSemver(raw.Version) {
		return Bundle{}, schema.Errorf(schema.InvalidVersion, doc, "version", "%q is not MAJOR.MINOR.PATCH[-PRERELEASE]", raw.Version)
	}
	b := Bundle{Name: raw.Name, Version: raw.Version, Source: source}
	want := map[string]bool{BundleFile: true}
	seen := map[string]bool{}
	for i, rs := range raw.Skills {
		f := fmt.Sprintf("skills[%d]", i)
		if !skillName.MatchString(rs.Name) || reserved[rs.Name] {
			return Bundle{}, schema.Errorf(schema.Invalid, doc, f+".name", "invalid or reserved skill name %q", rs.Name)
		}
		if seen[rs.Name] {
			return Bundle{}, schema.Errorf(schema.Conflict, doc, f+".name", "duplicate skill %q", rs.Name)
		}
		seen[rs.Name] = true
		kinds, err := orderedKinds(rs.Kinds)
		if err != nil {
			return Bundle{}, schema.Errorf(schema.Invalid, doc, f+".kinds", "%v", err)
		}
		s := Skill{Name: rs.Name, Kinds: kinds, Caveats: rs.Caveats, References: append([]string{}, rs.References...), Content: map[string]string{}}
		if s.Caveats == nil {
			s.Caveats = []string{}
		}
		for _, c := range s.Caveats {
			if strings.TrimSpace(c) == "" || strings.Contains(c, "\n") {
				return Bundle{}, schema.Errorf(schema.Invalid, doc, f+".caveats", "caveats are non-empty single lines")
			}
		}
		sort.Strings(s.References)
		for _, k := range kinds {
			p := contentPath(rs.Name, k)
			want[p] = true
			body, err := fs.ReadFile(fsys, p)
			if err != nil {
				return Bundle{}, schema.Errorf(schema.Invalid, doc, p, "declared kind %s has no content file", k)
			}
			if !utf8.Valid(body) {
				return Bundle{}, schema.Errorf(schema.Invalid, doc, p, "content is not UTF-8")
			}
			s.Content[k] = strings.TrimRight(string(body), "\n")
		}
		b.Skills = append(b.Skills, s)
	}
	for _, s := range b.Skills {
		for _, r := range s.References {
			ref := b.find(r)
			if ref == nil || !has(ref.Kinds, "reference") {
				return Bundle{}, schema.Errorf(schema.Invalid, doc, "skills."+s.Name+".references", "%q is not a reference entry of this bundle", r)
			}
		}
	}
	sort.Slice(b.Skills, func(i, j int) bool { return b.Skills[i].Name < b.Skills[j].Name })

	// Revision: every file, sorted by path, length-prefixed. Extra files
	// are rejected.
	var files []string
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !want[p] {
			return schema.Errorf(schema.Invalid, doc, p, "file is not bundle.yaml or a declared content file")
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return Bundle{}, err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, p := range files {
		data, _ := fs.ReadFile(fsys, p)
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(data))
		h.Write(data)
	}
	b.Revision = hex.EncodeToString(h.Sum(nil))[:12]

	// Mandatory caveats must fit the first response of every kind, or the
	// bundle can never serve that item: reject it at load.
	for _, s := range b.Skills {
		for _, k := range s.Kinds {
			if n := firstChunkFloor(b, s, k); n > MaxResponseBytes {
				return Bundle{}, schema.Errorf(schema.Invalid, doc, contentPath(s.Name, k),
					"mandatory caveats need %d bytes in the first response; the bound is %d", n, MaxResponseBytes)
			}
		}
	}
	return b, nil
}

func orderedKinds(in []string) ([]string, error) {
	set := map[string]bool{}
	for _, k := range in {
		if !has(Kinds, k) {
			return nil, fmt.Errorf("unknown kind %q; kinds: %s", k, strings.Join(Kinds, ", "))
		}
		if set[k] {
			return nil, fmt.Errorf("duplicate kind %q", k)
		}
		set[k] = true
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("at least one kind is required")
	}
	var out []string
	for _, k := range Kinds {
		if set[k] {
			out = append(out, k)
		}
	}
	return out, nil
}

func (b *Bundle) find(name string) *Skill {
	for i := range b.Skills {
		if b.Skills[i].Name == name {
			return &b.Skills[i]
		}
	}
	return nil
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// caveatBlock is the text every first chunk starts with.
func caveatBlock(s Skill) string {
	if len(s.Caveats) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Caveats:\n")
	for _, c := range s.Caveats {
		b.WriteString("- " + c + "\n")
	}
	if len(s.References) > 0 {
		b.WriteString("Must read: " + strings.Join(s.References, ", ") + "\n")
	}
	return b.String() + "\n"
}

// pathOf is exported for diagnostics: where an item lives in its bundle.
func pathOf(name, kind string) string { return path.Clean(contentPath(name, kind)) }
