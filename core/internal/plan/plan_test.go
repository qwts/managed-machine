package plan

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

func doc(id string, at time.Time, inputs string) *Document {
	return &Document{
		Plan: id, SchemaVersion: SchemaVersion, Command: "software --install", Software: "ripgrep", Target: "global",
		Account: "fixture", SourceRevision: "builtin@sha256:aa", CreatedAt: at.Format(time.RFC3339), State: Saved,
		Bindings: Bindings{Machine: "m1", Platform: "darwin/arm64", InputsDigest: inputs,
			Assets: []Asset{{Name: "builtin:ripgrep.yaml", SHA256: "aa"}}, Providers: []Provider{{Name: "brew", Version: "4.4.1"}}},
		Preconditions: []Precondition{{Kind: "software", Subject: "brew", Requirement: "present", Observed: "4.4.1"}},
		Operations:    []Operation{{ID: "1", Operation: "software:ripgrep", Target: "global", Action: "create", Provider: "brew", Params: map[string]string{"formula": "ripgrep"}}},
	}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestDocumentKeyOrder(t *testing.T) {
	b, err := encode(doc("plan-2026-10-21T12:00:00Z-3f9c", t0, "sha256:in"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"plan":"plan-2026-10-21T12:00:00Z-3f9c","schema_version":1,"command":"software --install","software":"ripgrep","target":"global","account":"fixture","source_revision":"builtin@sha256:aa","created_at":"2026-10-21T12:00:00Z","state":"saved","bindings":{"machine":"m1","platform":"darwin/arm64","inputs_digest":"sha256:in","assets":[{"name":"builtin:ripgrep.yaml","sha256":"aa"}],"providers":[{"name":"brew","version":"4.4.1"}]},"preconditions":[{"kind":"software","subject":"brew","requirement":"present","observed":"4.4.1"}],"operations":[{"id":"1","operation":"software:ripgrep","target":"global","action":"create","provider":"brew","params":{"formula":"ripgrep"}}]}` + "\n"
	if string(b) != want {
		t.Errorf("plan document:\n%s\nwant\n%s", b, want)
	}
	r, _ := encode(Record{Plan: "p", Operation: "1", Action: "create", Outcome: "ok", At: "t", Evidence: map[string]any{}, State: ""})
	if string(r) != `{"plan":"p","operation":"1","action":"create","outcome":"ok","at":"t","evidence":{}}`+"\n" {
		t.Errorf("journal record: %s", r)
	}
}

func TestNewIDAndValidate(t *testing.T) {
	id, err := NewID(t0)
	if err != nil || !ValidID(id) || !strings.HasPrefix(id, "plan-2026-10-21T12:00:00Z-") {
		t.Fatalf("NewID: %q %v", id, err)
	}
	d := doc(id, t0, "sha256:in")
	d.Config = "zsh"
	if d.Validate() == nil {
		t.Error("a plan naming software and config validated")
	}
	d = doc(id, t0, "sha256:in")
	d.Operations = append(d.Operations, d.Operations[0])
	if d.Validate() == nil {
		t.Error("a repeated operation id validated")
	}
}

func TestSaveSupersedesAndSelectOrders(t *testing.T) {
	s := open(t, t.TempDir())
	a := doc("plan-2026-10-21T12:00:00Z-0001", t0, "sha256:in")
	b := doc("plan-2026-10-21T12:00:00Z-0002", t0, "sha256:other")
	c := doc("plan-2026-10-21T12:00:01Z-0000", t0.Add(time.Second), "sha256:other")
	for _, d := range []*Document{a, b, c} {
		if err := s.Save(d); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s.Load(a.Plan)
	if got.State != Saved {
		t.Errorf("a plan with other inputs was superseded: %s", got.State)
	}
	got, _ = s.Load(b.Plan)
	if got.State != Superseded {
		t.Errorf("an older plan for the same inputs is %s, want superseded", got.State)
	}
	r := Request{Command: "software --install", Name: "ripgrep", Target: "global", Account: "fixture", InputsDigest: "sha256:other"}
	if sel, err := s.Select(r, ""); err != nil || sel.Plan != c.Plan {
		t.Fatalf("select: %v %v", sel, err)
	}
	// Same second: the id breaks the tie toward the later id.
	d := doc("plan-2026-10-21T12:00:01Z-ffff", t0.Add(time.Second), "sha256:in")
	e := doc("plan-2026-10-21T12:00:01Z-0aaa", t0.Add(time.Second), "sha256:in")
	s.write(d)
	s.write(e)
	r.InputsDigest = "sha256:in"
	if sel, err := s.Select(r, ""); err != nil || sel.Plan != d.Plan {
		t.Fatalf("tie-break: %v %v", sel, err)
	}
	r.Account = "someone-else"
	if _, err := s.Select(r, ""); !errors.Is(err, ErrNoPlan) {
		t.Errorf("another account's plan was eligible: %v", err)
	}
}

func TestExplicitReference(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	a := doc("plan-2026-10-21T12:00:00Z-0001", t0, "sha256:in")
	s.Save(a)
	r := Request{Command: "software --install", Name: "ripgrep", Target: "global", Account: "fixture"}
	if d, err := s.Select(r, a.Plan); err != nil || d.Plan != a.Plan {
		t.Fatalf("by id: %v %v", d, err)
	}
	if d, err := s.Select(r, s.planPath(a.Plan)); err != nil || d.Plan != a.Plan {
		t.Fatalf("by path: %v %v", d, err)
	}
	r.Target = "user"
	if _, err := s.Select(r, a.Plan); !errors.Is(err, ErrMismatch) {
		t.Errorf("a conflicting target was accepted: %v", err)
	}
	r.Target = "global"
	s.Move(a, Rejected)
	if _, err := s.Select(r, a.Plan); !errors.Is(err, ErrNotEligible) {
		t.Errorf("a rejected plan was selected explicitly: %v", err)
	}
}

func TestTransitions(t *testing.T) {
	for _, final := range []State{Applied, Failed, Rejected, Superseded} {
		for _, to := range []State{Saved, Executing, Applied, Failed} {
			if CanMove(final, to) {
				t.Errorf("%s can move to %s", final, to)
			}
		}
	}
	if !CanMove(Saved, Executing) || !CanMove(Executing, Failed) || CanMove(Saved, Applied) {
		t.Error("transition table")
	}
}

func TestJournalIgnoresTornLine(t *testing.T) {
	s := open(t, t.TempDir())
	s.Append(Record{Plan: "p", Operation: "1", Action: "create", Outcome: OutcomeStarted})
	f, _ := os.OpenFile(s.journalPath("p"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"plan":"p","oper`)
	f.Close()
	recs, err := s.Journal("p")
	if err != nil || len(recs) != 1 || recs[0].At != "2026-10-21T12:00:00Z" {
		t.Fatalf("journal: %+v %v", recs, err)
	}
}

func TestBusyWhileHeld(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	if _, err := Open(dir, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("second open: %v", err)
	}
	s.Close()
	s2, err := Open(dir, nil)
	if err != nil {
		t.Fatalf("open after close: %v", err)
	}
	s2.Close()
}

// TestKilledExecutionIsRecovered starts a child that takes the lock, moves
// a plan to executing, journals a started operation and blocks; the child
// is killed, and the next Open must get the lock and finish the plan as
// failed with an interrupted receipt.
func TestKilledExecutionIsRecovered(t *testing.T) {
	if os.Getenv("MM_PLAN_CHILD") != "" {
		return
	}
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestChildHoldsExecution$")
	cmd.Env = append(os.Environ(), "MM_PLAN_CHILD="+dir, "MM_PLAN_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("child never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := Open(dir, nil); !errors.Is(err, ErrBusy) {
		t.Fatalf("open while the child executes: %v", err)
	}
	cmd.Process.Kill()
	cmd.Wait()
	s := open(t, dir)
	d, err := s.Load("plan-2026-10-21T12:00:00Z-0001")
	if err != nil || d.State != Failed {
		t.Fatalf("recovered plan: %+v %v", d, err)
	}
	r, _ := s.Receipt(d.Plan)
	if r == nil || r.Outcome != OutcomeInterrupted || r.State != Failed || r.Evidence["last_operation"] != "1" {
		t.Fatalf("receipt: %+v", r)
	}
	if _, err := s.Select(Request{Command: d.Command, Name: d.Name(), Target: d.Target, Account: d.Account, InputsDigest: d.Bindings.InputsDigest}, ""); !errors.Is(err, ErrNoPlan) {
		t.Errorf("an interrupted plan is still eligible: %v", err)
	}
}

func TestChildHoldsExecution(t *testing.T) {
	dir := os.Getenv("MM_PLAN_CHILD")
	if dir == "" {
		t.Skip("helper for TestKilledExecutionIsRecovered")
	}
	s, err := Open(dir, func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	d := doc("plan-2026-10-21T12:00:00Z-0001", t0, "sha256:in")
	s.Save(d)
	s.Move(d, Executing)
	s.Append(Record{Plan: d.Plan, Operation: "1", Action: "create", Outcome: OutcomeStarted})
	os.WriteFile(os.Getenv("MM_PLAN_READY"), nil, 0o600)
	time.Sleep(time.Minute)
}
