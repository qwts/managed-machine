// Package execute runs a saved plan under the shared execution model
// (ADR-0012): it re-verifies the plan's bindings and preconditions against
// the machine, then runs the operations one at a time in plan order,
// journaling each as it starts and ends, and stops at the first failure.
// A plan runs at most once; a retry needs a new plan.
package execute

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/plan"
)

// Evidence is what an operation or check observed, journaled verbatim.
type Evidence = map[string]any

// Step performs one saved operation. Apply changes the machine; Verify
// observes the result independently of Apply's own report.
type Step interface {
	Apply(ctx context.Context) (Evidence, error)
	Verify(ctx context.Context) (Evidence, error)
}

// Machine is the current machine as the plan's providers see it.
type Machine interface {
	// Bindings re-derives the bindings for the plan's inputs now.
	Bindings(ctx context.Context, d *plan.Document) (plan.Bindings, error)
	// Check re-evaluates one precondition now; a non-nil error means it
	// no longer holds, and observed is what was found instead.
	Check(ctx context.Context, p plan.Precondition) (observed string, err error)
	// Step returns the performer of one saved operation.
	Step(op plan.Operation) (Step, error)
}

// Rejected means the machine no longer matches the plan; Changes names
// each difference. The plan is moved to rejected and never runs.
type Rejected struct{ Changes []string }

func (r *Rejected) Error() string {
	return "the plan no longer matches the machine: " + strings.Join(r.Changes, "; ")
}

// Failed means an operation failed; the operations after it did not run.
type Failed struct {
	Operation string
	Err       error
}

func (f *Failed) Error() string { return fmt.Sprintf("operation %s failed: %v", f.Operation, f.Err) }
func (f *Failed) Unwrap() error { return f.Err }

// Run executes the saved plan id from an open store and returns its
// receipt. The store's lock must be held for the whole run, which Open
// guarantees.
func Run(ctx context.Context, s *plan.Store, id string, m Machine) (*plan.Record, error) {
	d, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	if d.State != plan.Saved {
		return nil, fmt.Errorf("%w: %s is %s", plan.ErrNotEligible, d.Plan, d.State)
	}
	if changes, err := recheck(ctx, d, m); err != nil {
		return nil, err
	} else if len(changes) > 0 {
		return reject(s, d, changes)
	}
	// Resolve every performer before anything changes the machine, so an
	// unknown operation is a rejection rather than a partial run.
	steps := make([]Step, len(d.Operations))
	for i, op := range d.Operations {
		st, err := m.Step(op)
		if err != nil {
			return reject(s, d, []string{fmt.Sprintf("operation %s: %v", op.ID, err)})
		}
		steps[i] = st
	}
	if err := s.Move(d, plan.Executing); err != nil {
		return nil, err
	}
	var lastMode any
	for i, op := range d.Operations {
		if err := s.Append(plan.Record{Plan: d.Plan, Operation: op.ID, Action: op.Action, Outcome: plan.OutcomeStarted}); err != nil {
			return nil, err
		}
		ev, err := steps[i].Apply(ctx)
		if err == nil {
			var vev Evidence
			vev, err = steps[i].Verify(ctx)
			if m, ok := vev["mode"]; ok {
				if ev == nil {
					ev = Evidence{}
				}
				if _, has := ev["mode"]; !has {
					ev["mode"] = m
				}
				lastMode = m
			} else if m, ok := ev["mode"]; ok {
				lastMode = m
			}
			ev = merge(ev, "verify", vev)
		}
		if err == nil {
			err = ctx.Err()
		}
		if err != nil {
			return fail(s, d, op, ev, err)
		}
		if err := s.Append(plan.Record{Plan: d.Plan, Operation: op.ID, Action: op.Action, Outcome: plan.OutcomeOK, Evidence: ev}); err != nil {
			return nil, err
		}
	}
	recEv := Evidence{"operations": len(d.Operations)}
	if lastMode != nil {
		recEv["mode"] = lastMode
	}
	r := plan.Record{Plan: d.Plan, Action: plan.ActionReceipt, Outcome: plan.OutcomeApplied, State: plan.Applied,
		Evidence: recEv}
	if err := s.Append(r); err != nil {
		return nil, err
	}
	if err := s.Move(d, plan.Applied); err != nil {
		return nil, err
	}
	return &r, nil
}

