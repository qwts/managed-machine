//go:build !darwin

package appcopy

import (
	"context"
	"net/http"

	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
)

// Provider is the non-darwin stub for app-copy: every execution entrypoint
// refuses with ErrUnsupported on non-darwin targets.
type Provider struct {
	Run      Runner
	Environ  func() []string
	Client   *http.Client
	StateDir string
	PlanID   string
	AppDir   string
	Writable func(path string) bool
	URLHosts []string
}

// Name returns the provider name recipes use.
func (p *Provider) Name() string { return Name }

// Version refuses on non-darwin targets.
func (p *Provider) Version(context.Context) (string, error) {
	return "", ErrUnsupported
}

// Plan validates options and refuses on non-darwin targets.
func (p *Provider) Plan(_ context.Context, r Request) (*Resolution, error) {
	if _, err := ParseOptions(r.Options); err != nil {
		return nil, err
	}
	return nil, ErrUnsupported
}

// Check refuses on non-darwin targets.
func (p *Provider) Check(context.Context, plan.Precondition) (string, error) {
	return "", ErrUnsupported
}

// Step refuses on non-darwin targets.
func (p *Provider) Step(plan.Operation) (execute.Step, error) {
	return nil, ErrUnsupported
}

var _ provider.Provider = (*Provider)(nil)
