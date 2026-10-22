package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/compose"
	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/block"
	"github.com/qwts/managed-machine/core/internal/config/provider/file"
	"github.com/qwts/managed-machine/core/internal/config/provider/structured"
	"github.com/qwts/managed-machine/core/internal/config/provider/template"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/config/secret"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/target"
)

// PlanCommand is the command recorded in saved configuration plans.
const PlanCommand = "config"

// Provider plans, checks and performs one configuration operation provider
// (file, template, structured, managed-block). Planning and checking are
// read-only; Step performs the planned operation and independently verifies
// the result on disk before recording ownership in the ledger.
type Provider interface {
	Name() string
	Version() string
	Plan(fsys fs.FS, dir string, op plan.Operation, v render.Values, owned *ledger.Snapshot) ([]plan.Asset, []plan.Precondition, []plan.Operation, error)
	Check(fsys fs.FS, dir string, op plan.Operation, v render.Values) ([]ledger.Drift, error)
	Step(op plan.Operation, record func(ledger.Record) error) (execute.Step, error)
}

var (
	_ Provider = (*block.Provider)(nil)
	_ Provider = (*file.Provider)(nil)
	_ Provider = (*template.Provider)(nil)
	_ Provider = (*structured.Provider)(nil)
)

// DefaultProvider returns the built-in configuration provider for name, or
// nil when that provider has no executor yet.
func DefaultProvider(name string) Provider {
	switch name {
	case ProviderFile:
		return file.New()
	case ProviderTemplate:
		return template.New()
	case ProviderManagedBlock:
		return block.New()
	case ProviderStructured:
		return structured.New()
	}
	return nil
}

// prepared holds the validated catalog entry, resolved desired state and
// target metadata shared by --plan, --apply and --check.
type prepared struct {
	entry        Entry
	def          Definition
	desired      Desired
	resolved     Resolved
	composed     compose.Result
	secretAssets []plan.Asset
	secretInputs map[string]string
	secretValues []string
	targetKind   string
	customRoot   string
	target       string
	platform     target.Target
}

func (d Deps) getenv(k string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(k)
}

