// Package status implements the read-only "managed-machine status" command
// (S4 story C08, S5 story L06): it reports the software, config, flavor and
// fleet sections using the fixed record key order section, resource, target,
// state, facts, evidence, at (D7, D8).
//
// status never opens a plan-store lock, never creates or modifies any file or
// directory under the state directory, home or custom root, never performs a
// network request, and never spawns a provider or package-manager process.
package status

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/compose"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/config/provider/block"
	"github.com/qwts/managed-machine/internal/config/provider/file"
	"github.com/qwts/managed-machine/internal/config/provider/structured"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/software"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/statedir"
	"github.com/qwts/managed-machine/internal/target"
)

// Supported section names for `managed-machine status --section <name>`.
const (
	SectionSoftware = "software"
	SectionConfig   = "config"
	SectionFlavor   = "flavor"
	SectionFleet    = "fleet"
)

// Supported resource names for `managed-machine status --section fleet [name]` (D8).
const (
	FleetResourceDirectory      = "directory"
	FleetResourcePolicy         = "policy"
	FleetResourceAuthorizedKeys = "authorized-keys"
	FleetResourceEnrollment     = "enrollment"
)

// Deps are the read-only seams of the status command.
type Deps struct {
	// SoftwareCatalog returns the software definitions of every source.
	// When nil, software.LoadCatalog(Getenv) is used.
	SoftwareCatalog func() ([]source.Entry, *registry.Registry, error)
	// ConfigCatalog returns the configuration definitions of every source.
	// When nil, config.LoadCatalog(Getenv) is used.
	ConfigCatalog func() ([]config.Entry, error)
	// Flavors supplies flavor definitions directly. When nil and
	// FlavorCatalog is nil, flavors are loaded from the configuration
	// catalog's filesystems via compose.LoadCatalog.
	Flavors []compose.Flavor
	// FlavorCatalog returns the flavor definitions of every source.
	FlavorCatalog func() ([]compose.Entry, error)
	// Getenv reads the environment for locating the state directory and
	// catalog sources. When nil, no environment variable is set.
	Getenv func(string) string
	// GOOS is the operating system for locating the state directory. Empty
	// means runtime.GOOS.
	GOOS string
	// StateDir locates the platform state directory. When nil,
	// statedir.Path(GOOS, Getenv) is used.
	StateDir func() (string, error)
	// PolicyDir locates the fleet policy checkout (D2, D8). When nil,
	// policy.Dir(Getenv, stateDir) is used.
	PolicyDir func() (string, error)
	// Username returns the invoking OS account name for fleet status (D1,
	// D8). When nil, fleet.DefaultUsername(GOOS) is used.
	Username func() (string, error)
	// Home returns the current user's home directory for locating the
	// default <HOME>/.ssh/authorized_keys destination (D4, D8). When nil,
	// USERPROFILE (on Windows) or HOME is read from Getenv.
	Home func() (string, error)
	// Now is the clock for observed status records that have no recorded
	// timestamp (such as an absent custom root). When nil, time.Now is used.
	Now func() time.Time
}

type flags struct {
	section                 string
	name                    string
	custom                  string
	global, user, hasCustom bool
	sectionCount            int
}

// Command is `managed-machine status`: read-only reporting of software
// receipts, configuration ownership and on-disk state, composed flavors,
// and fleet state.
func Command(d Deps) cli.Command {
	return cli.Command{
		Name:    "status",
		Summary: "Report software, config, flavor and fleet status without changing the machine",
		Usage:   usage,
		Run:     func(ctx cli.Context, args []string) cli.Result { return run(d, ctx, args) },
	}
}

const usage = `Usage: managed-machine status [--section <software|config|flavor|fleet> [name]] [options]

Report software, config, flavor and fleet status without changing the machine.
Default output is compact JSON Lines on stdout.

Options
-h, --help                 Show usage; never changes the machine
--section <name>           Inspect one section: software, config, flavor or fleet
--global                   Filter to the whole-machine target
--user                     Filter to the current-user target
--custom <path>            Filter to a custom root target
--json                     Return results as one JSON document
--human                    Render results in a readable table
`

func (d Deps) getenv(k string) string {
	if d.Getenv == nil {
		return ""
	}
	return d.Getenv(k)
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

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) nowRFC3339() string {
	return d.now().UTC().Truncate(time.Second).Format(time.RFC3339)
}

