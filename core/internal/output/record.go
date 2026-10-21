// Package output serializes result records in the three accepted
// presentations: compact JSON Lines (the default), one JSON document
// (--json) and a readable table (--human). Bytes are deterministic for the
// same input; key order is the order fields were added; optional fields that
// are absent are omitted and never written as null.
package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
)

// Field is one key of a record. A nil Value, or a Value with Optional set and
// an empty value, is omitted from every presentation.
type Field struct {
	Key      string
	Value    any
	Optional bool
}

// Record is an ordered set of fields. Build it with the field order the
// record-fields table prescribes; the serializer never reorders.
type Record struct {
	fields []Field
}

// New returns an empty record.
func New() *Record { return &Record{} }

// Set appends a required field. Setting an existing key replaces its value
// in place, so order stays stable.
func (r *Record) Set(key string, value any) *Record {
	return r.put(Field{Key: key, Value: value})
}

// SetOptional appends a field that is omitted when its value is nil, an
// empty string, an empty slice or an empty map.
func (r *Record) SetOptional(key string, value any) *Record {
	return r.put(Field{Key: key, Value: value, Optional: true})
}

func (r *Record) put(f Field) *Record {
	for i := range r.fields {
		if r.fields[i].Key == f.Key {
			r.fields[i] = f
			return r
		}
	}
	r.fields = append(r.fields, f)
	return r
}

// Fields returns the fields that will be serialized, in order.
func (r *Record) Fields() []Field {
	out := make([]Field, 0, len(r.fields))
	for _, f := range r.fields {
		if present(f) {
			out = append(out, f)
		}
	}
	return out
}

// Get returns the value of a present field.
func (r *Record) Get(key string) (any, bool) {
	for _, f := range r.Fields() {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

func present(f Field) bool {
	if f.Value == nil {
		return false
	}
	if !f.Optional {
		return true
	}
	v := reflect.ValueOf(f.Value)
	switch v.Kind() {
	case reflect.String, reflect.Slice, reflect.Map, reflect.Array:
		return v.Len() > 0
	case reflect.Pointer, reflect.Interface:
		return !v.IsNil()
	}
	return true
}

// MarshalJSON writes the present fields in order. Map values are written
// with sorted keys (encoding/json's behavior), so nested objects are
// deterministic too.
func (r *Record) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, f := range r.Fields() {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := marshalCompact(f.Key)
		if err != nil {
			return nil, err
		}
		v, err := marshalCompact(f.Value)
		if err != nil {
			return nil, fmt.Errorf("field %q: %w", f.Key, err)
		}
		b.Write(k)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// UnmarshalJSON reads an object back, preserving the key order of the input
// so a compact line round-trips to identical bytes.
func (r *Record) UnmarshalJSON(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != json.Delim('{') {
		return fmt.Errorf("record: expected object")
	}
	r.fields = nil
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("record: expected key")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		var value any
		if err := decodeOrdered(raw, &value); err != nil {
			return err
		}
		r.fields = append(r.fields, Field{Key: key, Value: value})
	}
	if _, err = dec.Token(); err != nil { // closing brace
		return err
	}
	// Only whitespace may follow the object; "{}junk" or "{}{}" is not one
	// record.
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("record: trailing data after object")
	}
	return nil
}

// decodeOrdered decodes a value; nested objects become *Record so their key
// order survives, arrays become []any, numbers stay json.Number.
func decodeOrdered(raw json.RawMessage, out *any) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("record: empty value")
	}
	switch trimmed[0] {
	case '{':
		nested := New()
		if err := nested.UnmarshalJSON(trimmed); err != nil {
			return err
		}
		*out = nested
		return nil
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(trimmed, &items); err != nil {
			return err
		}
		arr := make([]any, 0, len(items))
		for _, item := range items {
			var v any
			if err := decodeOrdered(item, &v); err != nil {
				return err
			}
			arr = append(arr, v)
		}
		*out = arr
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	return dec.Decode(out)
}

func marshalCompact(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// Flatten renders a value for the human table: scalars as text, string
// slices comma-separated, maps as key=value pairs in sorted key order,
// nested records in field order.
func Flatten(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []string:
		return strings.Join(x, ", ")
	case []any:
		parts := make([]string, 0, len(x))
		for _, item := range x {
			parts = append(parts, Flatten(item))
		}
		return strings.Join(parts, ", ")
	case map[string]string:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+x[k])
		}
		return strings.Join(parts, ", ")
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+Flatten(x[k]))
		}
		return strings.Join(parts, ", ")
	case *Record:
		parts := make([]string, 0, len(x.fields))
		for _, f := range x.Fields() {
			parts = append(parts, f.Key+"="+Flatten(f.Value))
		}
		return strings.Join(parts, ", ")
	case json.Number:
		return x.String()
	case bool:
		if x {
			return "true"
		}
		return "false"
	}
	return fmt.Sprint(v)
}
