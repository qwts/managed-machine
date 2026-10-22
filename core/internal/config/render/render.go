// Package render is the logic-free substitution engine of the
// configuration schema: three namespaces, no conditionals, no loops, no
// filters and no escaping. A value is inserted verbatim, so what a
// template says is what the file gets.
//
// A token is "{{ namespace.name }}", with the inner whitespace free. Any
// other token, an unknown name and an unbalanced brace are errors: the
// engine never guesses what a template meant.
package render

import (
	"fmt"
	"strings"
)

// The namespaces a token may name.
const (
	Inputs       = "inputs"
	Destinations = "destinations"
	Settings     = "settings"
)

// Values holds the values of the three namespaces.
type Values struct {
	Inputs       map[string]string
	Destinations map[string]string
	Settings     map[string]string
}

// Names holds the names each namespace may use. A name outside a set is
// unknown: a template may not reach a value nobody declared.
type Names struct {
	Inputs       map[string]bool
	Destinations map[string]bool
	Settings     map[string]bool
}

// Render replaces every token in text with the value it names. The inner
// whitespace of a token is free, so "{{ inputs.name }}" and
// "{{inputs.name}}" name the same value. The value is inserted verbatim: it
// is never quoted, escaped or otherwise changed.
func Render(text string, v Values) (string, error) {
	return scan(text, func(expr string) (string, error) {
		ns, name, err := split(expr)
		if err != nil {
			return "", err
		}
		value, ok := lookup(v, ns, name)
		if !ok {
			return "", fmt.Errorf("unknown %s %q", ns, name)
		}
		return value, nil
	})
}

// Check reports whether text uses only the three namespaces and only names
// the given sets. It is the validation half of the engine: a template is
// checked before it is ever rendered.
func Check(text string, names Names) error {
	_, err := scan(text, func(expr string) (string, error) {
		ns, name, err := split(expr)
		if err != nil {
			return "", err
		}
		if !known(names, ns, name) {
			return "", fmt.Errorf("unknown %s %q", ns, name)
		}
		return "", nil
	})
	return err
}

// scan walks text and calls replace for every token; the string it returns
// replaces the token. The text outside a token is copied verbatim, so a
// stray closing brace is an error rather than a character.
func scan(text string, replace func(expr string) (string, error)) (string, error) {
	var b strings.Builder
	rest := text
	for {
		start := strings.Index(rest, "{{")
		if start < 0 {
			if strings.Contains(rest, "}}") {
				return "", fmt.Errorf("unbalanced closing brace")
			}
			b.WriteString(rest)
			return b.String(), nil
		}
		literal := rest[:start]
		if strings.Contains(literal, "}}") {
			return "", fmt.Errorf("unbalanced closing brace")
		}
		b.WriteString(literal)
		rest = rest[start+2:]
		end := strings.Index(rest, "}}")
		if end < 0 {
			return "", fmt.Errorf("unbalanced opening brace")
		}
		expr := strings.TrimSpace(rest[:end])
		rest = rest[end+2:]
		value, err := replace(expr)
		if err != nil {
			return "", err
		}
		b.WriteString(value)
	}
}

// split splits a token expression into its namespace and its name. The
// namespace is one of the three; anything else is not a token this engine
// knows.
func split(expr string) (string, string, error) {
	ns, name, ok := strings.Cut(expr, ".")
	if !ok {
		return "", "", fmt.Errorf("unknown token %q; the namespaces are %s, %s and %s", expr, Inputs, Destinations, Settings)
	}
	ns = strings.TrimSpace(ns)
	name = strings.TrimSpace(name)
	switch ns {
	case Inputs, Destinations, Settings:
	default:
		return "", "", fmt.Errorf("unknown namespace %q; the namespaces are %s, %s and %s", ns, Inputs, Destinations, Settings)
	}
	if name == "" {
		return "", "", fmt.Errorf("token %q names no value", expr)
	}
	return ns, name, nil
}

// lookup returns the value a token names.
func lookup(v Values, ns, name string) (string, bool) {
	var m map[string]string
	switch ns {
	case Inputs:
		m = v.Inputs
	case Destinations:
		m = v.Destinations
	case Settings:
		m = v.Settings
	}
	value, ok := m[name]
	return value, ok
}

// known reports whether a token names a value the sets allow.
func known(names Names, ns, name string) bool {
	var m map[string]bool
	switch ns {
	case Inputs:
		m = names.Inputs
	case Destinations:
		m = names.Destinations
	case Settings:
		m = names.Settings
	}
	return m[name]
}