func (d Deps) softwareEntries() ([]source.Entry, error) {
	if d.SoftwareCatalog != nil {
		entries, _, err := d.SoftwareCatalog()
		return entries, err
	}
	entries, _, err := software.LoadCatalog(d.getenv)
	return entries, err
}

func (d Deps) configEntries() ([]config.Entry, error) {
	if d.ConfigCatalog != nil {
		return d.ConfigCatalog()
	}
	return config.LoadCatalog(d.getenv)
}

func (d Deps) flavors(cfgEntries []config.Entry) ([]compose.Flavor, error) {
	if d.Flavors != nil {
		out := append([]compose.Flavor(nil), d.Flavors...)
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}
	if d.FlavorCatalog != nil {
		entries, err := d.FlavorCatalog()
		if err != nil {
			return nil, err
		}
		out := make([]compose.Flavor, 0, len(entries))
		for _, e := range entries {
			out = append(out, e.Flavor)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	}
	byID := map[string]compose.Flavor{}
	if catEntries, err := compose.LoadCatalog(d.getenv); err == nil {
		for _, fe := range catEntries {
			byID[fe.Flavor.ID] = fe.Flavor
		}
	}
	seenSource := map[source.Identity]bool{}
	for _, ce := range cfgEntries {
		if ce.FS == nil {
			continue
		}
		id := ce.Source
		if id.Kind == "" {
			id = source.Identity{Kind: "local", Location: "local"}
		}
		if seenSource[id] {
			continue
		}
		seenSource[id] = true
		dir := ce.Dir
		if dir == "" {
			dir = "."
		}
		snap, err := compose.Load(id, ce.FS, dir)
		if err != nil {
			continue
		}
		for _, fe := range snap.Entries {
			byID[fe.Flavor.ID] = fe.Flavor
		}
	}
	out := make([]compose.Flavor, 0, len(byID))
	for _, fl := range byID {
		out = append(out, fl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func usageErr(format string, args ...any) *diag.Diagnostic {
	d := diag.New(diag.Usage, fmt.Sprintf(format, args...))
	return &d
}

// newRecord constructs one status record with the fixed D7 key order:
// section, resource, target, state, facts, evidence, at.
func newRecord(section, resource, targetName, state string, facts map[string]string, evidence, at string) *output.Record {
	if facts == nil {
		facts = map[string]string{}
	}
	return output.New().
		Set("section", section).
		Set("resource", resource).
		Set("target", targetName).
		Set("state", state).
		Set("facts", facts).
		Set("evidence", evidence).
		Set("at", at)
}

func parse(args []string) (flags, *diag.Diagnostic) {
	var f flags
	value := func(i *int, name string) (string, *diag.Diagnostic) {
		if *i+1 >= len(args) || strings.HasPrefix(args[*i+1], "-") {
			return "", usageErr("%s needs a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		var d *diag.Diagnostic
		a := args[i]
		switch {
		case a == "--section":
			f.sectionCount++
			f.section, d = value(&i, a)
		case strings.HasPrefix(a, "--section="):
			f.sectionCount++
			f.section = strings.TrimPrefix(a, "--section=")
			if f.section == "" {
				d = usageErr("--section needs a value")
			}
		case a == "--custom":
			f.hasCustom = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				f.custom = args[i]
			}
		case strings.HasPrefix(a, "--custom="):
			f.hasCustom = true
			f.custom = strings.TrimPrefix(a, "--custom=")
		case a == "--global":
			f.global = true
		case a == "--user":
			f.user = true
		default:
			if strings.HasPrefix(a, "-") {
				return f, usageErr("unknown option %q", a)
			}
			if f.name != "" {
				return f, usageErr("at most one resource name; got %q and %q", f.name, a)
			}
			f.name = a
		}
		if d != nil {
			return f, d
		}
	}
	if f.sectionCount > 1 {
		return f, usageErr("at most one --section is allowed")
	}
	if strings.Contains(f.section, ",") {
		return f, usageErr("--section accepts one section per invocation, not a comma list %q", f.section)
	}
	if f.section != "" {
		switch f.section {
		case SectionSoftware, SectionConfig, SectionFlavor, SectionFleet:
		default:
			return f, usageErr("unknown section %q; supported: software, config, flavor, fleet", f.section)
		}
	}
	if f.name != "" && f.section == "" {
		return f, usageErr("resource name %q requires --section", f.name)
	}
	targets := 0
	for _, b := range []bool{f.global, f.user, f.hasCustom} {
		if b {
			targets++
		}
	}
	if targets > 1 {
		return f, usageErr("at most one of --global, --user, --custom")
	}
	if f.section == SectionFleet {
		if f.global || f.hasCustom {
			return f, usageErr("--user is the only target for --section fleet: fleet authorizes OS accounts, Souls have none (ADR-0009; --custom is not supported) and machine-wide sshd files are outside v1 (--global is not supported)")
		}
		if f.name != "" {
			switch f.name {
			case FleetResourceDirectory, FleetResourcePolicy, FleetResourceAuthorizedKeys, FleetResourceEnrollment:
			default:
				return f, usageErr("unknown fleet resource %q; supported: directory, policy, authorized-keys, enrollment", f.name)
			}
		}
	}
	if f.hasCustom && strings.TrimSpace(f.custom) == "" {
		return f, usageErr("--custom needs a value")
	}
	return f, nil
}

func run(d Deps, ctx cli.Context, args []string) cli.Result {
	f, dg := parse(args)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	host, dg := detectHost(ctx)
	if dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	if d.GOOS == "" {
		if i := strings.IndexByte(string(host.Target), '/'); i > 0 {
			d.GOOS = string(host.Target[:i])
		}
	}

	var targetFilter string
	var customRoot string
	switch {
	case f.global:
		targetFilter = "global"
	case f.user:
		targetFilter = "user"
	case f.hasCustom:
		abs, err := filepath.Abs(f.custom)
		if err != nil {
			return cli.Result{Diagnostic: usageErr("--custom %q: %v", f.custom, err)}
		}
		customRoot = abs
		targetFilter = output.CustomTarget(abs)
	}

	stateDir, err := d.stateDir()
	if err != nil {
		stateDir = ""
	}

	sections := []string{SectionSoftware, SectionConfig, SectionFlavor}
	if f.section != "" {
		sections = []string{f.section}
	}

	var records []*output.Record
	var partialDiag *diag.Diagnostic

	for _, sec := range sections {
		switch sec {
		case SectionSoftware:
			recs, usageDg := softwareSection(d, stateDir, f.name, targetFilter)
			if usageDg != nil {
				return cli.Result{Diagnostic: usageDg}
			}
			records = append(records, recs...)
		case SectionConfig:
			recs, usageDg, partDg := configSection(d, stateDir, customRoot, f.name, targetFilter)
			if usageDg != nil {
				return cli.Result{Diagnostic: usageDg}
			}
			if partDg != nil && partialDiag == nil {
				partialDiag = partDg
			}
			records = append(records, recs...)
		case SectionFlavor:
			recs, usageDg, partDg := flavorSection(d, stateDir, customRoot, f.name, targetFilter)
			if usageDg != nil {
				return cli.Result{Diagnostic: usageDg}
			}
			if partDg != nil && partialDiag == nil {
				partialDiag = partDg
			}
			records = append(records, recs...)
		case SectionFleet:
			recs, usageDg, partDg := fleetSection(d, stateDir, f.name)
			if usageDg != nil {
				return cli.Result{Diagnostic: usageDg}
			}
			if partDg != nil && partialDiag == nil {
				partialDiag = partDg
			}
			records = append(records, recs...)
		}
	}

	return cli.Result{
		Records:    records,
		Diagnostic: partialDiag,
	}
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

type softwareReceipt struct {
	software string
	target   string
	planID   string
	at       string
}

func softwareSection(d Deps, stateDir, nameFilter, targetFilter string) ([]*output.Record, *diag.Diagnostic) {
	receipts := readSoftwareReceipts(stateDir)
	if nameFilter != "" {
		known := false
		for _, r := range receipts {
			if r.software == nameFilter {
				known = true
				break
			}
		}
		if !known {
			entries, err := d.softwareEntries()
			if err == nil {
				for _, e := range entries {
					if e.Software.ID == nameFilter {
						known = true
						break
					}
				}
			}
		}
		if !known {
			return nil, usageErr("unknown software %q", nameFilter)
		}
	}

	var out []*output.Record
	for _, r := range receipts {
		if nameFilter != "" && r.software != nameFilter {
			continue
		}
		if targetFilter != "" && r.target != targetFilter {
			continue
		}
		out = append(out, newRecord(
			SectionSoftware,
			r.software,
			r.target,
			"unknown",
			map[string]string{"receipt": r.planID},
			"recorded",
			r.at,
		))
	}
	return out, nil
}

// readSoftwareReceipts scans existing plan and journal files in stateDir
// without opening a lock or creating any directory. Only plans with
// Command == "software --install", State == plan.Applied, and an applied
// receipt in their journal are returned, deduplicated by (software, target)
// so the latest receipt wins.
func readSoftwareReceipts(stateDir string) []softwareReceipt {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	plansDir := filepath.Join(stateDir, "plans")
	dirEntries, err := os.ReadDir(plansDir)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, "plan-") && strings.HasSuffix(n, ".json") {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	type key struct {
		software string
		target   string
	}
	byKey := map[key]softwareReceipt{}
	var order []key

	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(plansDir, n))
		if err != nil {
			continue
		}
		var doc plan.Document
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if doc.Command != "software --install" || doc.Software == "" || doc.Target == "" || doc.State != plan.Applied {
			continue
		}
		receiptAt, ok := readAppliedReceiptTime(filepath.Join(stateDir, "journal", doc.Plan+".jsonl"))
		if !ok {
			continue
		}
		if receiptAt == "" {
			receiptAt = doc.CreatedAt
		}
		k := key{software: doc.Software, target: doc.Target}
		prev, exists := byKey[k]
		if !exists {
			order = append(order, k)
			byKey[k] = softwareReceipt{
				software: doc.Software,
				target:   doc.Target,
				planID:   doc.Plan,
				at:       receiptAt,
			}
			continue
		}
		if receiptAt >= prev.at {
			byKey[k] = softwareReceipt{
				software: doc.Software,
				target:   doc.Target,
				planID:   doc.Plan,
				at:       receiptAt,
			}
		}
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].software != order[j].software {
			return order[i].software < order[j].software
		}
		return targetRank(order[i].target) < targetRank(order[j].target)
	})
	out := make([]softwareReceipt, 0, len(order))
	for _, k := range order {
		out = append(out, byKey[k])
	}
	return out
}

func readAppliedReceiptTime(journalPath string) (string, bool) {
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
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
			return rec.At, true
		}
		return "", false
	}
	return "", false
}

