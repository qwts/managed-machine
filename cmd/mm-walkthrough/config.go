package main

// The S4 configuration gate (issue #301, decision D9). It drives a built
// managed-machine binary through the D9 case list in an isolated harness
// (fresh HOME, MANAGED_MACHINE_STATE_DIR, MANAGED_MACHINE_SOURCE_DIR,
// MANAGED_MACHINE_DESIRED_FILE, and a custom root path containing spaces and
// non-ASCII characters). Unlike the S2 and S3 gates there is no refusal
// mode: configuration touches only files inside the harness, so all five
// hosted runners execute the full case list natively.

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/config/secret"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/plan"
)

// configEvidenceKind names the evidence record of the S4 configuration gate.
const configEvidenceKind = "s4-config-gate"

// configMode is the mode the S4 configuration gate runs in.
const configMode = "config"

// Canonical D9 case names recorded in the cases matrix of every target.
const (
	caseACT04User       = "ac-t-04-user"
	caseACT04Custom     = "ac-t-04-custom"
	caseCheckExit0      = "check-exit-0"
	caseCheckExit3      = "check-exit-3"
	caseCheckExit4      = "check-exit-4"
	caseConflictReplace = "conflict-replace-file"
	caseConflictBlock   = "conflict-foreign-block"
	caseConflictKey     = "conflict-foreign-key"
	caseConflictFlavor  = "conflict-flavor-cross-def"
	caseSecretMatrix    = "secret-matrix"
	caseStatusSections  = "status-sections"
	caseGuards          = "guards"
)

// configCaseNames are the D9 cases every target must prove.
var configCaseNames = []string{
	caseACT04User,
	caseACT04Custom,
	caseCheckExit0,
	caseCheckExit3,
	caseCheckExit4,
	caseConflictReplace,
	caseConflictBlock,
	caseConflictKey,
	caseConflictFlavor,
	caseSecretMatrix,
	caseStatusSections,
	caseGuards,
}

// Synthetic secret names and values for the D5 secret matrix. Both fixture
// values and all pass side-channel strings are grepped across the state
// directory, custom root, stdout/stderr captures and encoded evidence
// requiring zero hits.
const (
	secretEnvName        = "work/env-only"
	secretPassName       = "work/pass-only"
	secretUnresolvedName = "work/unresolved"
	secretEnvValue       = "mm-gate-secret-env-value-979323"
	secretPassValue      = "mm-gate-secret-pass-value-314159"
	secretPassSecondLine = "mm-gate-secret-pass-second-line-265358"
	secretPassShadow     = "mm-gate-secret-pass-shadow-846264" // gitleaks:allow (fixture sentinel, not a credential)
	secretPassStderr     = "mm-gate-secret-pass-stderr-338327" // gitleaks:allow (fixture sentinel, not a credential)
)

var secretSentinels = []string{
	secretEnvValue,
	secretPassValue,
	secretPassSecondLine,
	secretPassShadow,
	secretPassStderr,
}

// configSentinels are the elevation, keychain, GPG and package-manager
// programs the configuration gate must never start. Each is placed in the
// harness sentinel directory first on PATH and exits 97 if invoked.
var configSentinels = []string{
	"sudo", "osascript", "security", "gpg", "gpg2",
	"brew", "winget", "npm", "npx", "uv", "uvx", "cargo", "go", "pip", "pip3", "pipx",
}

// ConfigEvidence is the per-target record of the S4 configuration gate: the
// sanitized InstallEvidence base with the D9 cases matrix beside it. The
// field order is the serialized key order.
type ConfigEvidence struct {
	InstallEvidence
	Cases []ConfigCase `json:"cases"`
}

// ConfigCase is one proven case of the D9 configuration gate matrix.
type ConfigCase struct {
	Case     string `json:"case"`
	Expected string `json:"expected"`
	Observed string `json:"observed"`
	OK       bool   `json:"ok"`
	Detail   string `json:"detail"`
}

// configHarness is the isolated environment of the S4 configuration gate.
type configHarness struct {
	*installHarness
	cases         []ConfigCase
	machine       map[string]string
	desired       string
	flavorDesired string
	unresDesired  string
	customRoot    string
	customDesired string
	passLog       string
	captures      [][]byte
	customChanges []string

	userDests       configDestinations
	customDests     configDestinations
	unreadableDest  string
	replaceFileDest string
	foreignBlkDest  string
	foreignKeyDest  string
	flavorDest      string
	secretRCDest    string
	secretJSONDest  string
}

type configDestinations struct {
	file       string
	template   string
	structured string
	block      string
}

// newConfigHarness builds the isolated harness for the S4 configuration gate.
func newConfigHarness(bin, target, revision string, raw []byte) (*configHarness, error) {
	h, err := newInstallHarnessWith(bin, target, revision, raw, installHarnessOptions{
		sentinels:   configSentinels,
		definitions: writeConfigDefinitions,
		extraDirs:   []string{"desired"},
	})
	if err != nil {
		return nil, err
	}
	h.ev.Kind = configEvidenceKind
	h.ev.Mode = configMode

	passLog := filepath.Join(h.sents, "pass.log")
	if err := installConfigPass(h.sents, passLog); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	desiredPath := filepath.Join(h.root, "desired", "user-desired.yaml")
	flavorDesiredPath := filepath.Join(h.root, "desired", "flavor-desired.yaml")
	unresDesiredPath := filepath.Join(h.root, "desired", "unresolved-desired.yaml")
	customRoot := filepath.Join(h.root, "custom root ü 日本", "soul")
	customDesiredPath := filepath.Join(customRoot, ".managed-machine", "desired.yaml")

	h.env = append(h.env, "MANAGED_MACHINE_DESIRED_FILE="+desiredPath)

	machine, err := h.machineSnapshot()
	if err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}

	ch := &configHarness{
		installHarness: h,
		cases:          []ConfigCase{},
		machine:        machine,
		desired:        desiredPath,
		flavorDesired:  flavorDesiredPath,
		unresDesired:   unresDesiredPath,
		customRoot:     customRoot,
		customDesired:  customDesiredPath,
		passLog:        passLog,
		userDests: configDestinations{
			file:       filepath.Join(h.home, ".config", "mm-gate", "config.txt"),
			template:   filepath.Join(h.home, ".config", "mm-gate", "profile.conf"),
			structured: filepath.Join(h.home, ".config", "mm-gate", "settings.json"),
			block:      filepath.Join(h.home, ".mm-gate-rc"),
		},
		customDests: configDestinations{
			file:       filepath.Join(customRoot, "etc", "mm-gate", "config.txt"),
			template:   filepath.Join(customRoot, "etc", "mm-gate", "profile.conf"),
			structured: filepath.Join(customRoot, "etc", "mm-gate", "settings.json"),
			block:      filepath.Join(customRoot, "etc", "mm-gate", "rc"),
		},
		unreadableDest:  filepath.Join(h.home, ".config", "mm-gate", "unreadable.txt"),
		replaceFileDest: filepath.Join(h.home, ".config", "mm-gate", "replace-file.txt"),
		foreignBlkDest:  filepath.Join(h.home, ".config", "mm-gate", "foreign-block.rc"),
		foreignKeyDest:  filepath.Join(h.home, ".config", "mm-gate", "foreign-key.json"),
		flavorDest:      filepath.Join(h.home, ".config", "mm-gate", "flavor-shared.conf"),
		secretRCDest:    filepath.Join(h.home, ".config", "mm-gate", "secrets.rc"),
		secretJSONDest:  filepath.Join(h.home, ".config", "mm-gate", "secrets.json"),
	}
	if err := ch.writeDesiredFiles(); err != nil {
		os.RemoveAll(h.root)
		return nil, err
	}
	return ch, nil
}

