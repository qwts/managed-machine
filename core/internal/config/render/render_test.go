package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden")

func values() Values {
	return Values{
		Inputs:       map[string]string{"editor": "vim", "value": `a "quoted" $value \n`},
		Destinations: map[string]string{"shell-rc": "/home/user/.bashrc"},
		Settings:     map[string]string{"greeting": "hello"},
	}
}

// The rendered bytes are the contract: a value is inserted verbatim, and
// the inner whitespace of a token is free.
func TestRenderGoldens(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"inputs", "export EDITOR={{ inputs.editor }}\n"},
		{"destinations", "source {{ destinations.shell-rc }}\n"},
		{"settings", "greeting: {{ settings.greeting }}\n"},
		{"whitespace", "a {{inputs.editor}} b {{  inputs.editor  }} c\n"},
		{"verbatim", "value: {{ inputs.value }}\n"},
		{"literal", "no tokens here\n"},
		{"repeated", "{{ inputs.editor }} and {{ inputs.editor }}\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Render(c.text, values())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("testdata", c.name+".golden")
			if *update {
				if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("differs from %s:\ngot  %q\nwant %q", path, got, string(want))
			}
		})
	}
}

// The inner whitespace of a token is free, so the variants of one token
// render the same bytes.
func TestWhitespaceVariantsAreEquivalent(t *testing.T) {
	want, err := Render("{{ inputs.editor }}", values())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"{{inputs.editor}}", "{{ inputs.editor}}", "{{inputs.editor }}", "{{  inputs.editor  }}", "{{ inputs . editor }}"} {
		got, err := Render(text, values())
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		if got != want {
			t.Errorf("%s rendered %q, want %q", text, got, want)
		}
	}
}

// Anything the engine does not know is an error: it never guesses what a
// template meant.
func TestRenderRejects(t *testing.T) {
	cases := []struct {
		name, text, want string
	}{
		{"unknown namespace", "a {{ secrets.token }}", "unknown namespace"},
		{"unknown input", "a {{ inputs.nope }}", `unknown inputs "nope"`},
		{"unknown destination", "a {{ destinations.nope }}", `unknown destinations "nope"`},
		{"unknown setting", "a {{ settings.nope }}", `unknown settings "nope"`},
		{"no namespace", "a {{ editor }}", "unknown token"},
		{"empty name", "a {{ inputs. }}", "names no value"},
		{"unbalanced opening", "a {{ inputs.editor", "unbalanced opening brace"},
		{"unbalanced closing", "a }} b", "unbalanced closing brace"},
		{"stray closing", "{{ inputs.editor }} }}", "unbalanced closing brace"},
		{"conditionals are not tokens", "{{ if inputs.editor }}", "unknown namespace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Render(c.text, values())
			if err == nil {
				t.Fatalf("accepted %q", c.text)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("message %q does not mention %q", err, c.want)
			}
		})
	}
}

// Check is the validation half of the engine: a template is checked before
// it is ever rendered.
func TestCheck(t *testing.T) {
	names := Names{
		Inputs:       map[string]bool{"editor": true},
		Destinations: map[string]bool{"shell-rc": true},
		Settings:     map[string]bool{"greeting": true},
	}
	for _, text := range []string{
		"plain text",
		"{{ inputs.editor }}",
		"{{ destinations.shell-rc }}",
		"{{ settings.greeting }}",
	} {
		if err := Check(text, names); err != nil {
			t.Errorf("%q: %v", text, err)
		}
	}
	for _, text := range []string{
		"{{ inputs.nope }}",
		"{{ destinations.nope }}",
		"{{ settings.nope }}",
		"{{ secrets.token }}",
		"{{ inputs.editor",
		"a }} b",
	} {
		if err := Check(text, names); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}