func (d Deps) environ() []string {
	if d.Environ != nil {
		return d.Environ()
	}
	var env []string
	for _, k := range []string{"PATH", "PATHEXT"} {
		if v := d.getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func (d Deps) goos() string {
	if d.GOOS != "" {
		return d.GOOS
	}
	return runtime.GOOS
}

func (d Deps) stateDir() (string, error) {
	if d.StateDir != nil {
		return d.StateDir()
	}
	return statedir.Path(d.goos(), d.getenv)
}

func (d Deps) account() (string, error) {
	if d.Account != nil {
		return d.Account()
	}
	return "default", nil
}

func (d Deps) machine(ctx context.Context) (string, error) {
	if d.Machine != nil {
		return d.Machine(ctx)
	}
	return "local", nil
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) newPlanID(t time.Time) (string, error) {
	if d.NewPlanID != nil {
		return d.NewPlanID(t)
	}
	return plan.NewID(t)
}

func (d Deps) provider(name string) Provider {
	if d.Provider != nil {
		return d.Provider(name)
	}
	return DefaultProvider(name)
}

func (d Deps) isWritable(destPath string) bool {
	if d.Writable != nil {
		return d.Writable(destPath)
	}
	return writableDestination(destPath)
}

func (d Deps) isWritableDir(dirPath string) bool {
	if d.Writable != nil {
		return d.Writable(dirPath)
	}
	return writableDir(dirPath)
}

func (d Deps) resolveSecret(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", &secret.UnresolvedError{Name: name}
	}
	if d.Secrets != nil {
		return d.Secrets(ctx, name)
	}
	return secret.NewResolver(d.getenv, secret.Pass(nil, d.environ)).Resolve(ctx, name)
}

func detectHost(ctx cli.Context) (target.Host, *diag.Diagnostic) {
	detect := ctx.Detect
	if detect == nil {
		detect = target.Detect
	}
	host, err := detect()
	if err != nil {
		id := diag.PlatformProbeFailed
		var unsupported *target.UnsupportedError
		if errors.As(err, &unsupported) {
			id = diag.PlatformUnsupported
		}
		dg := diag.New(id, err.Error())
		return target.Host{}, &dg
	}
	return host, nil
}

// lookupEntry validates the catalog, category, configuration name and
// target/platform support without reading desired.yaml or opening the plan
// store.
func lookupEntry(d Deps, ctx cli.Context, f flags) (Entry, target.Target, string, string, *diag.Diagnostic) {
	fail := func(id, format string, args ...any) (Entry, target.Target, string, string, *diag.Diagnostic) {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return Entry{}, "", "", "", &dg
	}
	host, dg := detectHost(ctx)
	if dg != nil {
		return Entry{}, "", "", "", dg
	}
	entries, err := d.Catalog()
	if err != nil {
		return fail(Diagnostic(err), "%s", catalogMessage(err))
	}
	if f.category != "" && !anyCategory(entries, f.category) {
		return fail(diag.Usage, "no configuration carries category %q", f.category)
	}
	var entry Entry
	found := false
	for _, e := range entries {
		if e.Definition.ID == f.name {
			entry = e
			found = true
			break
		}
	}
	if !found {
		return fail(diag.ConfigUnknownName, "%q is not in the catalog", f.name)
	}
	if f.category != "" && !contains(entry.Definition.Categories, f.category) {
		return fail(diag.ConfigUnsupportedCombo, "%s is not in category %s", f.name, f.category)
	}
	tgtKind := f.target()
	var customRoot string
	tgt := tgtKind
	if f.hasCustom {
		abs, err := filepath.Abs(f.custom)
		if err != nil {
			return fail(diag.Usage, "--custom %q: %v", f.custom, err)
		}
		customRoot = abs
		tgt = output.CustomTarget(abs)
	}
	if !entry.Definition.Supports(tgtKind, host.Target) {
		ue := &UnsupportedError{Target: tgtKind, Platform: string(host.Target)}
		return fail(diag.ConfigUnsupportedCombo, "%s", ue.Error())
	}
	return entry, host.Target, customRoot, tgt, nil
}

// prepare loads the catalog entry and resolves desired.yaml and secrets.
func prepare(ctx context.Context, d Deps, cliCtx cli.Context, f flags) (*prepared, *diag.Diagnostic) {
	entry, platform, customRoot, tgt, dg := lookupEntry(d, cliCtx, f)
	if dg != nil {
		return nil, dg
	}
	tgtKind := f.target()
	desiredPath, err := DesiredPath(d.getenv, d.goos(), tgtKind, customRoot)
	if err != nil {
		out := diag.New(Diagnostic(err), err.Error())
		return nil, &out
	}
	desired, err := LoadDesired(desiredPath)
	if err != nil {
		out := diag.New(Diagnostic(err), err.Error())
		return nil, &out
	}
	def := entry.Definition
	if d.Compose != nil {
		def, err = d.Compose(def, desired)
		if err != nil {
			out := diag.New(Diagnostic(err), err.Error())
			return nil, &out
		}
	}
	resolved, err := Resolve(def, desired, Request{Target: tgtKind, Root: customRoot, Platform: platform})
	if err != nil {
		out := diag.New(Diagnostic(err), err.Error())
		return nil, &out
	}
	// Resolve secret:<name> inputs through the C05/C06 secret seam.
	var secretAssets []plan.Asset
	secretInputs := map[string]string{}
	var secretValues []string
	seenSecrets := map[string]string{}
	for _, k := range sortedKeys(resolved.Inputs) {
		val := resolved.Inputs[k]
		secName, ok := secret.ParseRef(val)
		if !ok {
			continue
		}
		secVal, ok := seenSecrets[secName]
		if !ok {
			var err error
			secVal, err = d.resolveSecret(ctx, secName)
			if err != nil || secVal == "" {
				out := diag.New(diag.ConfigSecretUnresolved, fmt.Sprintf("input %q: %s could not be resolved", k, secret.Ref(secName)))
				return nil, &out
			}
			seenSecrets[secName] = secVal
			secretAssets = append(secretAssets, secret.Asset(secName, secVal))
			secretValues = append(secretValues, secVal)
		}
		secretInputs[k] = secName
		resolved.Inputs[k] = secVal
	}
	prep := &prepared{
		entry:        entry,
		def:          def,
		desired:      desired,
		resolved:     resolved,
		secretAssets: secretAssets,
		secretInputs: secretInputs,
		secretValues: secretValues,
		targetKind:   tgtKind,
		customRoot:   customRoot,
		target:       tgt,
		platform:     platform,
	}
	if dg := composePrepared(ctx, d, prep); dg != nil {
		return nil, dg
	}
	return prep, nil
}

func (p *prepared) inputsDigest() string {
	if len(p.secretAssets) == 0 {
		return InputsDigest(p.def.ID, p.target, string(p.platform), p.resolved, p.desired.Flavors)
	}
	assetDigests := make(map[string]string, len(p.secretAssets))
	for _, a := range p.secretAssets {
		assetDigests[a.Name] = a.SHA256
	}
	inputs := make(map[string]string, len(p.resolved.Inputs))
	for k, v := range p.resolved.Inputs {
		if secName, ok := p.secretInputs[k]; ok {
			ref := secret.Ref(secName)
			inputs[k] = ref + ":" + assetDigests[ref]
		} else {
			inputs[k] = v
		}
	}
	return InputsDigest(p.def.ID, p.target, string(p.platform), Resolved{
		Inputs:       inputs,
		Destinations: p.resolved.Destinations,
	}, p.desired.Flavors)
}

func (p *prepared) activationInputs() map[string]string {
	if len(p.secretInputs) == 0 {
		return p.resolved.Inputs
	}
	out := make(map[string]string, len(p.resolved.Inputs))
	for k, v := range p.resolved.Inputs {
		if secName, ok := p.secretInputs[k]; ok {
			out[k] = secret.Ref(secName)
		} else {
			out[k] = v
		}
	}
	return out
}

// InputsDigest returns the SHA-256 digest of the resolved desired inputs,
// destination mappings and flavors for a configuration request.
func InputsDigest(defID, target, platform string, resolved Resolved, flavors []string) string {
	parts := []string{
		"config=" + defID,
		"target=" + target,
		"platform=" + platform,
	}
	for _, k := range sortedKeys(resolved.Inputs) {
		parts = append(parts, "input."+k+"="+resolved.Inputs[k])
	}
	for _, k := range sortedKeys(resolved.Destinations) {
		parts = append(parts, "destination."+k+"="+resolved.Destinations[k])
	}
	for _, fl := range flavors {
		parts = append(parts, "flavor="+fl)
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:])
}

