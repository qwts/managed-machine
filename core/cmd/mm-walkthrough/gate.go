package main

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/statedir"
)

// errNoProcess is the failure a kill reports when the process never
// started.
var errNoProcess = errors.New("the process did not start")

// pemCertificate encodes a DER certificate as PEM.
func pemCertificate(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The S2 install gate (issue #273) shares these types with the install
// command and the aggregate. The evidence record is the only artifact the
// hosted jobs attach, so it carries digests, plan ids and stable
// placeholders only: never a runner path, never a file content.

// installEvidenceKind names the evidence record of the install gate.
const installEvidenceKind = "s2-install-gate"

// InstallEvidence is the per-target record of the install gate. The field
// order is the serialized key order.
type InstallEvidence struct {
	SchemaVersion  int           `json:"schema_version"`
	Kind           string        `json:"kind"`
	Target         string        `json:"target"`
	OS             string        `json:"os"`
	Arch           string        `json:"arch"`
	SourceRevision string        `json:"source_revision"`
	BinarySHA256   string        `json:"binary_sha256"`
	Mode           string        `json:"mode"`
	Setup          []string      `json:"setup"`
	Steps          []InstallStep `json:"steps"`
	Plans          []PlanRecord  `json:"plans"`
	Guards         InstallGuards `json:"guards"`
	Result         string        `json:"result"`
	Failures       []string      `json:"failures"`
}

// InstallStep is one checked step of the gate.
type InstallStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// PlanRecord is one plan the gate observed: its id, the state it ended in
// and the outcome of its receipt.
type PlanRecord struct {
	Plan    string `json:"plan"`
	State   string `json:"state"`
	Receipt string `json:"receipt"`
}

// InstallGuards are the machine-change guards of the gate: the programs that
// must never be started and the profiles that must stay byte-identical.
type InstallGuards struct {
	SentinelInvocations []string `json:"sentinel_invocations"`
	ProfileChanges      []string `json:"profile_changes"`
}

// gatePaths are the paths the evidence never carries: the harness root, the
// runner's real home and the temporary directory that holds the harness.
type gatePaths struct {
	root, home, tmp string
}

// replacements lists the path and its placeholder, longest path first, so a
// nested path is replaced before the directory that holds it.
func (p gatePaths) replacements() [][2]string {
	return [][2]string{
		{p.root, "<harness>"},
		{p.home, "<home>"},
		{p.tmp, "<tmp>"},
	}
}

// clean replaces every known path in s.
func (p gatePaths) clean(s string) string {
	for _, r := range p.replacements() {
		if r[0] == "" {
			continue
		}
		s = strings.ReplaceAll(s, r[0], r[1])
		s = strings.ReplaceAll(s, filepath.ToSlash(r[0]), r[1])
	}
	return s
}

// scrub replaces every known path in the encoded evidence, in the form the
// encoder wrote it, so no path survives in any field.
func (p gatePaths) scrub(b []byte) []byte {
	for _, r := range p.replacements() {
		if r[0] == "" {
			continue
		}
		b = bytes.ReplaceAll(b, []byte(r[0]), []byte(r[1]))
		if esc, err := json.Marshal(r[0]); err == nil {
			b = bytes.ReplaceAll(b, esc[1:len(esc)-1], []byte(r[1]))
		}
	}
	return b
}

// brewPrefixOverride replaces the canonical Homebrew prefix the gate looks
// for. It is empty in production and is never set from a flag or an
// environment variable: only the gate's own tests set it, to a prefix that
// does not exist, so that no test can reach a real Homebrew.
var brewPrefixOverride string

// gatePrefix is the Homebrew prefix the gate resolves for a target.
func gatePrefix(target string) string {
	if brewPrefixOverride != "" {
		return brewPrefixOverride
	}
	return brewDefaultPrefix(target)
}

// installHarness is the isolated environment every core invocation of the
// install gate runs in: its own home, state directory, definition source
// and sentinel directory.
type installHarness struct {
	bin, target string
	root        string
	home        string
	state       string
	defs        string
	sents       string
	log         string
	realHome    string
	prefix      string
	env         []string
	paths       gatePaths
	ev          *InstallEvidence
	profiles    map[string]string
	ripgrepPlan string
	tapped      bool
}

// newInstallHarness builds the harness for one target: the directories, the
// sentinels, the fixture definitions and the explicit environment every
// core invocation gets.
func newInstallHarness(bin, target, revision string, raw []byte) (*installHarness, error) {
	root, err := os.MkdirTemp("", "mm-install-gate-")
	if err != nil {
		return nil, err
	}
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	h := &installHarness{
		bin: bin, target: target, root: root,
		home:     filepath.Join(root, "home"),
		state:    filepath.Join(root, "state"),
		defs:     filepath.Join(root, "defs"),
		sents:    filepath.Join(root, "sentinels"),
		log:      filepath.Join(root, "sentinels", "invocations.log"),
		realHome: realHome,
		prefix:   gatePrefix(target),
	}
	h.paths = gatePaths{root: root, home: realHome, tmp: strings.TrimSuffix(os.TempDir(), string(os.PathSeparator))}
	h.ev = &InstallEvidence{
		SchemaVersion: 1, Kind: installEvidenceKind, Target: target,
		OS: targetOS(target), Arch: targetArch(target),
		SourceRevision: revision, BinarySHA256: sum(raw), Mode: modeRefusal,
		Setup: []string{}, Steps: []InstallStep{}, Plans: []PlanRecord{},
		Guards:   InstallGuards{SentinelInvocations: []string{}, ProfileChanges: []string{}},
		Failures: []string{},
	}
	for _, d := range []string{h.home, h.state, h.defs, h.sents} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := installGateSentinels(h.sents, h.log); err != nil {
		return nil, err
	}
	if err := writeGateDefinitions(h.defs); err != nil {
		return nil, err
	}
	h.env = h.childEnv()
	profiles, err := h.profileSnapshot()
	if err != nil {
		return nil, err
	}
	h.profiles = profiles
	return h, nil
}

// childEnv is the environment every core invocation gets: the inherited
// environment without HOME, USERPROFILE or any MANAGED_MACHINE_* setting,
// plus the harness home, the harness state directory, the harness
// definition source and a PATH that starts at the sentinels.
func (h *installHarness) childEnv() []string {
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case k == "HOME", k == "USERPROFILE", k == "PATH":
			continue
		case strings.HasPrefix(k, "MANAGED_MACHINE_"):
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+h.home)
	if runtime.GOOS == "windows" {
		env = append(env, "USERPROFILE="+h.home)
	}
	env = append(env,
		"MANAGED_MACHINE_STATE_DIR="+h.state,
		"MANAGED_MACHINE_SOURCE_DIR="+h.defs,
		// Homebrew's cache and its logs live outside the guarded home, so a
		// download never shows up as a profile change.
		"HOMEBREW_CACHE="+filepath.Join(h.root, "brew-cache"),
		// Tap trust lives in Homebrew's user config home, which defaults to
		// ~/.homebrew under the harness home on macOS; both the harness and
		// the core use this one so the fixture formulae stay trusted.
		"HOMEBREW_USER_CONFIG_HOME="+filepath.Join(h.root, "brew-config"),
		"PATH="+h.sents+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	return env
}

// core runs the binary under test in the harness and returns its exit code
// and streams.
func (h *installHarness) core(args ...string) (int, []byte, []byte) {
	return h.coreEnv(nil, args...)
}

// coreEnv runs the binary under test with extra environment settings. The
// settings replace the ones the harness already sets.
func (h *installHarness) coreEnv(extra []string, args ...string) (int, []byte, []byte) {
	env := h.env
	if len(extra) > 0 {
		env = append(append([]string{}, h.env...), extra...)
	}
	c := exec.Command(h.bin, args...)
	c.Env, c.Dir = env, h.home
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	return code, o.Bytes(), e.Bytes()
}

// brew runs one Homebrew command of the harness itself. These calls are the
// setup and the cleanup of the gate; they use the runner's real home and
// never the harness home, so the machine's own configuration is untouched.
func (h *installHarness) brew(args ...string) (int, []byte, []byte) {
	h.ev.Setup = append(h.ev.Setup, "brew "+strings.Join(args, " "))
	env := []string{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "HOMEBREW_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env,
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_CACHE="+filepath.Join(h.root, "brew-cache"),
		// Tap trust lives in Homebrew's user config home, which defaults to
		// ~/.homebrew under the harness home on macOS; both the harness and
		// the core use this one so the fixture formulae stay trusted.
		"HOMEBREW_USER_CONFIG_HOME="+filepath.Join(h.root, "brew-config"),
	)
	c := exec.Command(filepath.Join(h.prefix, "bin", "brew"), args...)
	c.Env, c.Dir = env, h.root
	var o, e bytes.Buffer
	c.Stdout, c.Stderr = &o, &e
	err := c.Run()
	code := 0
	if x, ok := err.(*exec.ExitError); ok {
		code = x.ExitCode()
	} else if err != nil {
		code = -1
		e.WriteString(err.Error())
	}
	return code, o.Bytes(), e.Bytes()
}

// step records one checked step of the gate.
func (h *installHarness) step(name, detail string, ok bool) {
	h.ev.Steps = append(h.ev.Steps, InstallStep{Name: name, OK: ok, Detail: h.paths.clean(detail)})
	if !ok {
		h.fail(name + ": " + detail)
	}
}

// fail records one failure of the gate.
func (h *installHarness) fail(detail string) {
	h.ev.Failures = append(h.ev.Failures, h.paths.clean(detail))
}

// finish sets the result of the gate.
func (h *installHarness) finish() {
	h.ev.Result = "pass"
	if len(h.ev.Failures) > 0 {
		h.ev.Result = "fail"
	}
}

// profileSnapshot hashes the files a machine's shell profiles and its
// managed-machine configuration directory hold, plus every file in the
// harness home. An absent file stays absent.
func (h *installHarness) profileSnapshot() (map[string]string, error) {
	out := map[string]string{}
	if err := hashTree(h.home, "home", out); err != nil {
		return nil, err
	}
	for _, n := range []string{".zshrc", ".zprofile", ".bashrc", ".bash_profile", ".profile"} {
		b, err := os.ReadFile(filepath.Join(h.realHome, n))
		if err != nil {
			out["profile/"+n] = "absent"
			continue
		}
		out["profile/"+n] = sum(b)
	}
	dir := filepath.Join(h.realHome, ".config", "managed-machine")
	if _, err := os.Stat(dir); err != nil {
		out["config/managed-machine"] = "absent"
	} else {
		out["config/managed-machine"] = "present"
		if err := hashTree(dir, "config/managed-machine", out); err != nil {
			return nil, err
		}
	}
	// The platform's own state directory is guarded as well: it is where the
	// core writes when the state directory is not overridden.
	state, err := statedir.Path(runtime.GOOS, func(k string) string {
		if k == "MANAGED_MACHINE_STATE_DIR" {
			return ""
		}
		return os.Getenv(k)
	})
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(state); err != nil {
		out["state/managed-machine"] = "absent"
		return out, nil
	}
	out["state/managed-machine"] = "present"
	if err := hashTree(state, "state/managed-machine", out); err != nil {
		return nil, err
	}
	return out, nil
}

// hashTree records the digest of every regular file under root, named by
// its path relative to root and the given prefix.
func hashTree(root, prefix string, out map[string]string) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		out[prefix+"/"+filepath.ToSlash(rel)] = sum(b)
		return nil
	})
}

