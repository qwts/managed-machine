package status

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qwts/managed-machine/core/internal/config"
	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/block"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/fleet"
	"github.com/qwts/managed-machine/core/internal/fleet/directory"
	"github.com/qwts/managed-machine/core/internal/fleet/policy"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/source/remote"
)

func (d Deps) policyDir(stateDir string) (string, error) {
	if d.PolicyDir != nil {
		return d.PolicyDir()
	}
	if p := strings.TrimSpace(d.getenv(policy.PolicyEnv)); p != "" {
		return policy.Dir(d.getenv, "")
	}
	return policy.Dir(d.getenv, stateDir)
}

func (d Deps) username() (string, error) {
	goos := d.goos()
	if d.Username != nil {
		u, err := d.Username()
		if err != nil {
			return "", err
		}
		if goos == "windows" {
			if i := strings.LastIndexByte(u, '\\'); i >= 0 {
				return u[i+1:], nil
			}
		}
		return u, nil
	}
	return fleet.DefaultUsername(goos)
}

func (d Deps) home() (string, error) {
	if d.Home != nil {
		return d.Home()
	}
	if d.goos() == "windows" {
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

// resolveFleetDestination resolves the authorized_keys destination path per
// D4/D8 without mutating the filesystem.
func (d Deps) resolveFleetDestination() (string, error) {
	goos := d.goos()
	if explicit := strings.TrimSpace(d.getenv(config.DesiredEnv)); explicit != "" {
		des, err := config.LoadDesired(filepath.Clean(explicit))
		if err != nil {
			return "", err
		}
		if des.Target != "" && des.Target != fleet.TargetUser {
			return "", fmt.Errorf("desired configuration names target %q, not %q", des.Target, fleet.TargetUser)
		}
		if p := des.Fleet.Destinations["authorized_keys"]; p != "" {
			return filepath.Clean(p), nil
		}
	} else {
		if desiredPath, err := config.DesiredPath(d.getenv, goos, fleet.TargetUser, ""); err == nil {
			if fi, statErr := os.Stat(desiredPath); statErr == nil && !fi.IsDir() {
				des, err := config.LoadDesired(desiredPath)
				if err != nil {
					return "", err
				}
				if des.Target != "" && des.Target != fleet.TargetUser {
					return "", fmt.Errorf("desired configuration names target %q, not %q", des.Target, fleet.TargetUser)
				}
				if p := des.Fleet.Destinations["authorized_keys"]; p != "" {
					return filepath.Clean(p), nil
				}
			}
		}
		if goos == "windows" && strings.TrimSpace(d.getenv("APPDATA")) == "" {
			if home, err := d.home(); err == nil {
				fallback := filepath.Join(home, ".config", "managed-machine", "desired.yaml")
				if fi, statErr := os.Stat(fallback); statErr == nil && !fi.IsDir() {
					des, err := config.LoadDesired(fallback)
					if err != nil {
						return "", err
					}
					if des.Target != "" && des.Target != fleet.TargetUser {
						return "", fmt.Errorf("desired configuration names target %q, not %q", des.Target, fleet.TargetUser)
					}
					if p := des.Fleet.Destinations["authorized_keys"]; p != "" {
						return filepath.Clean(p), nil
					}
				}
			}
		}
	}
	home, err := d.home()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", nil
	}
	return filepath.Join(home, ".ssh", "authorized_keys"), nil
}

// fleetSection implements `status --section fleet [name]` (S5 story L06;
// decision D8). It emits the four fleet resources (directory, policy,
// authorized-keys, enrollment) in the fixed D7 record shape with target
// "user", never fetches over the network, never opens the plan-store lock,
// and marks the report partial (MM_STATUS_PARTIAL, exit 4) on observer
// failures while printing the remaining resources.
func fleetSection(d Deps, stateDir, nameFilter string) ([]*output.Record, *diag.Diagnostic, *diag.Diagnostic) {
	if nameFilter != "" {
		switch nameFilter {
		case FleetResourceDirectory, FleetResourcePolicy, FleetResourceAuthorizedKeys, FleetResourceEnrollment:
		default:
			return nil, usageErr("unknown fleet resource %q; supported: directory, policy, authorized-keys, enrollment", nameFilter), nil
		}
	}

	now := d.nowRFC3339()
	wantDir := nameFilter == "" || nameFilter == FleetResourceDirectory
	wantPol := nameFilter == "" || nameFilter == FleetResourcePolicy
	wantAuth := nameFilter == "" || nameFilter == FleetResourceAuthorizedKeys
	wantEnroll := nameFilter == "" || nameFilter == FleetResourceEnrollment

	var out []*output.Record
	var partDiag *diag.Diagnostic
	recordPart := func(dg *diag.Diagnostic) {
		if dg != nil && partDiag == nil {
			partDiag = dg
		}
	}

	if wantDir || wantPol {
		dirRec, polRec, pd := observePolicyAndDirectory(d, stateDir, now, wantDir, wantPol)
		recordPart(pd)
		if wantDir && dirRec != nil {
			out = append(out, dirRec)
		}
		if wantPol && polRec != nil {
			out = append(out, polRec)
		}
	}
	if wantAuth {
		authRec, pd := observeAuthorizedKeys(d, stateDir, now)
		recordPart(pd)
		if authRec != nil {
			out = append(out, authRec)
		}
	}
	if wantEnroll {
		enrollRec, pd := observeEnrollment(stateDir, now)
		recordPart(pd)
		if enrollRec != nil {
			out = append(out, enrollRec)
		}
	}
	return out, nil, partDiag
}

func observePolicyAndDirectory(d Deps, stateDir, now string, wantDir, wantPol bool) (*output.Record, *output.Record, *diag.Diagnostic) {
	absentDir := func() *output.Record {
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "absent", map[string]string{}, "observed", now)
	}
	absentPol := func() *output.Record {
		return newRecord(SectionFleet, FleetResourcePolicy, fleet.TargetUser, "absent", map[string]string{}, "observed", now)
	}
	unknownDir := func() *output.Record {
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now)
	}
	unknownPol := func() *output.Record {
		return newRecord(SectionFleet, FleetResourcePolicy, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now)
	}

	polDir, err := d.policyDir(stateDir)
	if err != nil || strings.TrimSpace(polDir) == "" {
		return absentDir(), absentPol(), nil
	}
	polInfo, statErr := os.Stat(polDir)
	if statErr != nil {
		if isTrueNotExist(polDir, statErr) {
			return absentDir(), absentPol(), nil
		}
		part := diag.New(diag.StatusPartial, fmt.Sprintf("policy directory %s: %v", polDir, statErr))
		return unknownDir(), unknownPol(), &part
	}
	if !polInfo.IsDir() {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("policy path %s is not a directory", polDir))
		return unknownDir(), unknownPol(), &part
	}

	kdPath := filepath.Join(polDir, filepath.FromSlash(policy.KeyDirectoryRelPath))
	kdInfo, kdErr := os.Stat(kdPath)
	if kdErr != nil {
		if isTrueNotExist(kdPath, kdErr) {
			return absentDir(), absentPol(), nil
		}
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading %s: %v", kdPath, kdErr))
		return unknownDir(), unknownPol(), &part
	}
	if kdInfo.IsDir() {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("%s is a directory", kdPath))
		return unknownDir(), unknownPol(), &part
	}

	pol, loadErr := policy.Load(polDir)
	if loadErr != nil {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading fleet policy %s: %v", polDir, loadErr))
		var dirRec *output.Record
		if wantDir {
			if kdData, rerr := os.ReadFile(kdPath); rerr == nil {
				if kd, perr := policy.ParseKeyDirectorySource(policy.KeyDirectoryRelPath, kdData); perr == nil {
					dirRec, _ = observeDirectorySource(kd, stateDir, now)
				}
			}
			if dirRec == nil {
				dirRec = unknownDir()
			}
		}
		return dirRec, unknownPol(), &part
	}

	var polRec *output.Record
	if wantPol {
		polRec = newRecord(SectionFleet, FleetResourcePolicy, fleet.TargetUser, "present", buildPolicyFacts(pol), "observed", now)
	}
	var dirRec *output.Record
	var partDiag *diag.Diagnostic
	if wantDir {
		dirRec, partDiag = observeDirectorySource(pol.KeyDirectory, stateDir, now)
	}
	return dirRec, polRec, partDiag
}