// planRun implements `config <name> --plan`.
func planRun(d Deps, cliCtx cli.Context, f flags) cli.Result {
	ctx := context.Background()
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	prep, dg := prepare(ctx, d, cliCtx, f)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	// Check writability of every mapped destination before touching state.
	for _, op := range prep.def.Operations {
		destPath := prep.resolved.Destinations[op.Destination]
		if !d.isWritable(destPath) {
			return fail(diag.ConfigPrivilegeRequired, "destination %s is not writable by the current user", destPath)
		}
	}
	if prep.customRoot != "" {
		customLedger := filepath.Join(prep.customRoot, ".managed-machine", ledger.FileName)
		if !d.isWritable(customLedger) {
			return fail(diag.ConfigPrivilegeRequired, "ledger %s is not writable by the current user", customLedger)
		}
	}
	stateDir, err := d.stateDir()
	if err != nil {
		return fail(diag.ConfigDesiredInvalid, "%v", err)
	}
	account, err := d.account()
	if err != nil {
		return fail(diag.ConfigApplyFailed, "%v", err)
	}
	machine, err := d.machine(ctx)
	if err != nil {
		return fail(diag.ConfigApplyFailed, "%v", err)
	}
	ledgerPath, err := ledger.Path(stateDir, prep.target)
	if err != nil {
		return fail(diag.ConfigDesiredInvalid, "%v", err)
	}
	snap, err := ledger.Load(ledgerPath)
	if err != nil {
		return fail(diag.ConfigApplyFailed, "reading ownership ledger: %v", err)
	}
	for _, old := range snap.OwnedBy(prep.def.ID, prep.target) {
		if !d.isWritable(old.Path) {
			return fail(diag.ConfigPrivilegeRequired, "destination %s is not writable by the current user", old.Path)
		}
	}

	preconditions := []plan.Precondition{{
		Kind:        "platform",
		Subject:     "host",
		Requirement: string(prep.platform),
		Observed:    string(prep.platform),
	}}
	var plannedOps []plan.Operation
	seenIDs := map[string]bool{}
	for _, op := range prep.def.Operations {
		seenIDs[op.ID] = true
	}

	// Check requirements (command on PATH or custom root's bin, else
	// applied software --install receipt for the same target and account).
	for _, req := range prep.def.Requires {
		subj := reqSubject(req)
		if obs, ok := checkRequirement(d, stateDir, prep.target, prep.customRoot, account, req); ok {
			preconditions = append(preconditions, plan.Precondition{
				Kind:        "software",
				Subject:     subj,
				Requirement: "present",
				Observed:    obs,
			})
		} else {
			opID := subj
			if seenIDs[opID] {
				opID = "requires:" + subj
			}
			seenIDs[opID] = true
			plannedOps = append(plannedOps, plan.Operation{
				ID:        opID,
				Operation: subj,
				Target:    prep.target,
				Action:    "missing-prerequisite " + subj,
				Provider:  "requires",
				Params:    map[string]string{"prerequisite": subj},
			})
		}
	}

	var extraAssets []plan.Asset
	providerMap := map[string]string{}
	seenDirPre := map[string]bool{}
	currentOps := map[string]bool{}
	for _, op := range prep.def.Operations {
		currentOps[op.ID] = true
		prov := d.provider(op.Provider)
		if prov == nil {
			return fail(diag.ConfigUnsupportedCombo, "provider %q has no executor in this release", op.Provider)
		}
		providerMap[prov.Name()] = prov.Version()
		destPath := prep.resolved.Destinations[op.Destination]
		dir := filepath.Dir(destPath)
		if !seenDirPre[dir] {
			seenDirPre[dir] = true
			preconditions = append(preconditions, plan.Precondition{
				Kind:        "privilege",
				Subject:     dir,
				Requirement: "writable",
				Observed:    "writable",
			})
		}
		inOp := opToPlanInput(prep.def.ID, prep.target, op, destPath)
		vals := render.Values{
			Inputs:       prep.resolved.Inputs,
			Destinations: prep.resolved.Destinations,
			Settings:     op.Settings,
		}
		assets, pre, ops, err := prov.Plan(prep.entry.FS, prep.entry.Dir, inOp, vals, snap)
		if err != nil {
			return fail(diag.ConfigSourceInvalid, "%v", err)
		}
		extraAssets = append(extraAssets, assets...)
		if conflictOp, ok := prep.conflictPlanOp(op, destPath); ok {
			seenIDs[conflictOp.ID] = true
			plannedOps = append(plannedOps, conflictOp)
			continue
		}
		preconditions = append(preconditions, pre...)
		for _, planned := range ops {
			seenIDs[planned.ID] = true
			plannedOps = append(plannedOps, sanitizePlanOp(planned, prep.secretValues))
		}
	}
	for _, extra := range prep.extraConflictOps(seenIDs) {
		plannedOps = append(plannedOps, sanitizePlanOp(extra, prep.secretValues))
	}

	// Plan delete operations for owned ledger records of this config whose
	// operation id no longer exists in the definition (D3).
	for _, old := range snap.OwnedBy(prep.def.ID, prep.target) {
		if currentOps[old.Operation] {
			continue
		}
		provName := providerForKind(old.Kind)
		prov := d.provider(provName)
		if prov == nil {
			return fail(diag.ConfigUnsupportedCombo, "provider %q has no executor in this release", provName)
		}
		providerMap[prov.Name()] = prov.Version()
		dir := filepath.Dir(old.Path)
		if !seenDirPre[dir] {
			seenDirPre[dir] = true
			preconditions = append(preconditions, plan.Precondition{
				Kind:        "privilege",
				Subject:     dir,
				Requirement: "writable",
				Observed:    "writable",
			})
		}
		if old.Kind == ledger.KindFile {
			if cur, err := os.ReadFile(old.Path); err == nil {
				sum := "sha256:" + file.SHA256Hex(cur)
				preconditions = append(preconditions, plan.Precondition{
					Kind:        "target",
					Subject:     old.Path,
					Requirement: sum,
					Observed:    sum,
				})
			}
		}
		delID := old.Operation
		if seenIDs[delID] {
			delID = "delete:" + old.Operation + ":" + old.ID
		}
		seenIDs[delID] = true
		plannedOps = append(plannedOps, plan.Operation{
			ID:        delID,
			Operation: old.Operation,
			Target:    prep.target,
			Action:    "delete",
			Provider:  provName,
			Params: map[string]string{
				"config": old.Config,
				"path":   old.Path,
				"kind":   old.Kind,
				"id":     old.ID,
				"format": old.Format,
			},
		})
	}
	if plannedOps == nil {
		plannedOps = []plan.Operation{}
	}

	now := d.now().UTC().Truncate(time.Second)
	planID, err := d.newPlanID(now)
	if err != nil {
		return fail(diag.ConfigApplyFailed, "%v", err)
	}
	bindings := buildBindings(prep, machine, extraAssets, providerMap)
	doc := &plan.Document{
		Plan:           planID,
		SchemaVersion:  plan.SchemaVersion,
		Command:        PlanCommand,
		Config:         prep.def.ID,
		Target:         prep.target,
		Account:        account,
		SourceRevision: prep.entry.Source.Location + "@" + prep.entry.Revision,
		CreatedAt:      now.Format(time.RFC3339),
		State:          plan.Saved,
		Bindings:       bindings,
		Preconditions:  preconditions,
		Operations:     plannedOps,
	}

	st, err := plan.Open(stateDir, d.now)
	if err != nil {
		if errors.Is(err, plan.ErrBusy) {
			return fail(diag.ConfigBusy, "%v", err)
		}
		return fail(diag.ConfigApplyFailed, "%v", err)
	}
	defer st.Close()
	if err := st.Save(doc); err != nil {
		return fail(diag.ConfigApplyFailed, "%v", err)
	}

	recs := make([]*output.Record, 0, 1+len(doc.Operations))
	recs = append(recs, output.New().
		Set("id", prep.def.ID).
		Set("target", prep.target).
		Set("plan", doc.Plan))
	for _, op := range doc.Operations {
		recs = append(recs, output.New().
			Set("id", prep.def.ID).
			Set("operation", op.Operation).
			Set("target", op.Target).
			Set("action", op.Action))
	}
	return cli.Result{Records: recs}
}