// profileDiff lists the paths that were created, removed or rewritten
// between two profile snapshots.
func profileDiff(before, after map[string]string) []string {
	var d []string
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			d = append(d, "removed "+p)
		case a != b:
			d = append(d, "changed "+p)
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			d = append(d, "created "+p)
		}
	}
	sort.Strings(d)
	return d
}

// guards reads the sentinel log and compares the profiles with the
// snapshot taken before the first core invocation.
func (h *installHarness) guards() {
	h.ev.Guards.SentinelInvocations = []string{}
	if b, err := os.ReadFile(h.log); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			h.ev.Guards.SentinelInvocations = append(h.ev.Guards.SentinelInvocations, h.paths.clean(line))
		}
	}
	after, err := h.profileSnapshot()
	if err != nil {
		h.ev.Guards.ProfileChanges = []string{"the profiles could not be read: " + err.Error()}
		h.step("guards", "the profiles could not be read", false)
		return
	}
	changes := profileDiff(h.profiles, after)
	if changes == nil {
		changes = []string{}
	}
	h.ev.Guards.ProfileChanges = changes
	problem := ""
	switch {
	case len(h.ev.Guards.SentinelInvocations) > 0:
		problem = "a guarded program was started: " + strings.Join(h.ev.Guards.SentinelInvocations, "; ")
	case len(changes) > 0:
		problem = "the profiles changed: " + strings.Join(changes, "; ")
	}
	detail := "no guarded program was started and the profiles are byte-identical"
	if problem != "" {
		detail = problem
	}
	h.step("guards", detail, problem == "")
}

