package structured

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// tomlDoc is an order-preserving TOML document backed by
// github.com/pelletier/go-toml/v2. Unrelated keys, nested tables and arrays
// keep their declaration order; TOML comments are not preserved (recorded
// limitation).
type tomlDoc struct {
	root *tomlTable
}

type tomlTable struct {
	entries  []tomlEntry
	inline   bool
	explicit bool
}

type tomlEntry struct {
	key   string
	value any // *tomlTable, []*tomlTable, or scalar/slice value
}

// tomlRaw holds a validated TOML date/time token so unrelated date/time
// values round-trip verbatim.
type tomlRaw string

func newTOMLDoc() *tomlDoc {
	return &tomlDoc{root: &tomlTable{}}
}

func parseTOMLDoc(data []byte) (*tomlDoc, error) {
	// Validate the entire document with go-toml/v2 first so syntax errors,
	// duplicate keys, redefined tables and invalid literals are rejected.
	var check map[string]any
	if err := toml.Unmarshal(data, &check); err != nil {
		return nil, fmt.Errorf("malformed toml: %w", err)
	}

	var p unstable.Parser
	p.Reset(data)
	root := &tomlTable{}
	cur := root
	for p.NextExpression() {
		expr := p.Expression()
		switch expr.Kind {
		case unstable.Table:
			parts := readTOMLKeyParts(expr.Key())
			t, err := root.ensureHeaderTable(parts)
			if err != nil {
				return nil, fmt.Errorf("malformed toml: %w", err)
			}
			t.explicit = true
			cur = t
		case unstable.ArrayTable:
			parts := readTOMLKeyParts(expr.Key())
			t, err := root.appendArrayTable(parts)
			if err != nil {
				return nil, fmt.Errorf("malformed toml: %w", err)
			}
			t.explicit = true
			cur = t
		case unstable.KeyValue:
			parts := readTOMLKeyParts(expr.Key())
			val, err := parseTOMLASTValue(&p, expr.Value())
			if err != nil {
				return nil, fmt.Errorf("malformed toml: %w", err)
			}
			if err := cur.setDottedAST(parts, val); err != nil {
				return nil, fmt.Errorf("malformed toml: %w", err)
			}
		}
	}
	if err := p.Error(); err != nil {
		return nil, fmt.Errorf("malformed toml: %w", err)
	}
	return &tomlDoc{root: root}, nil
}

func readTOMLKeyParts(it unstable.Iterator) []string {
	var parts []string
	for it.Next() {
		parts = append(parts, string(it.Node().Data))
	}
	return parts
}

func parseTOMLASTValue(p *unstable.Parser, n *unstable.Node) (any, error) {
	switch n.Kind {
	case unstable.String:
		return string(n.Data), nil
	case unstable.Bool:
		return string(n.Data) == "true", nil
	case unstable.Integer:
		raw := p.Raw(n.Raw)
		var box struct {
			V int64 `toml:"v"`
		}
		if err := toml.Unmarshal(append([]byte("v = "), raw...), &box); err != nil {
			return nil, err
		}
		return box.V, nil
	case unstable.Float:
		raw := p.Raw(n.Raw)
		var box struct {
			V float64 `toml:"v"`
		}
		if err := toml.Unmarshal(append([]byte("v = "), raw...), &box); err != nil {
			return nil, err
		}
		return box.V, nil
	case unstable.LocalDate, unstable.LocalTime, unstable.LocalDateTime, unstable.DateTime:
		return tomlRaw(p.Raw(n.Raw)), nil
	case unstable.Array:
		out := []any{}
		it := n.Children()
		for it.Next() {
			child := it.Node()
			if child.Kind == unstable.Comment {
				continue
			}
			v, err := parseTOMLASTValue(p, child)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case unstable.InlineTable:
		tbl := &tomlTable{inline: true}
		it := n.Children()
		for it.Next() {
			child := it.Node()
			if child.Kind != unstable.KeyValue {
				continue
			}
			parts := readTOMLKeyParts(child.Key())
			val, err := parseTOMLASTValue(p, child.Value())
			if err != nil {
				return nil, err
			}
			if err := tbl.setInlineDotted(parts, val); err != nil {
				return nil, err
			}
		}
		return tbl, nil
	default:
		return nil, fmt.Errorf("unsupported TOML node kind %v", n.Kind)
	}
}

func (t *tomlTable) ensureHeaderTable(parts []string) (*tomlTable, error) {
	cur := t
	for _, part := range parts {
		v, ok := cur.lookup(part)
		if !ok {
			child := &tomlTable{}
			cur.setEntry(part, child)
			cur = child
			continue
		}
		switch sub := v.(type) {
		case *tomlTable:
			cur = sub
		case []*tomlTable:
			if len(sub) == 0 {
				return nil, fmt.Errorf("empty array table %q", part)
			}
			cur = sub[len(sub)-1]
		default:
			return nil, fmt.Errorf("%q is not a table", part)
		}
	}
	return cur, nil
}

func (t *tomlTable) appendArrayTable(parts []string) (*tomlTable, error) {
	cur := t
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		v, ok := cur.lookup(part)
		if !ok {
			child := &tomlTable{}
			cur.setEntry(part, child)
			cur = child
			continue
		}
		switch sub := v.(type) {
		case *tomlTable:
			cur = sub
		case []*tomlTable:
			if len(sub) == 0 {
				return nil, fmt.Errorf("empty array table %q", part)
			}
			cur = sub[len(sub)-1]
		default:
			return nil, fmt.Errorf("%q is not a table", part)
		}
	}
	last := parts[len(parts)-1]
	elem := &tomlTable{explicit: true}
	v, ok := cur.lookup(last)
	if !ok {
		cur.setEntry(last, []*tomlTable{elem})
		return elem, nil
	}
	arr, ok := v.([]*tomlTable)
	if !ok {
		return nil, fmt.Errorf("%q is not an array of tables", last)
	}
	cur.setEntry(last, append(arr, elem))
	return elem, nil
}