// writeConfigDefinitions populates MANAGED_MACHINE_SOURCE_DIR with the
// synthetic configuration definitions, flavor definitions and source assets
// the gate exercises.
func writeConfigDefinitions(dir string) error {
	assets := map[string]string{
		"files/config.txt":       "managed-machine config gate file\n",
		"files/unreadable.txt":   "unreadable check fixture\n",
		"templates/profile.tmpl": "handle = {{ inputs.handle }}\nfile = {{ destinations.cfg-file }}\nbanner = {{ settings.banner }}\n",
		"templates/flavor.tmpl":  "editor = {{ inputs.editor }}\n",
		"templates/secrets.tmpl": "env_secret = {{ inputs.env-token }}\npass_secret = {{ inputs.pass-token }}\n",
	}
	for rel, content := range assets {
		if err := write(filepath.Join(dir, filepath.FromSlash(rel)), content); err != nil {
			return err
		}
	}

	defs := map[string]string{
		"mm-gate-config.yaml": `kind: config
schema_version: 1
id: mm-gate-config
description: Configuration gate multi-provider fixture.
categories: [walkthrough]
targets: [user, custom]
inputs:
  handle: gate-default
operations:
  - id: op-file
    provider: file
    destination: cfg-file
    source: files/config.txt
  - id: op-template
    provider: template
    destination: cfg-template
    source: templates/profile.tmpl
    settings:
      banner: "managed for {{ inputs.handle }}"
  - id: op-structured
    provider: structured
    destination: cfg-json
    format: json
    keys:
      editor.tabSize: 2
      user.handle: "{{ inputs.handle }}"
  - id: op-block
    provider: managed-block
    destination: cfg-rc
    block_id: mm-gate-block
    content: "export MM_GATE_HANDLE={{ inputs.handle }}"
activation:
  profile: "{{ destinations.cfg-template }}"
`,
		"mm-gate-unreadable.yaml": `kind: config
schema_version: 1
id: mm-gate-unreadable
description: Configuration gate unreadable destination fixture.
categories: [walkthrough]
targets: [user]
operations:
  - id: main
    provider: file
    destination: dest
    source: files/unreadable.txt
`,
		"mm-gate-conflict-file.yaml": `kind: config
schema_version: 1
id: mm-gate-conflict-file
description: Configuration gate replace-file conflict fixture.
categories: [walkthrough]
targets: [user]
operations:
  - id: main
    provider: file
    destination: dest
    source: files/config.txt
`,
		"mm-gate-conflict-block.yaml": `kind: config
schema_version: 1
id: mm-gate-conflict-block
description: Configuration gate foreign-block conflict fixture.
categories: [walkthrough]
targets: [user]
operations:
  - id: main
    provider: managed-block
    destination: dest
    block_id: foreign-block-id
    content: "export MM_FOREIGN=1"
`,
		"mm-gate-conflict-key.yaml": `kind: config
schema_version: 1
id: mm-gate-conflict-key
description: Configuration gate foreign-key conflict fixture.
categories: [walkthrough]
targets: [user]
operations:
  - id: main
    provider: structured
    destination: dest
    format: json
    keys:
      editor.tabSize: 2
`,
		"mm-gate-flavor-a.yaml": `kind: config
schema_version: 1
id: mm-gate-flavor-a
description: First flavored configuration fixture.
categories: [walkthrough]
targets: [user]
inputs:
  editor: vim
operations:
  - id: main
    provider: template
    destination: dest
    source: templates/flavor.tmpl
`,
		"mm-gate-flavor-b.yaml": `kind: config
schema_version: 1
id: mm-gate-flavor-b
description: Second flavored configuration fixture.
categories: [walkthrough]
targets: [user]
inputs:
  editor: emacs
operations:
  - id: main
    provider: template
    destination: dest
    source: templates/flavor.tmpl
`,
		"mm-gate-secrets.yaml": `kind: config
schema_version: 1
id: mm-gate-secrets
description: Configuration gate secret resolution fixture.
categories: [walkthrough]
targets: [user]
inputs:
  env-token: ""
  pass-token: ""
operations:
  - id: op-rc
    provider: template
    destination: rc
    source: templates/secrets.tmpl
  - id: op-json
    provider: structured
    destination: settings
    format: json
    keys:
      auth.env: "{{ inputs.env-token }}"
      auth.pass: "{{ inputs.pass-token }}"
`,
		"mm-gate-flavor-observed.yaml": `kind: flavor
schema_version: 1
id: mm-gate-flavor-observed
description: Configuration gate flavor with present observed config.
categories: [walkthrough]
config:
  - id: mm-gate-config
    target: user
`,
		"mm-gate-flavor-alpha.yaml": `kind: flavor
schema_version: 1
id: mm-gate-flavor-alpha
description: First flavor fixture for cross-definition conflict and status.
categories: [walkthrough]
software:
  - id: ripgrep
    target: user
    version: 14.1.0
config:
  - id: mm-gate-flavor-a
    target: user
`,
		"mm-gate-flavor-beta.yaml": `kind: flavor
schema_version: 1
id: mm-gate-flavor-beta
description: Second flavor fixture for cross-definition conflict.
categories: [walkthrough]
config:
  - id: mm-gate-flavor-b
    target: user
`,
	}
	for name, content := range defs {
		if err := write(filepath.Join(dir, name), content); err != nil {
			return err
		}
	}
	return nil
}

// installConfigPass writes the fixture pass script (pass.cmd on Windows)
// into the sentinel directory, which is first on the harness PATH, so the
// real pass program is never reached.
func installConfigPass(dir, logFile string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries := map[string]string{
		secretPassName: secretPassValue + "\n" + secretPassSecondLine,
		secretEnvName:  secretPassShadow + "\n" + secretPassSecondLine,
	}
	keys := []string{secretEnvName, secretPassName}
	sort.Strings(keys)

	if runtime.GOOS == "windows" {
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		fmt.Fprintf(&b, "echo %%~1 %%~2>>%q\r\n", logFile)
		fmt.Fprintf(&b, ">&2 echo %s\r\n", secretPassStderr)
		b.WriteString("if not \"%~1\"==\"show\" exit /b 1\r\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "if \"%%~2\"==%q (\r\n", k)
			for _, line := range strings.Split(entries[k], "\n") {
				line = strings.TrimSuffix(line, "\r")
				fmt.Fprintf(&b, "  echo %s\r\n", line)
			}
			b.WriteString("  exit /b 0\r\n)\r\n")
		}
		b.WriteString("exit /b 1\r\n")
		return os.WriteFile(filepath.Join(dir, "pass.cmd"), []byte(b.String()), 0o755)
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "printf '%%s %%s\\n' \"$1\" \"$2\" >> %q\n", logFile)
	fmt.Fprintf(&b, "printf '%%s\\n' %q >&2\n", secretPassStderr)
	b.WriteString("if [ \"$1\" != \"show\" ]; then exit 1; fi\n")
	b.WriteString("case \"$2\" in\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "  %q)\n", k)
		for _, line := range strings.Split(entries[k], "\n") {
			fmt.Fprintf(&b, "    printf '%%s\\n' %q\n", line)
		}
		b.WriteString("    exit 0\n    ;;\n")
	}
	b.WriteString("esac\nexit 1\n")
	return os.WriteFile(filepath.Join(dir, "pass"), []byte(b.String()), 0o755)
}

