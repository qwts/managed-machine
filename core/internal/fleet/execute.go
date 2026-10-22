package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/config"
	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/block"
	"github.com/qwts/managed-machine/core/internal/config/provider/file"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/execute"
	"github.com/qwts/managed-machine/core/internal/fleet/authz"
	"github.com/qwts/managed-machine/core/internal/fleet/directory"
	"github.com/qwts/managed-machine/core/internal/fleet/policy"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/target"
)

const (
	// PlanCommand is the command recorded in saved fleet plans (D4).
	PlanCommand = "fleet --apply"
	// FleetID is the configuration/operation identifier recorded in
	// fleet plans, output records and the fleet ownership ledger (D4).
	FleetID = "fleet"
	// OperationAuthorizedKeys is the operation name for the
	// authorized_keys managed block (D4).
	OperationAuthorizedKeys = "authorized-keys"
	// BlockID is the managed-block identifier in authorized_keys (D4).
	BlockID = "fleet-authorized-keys"
	// TargetUser is the sole installation target for fleet (D1).
	TargetUser = "user"

	// Legacy block markers removed after cutover and reported as drift
	// by --check (D6).
	LegacyManagedMachineBegin = "# BEGIN managed-machine"
	LegacyManagedMachineEnd   = "# END managed-machine"
	LegacyHomeBinBegin        = "# BEGIN home-bin new-machine"
	LegacyHomeBinEnd          = "# END home-bin new-machine"

	// Drift kinds emitted by `fleet --check` (D4, D6).
	DriftBlock       = "block"
	DriftLegacyBlock = "legacy-block"
)

func (d Deps) home(goos string) (string, error) {
	if d.Home != nil {
		return d.Home()
	}
	if goos == "windows" {
		if u := strings.TrimSpace(d.getenv("USERPROFILE")); u != "" {
			return filepath.Clean(u), nil
		}
		if h := strings.TrimSpace(d.getenv("HOME")); h != "" {
			return filepath.Clean(h), nil
		}
		return "", errors.New("the environment names no home directory (USERPROFILE)")
	}
	if h := strings.TrimSpace(d.getenv("HOME")); h != "" {
		return filepath.Clean(h), nil
	}
	return "", errors.New("the environment names no home directory (HOME)")
}

func (d Deps) machine(ctx context.Context) (string, error) {
	if d.Machine != nil {
		return d.Machine(ctx)
	}
	return "local", nil
}

