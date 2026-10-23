// Package extension loads extension manifests (schema v1, protocol
// mm-extension-1; ADR-0013). Discovery reads the manifest as data and
// verifies pinned files by digest. It never starts the extension: the only
// process an extension ever becomes is one the protocol client starts
// explicitly, after discovery and verification.
package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/protocol"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/target"
)

const (
	// SchemaVersion is the only manifest format version accepted.
	SchemaVersion = 1
	// Protocol is the only extension protocol version accepted.
	Protocol = "mm-extension-1"
	// ManifestFile is the manifest's name at the extension root.
	ManifestFile = "manifest.yaml"
)

// Capabilities an extension may declare. Each one must be backed by what
// the manifest provides, so a declaration is never an unbacked claim.
var capabilities = []string{"provider", "skills", "status-section", "plan-operation"}

// Manifest is a validated manifest. Field order is normalized key order.
type Manifest struct {
	SchemaVersion int          `json:"schema_version"`
	Name          string       `json:"name"`
	Version       string       `json:"version"`
	Protocol      string       `json:"protocol"`
	Capabilities  []string     `json:"capabilities"`
	Executables   []Executable `json:"executables"`
	Provides      Provides     `json:"provides"`
}

// Executable is the pinned entry point for one target.
type Executable struct {
	Platform string `json:"platform" yaml:"platform"`
	Path     string `json:"path" yaml:"path"`
	SHA256   string `json:"sha256" yaml:"sha256"`
}

// Provides lists what the extension contributes.
type Provides struct {
	Providers    []ProvidedProvider `json:"providers" yaml:"providers"`
	SkillBundles []SkillBundle      `json:"skill_bundles" yaml:"skill_bundles"`
	Assets       []Asset            `json:"assets" yaml:"assets"`
}

// ProvidedProvider is a provider the extension implements, declared with
// its option schema so definitions using it validate without running it.
type ProvidedProvider struct {
	Name    string                     `json:"name" yaml:"name"`
	Options map[string]registry.Option `json:"options" yaml:"options"`
}

// SkillBundle names a bundle and the capabilities its guidance covers.
type SkillBundle struct {
	ID     string   `json:"id" yaml:"id"`
	Covers []string `json:"covers" yaml:"covers"`
}

// Asset is a pinned non-executable file shipped with the extension.
type Asset struct {
	Path   string `json:"path" yaml:"path"`
	SHA256 string `json:"sha256" yaml:"sha256"`
}

type rawManifest struct {
	SchemaVersion *int         `yaml:"schema_version"`
	Name          string       `yaml:"name"`
	Version       string       `yaml:"version"`
	Protocol      string       `yaml:"protocol"`
	Capabilities  []string     `yaml:"capabilities"`
	Executables   []Executable `yaml:"executables"`
	Provides      Provides     `yaml:"provides"`
}