// installSentinels are the provider, package-manager and authorization
// programs an install must never start by name. Each one records the
// invocation and fails, so a lookup by name is caught.
var gateSentinels = []string{"sudo", "osascript", "security", "npm", "uv", "winget", "pip", "pipx"}

// installGateSentinels writes one sentinel per guarded program into dir.
// Each sentinel appends its name and arguments to log and exits 97.
func installGateSentinels(dir, log string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range gateSentinels {
		var body string
		if runtime.GOOS == "windows" {
			body = "@echo off\r\necho " + name + " %* >> \"" + log + "\"\r\nexit /b 97\r\n"
			if err := os.WriteFile(filepath.Join(dir, name+".cmd"), []byte(body), 0o755); err != nil {
				return err
			}
			continue
		}
		body = "#!/bin/sh\nprintf '%s %s\\n' \"" + name + "\" \"$*\" >> \"" + log + "\"\nexit 97\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			return err
		}
	}
	return nil
}

// objectKeys returns the keys of one JSON object in the order they appear.
func objectKeys(b []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("not a JSON object")
	}
	var keys []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		k, ok := tok.(string)
		if !ok {
			return nil, errors.New("not a JSON object key")
		}
		keys = append(keys, k)
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// firstLine returns the first non-empty line of the streams, in order.
func firstLineOf(bs ...[]byte) string {
	for _, b := range bs {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if s := strings.TrimSpace(line); s != "" {
				return s
			}
		}
	}
	return ""
}