type configObservation struct {
	config   string
	target   string
	state    string
	evidence string
	facts    map[string]string
	at       string
}

func configSection(d Deps, stateDir, customRoot, nameFilter, targetFilter string) ([]*output.Record, *diag.Diagnostic, *diag.Diagnostic) {
	var cfgEntries []config.Entry
	if nameFilter != "" {
		var err error
		cfgEntries, err = d.configEntries()
		if err == nil {
			knownInCatalog := false
			for _, e := range cfgEntries {
				if e.Definition.ID == nameFilter {
					knownInCatalog = true
					break
				}
			}
			if !knownInCatalog {
				// Also check if the ledger records this config before rejecting.
				if !configInLedger(stateDir, customRoot, nameFilter) {
					return nil, usageErr("unknown configuration %q", nameFilter), nil
				}
			}
		}
	}

	// Absent custom root yields one config record (resource: "root", state: "absent")
	// and creates no directory.
	if customRoot != "" {
		info, err := os.Stat(customRoot)
		if err != nil {
			if isTrueNotExist(customRoot, err) {
				rec := newRecord(
					SectionConfig,
					"root",
					output.CustomTarget(customRoot),
					"absent",
					map[string]string{},
					"observed",
					d.nowRFC3339(),
				)
				return []*output.Record{rec}, nil, nil
			}
			part := diag.New(diag.StatusPartial, fmt.Sprintf("custom root %s: %v", customRoot, err))
			return nil, nil, &part
		}
		if !info.IsDir() {
			part := diag.New(diag.StatusPartial, fmt.Sprintf("custom root %s is not a directory", customRoot))
			return nil, nil, &part
		}
	}

	ledgerPath := filepath.Join(stateDir, ledger.FileName)
	if customRoot != "" {
		ledgerPath = filepath.Join(customRoot, ".managed-machine", ledger.FileName)
	}
	snap, err := loadLedgerStrict(ledgerPath)
	if err != nil {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading ownership ledger %s: %v", ledgerPath, err))
		return nil, nil, &part
	}

	obs := inspectLedgerConfigs(snap, d.nowRFC3339())
	var out []*output.Record
	for _, o := range obs {
		if nameFilter != "" && o.config != nameFilter {
			continue
		}
		if targetFilter != "" && o.target != targetFilter {
			continue
		}
		out = append(out, newRecord(
			SectionConfig,
			o.config,
			o.target,
			o.state,
			o.facts,
			o.evidence,
			o.at,
		))
	}
	return out, nil, nil
}