func (t *tomlTable) setDottedAST(parts []string, val any) error {
	cur := t
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		v, ok := cur.lookup(part)
		if !ok {
			child := &tomlTable{}
			cur.setEntry(part, child)
			cur = child
			continue
		}
		sub, ok := v.(*tomlTable)
		if !ok {
			return fmt.Errorf("%q is not a table", part)
		}
		cur = sub
	}
	cur.setEntry(parts[len(parts)-1], val)
	return nil
}

func (t *tomlTable) setInlineDotted(parts []string, val any) error {
	cur := t
	for i := 0; i < len(parts)-1; i++ {
		part := parts[i]
		v, ok := cur.lookup(part)
		if !ok {
			child := &tomlTable{inline: true}
			cur.setEntry(part, child)
			cur = child
			continue
		}
		sub, ok := v.(*tomlTable)
		if !ok {
			return fmt.Errorf("%q is not a table", part)
		}
		cur = sub
	}
	cur.setEntry(parts[len(parts)-1], val)
	return nil
}

func (d *tomlDoc) get(segs []string) (any, bool, bool) {
	cur := d.root
	for i := 0; i < len(segs)-1; i++ {
		v, ok := cur.lookup(segs[i])
		if !ok {
			return nil, false, false
		}
		sub, isTbl := v.(*tomlTable)
		if !isTbl {
			return nil, false, true
		}
		cur = sub
	}
	v, ok := cur.lookup(segs[len(segs)-1])
	if !ok {
		return nil, false, false
	}
	return v, true, false
}

func (d *tomlDoc) set(segs []string, val any) error {
	cur := d.root
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		v, ok := cur.lookup(seg)
		if !ok {
			child := &tomlTable{}
			cur.setEntry(seg, child)
			cur = child
			continue
		}
		sub, isTbl := v.(*tomlTable)
		if !isTbl {
			return fmt.Errorf("%q is not a TOML table", strings.Join(segs[:i+1], "."))
		}
		cur = sub
	}
	cur.setEntry(segs[len(segs)-1], val)
	return nil
}

func (d *tomlDoc) del(segs []string) {
	deleteTOMLPath(d.root, segs)
}

func deleteTOMLPath(cur *tomlTable, segs []string) {
	if cur == nil || len(segs) == 0 {
		return
	}
	if len(segs) == 1 {
		cur.removeEntry(segs[0])
		return
	}
	v, ok := cur.lookup(segs[0])
	if !ok {
		return
	}
	sub, isTbl := v.(*tomlTable)
	if !isTbl {
		return
	}
	deleteTOMLPath(sub, segs[1:])
	if len(sub.entries) == 0 {
		cur.removeEntry(segs[0])
	}
}

func (t *tomlTable) lookup(key string) (any, bool) {
	for _, e := range t.entries {
		if e.key == key {
			return e.value, true
		}
	}
	return nil, false
}

func (t *tomlTable) setEntry(key string, val any) {
	for i := range t.entries {
		if t.entries[i].key == key {
			t.entries[i].value = val
			return
		}
	}
	t.entries = append(t.entries, tomlEntry{key: key, value: val})
}

func (t *tomlTable) removeEntry(key string) {
	for i := range t.entries {
		if t.entries[i].key == key {
			t.entries = append(t.entries[:i], t.entries[i+1:]...)
			return
		}
	}
}

func (d *tomlDoc) marshal() ([]byte, error) {
	var buf bytes.Buffer
	lastWasHeader := false
	if err := encodeTOMLTable(&buf, d.root, nil, false, &lastWasHeader); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	var check map[string]any
	if err := toml.Unmarshal(out, &check); err != nil {
		return nil, fmt.Errorf("encoding toml: %w", err)
	}
	return out, nil
}

