package structured

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// jsonDoc is an order-preserving JSON document whose root is a JSON object.
type jsonDoc struct {
	root *jsonObject
}

type jsonObject struct {
	entries []jsonEntry
}

type jsonEntry struct {
	key   string
	value any // *jsonObject, []any, string, bool, json.Number, nil
}

func newJSONDoc() *jsonDoc {
	return &jsonDoc{root: &jsonObject{}}
}

// parseJSONDoc parses data as strict RFC 8259 JSON whose root value is an
// object. Comments, trailing commas, trailing tokens and non-object roots
// are rejected as malformed.
func parseJSONDoc(data []byte) (*jsonDoc, error) {
	if !json.Valid(data) {
		return nil, errors.New("malformed json: invalid syntax")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("malformed json: %w", err)
	}
	if tok != json.Delim('{') {
		return nil, errors.New("malformed json: root must be a JSON object")
	}
	root, err := decodeJSONObject(dec)
	if err != nil {
		return nil, fmt.Errorf("malformed json: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("malformed json: trailing data after root object")
	}
	return &jsonDoc{root: root}, nil
}

func decodeJSONObject(dec *json.Decoder) (*jsonObject, error) {
	obj := &jsonObject{}
	seen := map[string]int{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := kt.(string)
		if !ok {
			return nil, fmt.Errorf("expected object key, got %v", kt)
		}
		val, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		if idx, dup := seen[key]; dup {
			obj.entries[idx].value = val
		} else {
			seen[key] = len(obj.entries)
			obj.entries = append(obj.entries, jsonEntry{key: key, value: val})
		}
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim('}') {
		return nil, fmt.Errorf("expected '}', got %v", end)
	}
	return obj, nil
}

func decodeJSONArray(dec *json.Decoder) ([]any, error) {
	out := []any{}
	for dec.More() {
		v, err := decodeJSONValue(dec)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	end, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if end != json.Delim(']') {
		return nil, fmt.Errorf("expected ']', got %v", end)
	}
	return out, nil
}

func decodeJSONValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			return decodeJSONObject(dec)
		case '[':
			return decodeJSONArray(dec)
		default:
			return nil, fmt.Errorf("unexpected delimiter %v", v)
		}
	case string, bool, json.Number, nil:
		return v, nil
	default:
		return nil, fmt.Errorf("unexpected JSON token %T", tok)
	}
}

func (d *jsonDoc) get(segs []string) (any, bool, bool) {
	cur := d.root
	for i := 0; i < len(segs)-1; i++ {
		v, ok := cur.lookup(segs[i])
		if !ok {
			return nil, false, false
		}
		sub, isObj := v.(*jsonObject)
		if !isObj {
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

func (d *jsonDoc) set(segs []string, val any) error {
	cur := d.root
	for i := 0; i < len(segs)-1; i++ {
		seg := segs[i]
		v, ok := cur.lookup(seg)
		if !ok {
			child := &jsonObject{}
			cur.setEntry(seg, child)
			cur = child
			continue
		}
		sub, isObj := v.(*jsonObject)
		if !isObj {
			return fmt.Errorf("%q is not a JSON object", strings.Join(segs[:i+1], "."))
		}
		cur = sub
	}
	cur.setEntry(segs[len(segs)-1], toJSONNodeValue(val))
	return nil
}

func (d *jsonDoc) del(segs []string) {
	deleteJSONPath(d.root, segs)
}

func deleteJSONPath(cur *jsonObject, segs []string) {
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
	sub, isObj := v.(*jsonObject)
	if !isObj {
		return
	}
	deleteJSONPath(sub, segs[1:])
	if len(sub.entries) == 0 {
		cur.removeEntry(segs[0])
	}
}

func (o *jsonObject) lookup(key string) (any, bool) {
	for _, e := range o.entries {
		if e.key == key {
			return e.value, true
		}
	}
	return nil, false
}

func (o *jsonObject) setEntry(key string, val any) {
	for i := range o.entries {
		if o.entries[i].key == key {
			o.entries[i].value = val
			return
		}
	}
	o.entries = append(o.entries, jsonEntry{key: key, value: val})
}

func (o *jsonObject) removeEntry(key string) {
	for i := range o.entries {
		if o.entries[i].key == key {
			o.entries = append(o.entries[:i], o.entries[i+1:]...)
			return
		}
	}
}

func toJSONNodeValue(val any) any {
	switch v := val.(type) {
	case int64:
		return json.Number(strconv.FormatInt(v, 10))
	case float64:
		return json.Number(strconv.FormatFloat(v, 'f', -1, 64))
	case []string:
		arr := make([]any, len(v))
		for i, s := range v {
			arr[i] = s
		}
		return arr
	default:
		return v
	}
}

func (d *jsonDoc) marshal() ([]byte, error) {
	var buf bytes.Buffer
	if err := writeJSONValue(&buf, d.root, 0); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

func writeJSONValue(buf *bytes.Buffer, v any, depth int) error {
	switch val := v.(type) {
	case *jsonObject:
		if len(val.entries) == 0 {
			buf.WriteString("{}")
			return nil
		}
		buf.WriteString("{\n")
		for i, e := range val.entries {
			writeJSONIndent(buf, depth+1)
			buf.WriteString(jsonToken(e.key))
			buf.WriteString(": ")
			if err := writeJSONValue(buf, e.value, depth+1); err != nil {
				return err
			}
			if i < len(val.entries)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		writeJSONIndent(buf, depth)
		buf.WriteByte('}')
		return nil
	case []any:
		if len(val) == 0 {
			buf.WriteString("[]")
			return nil
		}
		buf.WriteString("[\n")
		for i, elem := range val {
			writeJSONIndent(buf, depth+1)
			if err := writeJSONValue(buf, elem, depth+1); err != nil {
				return err
			}
			if i < len(val)-1 {
				buf.WriteByte(',')
			}
			buf.WriteByte('\n')
		}
		writeJSONIndent(buf, depth)
		buf.WriteByte(']')
		return nil
	default:
		buf.WriteString(jsonToken(val))
		return nil
	}
}

func writeJSONIndent(buf *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		buf.WriteString("  ")
	}
}

func (o *jsonObject) marshalCompact() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range o.entries {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(jsonToken(e.key))
		buf.WriteByte(':')
		switch sub := e.value.(type) {
		case *jsonObject:
			b, err := sub.marshalCompact()
			if err != nil {
				return nil, err
			}
			buf.Write(b)
		default:
			buf.WriteString(jsonToken(sub))
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