func configInLedger(stateDir, customRoot, name string) bool {
	ledgerPath := filepath.Join(stateDir, ledger.FileName)
	if customRoot != "" {
		ledgerPath = filepath.Join(customRoot, ".managed-machine", ledger.FileName)
	}
	snap, err := loadLedgerStrict(ledgerPath)
	if err != nil || snap == nil {
		return false
	}
	for _, r := range snap.Records() {
		if r.Config == name {
			return true
		}
	}
	return false
}

// loadLedgerStrict reads and validates the ownership ledger at path without
// creating any file or directory. A genuinely absent ledger file returns an
// empty snapshot; any read error, non-directory parent, or malformed ledger
// line returns an error so the caller can report MM_STATUS_PARTIAL (exit 4).
func loadLedgerStrict(path string) (*ledger.Snapshot, error) {
	if strings.TrimSpace(path) == "" {
		return &ledger.Snapshot{}, nil
	}
	info, statErr := os.Stat(path)
	if statErr != nil {
		if isTrueNotExist(path, statErr) {
			return &ledger.Snapshot{}, nil
		}
		return nil, statErr
	}
	if info.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []ledger.Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r ledger.Record
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
		recs = append(recs, r)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return ledger.NewSnapshot(recs), nil
}

func inspectLedgerConfigs(snap *ledger.Snapshot, fallbackAt string) []configObservation {
	if snap == nil {
		return nil
	}
	type cfgKey struct {
		config string
		target string
	}
	seen := map[cfgKey]bool{}
	var keys []cfgKey
	latestRec := map[cfgKey]ledger.Record{}
	for _, r := range snap.Records() {
		k := cfgKey{config: r.Config, target: r.Target}
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
		latestRec[k] = r
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].config != keys[j].config {
			return keys[i].config < keys[j].config
		}
		return targetRank(keys[i].target) < targetRank(keys[j].target)
	})

	var out []configObservation
	for _, k := range keys {
		owned := snap.OwnedBy(k.config, k.target)
		last := latestRec[k]
		at := last.At
		if at == "" {
			at = fallbackAt
		}
		if len(owned) == 0 {
			// All records for (config, target) were released.
			facts := map[string]string{}
			if last.Path != "" {
				facts["path"] = last.Path
			}
			if last.Plan != "" {
				facts["plan"] = last.Plan
			}
			out = append(out, configObservation{
				config:   k.config,
				target:   k.target,
				state:    "absent",
				evidence: "observed",
				facts:    facts,
				at:       at,
			})
			continue
		}

		anyUnknown := false
		anyAbsent := false
		var latestOwned ledger.Record
		for i, r := range owned {
			if i == 0 || r.At >= latestOwned.At {
				latestOwned = r
			}
			st := inspectOwnedRecord(r)
			switch st {
			case "unknown":
				anyUnknown = true
			case "absent":
				anyAbsent = true
			}
		}
		state := "present"
		evidence := "observed"
		if anyUnknown {
			state = "unknown"
			evidence = "recorded"
		} else if anyAbsent {
			state = "absent"
			evidence = "observed"
		}
		if latestOwned.At != "" {
			at = latestOwned.At
		}
		facts := map[string]string{
			"path":   latestOwned.Path,
			"plan":   latestOwned.Plan,
			"sha256": strings.TrimPrefix(latestOwned.SHA256, "sha256:"),
		}
		out = append(out, configObservation{
			config:   k.config,
			target:   k.target,
			state:    state,
			evidence: evidence,
			facts:    facts,
			at:       at,
		})
	}
	return out
}

