// Command mm-fleet-import is a one-off migration tool (S5 story L05;
// decision D6) that reads legacy machine records from
// <checkout>/fleet/machines/*.toml and writes a seed key-directory entry
// (<out>/keys/<owner>.yaml) and access-group policy document
// (<out>/fleet/access-groups/<group>.yaml) that validate through the L01
// loaders, plus a summary report on stdout.
//
//	mm-fleet-import [--json | --human] <checkout> -owner <id> -group <id> -account <name> -out <dir>
//
// Fingerprints are computed from public_key bytes via directory.ParsePublicKey.
// Any record whose recorded ssh_key_fingerprint disagrees with its computed
// fingerprint is reported on stdout with machine_id and excluded from both YAML
// files. Report records never carry hostnames or key bytes. The tool never
// reads .git, never writes outside -out, and refuses an -out equal to or inside
// <checkout> or the managed-machine state directory.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/fleet/policy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/statedir"
)

const usageLine = "usage: mm-fleet-import [--json | --human] <checkout> -owner <id> -group <id> -account <name> -out <dir>"

func main() {
	os.Exit(run(os.Args[1:], runtime.GOOS, os.Getenv, os.Stdout, os.Stderr))
}

type importKeyYAML struct {
	Fingerprint string `yaml:"fingerprint"`
	Key         string `yaml:"key"`
	Label       string `yaml:"label,omitempty"`
	PublishedAt string `yaml:"published_at"`
	Revision    int    `yaml:"revision"`
}

type importDirectoryYAML struct {
	SchemaVersion int             `yaml:"schema_version"`
	Kind          string          `yaml:"kind"`
	ID            string          `yaml:"id"`
	Keys          []importKeyYAML `yaml:"keys"`
}

type importAccessGroupYAML struct {
	SchemaVersion        int      `yaml:"schema_version"`
	Kind                 string   `yaml:"kind"`
	ID                   string   `yaml:"id"`
	MaintainedBy         []string `yaml:"maintained_by"`
	Accounts             []string `yaml:"accounts"`
	ApprovedFingerprints []string `yaml:"approved_fingerprints"`
}

type tomlMachineRecord struct {
	FileName          string
	SchemaVersion     int
	HasSchemaVersion  bool
	MachineID         string
	Hostname          string
	ManagedAt         string
	SSHKeyFingerprint string
	PublicKey         string
}

type reportRow struct {
	MachineID           string
	Owner               string
	Group               string
	Account             string
	RecordedFingerprint string
	ComputedFingerprint string
	FingerprintMatch    bool
	Status              string
	PublishedAt         string
}