// applyRun implements `config <name> --apply [--from-plan <id>]`.
func applyRun(d Deps, cliCtx cli.Context, f flags) cli.Result {
	ctx := context.Background()
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	prep, dg := prepare(ctx, d, cliCtx, f)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	for _, op := range prep.def.Operations {
		destPath := prep.resolved.Destinations[op.Destination]
		if !d.isWritable(destPath) {
			return fail(diag.ConfigPrivilegeRequired, "destination %s is not writable by the current user", destPath)
		}
	}
	if prep.customRoot != "" {
		customLedger := filepath.Join(prep.customRoot, ".managed-machine", ledger.FileName)
		if !d.isWritable(customLedger) {
			return fail(diag.ConfigPrivilegeRequired, "ledger %s is not writable by the current user", customLedger)
		}
	}
	stateDir, err := d.stateDir()
	if err != nil {
		return fail(diag.ConfigDesiredInvalid, "%v", err)
	}
	account, err := d.account()
	if err != nil {
		return fail(diag.ConfigApplyFailed, "%v", err)
	}
	ledgerPath, err := ledger.Path(stateDir, prep.target)
	if err != nil {
		return fail(diag.ConfigDesiredInvalid, "%v", err)
	}
	inputsDigest := prep.inputsDigest()

	st, err := plan.Open(stateDir, d.now)
	if err != nil {
		if errors.Is(err, plan.ErrBusy) {
			return fail(diag.ConfigBusy, "%v", err)
		}
		return fail(diag.ConfigApplyFailed, "%v", err)
	}
	defer st.Close()

	doc, err := st.Select(plan.Request{
		Command:      PlanCommand,
		Name:         prep.def.ID,
		Target:       prep.target,
		Account:      account,
		InputsDigest: inputsDigest,
	}, f.fromPlan)
	if err != nil {
		switch {
		case errors.Is(err, plan.ErrMismatch):
			return fail(diag.ConfigPlanMismatch, "%v", err)
		case errors.Is(err, plan.ErrNoPlan), errors.Is(err, plan.ErrNotEligible), errors.Is(err, os.ErrNotExist):
			return fail(diag.ConfigNoPlan, "%v", err)
		default:
			return fail(diag.ConfigPlanMismatch, "%v", err)
		}
	}

	// Refuse planned missing prerequisites and conflicts before writing.
	for _, op := range doc.Operations {
		if strings.HasPrefix(op.Action, "missing-prerequisite ") || op.Params["prerequisite"] != "" {
			return fail(diag.ConfigMissingPrerequisite, "%s", op.Action)
		}
	}
	for _, op := range doc.Operations {
		if strings.HasPrefix(op.Action, "conflict ") || op.Params["conflict"] != "" {
			return fail(diag.ConfigConflict, "%s", op.Action)
		}
	}
	for _, op := range doc.Operations {
		if p := op.Params["path"]; p != "" && !d.isWritable(p) {
			return fail(diag.ConfigPrivilegeRequired, "destination %s is not writable by the current user", p)
		}
	}

	m := &configMachine{
		deps:       d,
		prep:       prep,
		stateDir:   stateDir,
		account:    account,
		ledgerPath: ledgerPath,
		store:      st,
		doc:        doc,
	}
	if _, err := execute.Run(ctx, st, doc.Plan, m); err != nil {
		var rej *execute.Rejected
		var fl *execute.Failed
		switch {
		case errors.As(err, &rej):
			return fail(diag.ConfigPlanRejected, "plan %s rejected: %s", doc.Plan, strings.Join(rej.Changes, "; "))
		case errors.As(err, &fl):
			return fail(diag.ConfigApplyFailed, "plan %s: %v", doc.Plan, fl)
		default:
			return fail(diag.ConfigApplyFailed, "%v", err)
		}
	}

	var recs []*output.Record
	for _, op := range doc.Operations {
		recs = append(recs, output.New().
			Set("id", prep.def.ID).
			Set("operation", op.Operation).
			Set("target", prep.target).
			Set("state", "applied"))
	}
	vals := render.Values{
		Inputs:       prep.activationInputs(),
		Destinations: prep.resolved.Destinations,
	}
	for _, key := range sortedKeys(prep.def.Activation) {
		rendered, err := render.Render(prep.def.Activation[key], vals)
		if err != nil {
			return fail(diag.ConfigApplyFailed, "activation.%s: %v", key, err)
		}
		recs = append(recs, output.New().
			Set("id", prep.def.ID).
			Set("target", prep.target).
			Set("activation", key).
			Set("value", rendered))
	}
	return cli.Result{Records: recs}
}

