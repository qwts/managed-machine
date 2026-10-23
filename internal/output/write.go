package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Mode is the presentation selected by the flags.
type Mode int

const (
	// Compact is JSON Lines, the default without any presentation flag.
	Compact Mode = iota
	// JSON is one JSON document (--json).
	JSON
	// Human is a readable table (--human).
	Human
)

func (m Mode) String() string {
	switch m {
	case JSON:
		return "json"
	case Human:
		return "human"
	}
	return "compact"
}

// ErrConflictingModes is returned when --json and --human are both given.
var ErrConflictingModes = errors.New("--json and --human are mutually exclusive")

// SelectMode maps the two presentation flags to a Mode.
func SelectMode(jsonFlag, humanFlag bool) (Mode, error) {
	switch {
	case jsonFlag && humanFlag:
		return Compact, ErrConflictingModes
	case jsonFlag:
		return JSON, nil
	case humanFlag:
		return Human, nil
	}
	return Compact, nil
}

// Write renders the records in the mode. Compact writes one line per record
// and nothing for zero records; JSON writes the single record as the
// document or a {"records":[...]} document otherwise; Human writes a table
// with uppercase headers drawn from the union of keys in first-seen order.
func Write(w io.Writer, mode Mode, records []*Record) error {
	switch mode {
	case JSON:
		return writeJSON(w, records)
	case Human:
		return writeHuman(w, records)
	}
	return WriteCompact(w, records)
}

// WriteCompact writes JSON Lines: UTF-8, newline-terminated, no blank lines.
func WriteCompact(w io.Writer, records []*Record) error {
	for _, r := range records {
		line, err := r.MarshalJSON()
		if err != nil {
			return err
		}
		if !utf8.Valid(line) {
			return fmt.Errorf("record is not valid UTF-8")
		}
		if _, err := w.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(w io.Writer, records []*Record) error {
	var doc []byte
	var err error
	if len(records) == 1 {
		doc, err = records[0].MarshalJSON()
	} else {
		var b bytes.Buffer
		b.WriteString(`{"records":[`)
		for i, r := range records {
			if i > 0 {
				b.WriteByte(',')
			}
			line, err := r.MarshalJSON()
			if err != nil {
				return err
			}
			b.Write(line)
		}
		b.WriteString("]}")
		doc = b.Bytes()
	}
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, doc, "", "  "); err != nil {
		return err
	}
	indented.WriteByte('\n')
	_, err = w.Write(indented.Bytes())
	return err
}

func writeHuman(w io.Writer, records []*Record) error {
	if len(records) == 0 {
		return nil
	}
	var columns []string
	seen := map[string]bool{}
	for _, r := range records {
		for _, f := range r.Fields() {
			if !seen[f.Key] {
				seen[f.Key] = true
				columns = append(columns, f.Key)
			}
		}
	}
	rows := make([][]string, 0, len(records)+1)
	header := make([]string, len(columns))
	for i, c := range columns {
		header[i] = strings.ToUpper(c)
	}
	rows = append(rows, header)
	for _, r := range records {
		row := make([]string, len(columns))
		for i, c := range columns {
			if v, ok := r.Get(c); ok {
				row[i] = Flatten(v)
			}
		}
		rows = append(rows, row)
	}
	widths := make([]int, len(columns))
	for _, row := range rows {
		for i, cell := range row {
			if n := utf8.RuneCountInString(cell); n > widths[i] {
				widths[i] = n
			}
		}
	}
	// Columns are padded to the widest cell plus two spaces; a row never
	// carries trailing whitespace, so empty trailing cells leave no residue.
	var b strings.Builder
	for _, row := range rows {
		var line strings.Builder
		for i, cell := range row {
			line.WriteString(cell)
			if i < len(row)-1 {
				line.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(cell)+2))
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// ReadCompact parses JSON Lines back into records, preserving key order.
func ReadCompact(data []byte) ([]*Record, error) {
	var records []*Record
	for i, line := range bytes.Split(data, []byte{'\n'}) {
		if len(line) == 0 {
			if i == len(bytes.Split(data, []byte{'\n'}))-1 {
				break
			}
			return nil, fmt.Errorf("line %d: blank line in JSON Lines", i+1)
		}
		r := New()
		if err := r.UnmarshalJSON(line); err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		records = append(records, r)
	}
	return records, nil
}

// CustomTarget encodes a custom root as the documented "custom:<path>"
// string. The path is kept verbatim (Windows separators included); JSON
// escaping is the only transformation applied later.
func CustomTarget(path string) string {
	return "custom:" + path
}
