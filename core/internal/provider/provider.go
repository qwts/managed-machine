// Package provider is the contract every installation provider implements
// on the shared executor (S3 story X01): planning is read-only and pins
// what apply will do, preconditions are re-checked before apply, and every
// operation is applied and then independently verified. A provider never
// elevates; a target it cannot honor is refused before anything is written.
package provider

import (
	"bytes"
	"context"
	"errors"
	"os/exec"

	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/plan"
)

// Provider plans and performs installs for one recipe provider name.
type Provider interface {
	// Name is the provider name recipes use.
	Name() string
	// Version reports the installed provider tool's version; an absent
	// tool is ErrMissing.
	Version(ctx context.Context) (string, error)
	// Plan resolves a request without changing the machine.
	Plan(ctx context.Context, r Request) (*Resolution, error)
	// Check re-observes one precondition the provider planned.
	Check(ctx context.Context, c plan.Precondition) (string, error)
	// Step returns the performer for one planned operation.
	Step(op plan.Operation) (execute.Step, error)
}

// Request is one installation to plan.
type Request struct {
	Software string
	// Target is global, user or custom:<absolute path>.
	Target  string
	Options map[string]string
}

// Resolution is what planning found, ready to save.
type Resolution struct {
	Package         string
	Version         string // the version that will be (or already is) installed
	Installed       bool   // already installed: the plan has no operations
	ProviderVersion string
	Preconditions   []plan.Precondition
	Operations      []plan.Operation
}

// The kinds of refusal every provider reports; the software command maps
// them to diagnostics. A provider's own errors match one with errors.Is.
var (
	ErrUnsupported = errors.New("unsupported by the provider")
	ErrPrivilege   = errors.New("the provider's target is not writable by the current user")
	ErrMissing     = errors.New("the provider is not installed")
)

// Kind returns an error with its own message that matches kind.
func Kind(kind error, msg string) error { return &kindError{kind, msg} }

type kindError struct {
	kind error
	msg  string
}

func (e *kindError) Error() string        { return e.msg }
func (e *kindError) Is(target error) bool { return target == e.kind }

// Result is one finished process.
type Result struct {
	Stdout, Stderr []byte
	Exit           int
}

// Runner starts one process: an executable path and an argv, never a
// shell string, with an explicit environment.
type Runner func(ctx context.Context, path string, args, env []string) (Result, error)

// Exec is the real Runner.
func Exec(ctx context.Context, path string, args, env []string) (Result, error) {
	c := exec.CommandContext(ctx, path, args...)
	c.Env = env
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	r := Result{Stdout: o.Bytes(), Stderr: e.Bytes()}
	var x *exec.ExitError
	if errors.As(err, &x) {
		r.Exit = x.ExitCode()
		return r, nil
	}
	return r, err
}