// inspectOwnedRecord performs a fresh read-only check of one owned ledger
// record on disk and returns "present", "absent", or "unknown".
func inspectOwnedRecord(r ledger.Record) string {
	wantSum := strings.TrimPrefix(r.SHA256, "sha256:")
	info, statErr := os.Stat(r.Path)
	if statErr != nil {
		if isTrueNotExist(r.Path, statErr) {
			return "absent"
		}
		return "unknown"
	}
	if info.IsDir() {
		return "unknown"
	}
	data, err := os.ReadFile(r.Path)
	if err != nil {
		if isTrueNotExist(r.Path, err) {
			return "absent"
		}
		return "unknown"
	}
	switch r.Kind {
	case ledger.KindFile:
		if file.SHA256Hex(data) == wantSum {
			return "present"
		}
		return "absent"
	case ledger.KindBlock:
		gotSum, present, err := block.Inspect(data, r.ID)
		if err != nil {
			return "unknown"
		}
		if !present || gotSum != wantSum {
			return "absent"
		}
		return "present"
	case ledger.KindKey:
		gotSum, present, err := structured.Inspect(r.Format, data, r.ID)
		if err != nil {
			return "unknown"
		}
		if !present || gotSum != wantSum {
			return "absent"
		}
		return "present"
	default:
		return "unknown"
	}
}