func (d Deps) newPlanID(t time.Time) (string, error) {
	if d.NewPlanID != nil {
		return d.NewPlanID(t)
	}
	return plan.NewID(t)
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

// FleetLedgerPath returns the path to the fleet ownership ledger
// (<state>/fleet/ownership.jsonl, D4).
func FleetLedgerPath(stateDir string) string {
	return filepath.Join(filepath.Clean(stateDir), "fleet", ledger.FileName)
}

func detectPlatform(ctx cli.Context, goos string) (target.Target, *diag.Diagnostic) {
	if ctx.Detect != nil {
		host, err := ctx.Detect()
		if err != nil {
			id := diag.PlatformProbeFailed
			var unsupported *target.UnsupportedError
			if errors.As(err, &unsupported) {
				id = diag.PlatformUnsupported
			}
			dg := diag.New(id, err.Error())
			return "", &dg
		}
		return host.Target, nil
	}
	switch goos {
	case "windows":
		if runtime.GOARCH == "arm64" {
			return target.WindowsARM64, nil
		}
		return target.WindowsAMD64, nil
	case "linux":
		if runtime.GOARCH == "arm64" {
			return target.LinuxARM64, nil
		}
		return target.LinuxAMD64, nil
	default:
		return target.DarwinARM64, nil
	}
}

// resolveDestination resolves the authorized_keys destination path per D4:
// desired.yaml fleet.destinations.authorized_keys when declared, else
// <HOME>/.ssh/authorized_keys (%USERPROFILE%\.ssh\authorized_keys on Windows).
func (d Deps) resolveDestination(goos string) (string, *diag.Diagnostic) {
	fail := func(id, msg string) (string, *diag.Diagnostic) {
		dg := diag.New(id, msg)
		return "", &dg
	}
	if explicit := strings.TrimSpace(d.getenv(config.DesiredEnv)); explicit != "" {
		des, err := config.LoadDesired(filepath.Clean(explicit))
		if err != nil {
			return fail(diag.FleetPolicyInvalid, err.Error())
		}
		if des.Target != "" && des.Target != TargetUser {
			return fail(diag.FleetPolicyInvalid, fmt.Sprintf("desired configuration names target %q, not %q", des.Target, TargetUser))
		}
		if p := des.Fleet.Destinations["authorized_keys"]; p != "" {
			return filepath.Clean(p), nil
		}
	} else {
		if desiredPath, err := config.DesiredPath(d.getenv, goos, TargetUser, ""); err == nil {
			if fi, statErr := os.Stat(desiredPath); statErr == nil && !fi.IsDir() {
				des, err := config.LoadDesired(desiredPath)
				if err != nil {
					return fail(diag.FleetPolicyInvalid, err.Error())
				}
				if des.Target != "" && des.Target != TargetUser {
					return fail(diag.FleetPolicyInvalid, fmt.Sprintf("desired configuration names target %q, not %q", des.Target, TargetUser))
				}
				if p := des.Fleet.Destinations["authorized_keys"]; p != "" {
					return filepath.Clean(p), nil
				}
			}
		}
		if goos == "windows" && strings.TrimSpace(d.getenv("APPDATA")) == "" {
			if home, err := d.home(goos); err == nil {
				fallback := filepath.Join(home, ".config", "managed-machine", "desired.yaml")
				if fi, statErr := os.Stat(fallback); statErr == nil && !fi.IsDir() {
					des, err := config.LoadDesired(fallback)
					if err != nil {
						return fail(diag.FleetPolicyInvalid, err.Error())
					}
					if des.Target != "" && des.Target != TargetUser {
						return fail(diag.FleetPolicyInvalid, fmt.Sprintf("desired configuration names target %q, not %q", des.Target, TargetUser))
					}
					if p := des.Fleet.Destinations["authorized_keys"]; p != "" {
						return filepath.Clean(p), nil
					}
				}
			}
		}
	}
	home, err := d.home(goos)
	if err != nil {
		return fail(diag.FleetApplyFailed, err.Error())
	}
	return filepath.Join(home, ".ssh", "authorized_keys"), nil
}

// InputsDigest returns the SHA-256 digest of the fleet request inputs
// (account, target, platform and destination path).
func InputsDigest(account, targetName, platform, destPath string) string {
	parts := []string{
		"fleet=" + account,
		"target=" + targetName,
		"platform=" + platform,
		"destination.authorized_keys=" + filepath.Clean(destPath),
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:])
}

func buildBindings(pol *policy.Policy, dir directory.Directory, account, platform, destPath, machine string) (plan.Bindings, error) {
	normDir, err := directory.NormalizeEntries(dir.Entries)
	if err != nil {
		return plan.Bindings{}, err
	}
	dirSum := file.SHA256Hex(normDir)
	seenAssets := make(map[string]string, len(pol.Assets)+1)
	for _, a := range pol.Assets {
		seenAssets[a.Name] = strings.TrimPrefix(a.SHA256, "sha256:")
	}
	seenAssets["directory:"+dir.Identity] = dirSum

	names := make([]string, 0, len(seenAssets))
	for name := range seenAssets {
		names = append(names, name)
	}
	sort.Strings(names)
	assets := make([]plan.Asset, 0, len(names))
	for _, name := range names {
		assets = append(assets, plan.Asset{Name: name, SHA256: seenAssets[name]})
	}
	return plan.Bindings{
		Machine:      machine,
		Platform:     platform,
		InputsDigest: InputsDigest(account, TargetUser, platform, destPath),
		Assets:       assets,
		Providers:    []plan.Provider{{Name: block.Name, Version: block.Version}},
	}, nil
}

type legacyBlock struct {
	beginMarker string
	endMarker   string
	keys        int
	start       int
	end         int
	wellFormed  bool
}

