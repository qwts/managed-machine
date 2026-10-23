// Package template is the rendered whole-file configuration provider (S4
// story C02): it renders the operation's optional settings map first against
// the resolved inputs and destinations, then renders the template source
// asset with inputs, destinations and those settings through the logic-free
// substitution engine (D4). Like the file provider it owns the whole
// destination file, refuses to replace an unowned file, plans no operation
// when the destination already matches the rendered bytes, and independently
// verifies the written digest before recording ownership.
package template

import (
	"fmt"
	"io/fs"
	"path"
	"sort"

	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/config/provider/file"
	"github.com/qwts/managed-machine/internal/config/render"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
)

// Name is the provider name operations declare.
const Name = "template"

// Version is the provider version recorded in plan bindings.
const Version = "1"

// Provider is the template configuration provider.
type Provider struct{}

// New returns the template provider.
func New() *Provider { return &Provider{} }

func (*Provider) Name() string    { return Name }
func (*Provider) Version() string { return Version }

// Plan renders the template source and inspects the destination without
// changing it. The raw template source asset is pinned by digest in the
// returned assets, while the planned operation carries the rendered bytes
// and their digest.
func (p *Provider) Plan(fsys fs.FS, dir string, op plan.Operation, v render.Values, owned *ledger.Snapshot) ([]plan.Asset, []plan.Precondition, []plan.Operation, error) {
	raw, rendered, err := renderSource(fsys, dir, op.Params["source"], v)
	if err != nil {
		return nil, nil, nil, err
	}
	assets := []plan.Asset{{Name: op.Params["source"], SHA256: file.SHA256Hex(raw)}}
	pre, ops, err := file.PlanBytes(op, rendered, owned)
	if err != nil {
		return nil, nil, nil, err
	}
	return assets, pre, ops, nil
}

// Check renders the template source and compares it with the destination on
// disk.
func (p *Provider) Check(fsys fs.FS, dir string, op plan.Operation, v render.Values) ([]ledger.Drift, error) {
	_, rendered, err := renderSource(fsys, dir, op.Params["source"], v)
	if err != nil {
		return nil, err
	}
	return file.CheckBytes(op, rendered)
}

// Step returns the performer for one planned template operation.
func (p *Provider) Step(op plan.Operation, record func(ledger.Record) error) (execute.Step, error) {
	return file.StepFor(Name, op, record)
}

// renderSource reads src from fsys/dir, renders v.Settings first against
// v.Inputs and v.Destinations, and then renders the template text against
// all three namespaces.
func renderSource(fsys fs.FS, dir, src string, v render.Values) (raw, rendered []byte, err error) {
	if fsys == nil {
		return nil, nil, fmt.Errorf("source %q: no source filesystem", src)
	}
	raw, err = fs.ReadFile(fsys, path.Join(dir, src))
	if err != nil {
		return nil, nil, fmt.Errorf("source %q: %w", src, err)
	}
	base := render.Values{
		Inputs:       v.Inputs,
		Destinations: v.Destinations,
	}
	settings := make(map[string]string, len(v.Settings))
	keys := make([]string, 0, len(v.Settings))
	for k := range v.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val, err := render.Render(v.Settings[k], base)
		if err != nil {
			return nil, nil, fmt.Errorf("settings.%s: %w", k, err)
		}
		settings[k] = val
	}
	out, err := render.Render(string(raw), render.Values{
		Inputs:       v.Inputs,
		Destinations: v.Destinations,
		Settings:     settings,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("source %q: %w", src, err)
	}
	return raw, []byte(out), nil
}