// checkRun implements `config <name> --check`.
func checkRun(d Deps, cliCtx cli.Context, f flags) cli.Result {
	ctx := context.Background()
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	prep, dg := prepare(ctx, d, cliCtx, f)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if recs, dg := prep.checkFlavorConflicts(); dg != nil {
		return cli.Result{Records: recs, Diagnostic: dg}
	}
	var recs []*output.Record
	for _, op := range prep.def.Operations {
		prov := d.provider(op.Provider)
		if prov == nil {
			return fail(diag.ConfigUnsupportedCombo, "provider %q has no executor in this release", op.Provider)
		}
		destPath := prep.resolved.Destinations[op.Destination]
		inOp := opToPlanInput(prep.def.ID, prep.target, op, destPath)
		vals := render.Values{
			Inputs:       prep.resolved.Inputs,
			Destinations: prep.resolved.Destinations,
			Settings:     op.Settings,
		}
		drifts, err := prov.Check(prep.entry.FS, prep.entry.Dir, inOp, vals)
		if err != nil {
			return fail(diag.ConfigCheckIncomplete, "%s (%s): %v", op.ID, destPath, err)
		}
		for _, dr := range drifts {
			detail := dr.Detail
			if opUsesSecret(op, dr.Subject, prep.secretInputs) || containsAnySecret(detail, prep.secretValues) {
				detail = redactDriftDetail(detail)
			}
			recs = append(recs, output.New().
				Set("id", prep.def.ID).
				Set("operation", op.ID).
				Set("target", prep.target).
				Set("drift", dr.Subject).
				Set("detail", detail))
		}
	}
	if len(recs) == 0 {
		return cli.Result{}
	}
	msg := fmt.Sprintf("%d differences", len(recs))
	if len(recs) == 1 {
		msg = "1 difference"
	}
	driftDiag := diag.New(diag.ConfigDriftFound, msg)
	return cli.Result{Records: recs, Diagnostic: &driftDiag}
}