func (h *configHarness) writeDesiredFiles() error {
	userYAML := fmt.Sprintf(`target: user
config:
  mm-gate-config:
    inputs:
      handle: gate-user
    destinations:
      cfg-file: %s
      cfg-template: %s
      cfg-json: %s
      cfg-rc: %s
  mm-gate-unreadable:
    destinations:
      dest: %s
  mm-gate-conflict-file:
    destinations:
      dest: %s
  mm-gate-conflict-block:
    destinations:
      dest: %s
  mm-gate-conflict-key:
    destinations:
      dest: %s
  mm-gate-flavor-a:
    inputs:
      editor: vim
    destinations:
      dest: %s
  mm-gate-flavor-b:
    inputs:
      editor: emacs
    destinations:
      dest: %s
  mm-gate-secrets:
    inputs:
      env-token: secret:%s
      pass-token: secret:%s
    destinations:
      rc: %s
      settings: %s
`,
		filepath.ToSlash(h.userDests.file),
		filepath.ToSlash(h.userDests.template),
		filepath.ToSlash(h.userDests.structured),
		filepath.ToSlash(h.userDests.block),
		filepath.ToSlash(h.unreadableDest),
		filepath.ToSlash(h.replaceFileDest),
		filepath.ToSlash(h.foreignBlkDest),
		filepath.ToSlash(h.foreignKeyDest),
		filepath.ToSlash(h.flavorDest),
		filepath.ToSlash(h.flavorDest),
		secretEnvName,
		secretPassName,
		filepath.ToSlash(h.secretRCDest),
		filepath.ToSlash(h.secretJSONDest),
	)
	if err := write(h.desired, userYAML); err != nil {
		return err
	}

	flavorYAML := fmt.Sprintf(`target: user
flavors: [mm-gate-flavor-alpha, mm-gate-flavor-beta]
config:
  mm-gate-flavor-a:
    inputs:
      editor: vim
    destinations:
      dest: %s
  mm-gate-flavor-b:
    inputs:
      editor: emacs
    destinations:
      dest: %s
`,
		filepath.ToSlash(h.flavorDest),
		filepath.ToSlash(h.flavorDest),
	)
	if err := write(h.flavorDesired, flavorYAML); err != nil {
		return err
	}

	unresYAML := fmt.Sprintf(`target: user
config:
  mm-gate-secrets:
    inputs:
      env-token: secret:%s
      pass-token: secret:%s
    destinations:
      rc: %s
      settings: %s
`,
		secretUnresolvedName,
		secretPassName,
		filepath.ToSlash(h.secretRCDest),
		filepath.ToSlash(h.secretJSONDest),
	)
	if err := write(h.unresDesired, unresYAML); err != nil {
		return err
	}

	customYAML := fmt.Sprintf(`target: custom
root: %s
config:
  mm-gate-config:
    inputs:
      handle: gate-custom
    destinations:
      cfg-file: %s
      cfg-template: %s
      cfg-json: %s
      cfg-rc: %s
`,
		filepath.ToSlash(h.customRoot),
		filepath.ToSlash(h.customDests.file),
		filepath.ToSlash(h.customDests.template),
		filepath.ToSlash(h.customDests.structured),
		filepath.ToSlash(h.customDests.block),
	)
	return write(h.customDesired, customYAML)
}

// runCore executes the binary under test and records both output streams for
// the zero-hits secret scan.
func (h *configHarness) runCore(args ...string) (int, []byte, []byte) {
	return h.runCoreEnv(nil, args...)
}

// runCoreEnv executes the binary under test with extra environment variables
// and records both output streams for the zero-hits secret scan.
func (h *configHarness) runCoreEnv(extra []string, args ...string) (int, []byte, []byte) {
	code, out, errOut := h.coreEnv(extra, args...)
	h.captures = append(h.captures, out, errOut)
	return code, out, errOut
}

// recordCase appends one D9 case result to the evidence and records a step.
func (h *configHarness) recordCase(name, expected, observed string, ok bool, detail string) {
	cleaned := h.paths.clean(detail)
	h.cases = append(h.cases, ConfigCase{
		Case:     name,
		Expected: expected,
		Observed: observed,
		OK:       ok,
		Detail:   cleaned,
	})
	h.step(name, detail, ok)
}

