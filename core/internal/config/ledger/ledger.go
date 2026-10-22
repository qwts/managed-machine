// Package ledger records ownership of configuration destinations (D3): an
// append-only JSON Lines log keyed by (target, path, kind, id) where the
// latest record wins. It lives at <state>/ownership.jsonl for user and
// global targets and at <root>/.managed-machine/ownership.jsonl for custom
// targets, so a custom root carries its own ownership state.
//
// Reads never create a file or a directory; writes happen only during
// apply, under the plan store lock, after the provider's independent
// verification of the destination on disk.
package ledger

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SchemaVersion is the integer schema version of ownership ledger records.
const SchemaVersion = 1

// FileName is the file name of the ownership ledger in its directory.
const FileName = "ownership.jsonl"

// The kinds of resource a ledger record may own.
const (
	KindFile  = "file"
	KindBlock = "block"
	KindKey   = "key"
)

// State is the ownership state recorded by a ledger entry.
type State string

const (
	Owned    State = "owned"
	Released State = "released"
)

// Key identifies one owned resource in the ledger: (target, path, kind, id).
type Key struct {
	Target string
	Path   string
	Kind   string
	ID     string
}

// Drift is one difference between the destination on disk and the rendered
// desired state.
type Drift struct {
	Subject string
	Detail  string
}

// Record is one line of ownership.jsonl. Field order is the serialized
// JSON key order prescribed by D3.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	ID            string `json:"id,omitempty"`
	Format        string `json:"format,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Config        string `json:"config"`
	Operation     string `json:"operation"`
	Target        string `json:"target"`
	Account       string `json:"account"`
	Plan          string `json:"plan"`
	At            string `json:"at"`
	State         State  `json:"state"`
}

// Key returns the tuple that identifies the resource this record acts on.
func (r Record) Key() Key {
	return Key{
		Target: r.Target,
		Path:   filepath.Clean(r.Path),
		Kind:   r.Kind,
		ID:     r.ID,
	}
}

// Validate checks a record before it is written or after it is read.
func (r Record) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("ledger schema_version %d is not %d", r.SchemaVersion, SchemaVersion)
	}
	switch r.Kind {
	case KindFile, KindBlock, KindKey:
	default:
		return fmt.Errorf("unknown ledger kind %q", r.Kind)
	}
	switch r.State {
	case Owned, Released:
	default:
		return fmt.Errorf("unknown ledger state %q", r.State)
	}
	for name, v := range map[string]string{
		"path":      r.Path,
		"config":    r.Config,
		"operation": r.Operation,
		"target":    r.Target,
		"account":   r.Account,
		"plan":      r.Plan,
		"at":        r.At,
	} {
		if v == "" {
			return fmt.Errorf("ledger record is missing %s", name)
		}
	}
	if r.State == Owned && r.SHA256 == "" {
		return errors.New("owned ledger record is missing sha256")
	}
	return nil
}

// Path returns the ledger file path for target: <state>/ownership.jsonl for
// user and global targets, and <root>/.managed-machine/ownership.jsonl for
// a custom:<root> target.
func Path(stateDir, target string) (string, error) {
	if root, ok := strings.CutPrefix(target, "custom:"); ok {
		if strings.TrimSpace(root) == "" {
			return "", errors.New("custom target has no root")
		}
		return filepath.Join(filepath.Clean(root), ".managed-machine", FileName), nil
	}
	switch target {
	case "user", "global":
		if strings.TrimSpace(stateDir) == "" {
			return "", errors.New("state directory is required for user and global targets")
		}
		return filepath.Join(filepath.Clean(stateDir), FileName), nil
	default:
		return "", fmt.Errorf("unknown target %q", target)
	}
}

// Read returns the records of the ledger at path in append order. A
// missing ledger returns nil, nil without creating any file or directory.
// A trailing partial line left by an interrupted write is ignored.
func Read(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Record
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			continue
		}
		if err := r.Validate(); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Append writes records to the end of the ledger at path and syncs the
// file before returning.
func Append(path string, recs ...Record) error {
	if len(recs) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, r := range recs {
		if r.SchemaVersion == 0 {
			r.SchemaVersion = SchemaVersion
		}
		if err := r.Validate(); err != nil {
			return err
		}
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Snapshot is the resolved view of an ownership ledger where the latest
// record for each (target, path, kind, id) key wins.
type Snapshot struct {
	records []Record
	latest  map[Key]Record
}

// Load reads the ledger at path and builds its snapshot. A missing file
// yields an empty snapshot and creates nothing.
func Load(path string) (*Snapshot, error) {
	recs, err := Read(path)
	if err != nil {
		return nil, err
	}
	return NewSnapshot(recs), nil
}

// NewSnapshot resolves a slice of records into a snapshot.
func NewSnapshot(recs []Record) *Snapshot {
	s := &Snapshot{
		records: append([]Record(nil), recs...),
		latest:  make(map[Key]Record, len(recs)),
	}
	for _, r := range recs {
		s.latest[r.Key()] = r
	}
	return s
}

// Records returns all records in append order.
func (s *Snapshot) Records() []Record {
	if s == nil {
		return nil
	}
	return append([]Record(nil), s.records...)
}

// Lookup returns the active owned record for (target, path, kind, id), or
// false when the resource has no record or its latest record is released.
func (s *Snapshot) Lookup(target, path, kind, id string) (Record, bool) {
	if s == nil {
		return Record{}, false
	}
	r, ok := s.latest[Key{Target: target, Path: filepath.Clean(path), Kind: kind, ID: id}]
	if !ok || r.State != Owned {
		return Record{}, false
	}
	return r, true
}

// IsOwnedBy reports whether (target, path, kind, id) currently has an
// active owned record belonging to config.
func (s *Snapshot) IsOwnedBy(config, target, path, kind, id string) bool {
	r, ok := s.Lookup(target, path, kind, id)
	return ok && r.Config == config
}

// OwnedBy returns all active owned records for (config, target) in a
// deterministic order.
func (s *Snapshot) OwnedBy(config, target string) []Record {
	if s == nil {
		return nil
	}
	var out []Record
	for _, r := range s.latest {
		if r.State == Owned && r.Config == config && r.Target == target {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Operation != out[j].Operation {
			return out[i].Operation < out[j].Operation
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// OwnedByOperation groups the active owned records of (config, target) by
// operation id.
func (s *Snapshot) OwnedByOperation(config, target string) map[string][]Record {
	out := map[string][]Record{}
	for _, r := range s.OwnedBy(config, target) {
		out[r.Operation] = append(out[r.Operation], r)
	}
	return out
}

// LatestByOperation returns the most recent record for each operation of
// (config, target), regardless of whether it is owned or released.
func (s *Snapshot) LatestByOperation(config, target string) map[string]Record {
	out := map[string]Record{}
	if s == nil {
		return out
	}
	for _, r := range s.records {
		if r.Config == config && r.Target == target {
			out[r.Operation] = r
		}
	}
	return out
}