type lineSpan struct {
	start int
	end   int
	text  string
}

func splitLines(data []byte) []lineSpan {
	var lines []lineSpan
	start := 0
	for i := 0; i < len(data); i++ {
		if data[i] == '\n' {
			end := i + 1
			textEnd := i
			if i > start && data[i-1] == '\r' {
				textEnd = i - 1
			}
			lines = append(lines, lineSpan{
				start: start,
				end:   end,
				text:  string(data[start:textEnd]),
			})
			start = end
		}
	}
	if start < len(data) {
		lines = append(lines, lineSpan{
			start: start,
			end:   len(data),
			text:  string(data[start:]),
		})
	}
	return lines
}

// findLegacyBlocks scans data for legacy managed-machine and home-bin
// new-machine blocks in file order (D6).
func findLegacyBlocks(data []byte) []legacyBlock {
	lines := splitLines(data)
	var out []legacyBlock
	for i := 0; i < len(lines); i++ {
		var beginMarker, endMarker string
		switch lines[i].text {
		case LegacyManagedMachineBegin:
			beginMarker = LegacyManagedMachineBegin
			endMarker = LegacyManagedMachineEnd
		case LegacyHomeBinBegin:
			beginMarker = LegacyHomeBinBegin
			endMarker = LegacyHomeBinEnd
		default:
			continue
		}
		endIdx := -1
		for j := i + 1; j < len(lines); j++ {
			if lines[j].text == endMarker {
				endIdx = j
				break
			}
		}
		if endIdx < 0 {
			out = append(out, legacyBlock{
				beginMarker: beginMarker,
				endMarker:   endMarker,
				start:       lines[i].start,
				end:         lines[i].end,
				wellFormed:  false,
			})
			continue
		}
		keys := 0
		for k := i + 1; k < endIdx; k++ {
			trimmed := strings.TrimSpace(lines[k].text)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				keys++
			}
		}
		out = append(out, legacyBlock{
			beginMarker: beginMarker,
			endMarker:   endMarker,
			keys:        keys,
			start:       lines[i].start,
			end:         lines[endIdx].end,
			wellFormed:  true,
		})
		i = endIdx
	}
	return out
}

func findWellFormedLegacyBlocks(data []byte) []legacyBlock {
	all := findLegacyBlocks(data)
	var out []legacyBlock
	for _, b := range all {
		if b.wellFormed {
			out = append(out, b)
		}
	}
	return out
}

func stripLegacyBlocks(data []byte) ([]byte, []legacyBlock) {
	blocks := findWellFormedLegacyBlocks(data)
	if len(blocks) == 0 {
		return data, nil
	}
	out := make([]byte, 0, len(data))
	pos := 0
	for _, b := range blocks {
		out = append(out, data[pos:b.start]...)
		pos = b.end
	}
	out = append(out, data[pos:]...)
	return out, blocks
}