// configCmd runs the S4 configuration gate for one target and writes its
// sanitized evidence record.
func configCmd(args []string) error {
	fl := flag.NewFlagSet("config", flag.ContinueOnError)
	bin := fl.String("binary", "", "managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *bin == "" || *tgt == "" || *out == "" {
		return errors.New("config needs -binary, -target and -evidence")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	h, b, err := runConfigGate(absBin, *tgt, *rev)
	if err != nil {
		return err
	}
	defer os.RemoveAll(h.root)
	if err := os.WriteFile(*out, b, 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d steps, %d cases, result %s\n", *tgt, len(h.ev.Steps), len(h.cases), h.ev.Result)
	if h.ev.Result != "pass" {
		for _, f := range h.ev.Failures {
			fmt.Fprintln(os.Stderr, "FAIL:", f)
		}
		return errors.New("the config gate failed")
	}
	return nil
}

// runConfigGate builds the isolated harness for one target, runs every D9
// case in it, and encodes the sanitized evidence JSON. The caller owns
// h.root and removes it.
func runConfigGate(bin, target, revision string) (*configHarness, []byte, error) {
	raw, err := os.ReadFile(bin)
	if err != nil {
		return nil, nil, err
	}
	h, err := newConfigHarness(bin, target, revision, raw)
	if err != nil {
		return nil, nil, err
	}
	h.run()
	ev := ConfigEvidence{InstallEvidence: *h.ev, Cases: h.cases}
	b, err := json.MarshalIndent(ev, "", "  ")
	if err != nil {
		return h, nil, err
	}
	b = append(h.paths.scrub(b), '\n')
	if leak := grepBytes(b, "evidence", secretSentinels); leak != "" {
		h.fail(leak)
		h.finish()
		ev = ConfigEvidence{InstallEvidence: *h.ev, Cases: h.cases}
		b, err = json.MarshalIndent(ev, "", "  ")
		if err != nil {
			return h, nil, err
		}
		b = append(h.paths.scrub(b), '\n')
	}
	return h, b, nil
}

// run executes the platform probe and all twelve D9 cases in order.
func (h *configHarness) run() {
	if !h.probe() {
		h.finish()
		return
	}
	h.runACT04User()
	h.runACT04Custom()
	h.runCheckExit0()
	h.runCheckExit3()
	h.runCheckExit4()
	h.runConflictReplaceFile()
	h.runConflictForeignBlock()
	h.runConflictForeignKey()
	h.runConflictFlavorCrossDef()
	h.runSecretMatrix()
	h.runStatusSections()
	h.runGuards()
	h.finish()
}

// runACT04User proves the AC-T-04 lifecycle for the user target.
func (h *configHarness) runACT04User() {
	ledgerPath := filepath.Join(h.state, "ownership.jsonl")
	detail, ok := h.acT04Cycle(
		[]string{"--user"},
		"user",
		nil,
		"gate-user",
		h.userDests,
		ledgerPath,
	)
	obs := "applied"
	if !ok {
		obs = observedFailed
	}
	h.recordCase(caseACT04User, "applied", obs, ok, detail)
}

// runACT04Custom proves the AC-T-04 lifecycle for the custom target in a
// custom root with spaces and non-ASCII, and proves nothing outside the
// custom root and state directory changed.
func (h *configHarness) runACT04Custom() {
	before, err := h.containmentSnapshot(h.customRoot)
	if err != nil {
		h.recordCase(caseACT04Custom, "applied", observedFailed, false, err.Error())
		return
	}
	ledgerPath := filepath.Join(h.customRoot, ".managed-machine", "ownership.jsonl")
	extraEnv := []string{"MANAGED_MACHINE_DESIRED_FILE="}
	detail, ok := h.acT04Cycle(
		[]string{"--custom", h.customRoot},
		"custom:"+h.customRoot,
		extraEnv,
		"gate-custom",
		h.customDests,
		ledgerPath,
	)
	if !ok {
		h.recordCase(caseACT04Custom, "applied", observedFailed, false, detail)
		return
	}
	after, err := h.containmentSnapshot(h.customRoot)
	if err != nil {
		h.recordCase(caseACT04Custom, "applied", observedFailed, false, err.Error())
		return
	}
	h.customChanges = profileDiff(before, after)
	if len(h.customChanges) > 0 {
		problem := "the custom-target cycle changed " + strings.Join(h.customChanges, "; ")
		h.recordCase(caseACT04Custom, "applied", observedFailed, false, problem)
		return
	}
	detail += "; nothing outside the custom root and the state directory changed"
	h.recordCase(caseACT04Custom, "applied", "applied", true, detail)
}

// acT04Cycle runs: plan -> apply -> status -> fresh zero-op plan -> apply ->
// apply again (MM_CONFIG_NO_PLAN) for mm-gate-config on the given target.
func (h *configHarness) acT04Cycle(
	targetArgs []string,
	wantTarget string,
	extraEnv []string,
	wantHandle string,
	dests configDestinations,
	ledgerPath string,
) (string, bool) {
	// 1. --plan creates a saved plan with 4 operations and touches no destination.
	planArgs := append([]string{"config", "mm-gate-config", "--plan"}, targetArgs...)
	code, out, errOut := h.runCoreEnv(extraEnv, planArgs...)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("plan exited %d: %s", code, firstLineOf(errOut, out)), false
	}
	recs, err := parseJSONL(out)
	if err != nil {
		return "plan stdout: " + err.Error(), false
	}
	if len(recs) != 5 {
		return fmt.Sprintf("plan emitted %d records, want 5", len(recs)), false
	}
	plan1 := str(recs[0], "plan")
	if !planIDPattern.MatchString(plan1) {
		return "plan record names no valid plan id: " + plan1, false
	}
	if str(recs[0], "id") != "mm-gate-config" || str(recs[0], "target") != wantTarget {
		return fmt.Sprintf("plan header record = %v", recs[0]), false
	}
	for _, p := range []string{dests.file, dests.template, dests.structured, dests.block} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			return "plan created destination before apply: " + p, false
		}
	}
	doc1, err := h.configPlanDoc(plan1)
	if err != nil {
		return err.Error(), false
	}
	if doc1.State != "saved" || doc1.Command != "config" || doc1.Config != "mm-gate-config" || len(doc1.Operations) != 4 {
		return fmt.Sprintf("saved plan %s has state=%s command=%s operations=%d", plan1, doc1.State, doc1.Command, len(doc1.Operations)), false
	}

	// 2. --apply executes the saved plan, writes all four destinations, records
	// ownership in the ledger, and emits 4 apply records + 1 activation record.
	applyArgs := append([]string{"config", "mm-gate-config", "--apply"}, targetArgs...)
	code, out, errOut = h.runCoreEnv(extraEnv, applyArgs...)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("apply exited %d: %s", code, firstLineOf(errOut, out)), false
	}
	applyRecs, err := parseJSONL(out)
	if err != nil {
		return "apply stdout: " + err.Error(), false
	}
	if len(applyRecs) != 5 {
		return fmt.Sprintf("apply emitted %d records, want 5", len(applyRecs)), false
	}
	for i := 0; i < 4; i++ {
		if str(applyRecs[i], "state") != "applied" || str(applyRecs[i], "target") != wantTarget {
			return fmt.Sprintf("apply record %d = %v", i, applyRecs[i]), false
		}
	}
	// Desired files are written with filepath.ToSlash paths and render those paths as written.
	wantActivationVal := filepath.ToSlash(dests.template)
	if str(applyRecs[4], "activation") != "profile" || str(applyRecs[4], "value") != wantActivationVal {
		return fmt.Sprintf("activation record = %v, want value %q", applyRecs[4], wantActivationVal), false
	}
	doc1, err = h.configPlanDoc(plan1)
	if err != nil {
		return err.Error(), false
	}
	if doc1.State != "applied" {
		return fmt.Sprintf("plan %s is %s after apply, want applied", plan1, doc1.State), false
	}
	jRecs, err := h.journal(plan1)
	if err != nil {
		return err.Error(), false
	}
	if journalReceipt(jRecs) != "applied" || len(jRecs) != 9 {
		return fmt.Sprintf("plan %s journal has %d records and receipt %q, want 9 and applied", plan1, len(jRecs), journalReceipt(jRecs)), false
	}
	h.recordPlan(plan1)

	if problem := verifyAppliedDestinations(dests, wantHandle); problem != "" {
		return problem, false
	}
	if _, err := os.Stat(ledgerPath); err != nil {
		return "ownership ledger missing at " + ledgerPath + ": " + err.Error(), false
	}

	// 3. --status reports applied for all four operations.
	statusArgs := append([]string{"config", "mm-gate-config", "--status"}, targetArgs...)
	code, out, errOut = h.runCoreEnv(extraEnv, statusArgs...)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("config --status exited %d: %s", code, firstLineOf(errOut, out)), false
	}
	stRecs, err := parseJSONL(out)
	if err != nil {
		return "config --status stdout: " + err.Error(), false
	}
	if len(stRecs) != 4 {
		return fmt.Sprintf("config --status emitted %d records, want 4", len(stRecs)), false
	}
	for _, r := range stRecs {
		if str(r, "state") != "applied" || str(r, "target") != wantTarget {
			return fmt.Sprintf("config --status record = %v", r), false
		}
	}

	// 4. Fresh --plan plans zero operations under a new plan id.
	code, out, errOut = h.runCoreEnv(extraEnv, planArgs...)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("second plan exited %d: %s", code, firstLineOf(errOut, out)), false
	}
	recs2, err := parseJSONL(out)
	if err != nil {
		return "second plan stdout: " + err.Error(), false
	}
	if len(recs2) != 1 {
		return fmt.Sprintf("zero-operation plan emitted %d records, want 1", len(recs2)), false
	}
	plan2 := str(recs2[0], "plan")
	if !planIDPattern.MatchString(plan2) || plan2 == plan1 {
		return fmt.Sprintf("second plan id = %q (first %q)", plan2, plan1), false
	}
	doc2, err := h.configPlanDoc(plan2)
	if err != nil {
		return err.Error(), false
	}
	if len(doc2.Operations) != 0 {
		return fmt.Sprintf("zero-operation plan %s has %d operations", plan2, len(doc2.Operations)), false
	}

	// 5. --apply applies the zero-operation plan and emits only the activation record.
	code, out, errOut = h.runCoreEnv(extraEnv, applyArgs...)
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("second apply exited %d: %s", code, firstLineOf(errOut, out)), false
	}
	applyRecs2, err := parseJSONL(out)
	if err != nil {
		return "second apply stdout: " + err.Error(), false
	}
	if len(applyRecs2) != 1 || str(applyRecs2[0], "activation") != "profile" {
		return fmt.Sprintf("zero-operation apply emitted %v, want 1 activation record", applyRecs2), false
	}
	jRecs2, err := h.journal(plan2)
	if err != nil {
		return err.Error(), false
	}
	if problem := h.journalApplied(jRecs2, ""); problem != "" {
		return problem, false
	}
	h.recordPlan(plan2)

	// 6. --apply again without a saved plan fails with MM_CONFIG_NO_PLAN (exit 2).
	code, out, errOut = h.runCoreEnv(extraEnv, applyArgs...)
	if problem := checkDiagnostic(code, 2, out, errOut, diag.ConfigNoPlan); problem != "" {
		return "apply again without plan: " + problem, false
	}

	return fmt.Sprintf("plan %s applied 4 operations, status applied, zero-op plan %s applied, repeat apply refused with %s",
		plan1, plan2, diag.ConfigNoPlan), true
}