// Parse validates manifest bytes. doc names the manifest in errors.
func Parse(doc string, data []byte) (Manifest, error) {
	var raw rawManifest
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Manifest{}, err
	}
	if raw.SchemaVersion == nil || *raw.SchemaVersion != SchemaVersion {
		return Manifest{}, schema.Errorf(schema.InvalidVersion, doc, "schema_version", "supported: %d", SchemaVersion)
	}
	if !schema.ValidID(raw.Name) {
		return Manifest{}, schema.Errorf(schema.Invalid, doc, "name", "invalid extension name %q", raw.Name)
	}
	if !schema.ValidSemver(raw.Version) {
		return Manifest{}, schema.Errorf(schema.InvalidVersion, doc, "version", "%q is not MAJOR.MINOR.PATCH[-PRERELEASE]", raw.Version)
	}
	if raw.Protocol != Protocol {
		return Manifest{}, schema.Errorf(schema.InvalidVersion, doc, "protocol", "unsupported protocol %q; supported: %s", raw.Protocol, Protocol)
	}
	m := Manifest{SchemaVersion: SchemaVersion, Name: raw.Name, Version: raw.Version, Protocol: Protocol}

	caps := map[string]bool{}
	for _, c := range raw.Capabilities {
		known := false
		for _, k := range capabilities {
			known = known || c == k
		}
		if !known {
			return Manifest{}, schema.Errorf(schema.Invalid, doc, "capabilities", "unknown capability %q; supported: %s", c, strings.Join(capabilities, ", "))
		}
		if caps[c] {
			return Manifest{}, schema.Errorf(schema.Invalid, doc, "capabilities", "duplicate capability %q", c)
		}
		caps[c] = true
	}
	if len(caps) == 0 {
		return Manifest{}, schema.Errorf(schema.Invalid, doc, "capabilities", "at least one capability is required")
	}
	for _, c := range capabilities {
		if caps[c] {
			m.Capabilities = append(m.Capabilities, c)
		}
	}

	if len(raw.Executables) == 0 {
		return Manifest{}, schema.Errorf(schema.Invalid, doc, "executables", "at least one per-target executable is required")
	}
	byPlat := map[target.Target]Executable{}
	for i, e := range raw.Executables {
		f := "executables[" + strconv.Itoa(i) + "]"
		t, err := target.Parse(e.Platform)
		if err != nil {
			return Manifest{}, schema.Errorf(schema.Invalid, doc, f+".platform", "%v", err)
		}
		if _, dup := byPlat[t]; dup {
			return Manifest{}, schema.Errorf(schema.Conflict, doc, f+".platform", "duplicate executable for %s", t)
		}
		if err := checkPinned(doc, f, e.Path, e.SHA256); err != nil {
			return Manifest{}, err
		}
		byPlat[t] = e
	}
	for _, t := range target.All() {
		if e, ok := byPlat[t]; ok {
			m.Executables = append(m.Executables, e)
		}
	}

	m.Provides.Providers = []ProvidedProvider{}
	seen := map[string]bool{}
	for i, p := range raw.Provides.Providers {
		f := "provides.providers[" + strconv.Itoa(i) + "]"
		if !schema.ValidID(p.Name) {
			return Manifest{}, schema.Errorf(schema.Invalid, doc, f+".name", "invalid provider name %q", p.Name)
		}
		if seen[p.Name] {
			return Manifest{}, schema.Errorf(schema.Conflict, doc, f+".name", "duplicate provider %q", p.Name)
		}
		seen[p.Name] = true
		if p.Options == nil {
			p.Options = map[string]registry.Option{}
		}
		// Validate the option schema now so a bad manifest fails at
		// discovery, not when the first definition uses the provider.
		if err := registry.New().Register(doc, registry.Provider{Name: p.Name, Options: p.Options}); err != nil {
			return Manifest{}, err
		}
		m.Provides.Providers = append(m.Provides.Providers, p)
	}
	sort.Slice(m.Provides.Providers, func(a, b int) bool { return m.Provides.Providers[a].Name < m.Provides.Providers[b].Name })

	m.Provides.SkillBundles = []SkillBundle{}
	seen = map[string]bool{}
	for i, b := range raw.Provides.SkillBundles {
		f := "provides.skill_bundles[" + strconv.Itoa(i) + "]"
		if !schema.ValidID(b.ID) {
			return Manifest{}, schema.Errorf(schema.Invalid, doc, f+".id", "invalid bundle id %q", b.ID)
		}
		if seen[b.ID] {
			return Manifest{}, schema.Errorf(schema.Conflict, doc, f+".id", "duplicate bundle %q", b.ID)
		}
		seen[b.ID] = true
		// ADR-0015: a bundle serves guidance only about its own
		// extension's declared capabilities.
		if len(b.Covers) == 0 {
			return Manifest{}, schema.Errorf(schema.Invalid, doc, f+".covers", "at least one covered capability is required")
		}
		covers := append([]string(nil), b.Covers...)
		sort.Strings(covers)
		for j, c := range covers {
			if !schema.ValidID(c) || (j > 0 && covers[j-1] == c) {
				return Manifest{}, schema.Errorf(schema.Invalid, doc, f+".covers", "invalid or duplicate entry %q", c)
			}
		}
		m.Provides.SkillBundles = append(m.Provides.SkillBundles, SkillBundle{ID: b.ID, Covers: covers})
	}
	sort.Slice(m.Provides.SkillBundles, func(a, b int) bool { return m.Provides.SkillBundles[a].ID < m.Provides.SkillBundles[b].ID })

	m.Provides.Assets = []Asset{}
	seen = map[string]bool{}
	for i, a := range raw.Provides.Assets {
		f := "provides.assets[" + strconv.Itoa(i) + "]"
		if err := checkPinned(doc, f, a.Path, a.SHA256); err != nil {
			return Manifest{}, err
		}
		if seen[a.Path] {
			return Manifest{}, schema.Errorf(schema.Conflict, doc, f+".path", "duplicate asset %q", a.Path)
		}
		seen[a.Path] = true
		m.Provides.Assets = append(m.Provides.Assets, a)
	}
	sort.Slice(m.Provides.Assets, func(a, b int) bool { return m.Provides.Assets[a].Path < m.Provides.Assets[b].Path })

	if caps["provider"] != (len(m.Provides.Providers) > 0) {
		return Manifest{}, schema.Errorf(schema.Invalid, doc, "capabilities", "the provider capability and provides.providers must be declared together")
	}
	if caps["skills"] != (len(m.Provides.SkillBundles) > 0) {
		return Manifest{}, schema.Errorf(schema.Invalid, doc, "capabilities", "the skills capability and provides.skill_bundles must be declared together")
	}
	return m, nil
}