// statusRun implements `config <name> --status`. It reads only the
// ownership ledger: it never reads desired.yaml, never reads destination
// files on disk, and never opens the plan store or creates a directory.
func statusRun(d Deps, cliCtx cli.Context, f flags) cli.Result {
	entry, _, _, tgt, dg := lookupEntry(d, cliCtx, f)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	var stateDir string
	if !f.hasCustom {
		var err error
		stateDir, err = d.stateDir()
		if err != nil {
			out := diag.New(diag.ConfigDesiredInvalid, err.Error())
			return cli.Result{Diagnostic: &out}
		}
	}
	ledgerPath, err := ledger.Path(stateDir, tgt)
	if err != nil {
		out := diag.New(diag.ConfigDesiredInvalid, err.Error())
		return cli.Result{Diagnostic: &out}
	}
	snap, err := ledger.Load(ledgerPath)
	if err != nil {
		out := diag.New(diag.ConfigApplyFailed, err.Error())
		return cli.Result{Diagnostic: &out}
	}

	def := entry.Definition
	ownedByOp := snap.OwnedByOperation(def.ID, tgt)
	latestByOp := snap.LatestByOperation(def.ID, tgt)
	seen := map[string]bool{}
	var recs []*output.Record
	for _, op := range def.Operations {
		seen[op.ID] = true
		state := "absent"
		if len(ownedByOp[op.ID]) > 0 {
			state = "applied"
		} else if last, ok := latestByOp[op.ID]; ok && last.State == ledger.Released {
			state = "released"
		}
		recs = append(recs, output.New().
			Set("id", def.ID).
			Set("operation", op.ID).
			Set("target", tgt).
			Set("state", state))
	}
	for _, r := range snap.OwnedBy(def.ID, tgt) {
		if seen[r.Operation] {
			continue
		}
		seen[r.Operation] = true
		recs = append(recs, output.New().
			Set("id", def.ID).
			Set("operation", r.Operation).
			Set("target", tgt).
			Set("state", "applied"))
	}
	return cli.Result{Records: recs}
}

type configMachine struct {
	deps       Deps
	prep       *prepared
	stateDir   string
	account    string
	ledgerPath string
	store      *plan.Store
	doc        *plan.Document
	planned    map[string]plan.Operation
}

func (m *configMachine) Bindings(ctx context.Context, d *plan.Document) (plan.Bindings, error) {
	machine, err := m.deps.machine(ctx)
	if err != nil {
		return plan.Bindings{}, err
	}
	var extraAssets []plan.Asset
	providerMap := map[string]string{}
	for _, op := range m.prep.def.Operations {
		prov := m.deps.provider(op.Provider)
		if prov != nil {
			providerMap[prov.Name()] = prov.Version()
		}
		if op.Source != "" && m.prep.entry.FS != nil {
			if raw, err := fs.ReadFile(m.prep.entry.FS, path.Join(m.prep.entry.Dir, op.Source)); err == nil {
				extraAssets = append(extraAssets, plan.Asset{Name: op.Source, SHA256: file.SHA256Hex(raw)})
			}
		}
	}
	for _, op := range d.Operations {
		if op.Provider == "requires" {
			continue
		}
		if prov := m.deps.provider(op.Provider); prov != nil {
			providerMap[prov.Name()] = prov.Version()
		}
	}
	return buildBindings(m.prep, machine, extraAssets, providerMap), nil
}

func (m *configMachine) Check(_ context.Context, c plan.Precondition) (string, error) {
	switch c.Kind {
	case "platform":
		nowPlat := string(m.prep.platform)
		if c.Requirement != nowPlat {
			return nowPlat, fmt.Errorf("the host is %s", nowPlat)
		}
		return nowPlat, nil
	case "privilege":
		if !m.deps.isWritableDir(c.Subject) {
			return "unwritable", fmt.Errorf("%s is not writable by the current user", c.Subject)
		}
		return "writable", nil
	case "software":
		for _, req := range m.prep.def.Requires {
			if reqSubject(req) == c.Subject {
				if obs, ok := checkRequirement(m.deps, m.stateDir, m.prep.target, m.prep.customRoot, m.account, req); ok {
					return obs, nil
				}
				return "absent", fmt.Errorf("prerequisite %s is not satisfied", c.Subject)
			}
		}
		if obs, ok := checkRequirement(m.deps, m.stateDir, m.prep.target, m.prep.customRoot, m.account, Requirement{Software: c.Subject, Command: c.Subject}); ok {
			return obs, nil
		}
		return "absent", fmt.Errorf("prerequisite %s is not satisfied", c.Subject)
	case "target":
		data, err := os.ReadFile(c.Subject)
		nowObs := "absent"
		if err == nil {
			nowObs = "sha256:" + file.SHA256Hex(data)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "unreadable", err
		}
		if nowObs != c.Requirement {
			return nowObs, fmt.Errorf("expected %s, found %s", c.Requirement, nowObs)
		}
		return nowObs, nil
	default:
		return c.Observed, nil
	}
}