func verifyAppliedDestinations(dests configDestinations, wantHandle string) string {
	gotFile, err := os.ReadFile(dests.file)
	if err != nil {
		return "read file destination: " + err.Error()
	}
	if string(gotFile) != "managed-machine config gate file\n" {
		return fmt.Sprintf("file destination content = %q", string(gotFile))
	}

	gotTmpl, err := os.ReadFile(dests.template)
	if err != nil {
		return "read template destination: " + err.Error()
	}
	wantTmpl := "handle = " + wantHandle + "\nfile = " + filepath.ToSlash(dests.file) + "\nbanner = managed for " + wantHandle + "\n"
	if string(gotTmpl) != wantTmpl {
		return fmt.Sprintf("template destination content = %q, want %q", string(gotTmpl), wantTmpl)
	}

	gotJSON, err := os.ReadFile(dests.structured)
	if err != nil {
		return "read structured destination: " + err.Error()
	}
	if !strings.Contains(string(gotJSON), `"tabSize": 2`) || !strings.Contains(string(gotJSON), `"handle": "`+wantHandle+`"`) {
		return fmt.Sprintf("structured destination content = %q", string(gotJSON))
	}

	gotBlock, err := os.ReadFile(dests.block)
	if err != nil {
		return "read block destination: " + err.Error()
	}
	wantBlock := "# managed-machine:begin mm-gate-block\nexport MM_GATE_HANDLE=" + wantHandle + "\n# managed-machine:end mm-gate-block\n"
	if string(gotBlock) != wantBlock {
		return fmt.Sprintf("block destination content = %q, want %q", string(gotBlock), wantBlock)
	}
	return ""
}

// runCheckExit0 proves that --check returns 0 with empty streams when all
// destinations match on disk.
func (h *configHarness) runCheckExit0() {
	code, out, errOut := h.runCore("config", "mm-gate-config", "--check", "--user")
	if code != 0 || len(out) != 0 || len(errOut) != 0 {
		h.recordCase(caseCheckExit0, "exit 0", fmt.Sprintf("exit %d", code), false,
			fmt.Sprintf("user --check exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	code, out, errOut = h.runCoreEnv(
		[]string{"MANAGED_MACHINE_DESIRED_FILE="},
		"config", "mm-gate-config", "--check", "--custom", h.customRoot,
	)
	if code != 0 || len(out) != 0 || len(errOut) != 0 {
		h.recordCase(caseCheckExit0, "exit 0", fmt.Sprintf("exit %d", code), false,
			fmt.Sprintf("custom --check exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.recordCase(caseCheckExit0, "exit 0", "exit 0", true,
		"--check exited 0 with empty stdout and stderr for both user and custom targets")
}

// runCheckExit3 proves that --check returns 3 (MM_CONFIG_DRIFT_FOUND) after
// an external edit and returns to 0 once restored.
func (h *configHarness) runCheckExit3() {
	orig, err := os.ReadFile(h.userDests.file)
	if err != nil {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, observedFailed, false, err.Error())
		return
	}
	if err := os.WriteFile(h.userDests.file, []byte("external edit\n"), 0o644); err != nil {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, observedFailed, false, err.Error())
		return
	}
	code, out, errOut := h.runCore("config", "mm-gate-config", "--check", "--user")
	_ = os.WriteFile(h.userDests.file, orig, 0o644)

	if code != 3 {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, fmt.Sprintf("exit %d", code), false,
			fmt.Sprintf("--check after external edit exited %d, want 3: %s", code, firstLineOf(errOut, out)))
		return
	}
	if problem := checkDiagnosticLine(errOut, diag.ConfigDriftFound); problem != "" {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, observedFailed, false, problem)
		return
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 1 {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, observedFailed, false,
			fmt.Sprintf("expected 1 drift record on stdout, got %d (%v)", len(recs), err))
		return
	}
	if str(recs[0], "operation") != "op-file" || str(recs[0], "drift") != "cfg-file" {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, observedFailed, false,
			fmt.Sprintf("unexpected drift record: %v", recs[0]))
		return
	}
	// Confirm restoration returns --check to exit 0.
	if c0, o0, e0 := h.runCore("config", "mm-gate-config", "--check", "--user"); c0 != 0 || len(o0) != 0 || len(e0) != 0 {
		h.recordCase(caseCheckExit3, diag.ConfigDriftFound, observedFailed, false,
			fmt.Sprintf("restored destination did not return --check to 0 (exit %d)", c0))
		return
	}
	h.recordCase(caseCheckExit3, diag.ConfigDriftFound, diag.ConfigDriftFound, true,
		"--check exited 3 with MM_CONFIG_DRIFT_FOUND and one drift record after an external edit")
}

// runCheckExit4 applies mm-gate-unreadable so it is owned in the ledger, then
// replaces its destination file with a directory so --check portably observes
// an unreadable destination (exit 4, MM_CONFIG_CHECK_INCOMPLETE) and status
// later observes an unknown config record.
func (h *configHarness) runCheckExit4() {
	code, out, errOut := h.runCore("config", "mm-gate-unreadable", "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, observedFailed, false,
			fmt.Sprintf("plan mm-gate-unreadable exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) == 0 {
		h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, observedFailed, false, "plan mm-gate-unreadable returned no record")
		return
	}
	planID := str(recs[0], "plan")
	code, out, errOut = h.runCore("config", "mm-gate-unreadable", "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, observedFailed, false,
			fmt.Sprintf("apply mm-gate-unreadable exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.recordPlan(planID)

	// Replace the regular file with a directory so reading it as a file fails
	// portably on every platform without relying on chmod.
	if err := os.Remove(h.unreadableDest); err != nil {
		h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, observedFailed, false, err.Error())
		return
	}
	if err := os.MkdirAll(h.unreadableDest, 0o755); err != nil {
		h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, observedFailed, false, err.Error())
		return
	}

	code, out, errOut = h.runCore("config", "mm-gate-unreadable", "--check", "--user")
	if problem := checkDiagnostic(code, 4, out, errOut, diag.ConfigCheckIncomplete); problem != "" {
		h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, observedFailed, false, problem)
		return
	}
	h.recordCase(caseCheckExit4, diag.ConfigCheckIncomplete, diag.ConfigCheckIncomplete, true,
		"--check exited 4 with MM_CONFIG_CHECK_INCOMPLETE against a directory where a file was expected")
}

// runConflictReplaceFile proves that an unowned existing file plans
// `conflict replace-file <path>` and --apply refuses with MM_CONFIG_CONFLICT
// while keeping the destination byte-identical.
func (h *configHarness) runConflictReplaceFile() {
	initial := "unowned pre-existing file content\n"
	wantAction := "conflict replace-file " + filepath.ToSlash(h.replaceFileDest)
	detail, ok := h.proveConflict("mm-gate-conflict-file", h.replaceFileDest, initial, wantAction)
	obs := observedRefused
	if !ok {
		obs = observedFailed
	}
	h.recordCase(caseConflictReplace, observedRefused, obs, ok, detail)
}