func isSectionEntry(v any) bool {
	switch val := v.(type) {
	case *tomlTable:
		return !val.inline
	case []*tomlTable:
		return true
	default:
		return false
	}
}

func encodeTOMLTable(buf *bytes.Buffer, t *tomlTable, path []string, suppressHeader bool, lastWasHeader *bool) error {
	hasScalars := false
	hasSections := false
	for _, e := range t.entries {
		if isSectionEntry(e.value) {
			hasSections = true
		} else {
			hasScalars = true
		}
	}

	if len(path) > 0 && !suppressHeader && (hasScalars || !hasSections || t.explicit) {
		if buf.Len() > 0 && !*lastWasHeader {
			buf.WriteByte('\n')
		}
		buf.WriteByte('[')
		writeTOMLKeyPath(buf, path)
		buf.WriteString("]\n")
		*lastWasHeader = true
	}

	// Pass 1: emit scalar, array and inline-table key-values in order.
	for _, e := range t.entries {
		if isSectionEntry(e.value) {
			continue
		}
		buf.WriteString(formatTOMLKey(e.key))
		buf.WriteString(" = ")
		valStr, err := formatTOMLValue(e.value)
		if err != nil {
			return err
		}
		buf.WriteString(valStr)
		buf.WriteByte('\n')
		*lastWasHeader = false
	}

	// Pass 2: emit sub-tables and arrays of tables in order.
	for _, e := range t.entries {
		if !isSectionEntry(e.value) {
			continue
		}
		subPath := append(append([]string(nil), path...), e.key)
		switch val := e.value.(type) {
		case *tomlTable:
			if err := encodeTOMLTable(buf, val, subPath, false, lastWasHeader); err != nil {
				return err
			}
		case []*tomlTable:
			for _, elem := range val {
				if buf.Len() > 0 && !*lastWasHeader {
					buf.WriteByte('\n')
				}
				buf.WriteString("[[")
				writeTOMLKeyPath(buf, subPath)
				buf.WriteString("]]\n")
				*lastWasHeader = true
				if err := encodeTOMLTable(buf, elem, subPath, true, lastWasHeader); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func writeTOMLKeyPath(buf *bytes.Buffer, path []string) {
	for i, part := range path {
		if i > 0 {
			buf.WriteByte('.')
		}
		buf.WriteString(formatTOMLKey(part))
	}
}

func isBareTOMLKey(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

func formatTOMLKey(k string) string {
	if isBareTOMLKey(k) {
		return k
	}
	return formatTOMLString(k)
}

func formatTOMLValue(v any) (string, error) {
	switch val := v.(type) {
	case string:
		return formatTOMLString(val), nil
	case bool:
		if val {
			return "true", nil
		}
		return "false", nil
	case int64:
		return strconv.FormatInt(val, 10), nil
	case float64:
		switch {
		case math.IsNaN(val):
			return "nan", nil
		case math.IsInf(val, 1):
			return "inf", nil
		case math.IsInf(val, -1):
			return "-inf", nil
		default:
			s := strconv.FormatFloat(val, 'f', -1, 64)
			if !strings.ContainsAny(s, ".eE") {
				s += ".0"
			}
			return s, nil
		}
	case tomlRaw:
		return string(val), nil
	case []string:
		if len(val) == 0 {
			return "[]", nil
		}
		parts := make([]string, len(val))
		for i, item := range val {
			parts[i] = formatTOMLString(item)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case []any:
		if len(val) == 0 {
			return "[]", nil
		}
		parts := make([]string, len(val))
		for i, elem := range val {
			s, err := formatTOMLValue(elem)
			if err != nil {
				return "", err
			}
			parts[i] = s
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case *tomlTable:
		parts := make([]string, len(val.entries))
		for i, e := range val.entries {
			s, err := formatTOMLValue(e.value)
			if err != nil {
				return "", err
			}
			parts[i] = formatTOMLKey(e.key) + " = " + s
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	default:
		return "", fmt.Errorf("unsupported TOML value type %T", v)
	}
}

func formatTOMLString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\b':
			b.WriteString(`\b`)
		case '\t':
			b.WriteString(`\t`)
		case '\n':
			b.WriteString(`\n`)
		case '\f':
			b.WriteString(`\f`)
		case '\r':
			b.WriteString(`\r`)
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

func (t *tomlTable) toMap() map[string]any {
	out := make(map[string]any, len(t.entries))
	for _, e := range t.entries {
		switch sub := e.value.(type) {
		case *tomlTable:
			out[e.key] = sub.toMap()
		case []*tomlTable:
			arr := make([]any, len(sub))
			for i, elem := range sub {
				arr[i] = elem.toMap()
			}
			out[e.key] = arr
		case tomlRaw:
			out[e.key] = string(sub)
		default:
			out[e.key] = sub
		}
	}
	return out
}

func (t *tomlTable) marshalCompactJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range t.entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsonToken(e.key))
		buf.WriteByte(':')
		buf.WriteString(formatDriftValue(e.value))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
