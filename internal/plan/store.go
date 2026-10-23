package plan

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
	"time"
)

// Errors the commands map to their diagnostics.
var (
	ErrNoPlan      = errors.New("no eligible saved plan")
	ErrMismatch    = errors.New("the referenced plan does not match the command")
	ErrNotEligible = errors.New("the plan is not in state saved")
	ErrBusy        = errors.New("another managed-machine execution holds the state directory")
)

// Store keeps plans under <state>/plans/<id>.json and journals under
// <state>/journal/<id>.jsonl. Writes are atomic renames, journal appends
// are synced, and every write happens with the store lock held, so a plan
// found in state executing while the lock is held was interrupted.
type Store struct {
	dir  string
	now  func() time.Time
	lock *lockFile
}

// Open takes the store lock, creating the state directory, and recovers
// interrupted plans. It fails with ErrBusy while another execution holds
// the lock. Close releases it. Read-only commands never open a store.
func Open(stateDir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	for _, d := range []string{filepath.Join(stateDir, "plans"), filepath.Join(stateDir, "journal")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	l, err := acquire(filepath.Join(stateDir, "plans", ".lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{dir: stateDir, now: now, lock: l}
	if err := s.recover(); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the store lock.
func (s *Store) Close() error {
	if s.lock == nil {
		return nil
	}
	err := s.lock.release()
	s.lock = nil
	return err
}

// Now is the store's clock, in the journal's second resolution.
func (s *Store) Now() string { return s.now().UTC().Format(time.RFC3339) }

func (s *Store) planPath(id string) string    { return filepath.Join(s.dir, "plans", id+".json") }
func (s *Store) journalPath(id string) string { return filepath.Join(s.dir, "journal", id+".jsonl") }

func encode(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// write replaces a plan document atomically.
func (s *Store) write(d *Document) error {
	if err := d.Validate(); err != nil {
		return err
	}
	b, err := encode(d)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.dir, "plans"), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), s.planPath(d.Plan)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Load reads one plan by id.
func (s *Store) Load(id string) (*Document, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("%q is not a plan id", id)
	}
	return readDocument(s.planPath(id))
}

func readDocument(path string) (*Document, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var d Document
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	return &d, nil
}

// List returns every plan, oldest first by creation time then id.
func (s *Store) List() ([]*Document, error) {
	names, err := filepath.Glob(filepath.Join(s.dir, "plans", "plan-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Document
	for _, n := range names {
		d, err := readDocument(n)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return before(out[i], out[j]) })
	return out, nil
}

// before orders plans by creation time with the id as tie-break.
func before(a, b *Document) bool {
	ta, _ := time.Parse(time.RFC3339, a.CreatedAt)
	tb, _ := time.Parse(time.RFC3339, b.CreatedAt)
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return a.Plan < b.Plan
}

// sameRequest reports whether two plans are for the same command, name,
// target, account and inputs, which is when a new plan supersedes an old.
func sameRequest(a, b *Document) bool {
	return a.Command == b.Command && a.Name() == b.Name() && a.Target == b.Target &&
		a.Account == b.Account && a.Bindings.InputsDigest == b.Bindings.InputsDigest
}

// Save stores a new plan in state saved and supersedes any saved plan for
// the same request.
func (s *Store) Save(d *Document) error {
	if d.State != Saved {
		return fmt.Errorf("a new plan must be saved, not %s", d.State)
	}
	if _, err := os.Stat(s.planPath(d.Plan)); err == nil {
		return fmt.Errorf("plan %s already exists", d.Plan)
	}
	all, err := s.List()
	if err != nil {
		return err
	}
	if err := s.write(d); err != nil {
		return err
	}
	for _, old := range all {
		if old.State == Saved && sameRequest(old, d) {
			if err := s.Move(old, Superseded); err != nil {
				return err
			}
		}
	}
	return nil
}

// Move changes a plan's state when the transition is allowed.
func (s *Store) Move(d *Document, to State) error {
	if !CanMove(d.State, to) {
		return fmt.Errorf("plan %s cannot move from %s to %s", d.Plan, d.State, to)
	}
	prev := d.State
	d.State = to
	if err := s.write(d); err != nil {
		d.State = prev
		return err
	}
	return nil
}

// Request is what a command asks to apply.
type Request struct {
	Command, Name, Target, Account, InputsDigest string
}

// Select applies the ADR-0012 rule. An explicit reference (a plan id or a
// path to a plan document) selects that plan, and is rejected when its
// command, name, target or account conflicts with the request or when it
// is not saved. Otherwise the most recent saved plan for the request whose
// inputs digest matches is selected; none is ErrNoPlan.
func (s *Store) Select(r Request, explicit string) (*Document, error) {
	if explicit != "" {
		var d *Document
		var err error
		if ValidID(explicit) {
			d, err = s.Load(explicit)
		} else {
			d, err = readDocument(explicit)
		}
		if err != nil {
			return nil, err
		}
		if d.Command != r.Command || d.Name() != r.Name || d.Target != r.Target || d.Account != r.Account {
			return nil, fmt.Errorf("%w: %s is for %s %s %s as %s", ErrMismatch, d.Plan, d.Command, d.Name(), d.Target, d.Account)
		}
		if d.State != Saved {
			return nil, fmt.Errorf("%w: %s is %s", ErrNotEligible, d.Plan, d.State)
		}
		return d, nil
	}
	all, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := len(all) - 1; i >= 0; i-- {
		d := all[i]
		if d.State == Saved && d.Command == r.Command && d.Name() == r.Name && d.Target == r.Target &&
			d.Account == r.Account && d.Bindings.InputsDigest == r.InputsDigest {
			return d, nil
		}
	}
	return nil, ErrNoPlan
}

// Append adds one journal record and syncs it before returning, so a crash
// after Append never loses the record.
func (s *Store) Append(r Record) error {
	if r.At == "" {
		r.At = s.Now()
	}
	if r.Evidence == nil {
		r.Evidence = map[string]any{}
	}
	b, err := encode(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.journalPath(r.Plan), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Journal reads a plan's records in order. A trailing partial line, left
// by a crash during a write, is ignored.
func (s *Store) Journal(id string) ([]Record, error) {
	f, err := os.Open(s.journalPath(id))
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
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, sc.Err()
}

// Receipt returns a plan's receipt, or nil when it has none.
func (s *Store) Receipt(id string) (*Record, error) {
	recs, err := s.Journal(id)
	if err != nil {
		return nil, err
	}
	if n := len(recs); n > 0 && recs[n-1].Action == ActionReceipt {
		return &recs[n-1], nil
	}
	return nil, nil
}

// recover finishes plans a killed process left executing: with the lock
// held no execution is running, so each gets an interrupted receipt naming
// the last journaled operation and moves to failed. A retry needs a new
// plan.
func (s *Store) recover() error {
	all, err := s.List()
	if err != nil {
		return err
	}
	for _, d := range all {
		if d.State != Executing {
			continue
		}
		recs, err := s.Journal(d.Plan)
		if err != nil {
			return err
		}
		ev := map[string]any{"reason": "the executing process ended before writing a receipt"}
		if n := len(recs); n > 0 {
			ev["last_operation"], ev["last_outcome"] = recs[n-1].Operation, recs[n-1].Outcome
		}
		if err := s.Append(Record{Plan: d.Plan, Action: ActionReceipt, Outcome: OutcomeInterrupted, Evidence: ev, State: Failed}); err != nil {
			return err
		}
		if err := s.Move(d, Failed); err != nil {
			return err
		}
	}
	return nil
}
