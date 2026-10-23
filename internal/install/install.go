// Package install performs `software --install` under the shared execution
// model (S2 story E03): the selected recipe is planned read-only by its
// provider, the plan is saved, and the executor applies exactly that plan.
// Nothing is written before planning succeeds, so a rejected request never
// creates state.
package install

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/source"
)

// Command is the plan command recorded for software installs.
const Command = "software --install"

// Env is what an install consults. Every field is a seam for tests.
type Env struct {
	StateDir func() (string, error)
	Account  func() (string, error)
	Machine  func(ctx context.Context) (string, error)
	Now      func() time.Time
	// Provider returns the implementation of a recipe provider for the
	// host target, or nil when that provider has no executor yet.
	Provider func(name, target string) provider.Provider
}

// Request is one install: the catalog entry, the selected recipe, the
// target (global, user or custom:<path>) and the host's canonical target.
type Request struct {
	Entry    source.Entry
	Recipe   definition.Recipe
	Target   string
	Platform string
}

// Result is the installation result record's content.
type Result struct {
	ResolvedVersion string
	Outcome         string // ok, or unchanged when already installed
	Plan            string
}

// Errors the software command maps to diagnostics.
var (
	ErrUnavailable = errors.New("installation through this provider is not available in this release")
	ErrUnsupported = provider.ErrUnsupported
	ErrPrivilege   = provider.ErrPrivilege
	ErrMissing     = provider.ErrMissing
	ErrBusy        = plan.ErrBusy
)

// Failed is an install whose plan ran and failed; Plan names it.
type Failed struct {
	Plan string
	Err  error
}

func (f *Failed) Error() string { return fmt.Sprintf("plan %s failed: %v", f.Plan, f.Err) }
func (f *Failed) Unwrap() error { return f.Err }

// Rejected is a saved plan the machine no longer matched.
type Rejected struct {
	Plan    string
	Changes []string
}

func (r *Rejected) Error() string {
	return fmt.Sprintf("plan %s rejected: %s", r.Plan, strings.Join(r.Changes, "; "))
}

// Run plans, saves and applies one install.
func Run(ctx context.Context, env Env, r Request) (Result, error) {
	p := env.Provider(r.Recipe.Provider, r.Platform)
	if p == nil {
		// There is no fallback to a provider the user did not select.
		return Result{}, fmt.Errorf("%w: provider %s has no executor in this release", ErrUnavailable, r.Recipe.Provider)
	}
	opts := map[string]string{}
	for k, v := range r.Recipe.Options {
		opts[k] = fmt.Sprint(v)
	}
	res, err := p.Plan(ctx, provider.Request{Software: r.Entry.Software.ID, Target: r.Target, Options: opts})
	if err != nil {
		return Result{}, err
	}
	account, err := env.Account()
	if err != nil {
		return Result{}, err
	}
	machine, err := env.Machine(ctx)
	if err != nil {
		return Result{}, err
	}
	now := env.Now().UTC().Truncate(time.Second)
	id, err := plan.NewID(now)
	if err != nil {
		return Result{}, err
	}
	b := bindings(r, machine, p.Name(), res.ProviderVersion)
	d := &plan.Document{
		Plan: id, SchemaVersion: plan.SchemaVersion, Command: Command, Software: r.Entry.Software.ID, Target: r.Target,
		Account: account, SourceRevision: r.Entry.Source.Location + "@" + r.Entry.Revision, CreatedAt: now.Format(time.RFC3339),
		State: plan.Saved, Bindings: b,
		Preconditions: append([]plan.Precondition{{Kind: "platform", Subject: "host", Requirement: r.Platform, Observed: r.Platform}}, res.Preconditions...),
		Operations:    res.Operations,
	}
	if d.Operations == nil {
		d.Operations = []plan.Operation{}
	}
	dir, err := env.StateDir()
	if err != nil {
		return Result{}, err
	}
	st, err := plan.Open(dir, env.Now)
	if err != nil {
		return Result{}, err
	}
	defer st.Close()
	if err := st.Save(d); err != nil {
		return Result{}, err
	}
	m := &machineNow{env: env, req: r, p: p, platform: r.Platform}
	if _, err := execute.Run(ctx, st, d.Plan, m); err != nil {
		var rej *execute.Rejected
		var fl *execute.Failed
		switch {
		case errors.As(err, &rej):
			return Result{Plan: d.Plan}, &Rejected{Plan: d.Plan, Changes: rej.Changes}
		case errors.As(err, &fl):
			return Result{Plan: d.Plan}, &Failed{Plan: d.Plan, Err: fl.Err}
		}
		return Result{Plan: d.Plan}, err
	}
	out := Result{ResolvedVersion: res.Version, Outcome: "ok", Plan: d.Plan}
	if res.Installed {
		out.Outcome = "unchanged"
	}
	return out, nil
}

// InputsDigest is the digest of everything the plan was resolved from, so
// a plan for other inputs is never eligible.
func InputsDigest(r Request) string {
	parts := []string{"software=" + r.Entry.Software.ID, "recipe=" + r.Recipe.ID, "provider=" + r.Recipe.Provider,
		"package=" + r.Recipe.Package, "target=" + r.Target, "platform=" + r.Platform, "definition=" + r.Entry.Digest}
	keys := make([]string, 0, len(r.Recipe.Options))
	for k := range r.Recipe.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, "option."+k+"="+fmt.Sprint(r.Recipe.Options[k]))
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:])
}

func bindings(r Request, machine, name, version string) plan.Bindings {
	return plan.Bindings{
		Machine: machine, Platform: r.Platform, InputsDigest: InputsDigest(r),
		Assets:    []plan.Asset{{Name: r.Entry.Source.Location + ":" + r.Entry.File, SHA256: strings.TrimPrefix(r.Entry.Digest, "sha256:")}},
		Providers: []plan.Provider{{Name: name, Version: version}},
	}
}

// machineNow re-derives the bindings and preconditions at apply time.
type machineNow struct {
	env      Env
	req      Request
	p        provider.Provider
	platform string
}

func (m *machineNow) Bindings(ctx context.Context, d *plan.Document) (plan.Bindings, error) {
	machine, err := m.env.Machine(ctx)
	if err != nil {
		return plan.Bindings{}, err
	}
	v, err := m.p.Version(ctx)
	if err != nil {
		v = "absent"
	}
	return bindings(m.req, machine, m.p.Name(), v), nil
}

func (m *machineNow) Check(ctx context.Context, c plan.Precondition) (string, error) {
	if c.Kind == "platform" {
		if c.Requirement != m.platform {
			return m.platform, fmt.Errorf("the host is %s", m.platform)
		}
		return m.platform, nil
	}
	return m.p.Check(ctx, c)
}

func (m *machineNow) Step(op plan.Operation) (execute.Step, error) { return m.p.Step(op) }
