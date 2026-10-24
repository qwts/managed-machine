// Command mm-walkthrough is the F08 first-slice integration gate and the
// T01-T05 per-target evidence run (migration plan S1). It drives a built
// managed-machine binary through discovery, recipe inspection and skill
// reading in an isolated harness, guards against machine changes and
// installer processes, and writes a sanitized evidence record. It is
// migration tooling, not part of the managed-machine CLI contract.
//
// The install gate (issue #273) is the S2 completion gate: it drives the
// same binary through real installs on a hosted runner, or proves that the
// install path refuses without touching the machine.
//
// The provider gate (issue #285) is the S3 provider gate: it drives the
// same binary through one real install per provider, for the user target
// and for a custom root, on a hosted runner.
//
// The config gate (issue #301) is the S4 configuration gate: it drives the
// same binary through the D9 configuration, secret, flavor and status
// case list in an isolated harness on all five targets.
//
// The fleet gate (issue #319) is the S5 fleet gate: it drives the same
// binary through the D9 fleet-access case list in an isolated harness on
// all five targets.
//
// The release gate (issues #332, #335) is the S6 release gate: it drives the
// release archive binary through the full 15-case D10 release matrix in an
// isolated harness on all five targets, and aggregates the release, provider
// and install gate records with SHA256SUMS to emit support-matrix.md.
//
//	mm-walkthrough run -binary PATH -target OS/ARCH -evidence FILE [-source-revision SHA] [-compat FILE]
//	mm-walkthrough aggregate -source-revision SHA FILE...
//	mm-walkthrough install -binary PATH -target OS/ARCH -evidence FILE [-source-revision SHA]
//	mm-walkthrough install-aggregate -source-revision SHA FILE...
//	mm-walkthrough providers -binary PATH -target OS/ARCH -evidence FILE [-source-revision SHA]
//	mm-walkthrough providers-aggregate -source-revision SHA FILE...
//	mm-walkthrough config -binary PATH -target OS/ARCH -evidence FILE [-source-revision SHA]
//	mm-walkthrough config-aggregate -source-revision SHA FILE...
//	mm-walkthrough fleet -binary PATH -target OS/ARCH -evidence FILE [-source-revision SHA] [-compat FILE]
//	mm-walkthrough fleet-aggregate -source-revision SHA FILE...
//	mm-walkthrough release -dist DIR [-version X.Y.Z] [-binary PATH] -target OS/ARCH -evidence FILE [-source-revision SHA] [-repo DIR] [-compat FILE]
//	mm-walkthrough release-aggregate -source-revision SHA [-version X.Y.Z] [-checksums FILE] [-out FILE] [-run-id ID] FILE...
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"unicode/utf8"
)