// runConflictForeignBlock proves that an existing file with unowned block
// markers plans `conflict foreign-block <path>:<block_id>` and --apply refuses
// with MM_CONFIG_CONFLICT while keeping the destination byte-identical.
func (h *configHarness) runConflictForeignBlock() {
	initial := "# pre-existing rc\n# managed-machine:begin foreign-block-id\nexport MM_FOREIGN=0\n# managed-machine:end foreign-block-id\n"
	wantAction := "conflict foreign-block " + filepath.ToSlash(h.foreignBlkDest) + ":foreign-block-id"
	detail, ok := h.proveConflict("mm-gate-conflict-block", h.foreignBlkDest, initial, wantAction)
	obs := observedRefused
	if !ok {
		obs = observedFailed
	}
	h.recordCase(caseConflictBlock, observedRefused, obs, ok, detail)
}

// runConflictForeignKey proves that an existing structured file with an
// unowned key plans `conflict foreign-key <path>:<key>` and --apply refuses
// with MM_CONFIG_CONFLICT while keeping the destination byte-identical.
func (h *configHarness) runConflictForeignKey() {
	initial := "{\n  \"editor\": {\n    \"tabSize\": 4\n  }\n}\n"
	wantAction := "conflict foreign-key " + filepath.ToSlash(h.foreignKeyDest) + ":editor.tabSize"
	detail, ok := h.proveConflict("mm-gate-conflict-key", h.foreignKeyDest, initial, wantAction)
	obs := observedRefused
	if !ok {
		obs = observedFailed
	}
	h.recordCase(caseConflictKey, observedRefused, obs, ok, detail)
}

func (h *configHarness) proveConflict(cfgID, destPath, initialContent, wantAction string) (string, bool) {
	if err := write(destPath, initialContent); err != nil {
		return err.Error(), false
	}
	beforeSum := sum([]byte(initialContent))

	code, out, errOut := h.runCore("config", cfgID, "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		return fmt.Sprintf("plan %s exited %d: %s", cfgID, code, firstLineOf(errOut, out)), false
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 2 {
		return fmt.Sprintf("plan %s emitted %d records (%v), want 2", cfgID, len(recs), err), false
	}
	planID := str(recs[0], "plan")
	if got := str(recs[1], "action"); got != wantAction {
		return fmt.Sprintf("planned action = %q, want %q", got, wantAction), false
	}
	h.recordPlan(planID)

	code, out, errOut = h.runCore("config", cfgID, "--apply", "--user")
	if problem := checkDiagnostic(code, 1, out, errOut, diag.ConfigConflict); problem != "" {
		return "apply " + cfgID + ": " + problem, false
	}
	afterBytes, err := os.ReadFile(destPath)
	if err != nil {
		return "read destination after refused apply: " + err.Error(), false
	}
	if sum(afterBytes) != beforeSum {
		return "destination was modified despite conflict refusal", false
	}
	return fmt.Sprintf("planned %q and refused apply with %s; destination stayed byte-identical", wantAction, diag.ConfigConflict), true
}

// runConflictFlavorCrossDef proves that two flavored fixtures composing two
// configurations that map to the same destination with differing values plan a
// cross-definition conflict and are refused by --apply and --check with
// MM_CONFIG_CONFLICT while keeping the destination byte-identical.
func (h *configHarness) runConflictFlavorCrossDef() {
	initial := "# pre-existing flavor destination\n"
	if err := write(h.flavorDest, initial); err != nil {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false, err.Error())
		return
	}
	beforeSum := sum([]byte(initial))
	extra := []string{"MANAGED_MACHINE_DESIRED_FILE=" + h.flavorDesired}
	wantAction := "conflict config:mm-gate-flavor-a@user:main config:mm-gate-flavor-b@user:main " + filepath.ToSlash(h.flavorDest)

	code, out, errOut := h.runCoreEnv(extra, "config", "mm-gate-flavor-a", "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false,
			fmt.Sprintf("plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 2 {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false,
			fmt.Sprintf("plan emitted %d records (%v), want 2", len(recs), err))
		return
	}
	planID := str(recs[0], "plan")
	if got := str(recs[1], "action"); got != wantAction {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false,
			fmt.Sprintf("planned action = %q, want %q", got, wantAction))
		return
	}
	h.recordPlan(planID)

	code, out, errOut = h.runCoreEnv(extra, "config", "mm-gate-flavor-a", "--apply", "--user")
	if problem := checkDiagnostic(code, 1, out, errOut, diag.ConfigConflict); problem != "" {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false, "apply: "+problem)
		return
	}

	code, out, errOut = h.runCoreEnv(extra, "config", "mm-gate-flavor-a", "--check", "--user")
	if code != 1 || checkDiagnosticLine(errOut, diag.ConfigConflict) != "" {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false,
			fmt.Sprintf("check exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	checkRecs, err := parseJSONL(out)
	if err != nil || len(checkRecs) != 1 || str(checkRecs[0], "action") != wantAction {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false,
			fmt.Sprintf("check records = %v (%v)", checkRecs, err))
		return
	}

	afterBytes, err := os.ReadFile(h.flavorDest)
	if err != nil || sum(afterBytes) != beforeSum {
		h.recordCase(caseConflictFlavor, observedRefused, observedFailed, false,
			"flavor destination changed despite conflict refusal")
		return
	}
	h.recordCase(caseConflictFlavor, observedRefused, observedRefused, true,
		"flavors mm-gate-flavor-alpha and mm-gate-flavor-beta planned cross-definition conflict and refused apply and check with MM_CONFIG_CONFLICT")
}

