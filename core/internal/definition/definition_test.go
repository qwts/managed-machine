package definition

import (
	"errors"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/schema"
)

const valid = `schema_version: 1
id: ruff
description: A linter.
categories: [python, devtools]
recipes:
  - id: uv-tool
    provider: uv
    package: ruff
    platforms: [windows/amd64, darwin/arm64]
    targets: [custom, user]
    requires:
      - software: uv
    options:
      tool: ruff
`

func TestParseNormalizesOrder(t *testing.T) {
	sw, err := Parse("ruff.yaml", []byte(valid), registry.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	r := sw.Recipes[0]
	if got := strings.Join(sw.Categories, ","); got != "devtools,python" {
		t.Errorf("categories %q", got)
	}
	if got := strings.Join(r.Platforms, ","); got != "darwin/arm64,windows/amd64" {
		t.Errorf("platforms not in canonical target order: %q", got)
	}
	if got := strings.Join(r.Targets, ","); got != "user,custom" {
		t.Errorf("targets not in canonical order: %q", got)
	}
	if r.Options["tool"] != "ruff" || len(r.Requires) != 1 || r.Requires[0].Software != "uv" {
		t.Errorf("recipe %+v", r)
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		kind schema.Kind
		want string
	}{
		{"unknown provider", sub("provider: uv", "provider: pip"), schema.UnknownProvider, `unknown provider "pip"`},
		{"unknown option", sub("tool: ruff", "tool: ruff\n      shell: rm -rf /"), schema.UnknownOption, `no option "shell"`},
		{"missing required option", sub("    options:\n      tool: ruff\n", "    options: {}\n"), schema.Invalid, "required by provider"},
		{"unknown top-level field", sub("id: ruff", "id: ruff\nhomepage: https://example.invalid"), schema.UnknownField, "homepage"},
		{"unknown recipe field", sub("package: ruff", "package: ruff\n    script: install.sh"), schema.UnknownField, "script"},
		{"future schema version", sub("schema_version: 1", "schema_version: 2"), schema.InvalidVersion, "unsupported version 2"},
		{"missing schema version", sub("schema_version: 1\n", ""), schema.InvalidVersion, "missing"},
		{"string schema version", sub("schema_version: 1", `schema_version: "1"`), schema.Invalid, "cannot unmarshal"},
		{"platform alias", sub("darwin/arm64", "macos/arm64"), schema.Invalid, `unknown target "macos/arm64"`},
		{"legacy linux spelling", sub("darwin/arm64", "linux/x86_64"), schema.Invalid, `unknown target "linux/x86_64"`},
		{"duplicate platform", sub("[windows/amd64, darwin/arm64]", "[darwin/arm64, darwin/arm64]"), schema.Invalid, "duplicate platform"},
		{"unknown install target", sub("[custom, user]", "[system]"), schema.Invalid, `unknown installation target "system"`},
		{"invalid recipe id", sub("id: uv-tool", "id: UV Tool"), schema.Invalid, "invalid recipe id"},
		{"duplicate recipe id", dupRecipe, schema.Conflict, `duplicate recipe id "uv-tool"`},
		{"option of another provider", sub("provider: uv", "provider: brew"), schema.UnknownOption, `provider "brew" has no option "tool"`},
		{"self requirement", sub("software: uv", "software: ruff"), schema.Invalid, "requires itself"},
		{"invalid id", sub("id: ruff", "id: Ruff_Linter"), schema.Invalid, "invalid software id"},
		{"no recipes", func(s string) string { return s[:strings.Index(s, "recipes:")] + "recipes: []\n" }, schema.Invalid, "at least one recipe"},
		{"anchor", sub("categories: [python, devtools]", "categories: &c [python, devtools]"), schema.Invalid, "anchors"},
		{"explicit tag", sub("tool: ruff", "tool: !!str ruff"), schema.Invalid, "tags"},
		{"two documents", func(s string) string { return s + "---\n" + s }, schema.Invalid, "more than one YAML document"},
		{"duplicate key", sub("id: ruff", "id: ruff\nid: ruff2"), schema.Invalid, "already defined"},
		{"empty", func(string) string { return "" }, schema.Invalid, "empty document"},
		{"list option as scalar", nil, schema.Invalid, "list of strings"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := valid
			if c.edit != nil {
				doc = c.edit(valid)
			} else {
				doc = strings.NewReplacer("provider: uv", "provider: download", "id: uv-tool", "id: download",
					"tool: ruff", "url: https://example.invalid/ruff.tgz\n      url_hosts: example.invalid").Replace(valid)
			}
			if doc == valid {
				t.Fatal("edit did not change the document")
			}
			_, err := Parse("ruff.yaml", []byte(doc), registry.Builtin())
			if err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
			if !errors.Is(err, schema.Err(c.kind)) {
				t.Errorf("kind: got %v, want %s", err, c.kind)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("message %q does not mention %q", err, c.want)
			}
		})
	}
}

func TestBoolOptionMustBeBool(t *testing.T) {
	// The registry holds the fixture provider alone: the built-in one is
	// registered under the same name.
	reg := registry.New()
	if err := reg.Register("test", registry.Provider{Name: "cargo", Options: map[string]registry.Option{
		"crate": {Type: registry.String, Required: true}, "locked": {Type: registry.Bool}}}); err != nil {
		t.Fatal(err)
	}
	base := strings.NewReplacer("provider: uv", "provider: cargo", "id: uv-tool", "id: cargo", "tool: ruff", "crate: ruff\n      locked: LOCKED").Replace(valid)
	if _, err := Parse("ruff.yaml", []byte(strings.Replace(base, "LOCKED", "true", 1)), reg); err != nil {
		t.Fatalf("bool true rejected: %v", err)
	}
	for _, v := range []string{`"true"`, "yes", "1"} {
		if _, err := Parse("ruff.yaml", []byte(strings.Replace(base, "LOCKED", v, 1)), reg); err == nil {
			t.Errorf("locked: %s accepted as a bool", v)
		}
	}
}

func sub(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}

func dupRecipe(s string) string {
	i := strings.Index(s, "  - id:")
	return s + s[i:]
}