func buildPolicyFacts(pol *policy.Policy) map[string]string {
	seenAccounts := map[string]bool{}
	for _, g := range pol.Groups {
		for _, a := range g.Accounts {
			seenAccounts[a] = true
		}
	}
	cutover := "absent"
	if pol.Cutover != nil {
		cutover = "present"
	}
	return map[string]string{
		"accounts": strconv.Itoa(len(seenAccounts)),
		"cutover":  cutover,
		"groups":   strconv.Itoa(len(pol.Groups)),
	}
}

func observeDirectorySource(kd policy.KeyDirectorySource, stateDir, now string) (*output.Record, *diag.Diagnostic) {
	if kd.Path != "" {
		info, statErr := os.Stat(kd.Path)
		if statErr != nil {
			if isTrueNotExist(kd.Path, statErr) {
				return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "absent", map[string]string{}, "observed", now), nil
			}
			part := diag.New(diag.StatusPartial, fmt.Sprintf("key directory %s: %v", kd.Path, statErr))
			return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
		}
		if !info.IsDir() {
			part := diag.New(diag.StatusPartial, fmt.Sprintf("key directory %s is not a directory", kd.Path))
			return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
		}
		dir, err := directory.LoadPath(kd.Path)
		if err != nil {
			part := diag.New(diag.StatusPartial, fmt.Sprintf("reading key directory %s: %v", kd.Path, err))
			return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
		}
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "present", directoryFacts(dir), "observed", now), nil
	}

	if strings.TrimSpace(stateDir) == "" {
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "absent", map[string]string{}, "observed", now), nil
	}
	gistCacheDir := filepath.Join(stateDir, "fleet", "directory", "gist")
	if info, statErr := os.Stat(gistCacheDir); statErr != nil {
		if !isTrueNotExist(gistCacheDir, statErr) {
			part := diag.New(diag.StatusPartial, fmt.Sprintf("cached key directory %s: %v", gistCacheDir, statErr))
			return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
		}
	} else if !info.IsDir() {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("cached key directory %s is not a directory", gistCacheDir))
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}

	cached, err := remote.CachedGistDirectory(kd.Gist, stateDir)
	if err != nil {
		if errors.Is(err, remote.ErrNotCached) {
			return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "absent", map[string]string{}, "observed", now), nil
		}
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading cached key directory gist:%s: %v", kd.Gist, err))
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}
	dir, err := directory.LoadGistFS("gist:"+kd.Gist+"@"+cached.Revision, cached.FS)
	if err != nil {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading cached key directory gist:%s: %v", kd.Gist, err))
		return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}
	at := cached.At
	if at == "" {
		at = now
	}
	return newRecord(SectionFleet, FleetResourceDirectory, fleet.TargetUser, "present", directoryFacts(dir), "recorded", at), nil
}