func (m *configMachine) Step(op plan.Operation) (execute.Step, error) {
	prov := m.deps.provider(op.Provider)
	if prov == nil {
		return nil, fmt.Errorf("no provider %q", op.Provider)
	}
	hydrated, err := m.hydrateOp(op)
	if err != nil {
		return nil, err
	}
	return prov.Step(hydrated, func(r ledger.Record) error {
		r.SchemaVersion = ledger.SchemaVersion
		if r.Config == "" {
			r.Config = m.doc.Config
		}
		if r.Operation == "" {
			r.Operation = op.Operation
		}
		if r.Target == "" {
			r.Target = m.doc.Target
		}
		r.Account = m.doc.Account
		r.Plan = m.doc.Plan
		r.At = m.store.Now()
		return ledger.Append(m.ledgerPath, r)
	})
}

func (m *configMachine) hydrateOp(op plan.Operation) (plan.Operation, error) {
	if op.Action == "delete" {
		return op, nil
	}
	if m.planned == nil {
		snap, err := ledger.Load(m.ledgerPath)
		if err != nil {
			return plan.Operation{}, err
		}
		m.planned = map[string]plan.Operation{}
		for _, defOp := range m.prep.def.Operations {
			prov := m.deps.provider(defOp.Provider)
			if prov == nil {
				continue
			}
			destPath := m.prep.resolved.Destinations[defOp.Destination]
			inOp := opToPlanInput(m.prep.def.ID, m.prep.target, defOp, destPath)
			vals := render.Values{
				Inputs:       m.prep.resolved.Inputs,
				Destinations: m.prep.resolved.Destinations,
				Settings:     defOp.Settings,
			}
			_, _, ops, err := prov.Plan(m.prep.entry.FS, m.prep.entry.Dir, inOp, vals, snap)
			if err != nil {
				return plan.Operation{}, err
			}
			for _, live := range ops {
				m.planned[live.ID] = live
			}
		}
	}
	live, ok := m.planned[op.ID]
	if !ok {
		if op.Params["content"] != "" {
			return op, nil
		}
		return plan.Operation{}, fmt.Errorf("operation %s is no longer applicable", op.ID)
	}
	if op.Params["sha256"] != "" && live.Params["sha256"] != "" && op.Params["sha256"] != live.Params["sha256"] {
		return plan.Operation{}, fmt.Errorf("digest changed from %s to %s", op.Params["sha256"], live.Params["sha256"])
	}
	params := make(map[string]string, len(live.Params)+len(op.Params))
	for k, v := range live.Params {
		params[k] = v
	}
	for k, v := range op.Params {
		params[k] = v
	}
	op.Params = params
	return op, nil
}

// sanitizePlanOp strips rendered content and secret-bearing values from a
// planned operation before the plan document is saved to disk, leaving the
// operation digest-only (D5).
func sanitizePlanOp(op plan.Operation, secretValues []string) plan.Operation {
	if len(op.Params) == 0 {
		return op
	}
	params := make(map[string]string, len(op.Params))
	for k, v := range op.Params {
		if k == "content" || k == "value" || k == "keys" {
			continue
		}
		if k != "sha256" && containsAnySecret(v, secretValues) {
			continue
		}
		params[k] = v
	}
	op.Params = params
	return op
}

func containsAnySecret(s string, secretValues []string) bool {
	for _, sec := range secretValues {
		if sec != "" && strings.Contains(s, sec) {
			return true
		}
	}
	return false
}

func opUsesSecret(op Operation, subject string, secretInputs map[string]string) bool {
	if len(secretInputs) == 0 {
		return false
	}
	if op.Provider == ProviderStructured && len(op.Keys) > 0 {
		if kv, ok := op.Keys[subject]; ok {
			return valueRefsSecret(kv, secretInputs)
		}
		for _, kv := range op.Keys {
			if valueRefsSecret(kv, secretInputs) {
				return true
			}
		}
		return false
	}
	return true
}

func valueRefsSecret(kv any, secretInputs map[string]string) bool {
	switch v := kv.(type) {
	case string:
		for k := range secretInputs {
			if strings.Contains(v, k) {
				return true
			}
		}
	case []string:
		for _, item := range v {
			for k := range secretInputs {
				if strings.Contains(item, k) {
					return true
				}
			}
		}
	}
	return false
}

func redactDriftDetail(detail string) string {
	const actualPrefix = "actual="
	const desiredSep = " desired="
	if strings.HasPrefix(detail, actualPrefix) {
		rest := strings.TrimPrefix(detail, actualPrefix)
		if idx := strings.LastIndex(rest, desiredSep); idx >= 0 {
			act := rest[:idx]
			des := rest[idx+len(desiredSep):]
			if !strings.HasPrefix(act, "sha256:") {
				act = "sha256:" + file.SHA256Hex([]byte(act))
			}
			if !strings.HasPrefix(des, "sha256:") {
				des = "sha256:" + file.SHA256Hex([]byte(des))
			}
			return "actual=" + act + " desired=" + des
		}
	}
	return "actual=sha256:" + file.SHA256Hex([]byte(detail)) + " desired=sha256:" + file.SHA256Hex(nil)
}