type memberKey struct {
	id     string
	target string
}

func flavorSection(d Deps, stateDir, customRoot, nameFilter, targetFilter string) ([]*output.Record, *diag.Diagnostic, *diag.Diagnostic) {
	cfgEntries, _ := d.configEntries()
	flavors, err := d.flavors(cfgEntries)
	if err != nil {
		dg := diag.New(diag.ConfigSourceInvalid, err.Error())
		return nil, &dg, nil
	}
	if nameFilter != "" {
		found := false
		for _, fl := range flavors {
			if fl.ID == nameFilter {
				found = true
				break
			}
		}
		if !found {
			return nil, usageErr("unknown flavor %q", nameFilter), nil
		}
	}
	if len(flavors) == 0 {
		return nil, nil, nil
	}

	// Compose all flavors via internal/compose (D6) so any software or config
	// conflicts across flavors are surfaced.
	composed := compose.Compose(flavors, nil)

	receipts := readSoftwareReceipts(stateDir)
	receiptByKey := map[memberKey]softwareReceipt{}
	for _, r := range receipts {
		receiptByKey[memberKey{id: r.software, target: r.target}] = r
	}

	var partDiag *diag.Diagnostic
	ledgerPath := filepath.Join(stateDir, ledger.FileName)
	if customRoot != "" {
		ledgerPath = filepath.Join(customRoot, ".managed-machine", ledger.FileName)
	}
	var cfgObsByKey map[memberKey]configObservation
	ledgerUnreadable := false
	if customRoot != "" {
		if info, err := os.Stat(customRoot); err != nil {
			if !isTrueNotExist(customRoot, err) {
				ledgerUnreadable = true
				part := diag.New(diag.StatusPartial, fmt.Sprintf("custom root %s: %v", customRoot, err))
				partDiag = &part
			}
		} else if !info.IsDir() {
			ledgerUnreadable = true
			part := diag.New(diag.StatusPartial, fmt.Sprintf("custom root %s is not a directory", customRoot))
			partDiag = &part
		}
	}
	if !ledgerUnreadable {
		snap, err := loadLedgerStrict(ledgerPath)
		if err != nil {
			ledgerUnreadable = true
			part := diag.New(diag.StatusPartial, fmt.Sprintf("reading ownership ledger %s: %v", ledgerPath, err))
			partDiag = &part
		} else {
			cfgObsByKey = map[memberKey]configObservation{}
			for _, o := range inspectLedgerConfigs(snap, d.nowRFC3339()) {
				cfgObsByKey[memberKey{id: o.config, target: o.target}] = o
			}
		}
	}

	var out []*output.Record
	for _, fl := range flavors {
		if nameFilter != "" && fl.ID != nameFilter {
			continue
		}
		for _, tgt := range flavorTargets(fl, customRoot) {
			if targetFilter != "" && tgt != targetFilter {
				continue
			}
			rec := evaluateFlavorTarget(fl, tgt, composed, receiptByKey, cfgObsByKey, ledgerUnreadable, d.nowRFC3339())
			out = append(out, rec)
		}
	}
	return out, nil, partDiag
}

