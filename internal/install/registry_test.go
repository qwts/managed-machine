package install

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
)

// fakeProvider is a provider other than brew, driven through the registry
// the way every S3 provider is.
type fakeProvider struct {
	version string
	got     provider.Request
	applied []string
}

func (f *fakeProvider) Name() string                            { return "fake" }
func (f *fakeProvider) Version(context.Context) (string, error) { return f.version, nil }
func (f *fakeProvider) Plan(_ context.Context, r provider.Request) (*provider.Resolution, error) {
	f.got = r
	return &provider.Resolution{Package: "pkg", Version: "1.2.3", ProviderVersion: f.version,
		Preconditions: []plan.Precondition{{Kind: "software", Subject: "fake", Requirement: "present", Observed: f.version}},
		Operations: []plan.Operation{{ID: "1", Operation: "software:" + r.Software, Target: r.Target, Action: "create", Provider: "fake",
			Params: map[string]string{"package": "pkg", "version": "1.2.3"}}}}, nil
}
func (f *fakeProvider) Check(context.Context, plan.Precondition) (string, error) {
	return f.version, nil
}
func (f *fakeProvider) Step(op plan.Operation) (execute.Step, error) {
	return fakeStep{f, op.Params["version"]}, nil
}

type fakeStep struct {
	f *fakeProvider
	v string
}

func (s fakeStep) Apply(context.Context) (execute.Evidence, error) {
	s.f.applied = append(s.f.applied, s.v)
	return execute.Evidence{"applied": s.v}, nil
}
func (s fakeStep) Verify(context.Context) (execute.Evidence, error) {
	return execute.Evidence{"version": s.v}, nil
}

func TestRegistryDrivesAnyProvider(t *testing.T) {
	f := newFixture(t)
	fp := &fakeProvider{version: "9.9"}
	env := f.env()
	env.Provider = func(name, _ string) provider.Provider {
		if name == "fake" {
			return fp
		}
		return nil
	}
	root := filepath.Join(t.TempDir(), "root")
	req := request("fake", "custom:"+root)
	res, err := Run(context.Background(), env, req)
	if err != nil || res.Outcome != "ok" || res.ResolvedVersion != "1.2.3" || len(fp.applied) != 1 {
		t.Fatalf("fake install: %+v %v applied=%v", res, err, fp.applied)
	}
	if fp.got.Target != "custom:"+root || fp.got.Options["formula"] != "ripgrep" {
		t.Errorf("the provider saw %+v", fp.got)
	}
	d, err := f.store().Load(res.Plan)
	if err != nil || len(d.Bindings.Providers) != 1 || d.Bindings.Providers[0] != (plan.Provider{Name: "fake", Version: "9.9"}) || d.State != plan.Applied {
		t.Fatalf("plan: %+v %v", d, err)
	}

	// A changed provider version rejects the next plan before it runs.
	f2 := newFixture(t)
	fp2 := &fakeProvider{version: "1.0"}
	env2 := f2.env()
	env2.Provider = func(string, string) provider.Provider { return fp2 }
	orig := env2.Machine
	env2.Machine = func(ctx context.Context) (string, error) {
		m, err := orig(ctx)
		// Machine is read once while planning and again before apply;
		// the provider changes in between.
		fp2.version = "2.0"
		return m, err
	}
	_, err = Run(context.Background(), env2, request("fake", "user"))
	var rej *Rejected
	if !errors.As(err, &rej) || len(fp2.applied) != 0 {
		t.Fatalf("changed provider version: %v applied=%v", err, fp2.applied)
	}

	// A provider without an executor is unavailable before any state.
	f3 := newFixture(t)
	env3 := f3.env()
	env3.Provider = func(string, string) provider.Provider { return nil }
	if _, err := Run(context.Background(), env3, request("cargo", "user")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("no executor: %v", err)
	}
	f3.noState()
}