func directoryFacts(dir directory.Directory) map[string]string {
	invalid := 0
	for _, e := range dir.Entries {
		for _, k := range e.Keys {
			if !k.Valid() {
				invalid++
			}
		}
	}
	return map[string]string{
		"invalid": strconv.Itoa(invalid),
		"keys":    strconv.Itoa(dir.KeyCount()),
		"owners":  strconv.Itoa(dir.OwnerCount()),
		"source":  dir.Identity,
	}
}

func observeAuthorizedKeys(d Deps, stateDir, now string) (*output.Record, *diag.Diagnostic) {
	account, _ := d.username()
	destPath, desErr := d.resolveFleetDestination()

	var ledgerPath string
	if strings.TrimSpace(stateDir) != "" {
		ledgerPath = fleet.FleetLedgerPath(stateDir)
	}
	snap, ledgerErr := loadLedgerStrict(ledgerPath)
	if ledgerErr != nil {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading fleet ownership ledger %s: %v", ledgerPath, ledgerErr))
		legacyBlock := "absent"
		if destPath != "" {
			if data, rerr := os.ReadFile(destPath); rerr == nil && hasLegacyBlock(data) {
				legacyBlock = "present"
			}
		}
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": legacyBlock,
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "unknown", facts, "recorded", now), &part
	}

	var latestOwned *ledger.Record
	var lastRec *ledger.Record
	if snap != nil {
		for _, r := range snap.Records() {
			if r.Config == fleet.FleetID && r.Target == fleet.TargetUser && r.Kind == ledger.KindBlock && r.ID == fleet.BlockID {
				rec := r
				lastRec = &rec
			}
		}
		for _, r := range snap.OwnedBy(fleet.FleetID, fleet.TargetUser) {
			if r.Kind == ledger.KindBlock && r.ID == fleet.BlockID {
				rec := r
				if latestOwned == nil || rec.At >= latestOwned.At {
					latestOwned = &rec
				}
			}
		}
	}

	if latestOwned != nil && latestOwned.Path != "" {
		destPath = latestOwned.Path
	} else if destPath == "" && lastRec != nil && lastRec.Path != "" {
		destPath = lastRec.Path
	}
	if account == "" {
		if latestOwned != nil && latestOwned.Account != "" {
			account = latestOwned.Account
		} else if lastRec != nil && lastRec.Account != "" {
			account = lastRec.Account
		}
	}

	recAt := now
	if latestOwned != nil && latestOwned.At != "" {
		recAt = latestOwned.At
	} else if lastRec != nil && lastRec.At != "" {
		recAt = lastRec.At
	}

	if desErr != nil {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("resolving authorized_keys destination: %v", desErr))
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": "absent",
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "unknown", facts, "recorded", recAt), &part
	}

	if destPath == "" {
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": "absent",
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "absent", facts, "observed", recAt), nil
	}

	info, statErr := os.Stat(destPath)
	if statErr != nil {
		if isTrueNotExist(destPath, statErr) {
			facts := map[string]string{
				"account":      account,
				"authorized":   "0",
				"legacy_block": "absent",
			}
			return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "absent", facts, "observed", recAt), nil
		}
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": "absent",
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "unknown", facts, "recorded", recAt), nil
	}
	if info.IsDir() {
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": "absent",
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "unknown", facts, "recorded", recAt), nil
	}

	data, readErr := os.ReadFile(destPath)
	if readErr != nil {
		if isTrueNotExist(destPath, readErr) {
			facts := map[string]string{
				"account":      account,
				"authorized":   "0",
				"legacy_block": "absent",
			}
			return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "absent", facts, "observed", recAt), nil
		}
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": "absent",
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "unknown", facts, "recorded", recAt), nil
	}

	legacyBlock := "absent"
	if hasLegacyBlock(data) {
		legacyBlock = "present"
	}

	gotSum, present, inspectErr := block.Inspect(data, fleet.BlockID)
	if inspectErr != nil {
		facts := map[string]string{
			"account":      account,
			"authorized":   "0",
			"legacy_block": legacyBlock,
		}
		return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "unknown", facts, "recorded", recAt), nil
	}

	if latestOwned != nil {
		wantSum := strings.TrimPrefix(latestOwned.SHA256, "sha256:")
		if present && gotSum == wantSum {
			authCount := countBlockKeys(data, fleet.BlockID)
			facts := map[string]string{
				"account":      account,
				"authorized":   strconv.Itoa(authCount),
				"legacy_block": legacyBlock,
			}
			return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "present", facts, "observed", recAt), nil
		}
	}

	facts := map[string]string{
		"account":      account,
		"authorized":   "0",
		"legacy_block": legacyBlock,
	}
	return newRecord(SectionFleet, FleetResourceAuthorizedKeys, fleet.TargetUser, "absent", facts, "observed", recAt), nil
}