func planAuthorizedKeys(destPath string, res authz.Result, snap *ledger.Snapshot) ([]plan.Precondition, []plan.Operation, []legacyBlock, error) {
	cur, readErr := os.ReadFile(destPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return nil, nil, nil, readErr
	}

	if len(res.Authorized) == 0 {
		if errors.Is(readErr, os.ErrNotExist) {
			pre := []plan.Precondition{{
				Kind:        "target",
				Subject:     destPath,
				Requirement: "absent",
				Observed:    "absent",
			}}
			if snap != nil && snap.IsOwnedBy(FleetID, TargetUser, destPath, ledger.KindBlock, BlockID) {
				return pre, []plan.Operation{deletePlanOp(destPath)}, nil, nil
			}
			return pre, nil, nil, nil
		}
		curSum := "sha256:" + file.SHA256Hex(cur)
		pre := []plan.Precondition{{
			Kind:        "target",
			Subject:     destPath,
			Requirement: curSum,
			Observed:    curSum,
		}}
		_, present, inspectErr := block.Inspect(cur, BlockID)
		if inspectErr != nil || (present && (snap == nil || !snap.IsOwnedBy(FleetID, TargetUser, destPath, ledger.KindBlock, BlockID))) {
			return nil, []plan.Operation{conflictPlanOp(destPath)}, nil, nil
		}
		legacyBlocks := findWellFormedLegacyBlocks(cur)
		if present || (snap != nil && snap.IsOwnedBy(FleetID, TargetUser, destPath, ledger.KindBlock, BlockID)) || len(legacyBlocks) > 0 {
			return pre, []plan.Operation{deletePlanOp(destPath)}, legacyBlocks, nil
		}
		return pre, nil, nil, nil
	}

	prov := block.New()
	rendered := strings.Join(res.Lines(), "\n")
	inOp := plan.Operation{
		ID:        FleetID,
		Operation: OperationAuthorizedKeys,
		Target:    TargetUser,
		Provider:  block.Name,
		Params: map[string]string{
			"config":   FleetID,
			"path":     destPath,
			"block_id": BlockID,
			"comment":  block.DefaultComment,
			"content":  rendered,
		},
	}
	_, pre, ops, err := prov.Plan(nil, "", inOp, render.Values{}, snap)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(ops) == 1 && strings.HasPrefix(ops[0].Action, "conflict ") {
		return pre, ops, nil, nil
	}

	var legacyBlocks []legacyBlock
	if readErr == nil {
		legacyBlocks = findWellFormedLegacyBlocks(cur)
	}
	if len(legacyBlocks) > 0 {
		eol := block.DetectEOL(cur)
		wantBlock := block.FormatBlock(BlockID, block.DefaultComment, rendered, eol)
		wantSum := file.SHA256Hex(wantBlock)
		if len(ops) == 0 {
			ops = []plan.Operation{{
				ID:        FleetID,
				Operation: OperationAuthorizedKeys,
				Target:    TargetUser,
				Action:    "update",
				Provider:  block.Name,
				Params: map[string]string{
					"config":   FleetID,
					"path":     destPath,
					"kind":     ledger.KindBlock,
					"id":       BlockID,
					"block_id": BlockID,
					"comment":  block.DefaultComment,
					"sha256":   wantSum,
				},
			}}
		} else if len(ops) == 1 && ops[0].Action == "create" {
			ops[0].Action = "update"
		}
	}
	for i := range ops {
		delete(ops[i].Params, "content")
	}
	return pre, ops, legacyBlocks, nil
}

func deletePlanOp(destPath string) plan.Operation {
	return plan.Operation{
		ID:        FleetID,
		Operation: OperationAuthorizedKeys,
		Target:    TargetUser,
		Action:    "delete",
		Provider:  block.Name,
		Params: map[string]string{
			"config":   FleetID,
			"path":     destPath,
			"kind":     ledger.KindBlock,
			"id":       BlockID,
			"block_id": BlockID,
			"comment":  block.DefaultComment,
		},
	}
}

func conflictPlanOp(destPath string) plan.Operation {
	conflict := "foreign-block " + destPath + ":" + BlockID
	return plan.Operation{
		ID:        FleetID,
		Operation: OperationAuthorizedKeys,
		Target:    TargetUser,
		Action:    "conflict " + conflict,
		Provider:  block.Name,
		Params: map[string]string{
			"config":   FleetID,
			"path":     destPath,
			"kind":     ledger.KindBlock,
			"id":       BlockID,
			"block_id": BlockID,
			"conflict": conflict,
		},
	}
}