// targetOS and targetArch split a canonical target.
func targetOS(target string) string {
	os, _, _ := strings.Cut(target, "/")
	return os
}

func targetArch(target string) string {
	_, arch, _ := strings.Cut(target, "/")
	return arch
}

// installAggregateCmd checks that five install-gate records belong
// together: one per first-release target, the same source revision, every
// gate passed, and macOS in the mode that runs real installs.
func installAggregateCmd(args []string) error {
	fl := flag.NewFlagSet("install-aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("install-aggregate needs -source-revision")
	}
	files := fl.Args()
	var problems []string
	byTarget := map[string]InstallEvidence{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var ev InstallEvidence
		if err := json.Unmarshal(b, &ev); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		if ev.Kind != installEvidenceKind {
			problems = append(problems, fmt.Sprintf("%s is a %q record, not %q", ev.Target, ev.Kind, installEvidenceKind))
		}
		if _, dup := byTarget[ev.Target]; dup {
			problems = append(problems, "two records for "+ev.Target)
		}
		byTarget[ev.Target] = ev
		if ev.SourceRevision != *rev {
			problems = append(problems, fmt.Sprintf("%s ran %q, not %s", ev.Target, ev.SourceRevision, *rev))
		}
		if ev.Result != "pass" {
			problems = append(problems, ev.Target+" did not pass")
		}
	}
	want := []string{"darwin/arm64", "linux/arm64", "linux/amd64", "windows/arm64", "windows/amd64"}
	for _, t := range want {
		if _, ok := byTarget[t]; !ok {
			problems = append(problems, "no evidence for "+t)
		}
	}
	if len(files) != len(want) {
		problems = append(problems, fmt.Sprintf("%d records, want %d", len(files), len(want)))
	}
	if ev, ok := byTarget["darwin/arm64"]; ok && ev.Mode != modeBrew {
		problems = append(problems, "darwin/arm64 ran in "+ev.Mode+" mode; macOS must run real installs")
	}
	sort.Strings(problems)
	fmt.Printf("| target | mode | steps | plans | result |\n|---|---|---|---|---|\n")
	for _, t := range want {
		if ev, ok := byTarget[t]; ok {
			fmt.Printf("| %s | %s | %d | %d | %s |\n", t, ev.Mode, len(ev.Steps), len(ev.Plans), ev.Result)
		}
	}
	fmt.Printf("\nsource revision `%s`\n", *rev)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("FAIL:", p)
		}
		return errors.New("install-aggregate failed")
	}
	return nil
}