func run(args []string, goos string, getenv func(string) string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprintln(stdout, usageLine)
			return 0
		}
	}

	reordered, err := reorderArgs(args)
	if err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		fmt.Fprintln(stderr, usageLine)
		return 2
	}

	fs := flag.NewFlagSet("mm-fleet-import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		ownerFlag   string
		groupFlag   string
		accountFlag string
		outFlag     string
		jsonFlag    bool
		humanFlag   bool
	)
	fs.StringVar(&ownerFlag, "owner", "", "key-directory owner id")
	fs.StringVar(&groupFlag, "group", "", "access-group id")
	fs.StringVar(&accountFlag, "account", "", "local OS account name")
	fs.StringVar(&outFlag, "out", "", "output directory outside checkout and state directory")
	fs.BoolVar(&jsonFlag, "json", false, "emit one indented JSON document")
	fs.BoolVar(&humanFlag, "human", false, "emit a human-readable table")

	if err := fs.Parse(reordered); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	mode, err := output.SelectMode(jsonFlag, humanFlag)
	if err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		fmt.Fprintln(stderr, usageLine)
		return 2
	}
	pos := fs.Args()
	if len(pos) != 1 || strings.TrimSpace(pos[0]) == "" ||
		strings.TrimSpace(ownerFlag) == "" ||
		strings.TrimSpace(groupFlag) == "" ||
		strings.TrimSpace(accountFlag) == "" ||
		strings.TrimSpace(outFlag) == "" {
		fmt.Fprintln(stderr, usageLine)
		return 2
	}

	checkout := pos[0]
	if !policy.ValidOwnerID(ownerFlag) {
		fmt.Fprintf(stderr, "mm-fleet-import: invalid -owner %q\n", ownerFlag)
		return 1
	}
	if !schema.ValidID(groupFlag) {
		fmt.Fprintf(stderr, "mm-fleet-import: invalid -group %q\n", groupFlag)
		return 1
	}
	if !policy.ValidAccount(accountFlag) {
		fmt.Fprintf(stderr, "mm-fleet-import: invalid -account %q\n", accountFlag)
		return 1
	}

	if err := checkOutputBoundary(checkout, outFlag, goos, getenv); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}

	records, err := loadLegacyMachines(checkout)
	if err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}

	var (
		agreedKeys   []importKeyYAML
		approvedFPs  []string
		seenAgreedFP = map[string]string{}
		reportRows   = make([]reportRow, 0, len(records))
	)

	for _, rec := range records {
		pk, err := directory.ParsePublicKey(rec.PublicKey)
		if err != nil {
			fmt.Fprintf(stderr, "mm-fleet-import: %s: invalid public_key: %v\n", rec.FileName, err)
			return 1
		}
		match := rec.SSHKeyFingerprint == pk.Fingerprint
		status := "excluded"
		if match {
			if prev, dup := seenAgreedFP[pk.Fingerprint]; dup {
				fmt.Fprintf(stderr, "mm-fleet-import: %s: duplicate public key fingerprint %s (already in %s)\n", rec.FileName, pk.Fingerprint, prev)
				return 1
			}
			seenAgreedFP[pk.Fingerprint] = rec.FileName
			status = "imported"
			agreedKeys = append(agreedKeys, importKeyYAML{
				Fingerprint: pk.Fingerprint,
				Key:         pk.KeyType + " " + pk.Base64Blob,
				Label:       rec.Hostname,
				PublishedAt: rec.ManagedAt,
				Revision:    1,
			})
			approvedFPs = append(approvedFPs, pk.Fingerprint)
		}
		reportRows = append(reportRows, reportRow{
			MachineID:           rec.MachineID,
			Owner:               ownerFlag,
			Group:               groupFlag,
			Account:             accountFlag,
			RecordedFingerprint: rec.SSHKeyFingerprint,
			ComputedFingerprint: pk.Fingerprint,
			FingerprintMatch:    match,
			Status:              status,
			PublishedAt:         rec.ManagedAt,
		})
	}

	sort.Slice(agreedKeys, func(i, j int) bool {
		return agreedKeys[i].Fingerprint < agreedKeys[j].Fingerprint
	})
	sort.Strings(approvedFPs)
	if agreedKeys == nil {
		agreedKeys = []importKeyYAML{}
	}
	if approvedFPs == nil {
		approvedFPs = []string{}
	}

	dirDoc := importDirectoryYAML{
		SchemaVersion: directory.SchemaVersion,
		Kind:          directory.KindKeyDirectory,
		ID:            ownerFlag,
		Keys:          agreedKeys,
	}
	dirBytes, err := marshalYAML(dirDoc)
	if err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: marshal key directory: %v\n", err)
		return 1
	}
	ownerRel := "keys/" + ownerFlag + ".yaml"
	parsedEntry, err := directory.ParseEntry(ownerRel, ownerFlag+".yaml", dirBytes)
	if err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: validate %s: %v\n", ownerRel, err)
		return 1
	}
	for _, k := range parsedEntry.Keys {
		if !k.Valid() {
			fmt.Fprintf(stderr, "mm-fleet-import: validate %s: key %s is not valid (%s)\n", ownerRel, k.Fingerprint, k.Detail)
			return 1
		}
	}

	groupDoc := importAccessGroupYAML{
		SchemaVersion:        policy.SchemaVersion,
		Kind:                 policy.KindAccessGroup,
		ID:                   groupFlag,
		MaintainedBy:         []string{ownerFlag},
		Accounts:             []string{accountFlag},
		ApprovedFingerprints: approvedFPs,
	}
	groupBytes, err := marshalYAML(groupDoc)
	if err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: marshal access group: %v\n", err)
		return 1
	}
	groupRel := "fleet/access-groups/" + groupFlag + ".yaml"
	if _, err := policy.ParseAccessGroup(groupRel, groupBytes); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: validate %s: %v\n", groupRel, err)
		return 1
	}

	keysDir := filepath.Join(outFlag, "keys")
	groupsDir := filepath.Join(outFlag, "fleet", "access-groups")
	if err := os.MkdirAll(keysDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(groupsDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(keysDir, ownerFlag+".yaml"), dirBytes, 0o644); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}
	if err := os.WriteFile(filepath.Join(groupsDir, groupFlag+".yaml"), groupBytes, 0o644); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}

	// Verify the written key directory loads cleanly through directory.LoadPath.
	if _, err := directory.LoadPath(keysDir); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: validate written key directory: %v\n", err)
		return 1
	}

	outRecords := make([]*output.Record, 0, len(reportRows))
	for _, r := range reportRows {
		rec := output.New().
			Set("machine_id", r.MachineID).
			Set("owner", r.Owner).
			Set("group", r.Group).
			Set("account", r.Account).
			Set("recorded_fingerprint", r.RecordedFingerprint).
			Set("computed_fingerprint", r.ComputedFingerprint).
			Set("fingerprint_match", r.FingerprintMatch).
			Set("status", r.Status).
			Set("published_at", r.PublishedAt)
		outRecords = append(outRecords, rec)
	}
	if err := output.Write(stdout, mode, outRecords); err != nil {
		fmt.Fprintf(stderr, "mm-fleet-import: %v\n", err)
		return 1
	}
	return 0
}