func planRun(d Deps, cliCtx cli.Context, goos string) cli.Result {
	ctx := context.Background()
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if err := pol.RequireCutover(); err != nil {
		coDiag := policy.ToDiagnostic(err)
		return cli.Result{Diagnostic: &coDiag}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	platform, dg := detectPlatform(cliCtx, goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	account, err := d.username(goos)
	if err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	destPath, dg := d.resolveDestination(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if !d.isWritable(destPath) {
		return fail(diag.FleetPrivilegeRequired, "destination %s is not writable by the current user", destPath)
	}
	stateDir, err := d.stateDir(goos)
	if err != nil {
		return fail(diag.FleetPolicyUnavailable, "%v", err)
	}
	machine, err := d.machine(ctx)
	if err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	snap, err := ledger.Load(FleetLedgerPath(stateDir))
	if err != nil {
		return fail(diag.FleetApplyFailed, "reading fleet ownership ledger: %v", err)
	}

	res := authz.Evaluate(pol.Groups, dir, account, goos)
	targetPre, plannedOps, legacyBlocks, err := planAuthorizedKeys(destPath, res, snap)
	if err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	if plannedOps == nil {
		plannedOps = []plan.Operation{}
	}

	preconditions := []plan.Precondition{
		{
			Kind:        "policy",
			Subject:     "fleet-cutover",
			Requirement: "present",
			Observed:    "present",
		},
		{
			Kind:        "platform",
			Subject:     "host",
			Requirement: string(platform),
			Observed:    string(platform),
		},
	}
	preconditions = append(preconditions, targetPre...)

	bindings, err := buildBindings(pol, dir, account, string(platform), destPath, machine)
	if err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	now := d.now().UTC().Truncate(time.Second)
	planID, err := d.newPlanID(now)
	if err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	doc := &plan.Document{
		Plan:           planID,
		SchemaVersion:  plan.SchemaVersion,
		Command:        PlanCommand,
		Fleet:          account,
		Target:         TargetUser,
		Account:        account,
		SourceRevision: dir.Identity,
		CreatedAt:      now.Format(time.RFC3339),
		State:          plan.Saved,
		Bindings:       bindings,
		Preconditions:  preconditions,
		Operations:     plannedOps,
	}

	st, err := plan.Open(stateDir, d.now)
	if err != nil {
		if errors.Is(err, plan.ErrBusy) {
			return fail(diag.FleetBusy, "%v", err)
		}
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	defer st.Close()
	if err := st.Save(doc); err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}

	recs := make([]*output.Record, 0, 1+len(doc.Operations)+len(legacyBlocks)+len(res.Authorized))
	recs = append(recs, output.New().
		Set("plan", doc.Plan).
		Set("fleet", account).
		Set("target", TargetUser).
		Set("source_revision", dir.Identity).
		Set("created_at", doc.CreatedAt).
		Set("state", string(doc.State)))
	for _, op := range doc.Operations {
		recs = append(recs, output.New().
			Set("id", op.ID).
			Set("operation", op.Operation).
			Set("target", op.Target).
			Set("action", op.Action))
	}
	for _, lb := range legacyBlocks {
		recs = append(recs, output.New().
			Set("legacy_block", lb.beginMarker).
			Set("keys", lb.keys).
			Set("action", "remove"))
	}
	for _, a := range res.Authorized {
		recs = append(recs, output.New().
			Set("account", account).
			Set("fingerprint", a.Fingerprint).
			Set("state", AccessStateAuthorized).
			SetOptional("owner", a.Owner).
			Set("group", a.Group))
	}
	return cli.Result{Records: recs}
}

func applyRun(d Deps, cliCtx cli.Context, goos string, f flags) cli.Result {
	ctx := context.Background()
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if err := pol.RequireCutover(); err != nil {
		coDiag := policy.ToDiagnostic(err)
		return cli.Result{Diagnostic: &coDiag}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	platform, dg := detectPlatform(cliCtx, goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	account, err := d.username(goos)
	if err != nil {
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	destPath, dg := d.resolveDestination(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if !d.isWritable(destPath) {
		return fail(diag.FleetPrivilegeRequired, "destination %s is not writable by the current user", destPath)
	}
	stateDir, err := d.stateDir(goos)
	if err != nil {
		return fail(diag.FleetPolicyUnavailable, "%v", err)
	}
	inputsDigest := InputsDigest(account, TargetUser, string(platform), destPath)

	st, err := plan.Open(stateDir, d.now)
	if err != nil {
		if errors.Is(err, plan.ErrBusy) {
			return fail(diag.FleetBusy, "%v", err)
		}
		return fail(diag.FleetApplyFailed, "%v", err)
	}
	defer st.Close()

	doc, err := st.Select(plan.Request{
		Command:      PlanCommand,
		Name:         account,
		Target:       TargetUser,
		Account:      account,
		InputsDigest: inputsDigest,
	}, f.fromPlan)
	if err != nil {
		switch {
		case errors.Is(err, plan.ErrMismatch):
			return fail(diag.FleetPlanMismatch, "%v", err)
		case errors.Is(err, plan.ErrNoPlan), errors.Is(err, plan.ErrNotEligible), errors.Is(err, os.ErrNotExist):
			return fail(diag.FleetNoPlan, "%v", err)
		default:
			return fail(diag.FleetPlanMismatch, "%v", err)
		}
	}

	for _, op := range doc.Operations {
		if strings.HasPrefix(op.Action, "conflict ") || op.Params["conflict"] != "" {
			return fail(diag.FleetConflict, "%s", op.Action)
		}
	}
	for _, op := range doc.Operations {
		if p := op.Params["path"]; p != "" && !d.isWritable(p) {
			return fail(diag.FleetPrivilegeRequired, "destination %s is not writable by the current user", p)
		}
	}

	res := authz.Evaluate(pol.Groups, dir, account, goos)
	m := &fleetMachine{
		deps:       d,
		goos:       goos,
		platform:   platform,
		account:    account,
		destPath:   destPath,
		pol:        pol,
		dir:        dir,
		authz:      res,
		ledgerPath: FleetLedgerPath(stateDir),
		store:      st,
		doc:        doc,
		prov:       block.New(),
	}
	if _, err := execute.Run(ctx, st, doc.Plan, m); err != nil {
		var rej *execute.Rejected
		var fl *execute.Failed
		switch {
		case errors.As(err, &rej):
			return fail(diag.FleetPlanRejected, "plan %s rejected: %s", doc.Plan, strings.Join(rej.Changes, "; "))
		case errors.As(err, &fl):
			return fail(diag.FleetApplyFailed, "plan %s: %v", doc.Plan, fl)
		default:
			return fail(diag.FleetApplyFailed, "%v", err)
		}
	}

	recs := make([]*output.Record, 0, len(doc.Operations))
	for _, op := range doc.Operations {
		recs = append(recs, output.New().
			Set("id", op.ID).
			Set("operation", op.Operation).
			Set("target", op.Target).
			Set("action", op.Action).
			Set("verified", true).
			Set("plan", doc.Plan))
	}
	return cli.Result{Records: recs}
}

func checkRun(d Deps, cliCtx cli.Context, goos string) cli.Result {
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	if _, dg := detectPlatform(cliCtx, goos); dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	pol, dg := d.loadPolicy(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	dir, dg := d.loadDirectory(goos, pol.KeyDirectory)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	account, err := d.username(goos)
	if err != nil {
		return fail(diag.FleetCheckIncomplete, "%v", err)
	}
	destPath, dg := d.resolveDestination(goos)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}

	res := authz.Evaluate(pol.Groups, dir, account, goos)
	cur, readErr := os.ReadFile(destPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fail(diag.FleetCheckIncomplete, "%s: %v", destPath, readErr)
	}

	var recs []*output.Record
	if len(res.Authorized) == 0 {
		if readErr == nil {
			gotSum, present, inspectErr := block.Inspect(cur, BlockID)
			if inspectErr != nil {
				recs = append(recs, driftRecord(DriftBlock, "actual=malformed desired=absent"))
			} else if present {
				recs = append(recs, driftRecord(DriftBlock, "actual=sha256:"+gotSum+" desired=absent"))
			}
		}
	} else {
		rendered := strings.Join(res.Lines(), "\n")
		if errors.Is(readErr, os.ErrNotExist) {
			wantBlock := block.FormatBlock(BlockID, block.DefaultComment, rendered, "\n")
			wantSum := file.SHA256Hex(wantBlock)
			recs = append(recs, driftRecord(DriftBlock, "actual=absent desired=sha256:"+wantSum))
		} else {
			inOp := plan.Operation{
				ID:        FleetID,
				Operation: OperationAuthorizedKeys,
				Target:    TargetUser,
				Provider:  block.Name,
				Params: map[string]string{
					"config":   FleetID,
					"path":     destPath,
					"block_id": BlockID,
					"comment":  block.DefaultComment,
					"content":  rendered,
				},
			}
			drifts, err := block.New().Check(nil, "", inOp, render.Values{})
			if err != nil {
				return fail(diag.FleetCheckIncomplete, "%s: %v", destPath, err)
			}
			for _, dr := range drifts {
				recs = append(recs, driftRecord(DriftBlock, dr.Detail))
			}
		}
	}

	if readErr == nil {
		for _, lb := range findLegacyBlocks(cur) {
			recs = append(recs, driftRecord(DriftLegacyBlock, lb.beginMarker))
		}
	}

	if len(recs) == 0 {
		return cli.Result{}
	}
	msg := fmt.Sprintf("%d differences", len(recs))
	if len(recs) == 1 {
		msg = "1 difference"
	}
	driftDiag := diag.New(diag.FleetDriftFound, msg)
	return cli.Result{Records: recs, Diagnostic: &driftDiag}
}

func driftRecord(drift, detail string) *output.Record {
	return output.New().
		Set("id", FleetID).
		Set("operation", OperationAuthorizedKeys).
		Set("target", TargetUser).
		Set("drift", drift).
		Set("detail", detail)
}

type fleetMachine struct {
	deps       Deps
	goos       string
	platform   target.Target
	account    string
	destPath   string
	pol        *policy.Policy
	dir        directory.Directory
	authz      authz.Result
	ledgerPath string
	store      *plan.Store
	doc        *plan.Document
	prov       *block.Provider
}

func (m *fleetMachine) Bindings(ctx context.Context, _ *plan.Document) (plan.Bindings, error) {
	machine, err := m.deps.machine(ctx)
	if err != nil {
		return plan.Bindings{}, err
	}
	return buildBindings(m.pol, m.dir, m.account, string(m.platform), m.destPath, machine)
}

func (m *fleetMachine) Check(_ context.Context, c plan.Precondition) (string, error) {
	switch c.Kind {
	case "policy":
		if c.Subject == "fleet-cutover" {
			if m.pol != nil && m.pol.Cutover != nil {
				return "present", nil
			}
			return "absent", errors.New("fleet/cutover.yaml is absent")
		}
		return c.Observed, nil
	case "platform":
		nowPlat := string(m.platform)
		if c.Requirement != nowPlat {
			return nowPlat, fmt.Errorf("the host is %s", nowPlat)
		}
		return nowPlat, nil
	case "privilege":
		if !m.deps.isWritableDir(c.Subject) {
			return "unwritable", fmt.Errorf("%s is not writable by the current user", c.Subject)
		}
		return "writable", nil
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

func (m *fleetMachine) Step(op plan.Operation) (execute.Step, error) {
	destPath := op.Params["path"]
	if destPath == "" {
		destPath = m.destPath
	}
	hydrated := op
	params := make(map[string]string, len(op.Params)+2)
	for k, v := range op.Params {
		params[k] = v
	}
	params["path"] = destPath
	params["config"] = FleetID
	params["block_id"] = BlockID
	params["id"] = BlockID
	if params["comment"] == "" {
		params["comment"] = block.DefaultComment
	}

	var wantBlock []byte
	if op.Action == "create" || op.Action == "update" {
		eol := "\n"
		if cur, err := os.ReadFile(destPath); err == nil {
			eol = block.DetectEOL(cur)
		}
		rendered := strings.Join(m.authz.Lines(), "\n")
		wantBlock = block.FormatBlock(BlockID, params["comment"], rendered, eol)
		wantSum := file.SHA256Hex(wantBlock)
		if op.Params["sha256"] != "" && op.Params["sha256"] != wantSum {
			return nil, fmt.Errorf("digest changed from %s to %s", op.Params["sha256"], wantSum)
		}
		params["content"] = string(wantBlock)
		params["sha256"] = wantSum
	}
	hydrated.Params = params

	baseStep, err := m.prov.Step(hydrated, func(r ledger.Record) error {
		r.SchemaVersion = ledger.SchemaVersion
		r.Config = FleetID
		r.Operation = OperationAuthorizedKeys
		r.Target = TargetUser
		r.Account = m.doc.Account
		r.Plan = m.doc.Plan
		r.At = m.store.Now()
		return ledger.Append(m.ledgerPath, r)
	})
	if err != nil {
		return nil, err
	}
	return &fleetStep{
		base:      baseStep,
		action:    op.Action,
		path:      destPath,
		wantBlock: wantBlock,
		goos:      m.goos,
	}, nil
}

type fleetStep struct {
	base      execute.Step
	action    string
	path      string
	wantBlock []byte
	goos      string
}

func (s *fleetStep) Apply(ctx context.Context) (execute.Evidence, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if cur, err := os.ReadFile(s.path); err == nil {
		if stripped, removed := stripLegacyBlocks(cur); len(removed) > 0 {
			next, err := applyBlockToStripped(stripped, s.action, s.wantBlock, block.DetectEOL(cur))
			if err != nil {
				return nil, err
			}
			if err := writeAtomicFleet(s.path, next); err != nil {
				return nil, err
			}
			return execute.Evidence{"path": s.path, "block_id": BlockID}, nil
		}
	}
	return s.base.Apply(ctx)
}

func (s *fleetStep) Verify(ctx context.Context) (execute.Evidence, error) {
	vev, err := s.base.Verify(ctx)
	if err != nil {
		return vev, err
	}
	if got, readErr := os.ReadFile(s.path); readErr == nil {
		if rem := findLegacyBlocks(got); len(rem) > 0 {
			return vev, fmt.Errorf("verify %s: legacy block %q still exists", s.path, rem[0].beginMarker)
		}
	}
	if s.goos != "windows" {
		if runtime.GOOS != "windows" {
			dir := filepath.Dir(s.path)
			if _, statErr := os.Stat(dir); statErr == nil {
				if err := os.Chmod(dir, 0o700); err != nil {
					return vev, err
				}
			}
			if _, statErr := os.Stat(s.path); statErr == nil {
				if err := os.Chmod(s.path, 0o600); err != nil {
					return vev, err
				}
			}
		}
		if _, statErr := os.Stat(s.path); statErr == nil {
			if vev == nil {
				vev = execute.Evidence{}
			}
			vev["mode"] = "0600"
		}
	}
	return vev, nil
}

func applyBlockToStripped(stripped []byte, action string, wantBlock []byte, eol string) ([]byte, error) {
	beginLine := block.DefaultComment + " managed-machine:begin " + BlockID
	endLine := block.DefaultComment + " managed-machine:end " + BlockID
	lines := splitLines(stripped)
	var begins, ends []int
	for i, l := range lines {
		switch l.text {
		case beginLine:
			begins = append(begins, i)
		case endLine:
			ends = append(ends, i)
		}
	}
	switch {
	case len(begins) == 0 && len(ends) == 0:
		if action == "delete" {
			return stripped, nil
		}
		var sep []byte
		if len(stripped) > 0 && stripped[len(stripped)-1] != '\n' {
			sep = []byte(eol)
		}
		next := make([]byte, 0, len(stripped)+len(sep)+len(wantBlock))
		next = append(next, stripped...)
		next = append(next, sep...)
		next = append(next, wantBlock...)
		return next, nil
	case len(begins) == 1 && len(ends) == 1 && begins[0] < ends[0]:
		start := lines[begins[0]].start
		end := lines[ends[0]].end
		if action == "delete" {
			next := make([]byte, 0, start+len(stripped)-end)
			next = append(next, stripped[:start]...)
			next = append(next, stripped[end:]...)
			return next, nil
		}
		next := make([]byte, 0, start+len(wantBlock)+len(stripped)-end)
		next = append(next, stripped[:start]...)
		next = append(next, wantBlock...)
		next = append(next, stripped[end:]...)
		return next, nil
	default:
		return nil, fmt.Errorf("malformed markers for block %q", BlockID)
	}
}

func writeAtomicFleet(destPath string, content []byte) error {
	dir := filepath.Dir(destPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".mm-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	_ = os.Chmod(tmpName, 0o600)
	if err := os.Rename(tmpName, destPath); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