func merge(ev Evidence, key string, v Evidence) Evidence {
	if ev == nil {
		ev = Evidence{}
	}
	if v != nil {
		ev[key] = v
	}
	return ev
}

// recheck compares the saved bindings with the machine now and
// re-evaluates every precondition, returning each difference.
func recheck(ctx context.Context, d *plan.Document, m Machine) ([]string, error) {
	now, err := m.Bindings(ctx, d)
	if err != nil {
		return nil, err
	}
	was := d.Bindings
	var c []string
	for _, f := range []struct{ name, was, now string }{
		{"machine", was.Machine, now.Machine},
		{"platform", was.Platform, now.Platform},
		{"inputs", was.InputsDigest, now.InputsDigest},
	} {
		if f.was != f.now {
			c = append(c, fmt.Sprintf("%s changed from %s to %s", f.name, f.was, f.now))
		}
	}
	c = append(c, diffPairs("asset", assetMap(was.Assets), assetMap(now.Assets))...)
	c = append(c, diffPairs("provider", providerMap(was.Providers), providerMap(now.Providers))...)
	for _, p := range d.Preconditions {
		if observed, err := m.Check(ctx, p); err != nil {
			c = append(c, fmt.Sprintf("precondition %s %s %s no longer holds (observed %s): %v", p.Kind, p.Subject, p.Requirement, observed, err))
		}
	}
	return c, nil
}

func assetMap(a []plan.Asset) map[string]string {
	m := map[string]string{}
	for _, x := range a {
		m[x.Name] = x.SHA256
	}
	return m
}

func providerMap(p []plan.Provider) map[string]string {
	m := map[string]string{}
	for _, x := range p {
		m[x.Name] = x.Version
	}
	return m
}

func diffPairs(kind string, was, now map[string]string) []string {
	var c []string
	for k, v := range was {
		switch n, ok := now[k]; {
		case !ok:
			c = append(c, fmt.Sprintf("%s %s is gone", kind, k))
		case n != v:
			c = append(c, fmt.Sprintf("%s %s changed from %s to %s", kind, k, v, n))
		}
	}
	for k := range now {
		if _, ok := was[k]; !ok {
			c = append(c, fmt.Sprintf("%s %s is new", kind, k))
		}
	}
	sort.Strings(c)
	return c
}

func reject(s *plan.Store, d *plan.Document, changes []string) (*plan.Record, error) {
	r := plan.Record{Plan: d.Plan, Operation: plan.OperationBindings, Action: plan.ActionReceipt, Outcome: plan.OutcomeRejected,
		State: plan.Rejected, Evidence: Evidence{"changes": changes}}
	if err := s.Append(r); err != nil {
		return nil, err
	}
	if err := s.Move(d, plan.Rejected); err != nil {
		return nil, err
	}
	return &r, &Rejected{Changes: changes}
}

func fail(s *plan.Store, d *plan.Document, op plan.Operation, ev Evidence, cause error) (*plan.Record, error) {
	ev = merge(ev, "", nil)
	ev["error"] = cause.Error()
	outcome := plan.OutcomeFailed
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		outcome = plan.OutcomeInterrupted
	}
	if err := s.Append(plan.Record{Plan: d.Plan, Operation: op.ID, Action: op.Action, Outcome: plan.OutcomeFailed, Evidence: ev}); err != nil {
		return nil, err
	}
	r := plan.Record{Plan: d.Plan, Operation: op.ID, Action: plan.ActionReceipt, Outcome: outcome, State: plan.Failed,
		Evidence: Evidence{"failed_operation": op.ID, "not_run": notRun(d, op.ID)}}
	if err := s.Append(r); err != nil {
		return nil, err
	}
	if err := s.Move(d, plan.Failed); err != nil {
		return nil, err
	}
	return &r, &Failed{Operation: op.ID, Err: cause}
}

func notRun(d *plan.Document, failed string) []string {
	out := []string{}
	after := false
	for _, op := range d.Operations {
		if after {
			out = append(out, op.ID)
		}
		if op.ID == failed {
			after = true
		}
	}
	return out
}
