package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/schema"
)

const validConfig = `kind: config
schema_version: 1
id: shell
description: Configure the interactive shell.
categories: [tools, shell]
platforms: [linux/amd64, darwin/arm64]
targets: [custom, user]
inputs:
  editor: vim
operations:
  - id: rc
    provider: managed-block
    destination: shell-rc
    block_id: managed-machine
    content: |
      export EDITOR={{ inputs.editor }}
activation:
  shell: /bin/sh
requires:
  - command: sh
`

// A definition normalizes to deterministic bytes: the lists are in
// canonical order, so the declaration order never changes the record.
func TestParseNormalizesOrder(t *testing.T) {
	def, err := Parse("shell.yaml", []byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(def.Categories, ","); got != "shell,tools" {
		t.Errorf("categories %q", got)
	}
	if got := strings.Join(def.Platforms, ","); got != "darwin/arm64,linux/amd64" {
		t.Errorf("platforms not in canonical target order: %q", got)
	}
	if got := strings.Join(def.Targets, ","); got != "user,custom" {
		t.Errorf("targets not in canonical order: %q", got)
	}
	if def.Inputs["editor"] != "vim" || def.Activation["shell"] != "/bin/sh" {
		t.Errorf("definition %+v", def)
	}
	if len(def.Requires) != 1 || def.Requires[0].Command != "sh" {
		t.Errorf("requires %+v", def.Requires)
	}
	op := def.Operations[0]
	if op.Provider != ProviderManagedBlock || op.Destination != "shell-rc" || op.Comment != DefaultComment {
		t.Errorf("operation %+v", op)
	}
	if !def.Supports("user", "darwin/arm64") || def.Supports("global", "darwin/arm64") {
		t.Error("the definition supports the wrong targets")
	}
}

// Every failure names the document and the field, so a diagnostic can
// point at the one thing that is wrong.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		kind schema.Kind
		want string
	}{
		{"unknown provider", sub("provider: managed-block", "provider: none"), schema.UnknownProvider, `unknown provider "none"`},
		{"duplicate operation id", dupOperation, schema.Invalid, `duplicate operation id "rc"`},
		{"missing format", sub("    provider: managed-block\n    destination: shell-rc\n    block_id: managed-machine\n    content: |\n      export EDITOR={{ inputs.editor }}\n",
			"    provider: structured\n    destination: shell-rc\n    keys:\n      editor: vim\n"), schema.Invalid, "a structured operation declares format"},
		{"unknown format", sub("    provider: managed-block\n    destination: shell-rc\n    block_id: managed-machine\n    content: |\n      export EDITOR={{ inputs.editor }}\n",
			"    provider: structured\n    destination: shell-rc\n    format: yaml\n"), schema.Invalid, `unknown format "yaml"`},
		{"empty key segment", sub("    provider: managed-block\n    destination: shell-rc\n    block_id: managed-machine\n    content: |\n      export EDITOR={{ inputs.editor }}\n",
			"    provider: structured\n    destination: shell-rc\n    format: json\n    keys:\n      a..b: value\n"), schema.Invalid, "empty segment"},
		{"token outside the namespaces", sub(`shell: /bin/sh`, `shell: "{{ secrets.token }}"`), schema.Invalid, "unknown namespace"},
		{"undeclared input", sub("export EDITOR={{ inputs.editor }}", "export EDITOR={{ inputs.nope }}"), schema.Invalid, `unknown inputs "nope"`},
		{"undeclared destination", sub("export EDITOR={{ inputs.editor }}", "source {{ destinations.nope }}"), schema.Invalid, `unknown destinations "nope"`},
		{"field of another provider", sub("    block_id: managed-machine", "    format: json"), schema.Invalid, `does not accept "format"`},
		{"unknown top-level field", sub("id: shell", "id: shell\nhomepage: https://example.invalid"), schema.UnknownField, "homepage"},
		{"missing kind", sub("kind: config\n", ""), schema.Invalid, "declares kind: config"},
		{"software kind", sub("kind: config", "kind: software"), schema.Invalid, "declares kind: config"},
		{"missing operations", func(s string) string { return s[:strings.Index(s, "operations:")] }, schema.Invalid, "at least one operation"},
		{"asset outside the source", sub("    provider: managed-block\n    destination: shell-rc\n    block_id: managed-machine\n    content: |\n      export EDITOR={{ inputs.editor }}\n",
			"    provider: file\n    destination: shell-rc\n    source: ../secret.tmpl\n"), schema.Invalid, "relative path inside the source"},
		{"requirement without a subject", sub("  - command: sh", "  - {}"), schema.Invalid, "names a software or a command"},
		{"unknown installation target", sub("targets: [custom, user]", "targets: [system]"), schema.Invalid, `unknown installation target "system"`},
		{"platform alias", sub("linux/amd64", "linux/x86_64"), schema.Invalid, `unknown target "linux/x86_64"`},
		{"future schema version", sub("schema_version: 1", "schema_version: 2"), schema.InvalidVersion, "unsupported version 2"},
		{"anchor", sub("categories: [tools, shell]", "categories: &c [tools, shell]"), schema.Invalid, "anchors"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := c.edit(validConfig)
			if doc == validConfig {
				t.Fatal("edit did not change the document")
			}
			_, err := Parse("builtin/shell.yaml", []byte(doc))
			if err == nil {
				t.Fatalf("accepted:\n%s", doc)
			}
			if !errors.Is(err, schema.Err(c.kind)) {
				t.Errorf("kind: got %v, want %s", err, c.kind)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("message %q does not mention %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), "builtin/shell.yaml: ") {
				t.Errorf("message %q does not name the document", err)
			}
		})
	}
}

// A template source is a file in the source: the definition declares where
// it is, and the loader checks its tokens.
func TestTemplateSourceIsDeclared(t *testing.T) {
	def, err := Parse("shell.yaml", []byte(strings.Replace(validConfig,
		"    provider: managed-block\n    destination: shell-rc\n    block_id: managed-machine\n    content: |\n      export EDITOR={{ inputs.editor }}\n",
		"    provider: template\n    destination: shell-rc\n    source: templates/shell.tmpl\n    settings:\n      greeting: hello\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if def.Operations[0].Source != "templates/shell.tmpl" || def.Operations[0].Settings["greeting"] != "hello" {
		t.Errorf("operation %+v", def.Operations[0])
	}
}

func sub(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}

// dupOperation repeats the operation list of the document, so one id is
// declared twice.
func dupOperation(s string) string {
	start := strings.Index(s, "operations:")
	end := strings.Index(s, "activation:")
	items := strings.TrimPrefix(s[start:end], "operations:\n")
	return s[:end] + items + s[end:]
}