func opToPlanInput(cfgID, tgt string, op Operation, destPath string) plan.Operation {
	params := map[string]string{
		"config":      cfgID,
		"destination": op.Destination,
		"path":        destPath,
	}
	if op.Source != "" {
		params["source"] = op.Source
	}
	if op.Format != "" {
		params["format"] = op.Format
	}
	if len(op.Keys) > 0 {
		if b, err := json.Marshal(op.Keys); err == nil {
			params["keys"] = string(b)
		}
	}
	if op.BlockID != "" {
		params["block_id"] = op.BlockID
	}
	if op.Content != "" {
		params["content"] = op.Content
	}
	if op.Comment != "" {
		params["comment"] = op.Comment
	}
	return plan.Operation{
		ID:        op.ID,
		Operation: op.ID,
		Target:    tgt,
		Provider:  op.Provider,
		Params:    params,
	}
}

func buildBindings(prep *prepared, machine string, extraAssets []plan.Asset, providerMap map[string]string) plan.Bindings {
	loc := prep.entry.Source.Location
	seenAssets := map[string]string{
		loc + ":" + prep.entry.File: strings.TrimPrefix(prep.entry.Digest, "sha256:"),
	}
	for _, a := range prep.secretAssets {
		seenAssets[a.Name] = strings.TrimPrefix(a.SHA256, "sha256:")
	}
	for _, a := range extraAssets {
		name := a.Name
		if !strings.HasPrefix(name, loc+":") && !strings.HasPrefix(name, secret.Prefix) {
			name = loc + ":" + name
		}
		seenAssets[name] = strings.TrimPrefix(a.SHA256, "sha256:")
	}
	assetNames := make([]string, 0, len(seenAssets))
	for name := range seenAssets {
		assetNames = append(assetNames, name)
	}
	sort.Strings(assetNames)
	assets := make([]plan.Asset, 0, len(assetNames))
	for _, name := range assetNames {
		assets = append(assets, plan.Asset{Name: name, SHA256: seenAssets[name]})
	}

	provNames := make([]string, 0, len(providerMap))
	for name := range providerMap {
		provNames = append(provNames, name)
	}
	sort.Strings(provNames)
	providers := make([]plan.Provider, 0, len(provNames))
	for _, name := range provNames {
		providers = append(providers, plan.Provider{Name: name, Version: providerMap[name]})
	}

	return plan.Bindings{
		Machine:      machine,
		Platform:     string(prep.platform),
		InputsDigest: prep.inputsDigest(),
		Assets:       assets,
		Providers:    providers,
	}
}

func reqSubject(r Requirement) string {
	if r.Software != "" {
		return r.Software
	}
	return r.Command
}

// checkRequirement reports whether req is satisfied: when req.Command is
// set, presence on PATH or under the custom root's bin directory satisfies
// it; otherwise an applied software --install receipt for the same target
// and account does.
func checkRequirement(d Deps, stateDir, tgt, customRoot, account string, req Requirement) (string, bool) {
	if req.Command != "" {
		var extraDirs []string
		if customRoot != "" {
			extraDirs = []string{filepath.Join(customRoot, "bin")}
		}
		if p, err := lookPath(req.Command, extraDirs, d.environ()); err == nil && p != "" {
			return "command:" + req.Command, true
		}
	}
	subj := req.Software
	if subj == "" {
		subj = req.Command
	}
	if subj != "" && hasSoftwareReceipt(stateDir, subj, tgt, account) {
		return "receipt:" + subj, true
	}
	return "", false
}

// hasSoftwareReceipt checks whether stateDir holds an applied
// "software --install" plan with an applied receipt for software, target
// and account. It reads existing files only and never creates or locks
// stateDir.
func hasSoftwareReceipt(stateDir, software, target, account string) bool {
	if stateDir == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(stateDir, "plans", "plan-*.json"))
	if err != nil {
		return false
	}
	for _, p := range matches {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var doc plan.Document
		if err := json.Unmarshal(b, &doc); err != nil {
			continue
		}
		if doc.Command != "software --install" || doc.Software != software || doc.Target != target || doc.Account != account || doc.State != plan.Applied {
			continue
		}
		jPath := filepath.Join(stateDir, "journal", doc.Plan+".jsonl")
		jb, err := os.ReadFile(jPath)
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(jb)), "\n")
		for i := len(lines) - 1; i >= 0; i-- {
			line := strings.TrimSpace(lines[i])
			if line == "" {
				continue
			}
			var rec plan.Record
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				continue
			}
			if rec.Action == plan.ActionReceipt && rec.Outcome == plan.OutcomeApplied {
				return true
			}
			break
		}
	}
	return false
}

func providerForKind(kind string) string {
	switch kind {
	case ledger.KindBlock:
		return ProviderManagedBlock
	case ledger.KindKey:
		return ProviderStructured
	default:
		return ProviderFile
	}
}