func flavorTargets(fl compose.Flavor, customRoot string) []string {
	seen := map[string]bool{}
	var targets []string
	add := func(t string) {
		if t == "custom" && customRoot != "" {
			t = output.CustomTarget(customRoot)
		}
		if t == "" || seen[t] {
			return
		}
		seen[t] = true
		targets = append(targets, t)
	}
	for _, s := range fl.Software {
		add(s.Target)
	}
	for _, c := range fl.Config {
		add(c.Target)
	}
	sort.Slice(targets, func(i, j int) bool {
		if rA, rB := targetRank(targets[i]), targetRank(targets[j]); rA != rB {
			return rA < rB
		}
		return targets[i] < targets[j]
	})
	return targets
}

func matchFlavorTarget(memberTarget, resolvedTarget string) bool {
	if memberTarget == resolvedTarget {
		return true
	}
	return memberTarget == "custom" && strings.HasPrefix(resolvedTarget, "custom:")
}

func evaluateFlavorTarget(
	fl compose.Flavor,
	tgt string,
	composed compose.Result,
	receipts map[memberKey]softwareReceipt,
	cfgObs map[memberKey]configObservation,
	ledgerUnreadable bool,
	fallbackAt string,
) *output.Record {
	single := compose.Compose([]compose.Flavor{fl}, nil)

	var swNames []string
	var cfgNames []string
	var conflicts []string
	anyUnknown := false
	anyAbsent := false
	latestAt := ""

	for _, s := range single.Software {
		if !matchFlavorTarget(s.Target, tgt) {
			continue
		}
		swNames = append(swNames, s.ID)
		if c, ok := composed.ConflictFor(compose.SoftwareIdentity(s.ID, s.Target)); ok {
			conflicts = append(conflicts, c.Detail)
		}
		// Software state is recorded-only ("unknown"), never rendered as "present".
		anyUnknown = true
		if rec, ok := receipts[memberKey{id: s.ID, target: tgt}]; ok && rec.at > latestAt {
			latestAt = rec.at
		}
	}

	for _, c := range single.Config {
		if !matchFlavorTarget(c.Target, tgt) {
			continue
		}
		cfgNames = append(cfgNames, c.ID)
		if ledgerUnreadable {
			anyUnknown = true
			continue
		}
		obs, ok := cfgObs[memberKey{id: c.ID, target: tgt}]
		if !ok {
			anyAbsent = true
			continue
		}
		if obs.at > latestAt {
			latestAt = obs.at
		}
		switch obs.state {
		case "unknown":
			anyUnknown = true
		case "absent":
			anyAbsent = true
		}
	}

	if len(conflicts) > 0 {
		anyUnknown = true
	}

	state := "present"
	evidence := "observed"
	if anyUnknown {
		state = "unknown"
		evidence = "recorded"
	} else if anyAbsent {
		state = "absent"
		evidence = "observed"
	}
	if latestAt == "" {
		latestAt = fallbackAt
	}

	facts := map[string]string{}
	if len(swNames) > 0 {
		facts["software"] = strings.Join(swNames, ",")
	}
	if len(cfgNames) > 0 {
		facts["config"] = strings.Join(cfgNames, ",")
	}
	if len(conflicts) > 0 {
		facts["conflict"] = strings.Join(conflicts, "; ")
	}
	return newRecord(SectionFlavor, fl.ID, tgt, state, facts, evidence, latestAt)
}

// isTrueNotExist reports whether err is os.ErrNotExist AND every existing
// ancestor of p is a directory. On Windows, opening a child of a regular file
// returns ERROR_PATH_NOT_FOUND (which matches os.ErrNotExist); walking the
// ancestors distinguishes a genuinely absent path from a path blocked by a
// regular file on all platforms without relying on chmod.
func isTrueNotExist(p string, err error) bool {
	if !errors.Is(err, os.ErrNotExist) {
		return false
	}
	for dir := filepath.Dir(filepath.Clean(p)); dir != "" && dir != "." && dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		info, statErr := os.Stat(dir)
		if statErr == nil {
			return info.IsDir()
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return false
		}
	}
	return true
}

func targetRank(t string) int {
	switch {
	case t == "global":
		return 0
	case t == "user":
		return 1
	case strings.HasPrefix(t, "custom"):
		return 2
	default:
		return 3
	}
}