// runSecretMatrix proves the D5 secret matrix (env-only name, fixture-Pass-only
// name, neither -> MM_CONFIG_SECRET_UNRESOLVED) and greps the state directory,
// custom root, stdout/stderr captures and encoded evidence for both fixture
// secret values requiring zero hits.
func (h *configHarness) runSecretMatrix() {
	envSecretVar := secret.EnvVar(secretEnvName) + "=" + secretEnvValue

	// 1. Unresolved secret in neither MM_SECRET_* nor fixture Pass fails with
	// MM_CONFIG_SECRET_UNRESOLVED at --plan, --check and --apply.
	unresExtra := []string{
		"MANAGED_MACHINE_DESIRED_FILE=" + h.unresDesired,
		envSecretVar,
	}
	for _, flagName := range []string{"--plan", "--check", "--apply"} {
		code, out, errOut := h.runCoreEnv(unresExtra, "config", "mm-gate-secrets", flagName, "--user")
		if problem := checkDiagnostic(code, 1, out, errOut, diag.ConfigSecretUnresolved); problem != "" {
			h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
				"unresolved "+flagName+": "+problem)
			return
		}
		if !strings.Contains(string(errOut), "secret:"+secretUnresolvedName) {
			h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
				fmt.Sprintf("unresolved %s did not name secret:%s: %s", flagName, secretUnresolvedName, firstLineOf(errOut)))
			return
		}
	}

	// 2. Resolve env-only name via MM_SECRET_WORK_ENV_ONLY and pass-only name
	// via the fixture pass script first on PATH.
	_ = os.Remove(h.passLog)
	secExtra := []string{envSecretVar}
	code, out, errOut := h.runCoreEnv(secExtra, "config", "mm-gate-secrets", "--plan", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			fmt.Sprintf("secret plan exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	recs, err := parseJSONL(out)
	if err != nil || len(recs) != 3 {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			fmt.Sprintf("secret plan emitted %d records (%v), want 3", len(recs), err))
		return
	}
	planID := str(recs[0], "plan")
	calls, err := readLines(h.passLog)
	if err != nil {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			"fixture pass was not invoked: "+err.Error())
		return
	}
	wantCalls := []string{"show " + secretPassName}
	if !reflect.DeepEqual(calls, wantCalls) {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			fmt.Sprintf("fixture pass calls = %v, want %v", calls, wantCalls))
		return
	}

	// Check that bindings.assets records digest-only secret assets.
	doc, err := h.configPlanDoc(planID)
	if err != nil {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false, err.Error())
		return
	}
	wantEnvAsset := secret.Asset(secretEnvName, secretEnvValue)
	wantPassAsset := secret.Asset(secretPassName, secretPassValue)
	if !hasPlanAsset(doc.Bindings.Assets, wantEnvAsset) || !hasPlanAsset(doc.Bindings.Assets, wantPassAsset) {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			fmt.Sprintf("saved plan assets missing digest-only secret entries: %+v", doc.Bindings.Assets))
		return
	}

	code, out, errOut = h.runCoreEnv(secExtra, "config", "mm-gate-secrets", "--apply", "--user")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			fmt.Sprintf("secret apply exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	h.recordPlan(planID)

	gotRC, err := os.ReadFile(h.secretRCDest)
	if err != nil {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false, err.Error())
		return
	}
	wantRC := "env_secret = " + secretEnvValue + "\npass_secret = " + secretPassValue + "\n"
	if string(gotRC) != wantRC {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			"rendered secret template did not match expected content")
		return
	}
	gotJSON, err := os.ReadFile(h.secretJSONDest)
	if err != nil {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false, err.Error())
		return
	}
	if !strings.Contains(string(gotJSON), secretEnvValue) || !strings.Contains(string(gotJSON), secretPassValue) {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			"rendered structured secret destination missing expected values")
		return
	}

	code, out, errOut = h.runCoreEnv(secExtra, "config", "mm-gate-secrets", "--check", "--user")
	if code != 0 || len(out) != 0 || len(errOut) != 0 {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false,
			fmt.Sprintf("secret check exited %d", code))
		return
	}

	// 3. Grep state dir, custom root, stdout/stderr captures and evidence for
	// both fixture secret values (zero hits required).
	if leak := h.grepSecrets(); leak != "" {
		h.recordCase(caseSecretMatrix, "resolved+redacted", observedFailed, false, leak)
		return
	}
	h.recordCase(caseSecretMatrix, "resolved+redacted", "resolved+redacted", true,
		"env-only, fixture-Pass-only and unresolved secrets verified; zero hits across state dir, custom root, captures and evidence")
}

func (h *configHarness) grepSecrets() string {
	if leak := grepTree(h.state, "state", secretSentinels); leak != "" {
		return leak
	}
	if leak := grepTree(h.customRoot, "custom-root", secretSentinels); leak != "" {
		return leak
	}
	for i, c := range h.captures {
		if leak := grepBytes(c, fmt.Sprintf("capture[%d]", i), secretSentinels); leak != "" {
			return leak
		}
	}
	preview, err := json.Marshal(ConfigEvidence{InstallEvidence: *h.ev, Cases: h.cases})
	if err != nil {
		return err.Error()
	}
	return grepBytes(preview, "evidence", secretSentinels)
}

func grepTree(root, label string, sentinels []string) string {
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return ""
	}
	var found string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if hit := grepBytes(b, label+"/"+filepath.ToSlash(rel), sentinels); hit != "" {
			found = hit
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func grepBytes(b []byte, where string, sentinels []string) string {
	for _, s := range sentinels {
		if s != "" && bytes.Contains(b, []byte(s)) {
			return "secret sentinel leaked in " + where
		}
	}
	return ""
}

// runStatusSections seeds one applied software receipt in the isolated state
// directory and runs `managed-machine status` (all sections and per-section),
// proving software, config and flavor sections with recorded, observed and
// unknown present in one run and zero filesystem mutations.
func (h *configHarness) runStatusSections() {
	if err := h.seedSoftwareReceipt(); err != nil {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false, err.Error())
		return
	}

	beforeState := map[string]string{}
	if err := hashTree(h.state, "state", beforeState); err != nil {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false, err.Error())
		return
	}

	code, out, errOut := h.runCore("status")
	if code != 0 || len(errOut) != 0 {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
			fmt.Sprintf("status exited %d: %s", code, firstLineOf(errOut, out)))
		return
	}
	recs, problem := parseStatusRecords(out)
	if problem != "" {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false, problem)
		return
	}

	seenSection := map[string]bool{}
	seenEvidence := map[string]bool{}
	seenState := map[string]bool{}
	for _, r := range recs {
		seenSection[str(r, "section")] = true
		seenEvidence[str(r, "evidence")] = true
		seenState[str(r, "state")] = true
	}
	for _, sec := range []string{"software", "config", "flavor"} {
		if !seenSection[sec] {
			h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
				fmt.Sprintf("status omitted section %q: %v", sec, recs))
			return
		}
	}
	for _, ev := range []string{"recorded", "observed"} {
		if !seenEvidence[ev] {
			h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
				fmt.Sprintf("status omitted evidence %q: %v", ev, recs))
			return
		}
	}
	for _, st := range []string{"present", "unknown", "absent"} {
		if !seenState[st] {
			h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
				fmt.Sprintf("status omitted state %q: %v", st, recs))
			return
		}
	}

	// Verify per-section invocations and custom-root status agree.
	for _, sec := range []string{"software", "config", "flavor"} {
		sc, so, se := h.runCore("status", "--section", sec)
		if sc != 0 || len(se) != 0 {
			h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
				fmt.Sprintf("status --section %s exited %d: %s", sec, sc, firstLineOf(se, so)))
			return
		}
		if _, p := parseStatusRecords(so); p != "" {
			h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
				fmt.Sprintf("status --section %s: %s", sec, p))
			return
		}
	}
	cc, co, ce := h.runCore("status", "--section", "config", "--custom", h.customRoot)
	if cc != 0 || len(ce) != 0 {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
			fmt.Sprintf("status --custom exited %d: %s", cc, firstLineOf(ce, co)))
		return
	}
	customRecs, problem := parseStatusRecords(co)
	if problem != "" || len(customRecs) != 1 || str(customRecs[0], "resource") != "mm-gate-config" || str(customRecs[0], "state") != "present" {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
			fmt.Sprintf("status --custom records = %v (%s)", customRecs, problem))
		return
	}

	afterState := map[string]string{}
	if err := hashTree(h.state, "state", afterState); err != nil {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false, err.Error())
		return
	}
	if diff := profileDiff(beforeState, afterState); len(diff) > 0 {
		h.recordCase(caseStatusSections, "software+config+flavor", observedFailed, false,
			"status mutated state directory: "+strings.Join(diff, "; "))
		return
	}

	h.recordCase(caseStatusSections, "software+config+flavor", "software+config+flavor", true,
		"status reported software, config and flavor sections with recorded, observed and unknown in one read-only run")
}

