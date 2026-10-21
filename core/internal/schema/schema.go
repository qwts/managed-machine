// Package schema holds the validation pieces shared by every declarative
// document the core loads: strict YAML decoding, the error kinds callers
// branch on, and the identifier and version grammars.
package schema

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"

	"go.yaml.in/yaml/v3"
)

// Kind classifies a load failure so later stories can map it to a
// diagnostic identifier without parsing messages.
type Kind string

const (
	Invalid         Kind = "invalid"
	UnknownField    Kind = "unknown-field"
	UnknownProvider Kind = "unknown-provider"
	UnknownOption   Kind = "unknown-option"
	InvalidVersion  Kind = "invalid-version"
	Conflict        Kind = "conflict"
	DigestMismatch  Kind = "digest-mismatch"
)

// Error reports where a document failed and why. Doc is the document's
// identity inside its source (never a host path); Field is a dotted path.
type Error struct {
	Kind    Kind
	Doc     string
	Field   string
	Message string
}

func (e *Error) Error() string {
	loc := e.Doc
	if e.Field != "" {
		loc += ": " + e.Field
	}
	return fmt.Sprintf("%s: %s (%s)", loc, e.Message, e.Kind)
}

// Is matches any *Error with the same Kind, so callers can write
// errors.Is(err, schema.Err(schema.Conflict)).
func (e *Error) Is(target error) bool {
	var t *Error
	return errors.As(target, &t) && t.Doc == "" && t.Field == "" && t.Message == "" && t.Kind == e.Kind
}

// Err returns a sentinel for errors.Is comparisons against a Kind.
func Err(k Kind) error { return &Error{Kind: k} }

// Errorf builds an *Error.
func Errorf(k Kind, doc, field, format string, args ...any) *Error {
	return &Error{Kind: k, Doc: doc, Field: field, Message: fmt.Sprintf(format, args...)}
}

// Decode strictly decodes one YAML document into v: unknown fields are
// rejected, a second document in the same file is rejected, and an empty
// file is rejected. YAML anchors, tags and duplicate keys are left to the
// decoder, which rejects duplicate keys.
func Decode(doc string, data []byte, v any) error {
	if err := plainYAML(doc, data); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return Errorf(Invalid, doc, "", "empty document")
		}
		var te *yaml.TypeError
		if errors.As(err, &te) {
			for _, m := range te.Errors {
				if unknownField.MatchString(m) {
					return Errorf(UnknownField, doc, "", "%s", m)
				}
			}
		}
		return Errorf(Invalid, doc, "", "%v", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Errorf(Invalid, doc, "", "more than one YAML document")
	}
	return nil
}

var unknownField = regexp.MustCompile(`field \S+ not found`)

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidID reports whether s is a lowercase, hyphen-separated identifier,
// the grammar for software, recipe, provider, extension and bundle ids.
func ValidID(s string) bool { return len(s) <= 64 && idPattern.MatchString(s) }

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`)

// ValidSemver reports whether s is MAJOR.MINOR.PATCH with an optional
// pre-release, without build metadata, so two spellings never name one
// version.
func ValidSemver(s string) bool { return semverPattern.MatchString(s) }

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidSHA256 reports whether s is a lowercase hex SHA-256 digest.
func ValidSHA256(s string) bool { return sha256Pattern.MatchString(s) }

// plainYAML rejects anchors, aliases and explicit tags anywhere in the
// document. Definitions are data written by hand; references and type
// overrides only make two readers disagree about what a file says.
func plainYAML(doc string, data []byte) error {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return Errorf(Invalid, doc, "", "%v", err)
	}
	var walk func(n *yaml.Node) error
	walk = func(n *yaml.Node) error {
		if n.Anchor != "" || n.Kind == yaml.AliasNode {
			return Errorf(Invalid, doc, "", "line %d: YAML anchors and aliases are not accepted", n.Line)
		}
		if n.Style&yaml.TaggedStyle != 0 {
			return Errorf(Invalid, doc, "", "line %d: explicit YAML tags are not accepted", n.Line)
		}
		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(&root)
}