// checkPinned requires a clean relative slash path inside the extension
// root and a SHA-256 digest.
func checkPinned(doc, f, p, sum string) error {
	if p == "" || path.IsAbs(p) || strings.Contains(p, `\`) || path.Clean(p) != p || p == "." || strings.HasPrefix(p, "../") || p == ".." || p == ManifestFile {
		return schema.Errorf(schema.Invalid, doc, f+".path", "%q must be a clean relative path inside the extension", p)
	}
	if !schema.ValidSHA256(sum) {
		return schema.Errorf(schema.Invalid, doc, f+".sha256", "must be 64 lowercase hex characters")
	}
	return nil
}

// Discover reads and validates the manifest at the root of fsys. It takes
// an fs.FS on purpose: discovery has no way to start a process.
func Discover(fsys fs.FS, doc string) (Manifest, error) {
	data, err := fs.ReadFile(fsys, ManifestFile)
	if err != nil {
		return Manifest{}, schema.Errorf(schema.Invalid, doc, "", "read manifest: %v", err)
	}
	return Parse(doc, data)
}

// Verify checks the executable for t and every asset against their pinned
// digests by reading the bytes. It returns the executable entry for t.
func Verify(fsys fs.FS, doc string, m Manifest, t target.Target) (Executable, error) {
	var exe *Executable
	for i := range m.Executables {
		if m.Executables[i].Platform == string(t) {
			exe = &m.Executables[i]
		}
	}
	if exe == nil {
		return Executable{}, schema.Errorf(schema.Invalid, doc, "executables", "no executable for %s", t)
	}
	if err := verifyFile(fsys, doc, exe.Path, exe.SHA256); err != nil {
		return Executable{}, err
	}
	for _, a := range m.Provides.Assets {
		if err := verifyFile(fsys, doc, a.Path, a.SHA256); err != nil {
			return Executable{}, err
		}
	}
	return *exe, nil
}

func verifyFile(fsys fs.FS, doc, p, want string) error {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return schema.Errorf(schema.DigestMismatch, doc, p, "read: %v", err)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return schema.Errorf(schema.DigestMismatch, doc, p, "sha256 %s, manifest pins %s", got, want)
	}
	return nil
}

// RegisterProviders adds the manifest's providers to reg with the
// extension as their origin. A name the registry already holds, built-in
// or from another extension, is a conflict.
func RegisterProviders(reg *registry.Registry, m Manifest) error {
	for _, p := range m.Provides.Providers {
		if err := reg.Register("extension:"+m.Name+"@"+m.Version, registry.Provider{Name: p.Name, Options: p.Options}); err != nil {
			return err
		}
	}
	return nil
}

// Description is the result of the describe method: what the running
// extension says it is. It must match the manifest it was discovered by.
type Description struct {
	Name      string   `json:"name"`
	Version   string   `json:"version"`
	Providers []string `json:"providers"`
}

// Describe runs the verified executable once with the describe method and
// checks that the running extension is the one its manifest pins.
func Describe(ctx context.Context, argv, env []string, m Manifest) (Description, error) {
	resp, err := protocol.Call(ctx, argv, env, 10*time.Second, protocol.Request{ID: "describe-1", Method: "describe"})
	if err != nil {
		return Description{}, err
	}
	if resp.Error != nil {
		return Description{}, fmt.Errorf("extension %s: describe failed: %s: %s", m.Name, resp.Error.Code, resp.Error.Message)
	}
	dec := json.NewDecoder(bytes.NewReader(resp.Result))
	dec.DisallowUnknownFields()
	var d Description
	if err := dec.Decode(&d); err != nil {
		return Description{}, fmt.Errorf("%w: describe result: %v", protocol.ErrViolation, err)
	}
	want := make([]string, 0, len(m.Provides.Providers))
	for _, p := range m.Provides.Providers {
		want = append(want, p.Name)
	}
	got := append([]string(nil), d.Providers...)
	sort.Strings(got)
	if d.Name != m.Name || d.Version != m.Version || strings.Join(got, ",") != strings.Join(want, ",") {
		return Description{}, fmt.Errorf("extension %s: running %s@%s providing [%s] does not match manifest %s@%s providing [%s]",
			m.Name, d.Name, d.Version, strings.Join(got, ","), m.Name, m.Version, strings.Join(want, ","))
	}
	return d, nil
}