// seedSoftwareReceipt writes one applied `software --install` plan and
// journal receipt into h.state so `status` can report a recorded software
// receipt without running a package manager.
func (h *configHarness) seedSoftwareReceipt() error {
	const planID = "plan-20261021T120000Z-00aa"
	const at = "2026-10-21T12:00:00Z"
	doc := plan.Document{
		Plan:           planID,
		SchemaVersion:  plan.SchemaVersion,
		Command:        "software --install",
		Software:       "ripgrep",
		Target:         "user",
		Account:        "default",
		SourceRevision: "builtin@sha256:0000",
		CreatedAt:      at,
		State:          plan.Applied,
		Bindings: plan.Bindings{
			Machine:      "local",
			Platform:     h.target,
			InputsDigest: "sha256:0000",
			Assets:       []plan.Asset{},
			Providers:    []plan.Provider{{Name: "brew", Version: "4.4.0"}},
		},
		Preconditions: []plan.Precondition{},
		Operations:    []plan.Operation{},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := write(filepath.Join(h.state, "plans", planID+".json"), string(b)+"\n"); err != nil {
		return err
	}
	rec := plan.Record{
		Plan:    planID,
		Action:  plan.ActionReceipt,
		Outcome: plan.OutcomeApplied,
		State:   plan.Applied,
		At:      at,
	}
	rb, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := write(filepath.Join(h.state, "journal", planID+".jsonl"), string(rb)+"\n"); err != nil {
		return err
	}
	h.recordPlan(planID)
	return nil
}

func parseStatusRecords(out []byte) ([]record, string) {
	wantKeys := []string{"section", "resource", "target", "state", "facts", "evidence", "at"}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	var recs []record
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		keys, err := objectKeys([]byte(line))
		if err != nil {
			return nil, err.Error()
		}
		if !sameStrings(keys, wantKeys) {
			return nil, "status record keys are " + strings.Join(keys, ",") + ", want " + strings.Join(wantKeys, ",")
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, err.Error()
		}
		recs = append(recs, r)
	}
	if len(recs) == 0 {
		return nil, "status emitted no records"
	}
	return recs, ""
}

// runGuards verifies that no sentinel program was started, the real home's
// profile snapshot and ~/.config/managed-machine are byte-identical, nothing
// outside the custom root changed during the custom-target cycle except the
// state directory, and no secret leaked.
func (h *configHarness) runGuards() {
	h.ev.Guards.SentinelInvocations = []string{}
	if b, err := os.ReadFile(h.log); err == nil {
		for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			h.ev.Guards.SentinelInvocations = append(h.ev.Guards.SentinelInvocations, h.paths.clean(line))
		}
	}
	after, err := h.machineSnapshot()
	if err != nil {
		h.ev.Guards.ProfileChanges = []string{"the profiles could not be read: " + err.Error()}
		h.recordCase(caseGuards, "intact", observedFailed, false, "the profiles could not be read")
		return
	}
	changes := profileDiff(h.machine, after)
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
	case len(h.customChanges) > 0:
		problem = "custom root containment changed: " + strings.Join(h.customChanges, "; ")
	default:
		problem = h.grepSecrets()
	}
	if problem != "" {
		h.recordCase(caseGuards, "intact", observedFailed, false, problem)
		return
	}
	h.recordCase(caseGuards, "intact", "intact", true,
		"no guarded program was started, real-home profiles and ~/.config/managed-machine are byte-identical, and custom-root containment held")
}

// containmentSnapshot hashes every regular file under the harness root except
// the custom root and the guarded state directory.
func (h *configHarness) containmentSnapshot(customRoot string) (map[string]string, error) {
	out := map[string]string{}
	inet := filepath.Join(h.home, "AppData", "Local", "Microsoft", "Windows", "INetCache")
	if err := hashTreeExcept(h.root, "harness", []string{customRoot, h.state, inet}, out); err != nil {
		return nil, err
	}
	return out, nil
}

// configPlanDoc reads one saved configuration plan from h.state.
func (h *configHarness) configPlanDoc(id string) (plan.Document, error) {
	var d plan.Document
	b, err := os.ReadFile(filepath.Join(h.state, "plans", id+".json"))
	if err != nil {
		return d, err
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return d, err
	}
	return d, nil
}

func checkDiagnostic(code, wantCode int, out, errOut []byte, wantID string) string {
	if code != wantCode {
		return fmt.Sprintf("exit %d, want %d: %s", code, wantCode, firstLineOf(errOut, out))
	}
	if len(out) != 0 {
		return "stdout not empty on failure: " + firstLineOf(out)
	}
	return checkDiagnosticLine(errOut, wantID)
}

func checkDiagnosticLine(errOut []byte, wantID string) string {
	lines := strings.Split(strings.TrimRight(string(errOut), "\n"), "\n")
	if len(lines) != 1 || !diagLine.MatchString(lines[0]) {
		return "stderr is not one F03 diagnostic line: " + strings.TrimSpace(string(errOut))
	}
	if wantID != "" && !strings.HasPrefix(lines[0], `{"diagnostic":"`+wantID+`"`) {
		return "diagnostic is not " + wantID + ": " + lines[0]
	}
	return ""
}

func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

func hasPlanAsset(assets []plan.Asset, want plan.Asset) bool {
	for _, a := range assets {
		if a == want {
			return true
		}
	}
	return false
}

// configAggregateCmd checks that five S4 config-gate records belong together:
// one per first-release target, the same source revision, every gate in
// config mode with result pass, and every D9 case proven. It prints the D9
// case matrix.
func configAggregateCmd(args []string) error {
	fl := flag.NewFlagSet("config-aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("config-aggregate needs -source-revision")
	}
	files := fl.Args()
	var problems []string
	byTarget := map[string]ConfigEvidence{}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		var ev ConfigEvidence
		if err := json.Unmarshal(b, &ev); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", filepath.Base(p), err))
			continue
		}
		if ev.Kind != configEvidenceKind {
			problems = append(problems, fmt.Sprintf("%s is a %q record, not %q", ev.Target, ev.Kind, configEvidenceKind))
		}
		if ev.Mode != configMode {
			problems = append(problems, fmt.Sprintf("%s ran in %q mode, not %q", ev.Target, ev.Mode, configMode))
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
		for _, c := range ev.Cases {
			if !c.OK {
				problems = append(problems, fmt.Sprintf("%s: case %s is %s", ev.Target, c.Case, c.Observed))
			}
		}
	}
	for _, t := range firstReleaseTargets {
		ev, ok := byTarget[t]
		if !ok {
			problems = append(problems, "no evidence for "+t)
			continue
		}
		for _, name := range configCaseNames {
			if !configCaseProven(ev, name) {
				problems = append(problems, fmt.Sprintf("%s: no case for %s", t, name))
			}
		}
	}
	if len(files) != len(firstReleaseTargets) {
		problems = append(problems, fmt.Sprintf("%d records, want %d", len(files), len(firstReleaseTargets)))
	}
	sort.Strings(problems)
	printConfigMatrix(byTarget)
	fmt.Printf("\nsource revision `%s`\n", *rev)
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("FAIL:", p)
		}
		return errors.New("config-aggregate failed")
	}
	return nil
}

func configCaseProven(ev ConfigEvidence, name string) bool {
	for _, c := range ev.Cases {
		if c.Case == name {
			return true
		}
	}
	return false
}

func printConfigMatrix(byTarget map[string]ConfigEvidence) {
	fmt.Printf("| case | %s |\n", strings.Join(firstReleaseTargets, " | "))
	fmt.Printf("|---|%s\n", strings.Repeat("---|", len(firstReleaseTargets)))
	for _, name := range configCaseNames {
		fmt.Printf("| %s |", name)
		for _, t := range firstReleaseTargets {
			fmt.Printf(" %s |", configCell(byTarget, name, t))
		}
		fmt.Println()
	}
}

func configCell(byTarget map[string]ConfigEvidence, name, target string) string {
	ev, ok := byTarget[target]
	if !ok {
		return "FAIL"
	}
	seen, failed := false, false
	for _, c := range ev.Cases {
		if c.Case != name {
			continue
		}
		seen = true
		if !c.OK {
			failed = true
		}
	}
	if !seen || failed {
		return "FAIL"
	}
	return "ok"
}