func main() {
	sentinelMode()
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: mm-walkthrough run|aggregate|install|install-aggregate|providers|providers-aggregate|config|config-aggregate|fleet|fleet-aggregate|release|release-aggregate [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = runCmd(os.Args[2:])
	case "aggregate":
		err = aggregateCmd(os.Args[2:])
	case "install":
		err = installCmd(os.Args[2:])
	case "install-aggregate":
		err = installAggregateCmd(os.Args[2:])
	case "providers":
		err = providersCmd(os.Args[2:])
	case "providers-aggregate":
		err = providersAggregateCmd(os.Args[2:])
	case "config":
		err = configCmd(os.Args[2:])
	case "config-aggregate":
		err = configAggregateCmd(os.Args[2:])
	case "fleet":
		err = fleetCmd(os.Args[2:])
	case "fleet-aggregate":
		err = fleetAggregateCmd(os.Args[2:])
	case "release":
		err = releaseCmd(os.Args[2:])
	case "release-aggregate":
		err = releaseAggregateCmd(os.Args[2:])
	case "_interrupt-child":
		err = interruptChildCmd(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mm-walkthrough:", err)
		os.Exit(1)
	}
}

// Evidence is the per-target record attached to the pull request. It holds
// digests and harness-relative names only, never runner paths or contents
// outside the fixture.
type Evidence struct {
	SchemaVersion  int          `json:"schema_version"`
	Story          string       `json:"story"`
	SourceRevision string       `json:"source_revision"`
	BinarySHA256   string       `json:"binary_sha256"`
	Target         string       `json:"target"`
	Probe          probe        `json:"probe"`
	Runtime        string       `json:"walkthrough_runtime"`
	OSVersion      string       `json:"os_version"`
	Steps          []StepResult `json:"steps"`
	Guards         Guards       `json:"guards"`
	Compat         Compat       `json:"f01_compatibility"`
	Result         string       `json:"result"`
	Failures       []string     `json:"failures"`
}

type probe struct {
	Platform string `json:"platform"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Emulated bool   `json:"emulated"`
}

type StepResult struct {
	Name         string   `json:"name"`
	Args         []string `json:"args"`
	Mode         string   `json:"mode"`
	Exit         int      `json:"exit"`
	StdoutSHA256 string   `json:"stdout_sha256"`
	StderrSHA256 string   `json:"stderr_sha256"`
	Records      int      `json:"records"`
	Portable     bool     `json:"portable,omitempty"`
	OK           bool     `json:"ok"`
	Detail       string   `json:"detail,omitempty"`
}

type Guards struct {
	HarnessEntries      int      `json:"harness_entries"`
	FilesystemChanges   []string `json:"filesystem_changes"`
	SentinelInvocations []string `json:"sentinel_invocations"`
	Sentinels           int      `json:"sentinels"`
	ExtensionExecuted   bool     `json:"extension_executed"`
	CustomRootAbsent    bool     `json:"custom_root_absent"`
	StateDirUntouched   bool     `json:"state_dir_untouched"`
	HomeOutsideHarness  bool     `json:"home_outside_harness_used"`
}

// Compat records the F01 golden comparison. The legacy Bash routes exist
// only on macOS under compatibility B, so the other targets record it as
// not applicable rather than passed.
type Compat struct {
	Status  string   `json:"status"`
	Results []string `json:"results,omitempty"`
}

type harness struct {
	bin, sentinelLog, marker string
	l                        layout
	env                      []string
	ev                       *Evidence
}

func runCmd(args []string) error {
	fl := flag.NewFlagSet("run", flag.ContinueOnError)
	bin := fl.String("binary", "", "managed-machine binary under test")
	tgt := fl.String("target", "", "the runner's expected canonical target")
	out := fl.String("evidence", "", "evidence file to write")
	rev := fl.String("source-revision", "", "source revision under test")
	compat := fl.String("compat", "", "F01 harness output to embed (macOS)")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *bin == "" || *tgt == "" || *out == "" {
		return errors.New("run needs -binary, -target and -evidence")
	}
	absBin, err := filepath.Abs(*bin)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(absBin)
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "mm-walkthrough-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	h := &harness{
		bin:         absBin,
		sentinelLog: filepath.Join(work, "guard", "sentinels.log"),
		marker:      filepath.Join(work, "guard", "extension-ran"),
		l:           newLayout(filepath.Join(work, "mm walkthrough ü")),
		ev: &Evidence{SchemaVersion: 1, Story: "F08", SourceRevision: *rev, BinarySHA256: sum(raw), Target: *tgt,
			Runtime: runtime.GOOS + "/" + runtime.GOARCH, OSVersion: osVersion(), Failures: []string{}},
	}
	sentinelDir := filepath.Join(work, "guard", "bin")
	if err := installSentinels(sentinelDir); err != nil {
		return err
	}
	h.ev.Guards.Sentinels = len(sentinels)
	if err := h.l.build(h.marker); err != nil {
		return err
	}
	h.env = h.childEnv(sentinelDir)
	h.ev.Compat = readCompat(*compat, *tgt)

	before, err := snapshot(h.l.root)
	if err != nil {
		return err
	}
	h.ev.Guards.HarnessEntries = len(before)
	h.walk()
	after, err := snapshot(h.l.root)
	if err != nil {
		return err
	}
	h.guards(before, after)
	h.finish()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(h.ev); err != nil {
		return err
	}
	if err := os.WriteFile(*out, buf.Bytes(), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s: %d steps, result %s\n", *tgt, len(h.ev.Steps), h.ev.Result)
	for _, f := range h.ev.Failures {
		fmt.Println("FAIL:", f)
	}
	if h.ev.Result != "pass" {
		return errors.New("walkthrough failed")
	}
	return nil
}

// childEnv is built from nothing, like env -i: the binary sees only the
// isolated home, the fixture inputs and a PATH of sentinels.
func (h *harness) childEnv(path string) []string {
	l := h.l
	tmp := filepath.Join(l.home, "tmp")
	os.MkdirAll(tmp, 0o755)
	env := []string{
		"HOME=" + l.home, "USERPROFILE=" + l.home,
		"APPDATA=" + filepath.Join(l.home, "AppData", "Roaming"), "LOCALAPPDATA=" + filepath.Join(l.home, "AppData", "Local"),
		"XDG_CONFIG_HOME=" + filepath.Join(l.home, ".config"), "XDG_STATE_HOME=" + filepath.Join(l.home, ".local", "state"),
		"XDG_DATA_HOME=" + filepath.Join(l.home, ".local", "share"), "XDG_CACHE_HOME=" + filepath.Join(l.home, ".cache"),
		"TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp,
		"PATH=" + path, "LANG=C.UTF-8", "LC_ALL=C.UTF-8",
		"MANAGED_MACHINE_SOURCE_DIR=" + l.definitions,
		"MANAGED_MACHINE_EXTENSIONS_DIR=" + l.extensions,
		sentinelLogEnv + "=" + h.sentinelLog,
	}
	if runtime.GOOS == "windows" {
		env = append(env, "PATHEXT=.COM;.EXE;.BAT;.CMD", "SystemRoot="+os.Getenv("SystemRoot"), "windir="+os.Getenv("windir"))
	}
	return env
}

func (h *harness) exec(args ...string) (int, []byte, []byte) { return h.execEnv(h.env, args...) }

func (h *harness) execEnv(env []string, args ...string) (int, []byte, []byte) {
	c := exec.Command(h.bin, args...)
	c.Env, c.Dir = env, h.l.home
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

func (h *harness) sanitize(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(filepath.ToSlash(a), filepath.ToSlash(h.l.root), "<harness>")
	}
	return out
}

func (h *harness) record(name, mode string, args []string, code int, o, e []byte, n int, portable bool, problem string) {
	// Every stream is LF-terminated UTF-8 on every target (T05).
	switch {
	case problem != "":
	case bytes.ContainsRune(o, '\r') || bytes.ContainsRune(e, '\r'):
		problem = "carriage return in output"
	case !utf8.Valid(o) || !utf8.Valid(e):
		problem = "output is not valid UTF-8"
	}
	r := StepResult{Name: name, Args: h.sanitize(args), Mode: mode, Exit: code, StdoutSHA256: sum(o), StderrSHA256: sum(e),
		Records: n, Portable: portable, OK: problem == "", Detail: problem}
	h.ev.Steps = append(h.ev.Steps, r)
	if problem != "" {
		h.ev.Failures = append(h.ev.Failures, name+" ("+mode+"): "+problem)
	}
}

type record = map[string]any

func parseJSONL(b []byte) ([]record, error) {
	var out []record
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			return nil, fmt.Errorf("stdout line is not a JSON object: %v", err)
		}
		out = append(out, r)
	}
	return out, nil
}

// parseDocument maps --json output onto the record list the default mode
// emits: a single object is one record, {"records":[...]} is the list, and
// any sibling field (a continuation) is the trailing record.
func parseDocument(b []byte) ([]record, error) {
	var doc record
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("--json output is not one JSON object: %v", err)
	}
	list, ok := doc["records"].([]any)
	if !ok {
		return []record{doc}, nil
	}
	var out []record
	for _, r := range list {
		m, ok := r.(record)
		if !ok {
			return nil, errors.New("--json records entry is not an object")
		}
		out = append(out, m)
	}
	delete(doc, "records")
	if len(doc) > 0 {
		out = append(out, doc)
	}
	return out, nil
}

// query runs args in all three output modes, checks they agree, applies
// check to the records and returns them.
func (h *harness) query(name string, portable bool, check func([]record) string, args ...string) []record {
	code, o, e := h.exec(args...)
	recs, perr := parseJSONL(o)
	problem := ""
	switch {
	case code != 0:
		problem = fmt.Sprintf("exit %d, stderr %s", code, strings.TrimSpace(string(e)))
	case len(e) != 0:
		problem = "stderr not empty: " + strings.TrimSpace(string(e))
	case perr != nil:
		problem = perr.Error()
	case len(recs) == 0:
		problem = "no records"
	case check != nil:
		problem = check(recs)
	}
	h.record(name, "default", args, code, o, e, len(recs), portable, problem)

	jargs := append(append([]string{}, args...), "--json")
	jc, jo, je := h.exec(jargs...)
	jrecs, jerr := parseDocument(jo)
	jp := ""
	switch {
	case jc != code || len(je) != 0:
		jp = fmt.Sprintf("exit %d (default %d), stderr %q", jc, code, je)
	case jerr != nil:
		jp = jerr.Error()
	case !sameRecords(recs, jrecs):
		jp = "--json records differ from the default mode's"
	}
	h.record(name, "json", jargs, jc, jo, je, len(jrecs), portable, jp)

	hargs := append(append([]string{}, args...), "--human")
	hc, ho, he := h.exec(hargs...)
	hp := ""
	switch {
	case hc != code || len(he) != 0:
		hp = fmt.Sprintf("exit %d (default %d), stderr %q", hc, code, he)
	case len(bytes.TrimSpace(ho)) == 0:
		hp = "--human output is empty"
	case json.Valid(bytes.TrimSpace(ho)):
		hp = "--human output is JSON"
	default:
		for _, r := range recs {
			if id := identity(r); id != "" && !bytes.Contains(ho, []byte(id)) {
				hp = fmt.Sprintf("--human output omits %q", id)
				break
			}
		}
	}
	h.record(name, "human", hargs, hc, ho, he, len(recs), portable, hp)
	return recs
}

func sameRecords(a, b []record) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// identity is the value a reader looks for in the human rendering.
func identity(r record) string {
	for _, k := range []string{"software", "name", "platform"} {
		if s, ok := r[k].(string); ok {
			return s
		}
	}
	return ""
}

var (
	diagLine      = regexp.MustCompile(`^\{"diagnostic":"MM_[A-Z_]+","message":"[^"\n]`)
	humanDiagLine = regexp.MustCompile(`^MM_[A-Z_]+: \S`)
)

// reject runs args expecting failure with the shared F03 diagnostic shape.
func (h *harness) reject(name string, want int, id string, args ...string) {
	human := args[len(args)-1] == "--human" && !contains(args, "--json")
	code, o, e := h.exec(args...)
	problem := ""
	lines := strings.Split(strings.TrimRight(string(e), "\n"), "\n")
	switch {
	case code != want:
		problem = fmt.Sprintf("exit %d, want %d", code, want)
	case len(o) != 0:
		problem = "stdout not empty on failure"
	case human && (len(lines) != 1 || !humanDiagLine.MatchString(lines[0])):
		problem = "stderr is not one human F03 diagnostic line: " + strings.TrimSpace(string(e))
	case !human && (len(lines) != 1 || !diagLine.MatchString(lines[0])):
		problem = "stderr is not one F03 diagnostic line: " + strings.TrimSpace(string(e))
	case id != "" && !strings.HasPrefix(lines[0], `{"diagnostic":"`+id+`"`) && !strings.HasPrefix(lines[0], id+": "):
		problem = "diagnostic is not " + id + ": " + lines[0]
	}
	h.record(name, "default", args, code, o, e, 0, true, problem)
}

// help runs a -h form, which must succeed, print text and never be JSON.
func (h *harness) help(name string, must []string, args ...string) {
	code, o, e := h.exec(args...)
	problem := ""
	switch {
	case code != 0 || len(e) != 0:
		problem = fmt.Sprintf("exit %d, stderr %q", code, e)
	default:
		for _, m := range must {
			if !bytes.Contains(o, []byte(m)) {
				problem = fmt.Sprintf("help omits %q", m)
				break
			}
		}
	}
	h.record(name, "help", args, code, o, e, 0, false, problem)
}

func contains(args []string, s string) bool {
	for _, a := range args {
		if a == s {
			return true
		}
	}
	return false
}