func reorderArgs(args []string) ([]string, error) {
	valueFlags := map[string]bool{
		"-owner":    true,
		"--owner":   true,
		"-group":    true,
		"--group":   true,
		"-account":  true,
		"--account": true,
		"-out":      true,
		"--out":     true,
	}
	var flags []string
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			if !strings.Contains(a, "=") && valueFlags[a] {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("flag needs an argument: %s", a)
				}
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...), nil
}

func checkOutputBoundary(checkout, outDir, goos string, getenv func(string) string) error {
	checkoutAbs, err := filepath.Abs(checkout)
	if err != nil {
		return fmt.Errorf("resolve checkout path: %w", err)
	}
	outAbs, err := filepath.Abs(outDir)
	if err != nil {
		return fmt.Errorf("resolve -out path: %w", err)
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	if getenv == nil {
		getenv = os.Getenv
	}
	stateDir, err := statedir.Path(goos, getenv)
	if err != nil {
		return fmt.Errorf("resolve state directory: %w", err)
	}
	stateAbs, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("resolve state directory path: %w", err)
	}

	ci := goos == "windows"

	if pathWithinOrEqual(outAbs, checkoutAbs, ci) || pathWithinOrEqual(resolveExistingSymlinks(outAbs), resolveExistingSymlinks(checkoutAbs), ci) {
		return fmt.Errorf("refusing -out %q inside or equal to checkout %q", outDir, checkout)
	}
	if pathWithinOrEqual(outAbs, stateAbs, ci) || pathWithinOrEqual(resolveExistingSymlinks(outAbs), resolveExistingSymlinks(stateAbs), ci) {
		return fmt.Errorf("refusing -out %q inside or equal to state directory %q", outDir, stateDir)
	}
	return nil
}

