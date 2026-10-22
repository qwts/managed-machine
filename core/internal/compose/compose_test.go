package compose

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/source"
)

const validFlavor = `kind: flavor
schema_version: 1
id: work
description: Work environment with developer tools and git configuration.
categories: [devtools, work]
software:
  - id: ripgrep
    target: user
    version: 14.1.0
  - id: jq
    target: global
config:
  - id: shell
    target: user
  - id: gitconfig
    target: custom
    inputs:
      name: Alice
`

// A flavor definition normalizes its categories, software entries and
// configuration entries to canonical order, so declaration order never
// changes the normalized bytes.
func TestParseNormalizesOrder(t *testing.T) {
	fl, err := Parse("work.yaml", []byte(validFlavor))
	if err != nil {
		t.Fatal(err)
	}
	if fl.ID != "work" || fl.SchemaVersion != SchemaVersion {
		t.Fatalf("flavor %+v", fl)
	}
	if got := strings.Join(fl.Categories, ","); got != "devtools,work" {
		t.Errorf("categories %q", got)
	}
	if len(fl.Software) != 2 || fl.Software[0].ID != "jq" || fl.Software[1].ID != "ripgrep" || fl.Software[1].Version != "14.1.0" {
		t.Errorf("software not in canonical order: %+v", fl.Software)
	}
	if len(fl.Config) != 2 || fl.Config[0].ID != "gitconfig" || fl.Config[0].Target != "custom" || fl.Config[0].Inputs["name"] != "Alice" || fl.Config[1].ID != "shell" {
		t.Errorf("config not in canonical order: %+v", fl.Config)
	}

	reordered := `kind: flavor
schema_version: 1
id: work
description: Work environment with developer tools and git configuration.
categories: [work, devtools]
software:
  - id: jq
    target: global
  - id: ripgrep
    target: user
    version: 14.1.0
config:
  - id: gitconfig
    target: custom
    inputs:
      name: Alice
  - id: shell
    target: user
`
	srcID := source.Identity{Kind: "local", Location: "fixtures/local"}
	s1, err := Load(srcID, fstest.MapFS{"work.yaml": {Data: []byte(validFlavor)}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Load(srcID, fstest.MapFS{"work.yaml": {Data: []byte(reordered)}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	// Compare normalized flavor payload (file digests differ because raw YAML order differs).
	s2.Entries[0].Revision = s1.Entries[0].Revision
	s2.Entries[0].Digest = s1.Entries[0].Digest
	b1, err := Normalize(s1.Entries)
	if err != nil {
		t.Fatal(err)
	}
	b2, err := Normalize(s2.Entries)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Errorf("normalized flavor differs by declaration order:\n%s\n%s", b1, b2)
	}
}

// Every validation failure names the document and field; a flavor entry
// naming a custom target with a root or an unknown target is rejected.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		kind schema.Kind
		want string
	}{
		{"custom software target with colon root", sub("target: user", "target: custom:/opt/soul"), schema.Invalid, "must not name a root"},
		{"custom software target with root field", sub("target: user", "target: custom\n    root: /opt/soul"), schema.Invalid, "must not name a root"},
		{"custom config target with colon root", sub("target: custom", "target: custom:/opt/soul"), schema.Invalid, "must not name a root"},
		{"custom config target with root field", sub("target: custom", "target: custom\n    root: /opt/soul"), schema.Invalid, "must not name a root"},
		{"unknown software target", sub("target: user", "target: system"), schema.Invalid, `unknown installation target "system"`},
		{"unknown config target", sub("target: custom", "target: machine"), schema.Invalid, `unknown installation target "machine"`},
		{"missing software target", sub("    target: user\n", ""), schema.Invalid, "software[0].target: missing"},
		{"missing config target", sub("    target: custom\n", ""), schema.Invalid, "config[1].target: missing"},
		{"invalid software id", sub("id: ripgrep", "id: Rip_Grep"), schema.Invalid, `invalid software id "Rip_Grep"`},
		{"invalid config id", sub("id: shell", "id: Shell_Config"), schema.Invalid, `invalid configuration id "Shell_Config"`},
		{"invalid config input name", sub("name: Alice", "Bad_Key: Alice"), schema.Invalid, `invalid input name "Bad_Key"`},
		{"blank version", sub("version: 14.1.0", `version: "   "`), schema.Invalid, "must not be blank"},
		{"missing kind", sub("kind: flavor\n", ""), schema.Invalid, "declares kind: flavor"},
		{"wrong kind", sub("kind: flavor", "kind: config"), schema.Invalid, "declares kind: flavor"},
		{"invalid flavor id", sub("id: work", "id: Work_Flavor"), schema.Invalid, `invalid flavor id "Work_Flavor"`},
		{"missing description", sub("description: Work environment with developer tools and git configuration.\n", ""), schema.Invalid, "description: missing"},
		{"duplicate category", sub("[devtools, work]", "[work, work]"), schema.Invalid, `duplicate "work"`},
		{"unknown top-level field", sub("id: work", "id: work\nhomepage: https://example.invalid"), schema.UnknownField, "homepage"},
		{"future schema version", sub("schema_version: 1", "schema_version: 2"), schema.InvalidVersion, "unsupported version 2"},
		{"missing schema version", sub("schema_version: 1\n", ""), schema.InvalidVersion, "schema_version: missing"},
		{"yaml anchor", sub("categories: [devtools, work]", "categories: &c [devtools, work]"), schema.Invalid, "anchors"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := c.edit(validFlavor)
			if doc == validFlavor {
				t.Fatal("edit did not change the document")
			}
			_, err := Parse("builtin/work.yaml", []byte(doc))
			if err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
			if !errors.Is(err, schema.Err(c.kind)) {
				t.Errorf("kind: got %v, want %s", err, c.kind)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("message %q does not mention %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), "builtin/work.yaml: ") {
				t.Errorf("message %q does not name the document", err)
			}
		})
	}
}

// The flavor loader ignores software and configuration definitions in the
// same source, enforces file-name matching, and rejects duplicate flavor
// ids across sources.
func TestCatalogLoadAndConflict(t *testing.T) {
	id1 := source.Identity{Kind: "local", Location: "fixtures/one"}
	id2 := source.Identity{Kind: "local", Location: "fixtures/two"}
	fs1 := fstest.MapFS{
		"work.yaml": {Data: []byte(validFlavor)},
		"shell.yaml": {Data: []byte(`kind: config
schema_version: 1
id: shell
description: Shell config.
targets: [user]
operations:
  - id: rc
    provider: file
    destination: rc
    source: files/rc
`)},
		"ripgrep.yaml": {Data: []byte(`schema_version: 1
id: ripgrep
description: Search tool.
recipes:
  - id: homebrew
    provider: brew
    package: ripgrep
    platforms: [darwin/arm64]
    targets: [global]
    options:
      formula: ripgrep
`)},
	}
	snap1, err := Load(id1, fs1, ".")
	if err != nil {
		t.Fatal(err)
	}
	if len(snap1.Entries) != 1 || snap1.Entries[0].Flavor.ID != "work" {
		t.Fatalf("flavor snapshot entries: %+v", snap1.Entries)
	}

	// File name mismatch is rejected.
	if _, err := Load(id1, fstest.MapFS{"other.yaml": {Data: []byte(validFlavor)}}, "."); err == nil || !strings.Contains(err.Error(), "file name must match the id") {
		t.Errorf("mismatched file name: %v", err)
	}

	// Duplicate flavor id across two sources is a conflict.
	snap2, err := Load(id2, fstest.MapFS{"work.yaml": {Data: []byte(validFlavor)}}, ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Catalog(snap1, snap2); !errors.Is(err, schema.Err(schema.Conflict)) || !strings.Contains(err.Error(), `flavor "work" is already defined by fixtures/one/work.yaml`) {
		t.Errorf("duplicate flavor id across sources: %v", err)
	}
}

// Pure composer tests:
//   - duplicate entries dedupe (both software and config)
//   - differing version strings for one software:<id>@<target> conflict
//   - the same software id at two targets does not conflict
//   - two configs resolving to one destination with different values
//     conflict as config:<id>@<target>:<operation>
//   - identical destination-and-value pairs dedupe without conflict
func TestPureComposer(t *testing.T) {
	t.Run("duplicate entries dedupe", func(t *testing.T) {
		f1 := Flavor{
			ID: "base",
			Software: []SoftwareEntry{
				{ID: "ripgrep", Target: "user", Version: "14.1.0"},
				{ID: "ripgrep", Target: "user", Version: "14.1.0"},
				{ID: "jq", Target: "user"},
			},
			Config: []ConfigEntry{
				{ID: "gitconfig", Target: "user"},
			},
		}
		f2 := Flavor{
			ID: "work",
			Software: []SoftwareEntry{
				{ID: "ripgrep", Target: "user"},
				{ID: "jq", Target: "user", Version: "1.7.1"},
			},
			Config: []ConfigEntry{
				{ID: "gitconfig", Target: "user"},
			},
		}
		ops := []ConfigOperation{
			{Config: "gitconfig", Target: "user", Operation: "main", Destination: "/tmp/home/.gitconfig", Value: "[user]\nname = Alice\n"},
			{Config: "gitconfig", Target: "user", Operation: "main", Destination: "/tmp/home/.gitconfig", Value: "[user]\nname = Alice\n"},
		}
		res := Compose([]Flavor{f1, f2}, ops)
		if len(res.Conflicts) != 0 {
			t.Fatalf("unexpected conflicts: %+v", res.Conflicts)
		}
		if len(res.Software) != 2 {
			t.Fatalf("want 2 deduplicated software entries, got %+v", res.Software)
		}
		if res.Software[0] != (SoftwareEntry{ID: "jq", Target: "user", Version: "1.7.1"}) ||
			res.Software[1] != (SoftwareEntry{ID: "ripgrep", Target: "user", Version: "14.1.0"}) {
			t.Errorf("software entries: %+v", res.Software)
		}
		if len(res.Config) != 1 || res.Config[0].ID != "gitconfig" {
			t.Errorf("config entries: %+v", res.Config)
		}
		if len(res.Operations) != 1 || res.Operations[0].Identity() != "config:gitconfig@user:main" {
			t.Errorf("operations: %+v", res.Operations)
		}
	})

	t.Run("differing version strings for one software:<id>@<target> conflict", func(t *testing.T) {
		f1 := Flavor{
			ID:       "one",
			Software: []SoftwareEntry{{ID: "ripgrep", Target: "user", Version: "14.1.0"}},
		}
		f2 := Flavor{
			ID:       "two",
			Software: []SoftwareEntry{{ID: "ripgrep", Target: "user", Version: "13.0.0"}},
		}
		res := Compose([]Flavor{f1, f2}, nil)
		if len(res.Conflicts) != 1 {
			t.Fatalf("want 1 conflict, got %+v", res.Conflicts)
		}
		c, ok := res.ConflictFor("software:ripgrep@user")
		if !ok {
			t.Fatalf("missing conflict for software:ripgrep@user: %+v", res.Conflicts)
		}
		if c.Kind != KindSoftware || c.Target != "user" || strings.Join(c.Values, ",") != "13.0.0,14.1.0" {
			t.Errorf("conflict: %+v", c)
		}
	})

	t.Run("same software id at two targets does not conflict", func(t *testing.T) {
		f1 := Flavor{
			ID:       "one",
			Software: []SoftwareEntry{{ID: "ripgrep", Target: "user", Version: "14.1.0"}},
		}
		f2 := Flavor{
			ID: "two",
			Software: []SoftwareEntry{
				{ID: "ripgrep", Target: "global", Version: "13.0.0"},
				{ID: "ripgrep", Target: "custom", Version: "12.0.0"},
			},
		}
		res := Compose([]Flavor{f1, f2}, nil)
		if len(res.Conflicts) != 0 {
			t.Fatalf("unexpected conflicts across targets: %+v", res.Conflicts)
		}
		if len(res.Software) != 3 {
			t.Fatalf("want 3 target-scoped software entries, got %+v", res.Software)
		}
		if res.Software[0].Target != "global" || res.Software[1].Target != "user" || res.Software[2].Target != "custom" {
			t.Errorf("software target order: %+v", res.Software)
		}
	})

	t.Run("two configs resolving to one destination with different values conflict as config:<id>@<target>:<operation>", func(t *testing.T) {
		ops := []ConfigOperation{
			{Config: "git-personal", Target: "user", Operation: "gitconfig", Destination: "/tmp/home/.gitconfig", Value: "[user]\nname = Personal\n"},
			{Config: "git-work", Target: "user", Operation: "gitconfig", Destination: "/tmp/home/.gitconfig", Value: "[user]\nname = Work\n"},
		}
		res := Compose(nil, ops)
		if len(res.Conflicts) != 2 {
			t.Fatalf("want 2 per-identity conflicts, got %+v", res.Conflicts)
		}
		c1, ok1 := res.ConflictFor("config:git-personal@user:gitconfig")
		c2, ok2 := res.ConflictFor("config:git-work@user:gitconfig")
		if !ok1 || !ok2 {
			t.Fatalf("missing expected config conflicts: %+v", res.Conflicts)
		}
		if c1.Destination != "/tmp/home/.gitconfig" || len(c1.With) != 1 || c1.With[0] != "config:git-work@user:gitconfig" {
			t.Errorf("conflict 1: %+v", c1)
		}
		if c2.Destination != "/tmp/home/.gitconfig" || len(c2.With) != 1 || c2.With[0] != "config:git-personal@user:gitconfig" {
			t.Errorf("conflict 2: %+v", c2)
		}
	})

	t.Run("two configs resolving to one destination with identical values dedupe", func(t *testing.T) {
		ops := []ConfigOperation{
			{Config: "git-personal", Target: "user", Operation: "gitconfig", Destination: "/tmp/home/.gitconfig", Value: "[user]\nname = Shared\n"},
			{Config: "git-work", Target: "user", Operation: "gitconfig", Destination: "/tmp/home/.gitconfig", Value: "[user]\nname = Shared\n"},
		}
		res := Compose(nil, ops)
		if len(res.Conflicts) != 0 {
			t.Fatalf("identical destination-and-value pair conflicted: %+v", res.Conflicts)
		}
		if len(res.Operations) != 1 {
			t.Fatalf("want 1 deduplicated operation, got %+v", res.Operations)
		}
	})
}

func sub(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}