func hasLegacyBlock(data []byte) bool {
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if line == fleet.LegacyManagedMachineBegin || line == fleet.LegacyHomeBinBegin {
			return true
		}
	}
	return false
}

func countBlockKeys(data []byte, blockID string) int {
	beginLine := block.DefaultComment + " managed-machine:begin " + blockID
	endLine := block.DefaultComment + " managed-machine:end " + blockID
	inBlock := false
	keys := 0
	for _, rawLine := range strings.Split(string(data), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if line == beginLine {
			inBlock = true
			continue
		}
		if line == endLine {
			break
		}
		if inBlock {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
				keys++
			}
		}
	}
	return keys
}

func observeEnrollment(stateDir, now string) (*output.Record, *diag.Diagnostic) {
	if strings.TrimSpace(stateDir) == "" {
		return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "absent", map[string]string{}, "observed", now), nil
	}
	enrollPath := filepath.Join(stateDir, "fleet", "enrollment.json")
	info, statErr := os.Stat(enrollPath)
	if statErr != nil {
		if isTrueNotExist(enrollPath, statErr) {
			return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "absent", map[string]string{}, "observed", now), nil
		}
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading enrollment %s: %v", enrollPath, statErr))
		return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}
	if info.IsDir() {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("enrollment path %s is a directory", enrollPath))
		return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}
	raw, err := os.ReadFile(enrollPath)
	if err != nil {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("reading enrollment %s: %v", enrollPath, err))
		return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}
	var e fleet.Enrollment
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil ||
		e.SchemaVersion != fleet.EnrollmentSchemaVersion ||
		e.Owner == "" ||
		e.Fingerprint == "" ||
		e.MachineID == "" ||
		(e.Outcome != fleet.OutcomeWritten && e.Outcome != fleet.OutcomeStaged) ||
		e.At == "" {
		part := diag.New(diag.StatusPartial, fmt.Sprintf("invalid enrollment record %s", enrollPath))
		return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "unknown", map[string]string{}, "recorded", now), &part
	}
	facts := map[string]string{
		"fingerprint": e.Fingerprint,
		"machine_id":  e.MachineID,
		"outcome":     e.Outcome,
		"owner":       e.Owner,
	}
	return newRecord(SectionFleet, FleetResourceEnrollment, fleet.TargetUser, "present", facts, "recorded", e.At), nil
}