func pathWithinOrEqual(child, parent string, caseInsensitive bool) bool {
	c := filepath.Clean(child)
	p := filepath.Clean(parent)
	if caseInsensitive {
		c = strings.ToLower(c)
		p = strings.ToLower(p)
	}
	if c == p {
		return true
	}
	rel, err := filepath.Rel(p, c)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

func resolveExistingSymlinks(p string) string {
	clean := filepath.Clean(p)
	var tail []string
	cur := clean
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, tail[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return clean
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func loadLegacyMachines(checkout string) ([]tomlMachineRecord, error) {
	machinesDir := filepath.Join(checkout, "fleet", "machines")
	ents, err := os.ReadDir(machinesDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", machinesDir, err)
	}
	var names []string
	for _, e := range ents {
		name := e.Name()
		if !strings.HasSuffix(name, ".toml") {
			continue
		}
		if e.IsDir() || !e.Type().IsRegular() {
			return nil, fmt.Errorf("%s: must be a regular file", filepath.Join(machinesDir, name))
		}
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("no legacy machine records found in %s", machinesDir)
	}

	out := make([]tomlMachineRecord, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(machinesDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		rec, err := parseTOMLMachineRecord(name, data)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}

func parseTOMLMachineRecord(fileName string, data []byte) (tomlMachineRecord, error) {
	rec := tomlMachineRecord{FileName: fileName}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	for lineNo, raw := range lines {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return tomlMachineRecord{}, fmt.Errorf("%s:%d: expected key = value", fileName, lineNo+1)
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.TrimSpace(line[eq+1:])
		if key == "schema_version" {
			v, err := strconv.Atoi(val)
			if err != nil {
				return tomlMachineRecord{}, fmt.Errorf("%s:%d: invalid schema_version %q", fileName, lineNo+1, val)
			}
			rec.SchemaVersion = v
			rec.HasSchemaVersion = true
			continue
		}
		if len(val) < 2 || val[0] != '"' || val[len(val)-1] != '"' {
			return tomlMachineRecord{}, fmt.Errorf("%s:%d: expected quoted string for %s", fileName, lineNo+1, key)
		}
		strVal := val[1 : len(val)-1]
		switch key {
		case "machine_id":
			rec.MachineID = strVal
		case "hostname":
			rec.Hostname = strVal
		case "managed_at":
			rec.ManagedAt = strVal
		case "ssh_key_fingerprint":
			rec.SSHKeyFingerprint = strVal
		case "public_key":
			rec.PublicKey = strVal
		case "managed_machine_ref", "local_bin_ref":
			// Legacy pin fields are not part of key-directory or access-group documents.
		default:
			return tomlMachineRecord{}, fmt.Errorf("%s:%d: unknown field %q", fileName, lineNo+1, key)
		}
	}

	if !rec.HasSchemaVersion || rec.SchemaVersion != 1 {
		return tomlMachineRecord{}, fmt.Errorf("%s: unsupported or missing schema_version %d", fileName, rec.SchemaVersion)
	}
	if rec.MachineID == "" {
		return tomlMachineRecord{}, fmt.Errorf("%s: missing machine_id", fileName)
	}
	if rec.Hostname == "" {
		return tomlMachineRecord{}, fmt.Errorf("%s: missing hostname", fileName)
	}
	if !policy.ValidRFC3339UTC(rec.ManagedAt) {
		return tomlMachineRecord{}, fmt.Errorf("%s: invalid managed_at %q; must be RFC 3339 UTC", fileName, rec.ManagedAt)
	}
	if rec.SSHKeyFingerprint == "" {
		return tomlMachineRecord{}, fmt.Errorf("%s: missing ssh_key_fingerprint", fileName)
	}
	if rec.PublicKey == "" {
		return tomlMachineRecord{}, fmt.Errorf("%s: missing public_key", fileName)
	}
	return rec, nil
}

func marshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	out := bytes.ReplaceAll(buf.Bytes(), []byte("\r\n"), []byte("\n"))
	if len(out) == 0 {
		return nil, errors.New("empty YAML output")
	}
	return out, nil
}
