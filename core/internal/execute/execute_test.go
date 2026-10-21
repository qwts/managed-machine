package execute

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/core/internal/plan"
)

var t0 = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

type fakeStep struct {
	m  *fakeMachine
	id string
}

func (f fakeStep) Apply(ctx context.Context) (Evidence, error) {
	f.m.calls = append(f.m.calls, "apply "+f.id)
	if f.m.cancel != nil && f.id == f.m.cancelAt {
		f.m.cancel()
	}
	if f.id == f.m.failApply {
		return Evidence{"exit": 1}, errors.New("installer exited 1")
	}
	return Evidence{"exit": 0}, nil
}

func (f fakeStep) Verify(ctx context.Context) (Evidence, error) {
	f.m.calls = append(f.m.calls, "verify "+f.id)
	if f.id == f.m.failVerify {
		return nil, errors.New("not installed after apply")
	}
	return Evidence{"version": "14.1.1"}, nil
}

type fakeMachine struct {
	bindings              plan.Bindings
	failApply, failVerify string
	failCheck             string
	unknownStep           string
	calls                 []string
	cancel                context.CancelFunc
	cancelAt              string
}

func (m *fakeMachine) Bindings(context.Context, *plan.Document) (plan.Bindings, error) {
	return m.bindings, nil
}
func (m *fakeMachine) Check(_ context.Context, p plan.Precondition) (string, error) {
	m.calls = append(m.calls, "check "+p.Subject)
	if p.Subject == m.failCheck {
		return "absent", errors.New("not found")
	}
	return p.Observed, nil
}
func (m *fakeMachine) Step(op plan.Operation) (Step, error) {
	if op.ID == m.unknownStep {
		return nil, fmt.Errorf("no provider %q", op.Provider)
	}
	return fakeStep{m, op.ID}, nil
}

func fixture(t *testing.T) (*plan.Store, *plan.Document, *fakeMachine) {
	t.Helper()
	s, err := plan.Open(t.TempDir(), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	b := plan.Bindings{Machine: "m1", Platform: "darwin/arm64", InputsDigest: "sha256:in",
		Assets: []plan.Asset{{Name: "builtin:ripgrep.yaml", SHA256: "aa"}}, Providers: []plan.Provider{{Name: "brew", Version: "4.4.1"}}}
	d := &plan.Document{Plan: "plan-20261021T120000Z-0001", SchemaVersion: 1, Command: "software --install", Software: "ripgrep",
		Target: "global", Account: "fixture", SourceRevision: "builtin@sha256:aa", CreatedAt: t0.Format(time.RFC3339), State: plan.Saved,
		Bindings: b, Preconditions: []plan.Precondition{{Kind: "software", Subject: "brew", Requirement: "present", Observed: "4.4.1"}}}
	for _, id := range []string{"1", "2", "3"} {
		d.Operations = append(d.Operations, plan.Operation{ID: id, Operation: "software:pkg" + id, Target: "global", Action: "create", Provider: "brew"})
	}
	if err := s.Save(d); err != nil {
		t.Fatal(err)
	}
	return s, d, &fakeMachine{bindings: b}
}

func outcomes(t *testing.T, s *plan.Store, id string) string {
	t.Helper()
	recs, err := s.Journal(id)
	if err != nil {
		t.Fatal(err)
	}
	var o []string
	for _, r := range recs {
		o = append(o, r.Operation+":"+r.Outcome)
	}
	return strings.Join(o, " ")
}

func state(t *testing.T, s *plan.Store, id string) plan.State {
	d, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	return d.State
}

func TestRunsInOrderAndJournals(t *testing.T) {
	s, d, m := fixture(t)
	r, err := Run(context.Background(), s, d.Plan, m)
	if err != nil || r.State != plan.Applied {
		t.Fatalf("run: %+v %v", r, err)
	}
	if got := strings.Join(m.calls, ","); got != "check brew,apply 1,verify 1,apply 2,verify 2,apply 3,verify 3" {
		t.Errorf("calls: %s", got)
	}
	if got := outcomes(t, s, d.Plan); got != "1:started 1:ok 2:started 2:ok 3:started 3:ok :applied" {
		t.Errorf("journal: %s", got)
	}
	if state(t, s, d.Plan) != plan.Applied {
		t.Error("plan not applied")
	}
	if _, err := Run(context.Background(), s, d.Plan, m); !errors.Is(err, plan.ErrNotEligible) {
		t.Errorf("an applied plan ran again: %v", err)
	}
}

func TestStopsAtFirstFailure(t *testing.T) {
	for _, mode := range []string{"apply", "verify"} {
		s, d, m := fixture(t)
		if mode == "apply" {
			m.failApply = "2"
		} else {
			m.failVerify = "2"
		}
		r, err := Run(context.Background(), s, d.Plan, m)
		var f *Failed
		if !errors.As(err, &f) || f.Operation != "2" || r.State != plan.Failed || r.Outcome != plan.OutcomeFailed {
			t.Fatalf("%s: %+v %v", mode, r, err)
		}
		for _, c := range m.calls {
			if strings.HasSuffix(c, " 3") {
				t.Errorf("%s: operation 3 ran after 2 failed: %v", mode, m.calls)
			}
		}
		if nr := r.Evidence["not_run"].([]string); len(nr) != 1 || nr[0] != "3" {
			t.Errorf("%s: not_run %v", mode, nr)
		}
		if got := outcomes(t, s, d.Plan); got != "1:started 1:ok 2:started 2:failed 2:failed" {
			t.Errorf("%s: journal %s", mode, got)
		}
		if _, err := Run(context.Background(), s, d.Plan, m); !errors.Is(err, plan.ErrNotEligible) {
			t.Errorf("%s: a failed plan ran again: %v", mode, err)
		}
	}
}

func TestRejectsChangedMachine(t *testing.T) {
	cases := map[string]func(m *fakeMachine){
		"machine":      func(m *fakeMachine) { m.bindings.Machine = "m2" },
		"inputs":       func(m *fakeMachine) { m.bindings.InputsDigest = "sha256:new" },
		"asset":        func(m *fakeMachine) { m.bindings.Assets = []plan.Asset{{Name: "builtin:ripgrep.yaml", SHA256: "bb"}} },
		"provider":     func(m *fakeMachine) { m.bindings.Providers = []plan.Provider{{Name: "brew", Version: "4.5.0"}} },
		"precondition": func(m *fakeMachine) { m.failCheck = "brew" },
		"unknown step": func(m *fakeMachine) { m.unknownStep = "3" },
	}
	for name, change := range cases {
		s, d, m := fixture(t)
		change(m)
		r, err := Run(context.Background(), s, d.Plan, m)
		var rej *Rejected
		if !errors.As(err, &rej) || r.State != plan.Rejected || len(rej.Changes) == 0 {
			t.Fatalf("%s: %+v %v", name, r, err)
		}
		for _, c := range m.calls {
			if strings.HasPrefix(c, "apply") {
				t.Errorf("%s: an operation ran on a rejected plan", name)
			}
		}
		if state(t, s, d.Plan) != plan.Rejected {
			t.Errorf("%s: plan not rejected", name)
		}
	}
}

func TestCancelledRunIsInterrupted(t *testing.T) {
	s, d, m := fixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel, m.cancelAt = cancel, "1"
	r, err := Run(ctx, s, d.Plan, m)
	if err == nil || r.Outcome != plan.OutcomeInterrupted || r.State != plan.Failed {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	if len(m.calls) > 3 {
		t.Errorf("operations ran after cancellation: %v", m.calls)
	}
}
