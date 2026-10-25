//go:build !darwin

package appcopy

import (
	"context"
	"errors"
	"testing"

	"github.com/qwts/managed-machine/internal/plan"
)

func TestNonDarwinStubRefuses(t *testing.T) {
	p := &Provider{}
	if p.Name() != Name {
		t.Fatalf("Name = %q, want %q", p.Name(), Name)
	}
	if _, err := p.Version(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Version: %v", err)
	}
	if _, err := p.Plan(context.Background(), Request{Target: "global", Options: validOptionMap()}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Plan: %v", err)
	}
	if _, err := p.Check(context.Background(), plan.Precondition{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Check: %v", err)
	}
	if _, err := p.Step(plan.Operation{}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Step: %v", err)
	}
}
