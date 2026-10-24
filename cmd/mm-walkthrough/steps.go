package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/qwts/managed-machine/internal/skill"
	"github.com/qwts/managed-machine/internal/target"
)

func str(r record, k string) string { s, _ := r[k].(string); return s }

func all(recs []record, f func(record) string) string {
	for _, r := range recs {
		if p := f(r); p != "" {
			return p
		}
	}
	return ""
}

func has(recs []record, k, v string) bool {
	for _, r := range recs {
		if str(r, k) == v {
			return true
		}
	}
	return false
}

func hasTarget(r record, t string) bool {
	ts, _ := r["targets"].([]any)
	for _, x := range ts {
		if x == t {
			return true
		}
	}
	return false
}

// walk is the F08 walkthrough: platform, discovery and filters, recipe
// inspection, every skill kind with continuation and search, then the
// staged-functionality rejections.
func (h *harness) walk() {
	tgt := h.ev.Target
	recs := h.query("platform probe", false, func(r []record) string {
		if len(r) != 1 || str(r[0], "platform") != tgt || r[0]["emulated"] != false {
			return fmt.Sprintf("probe reports %v, want %s native", r, tgt)
		}
		return ""
	}, "platform")
	if len(recs) == 1 {
		h.ev.Probe = probe{str(recs[0], "platform"), str(recs[0], "os"), str(recs[0], "arch"), recs[0]["emulated"] == true}
	}
	if h.ev.Runtime != tgt {
		h.ev.Failures = append(h.ev.Failures, "the walkthrough itself runs as "+h.ev.Runtime+", not "+tgt)
	}

	h.help("top-level help", []string{"software", "skill", "Exit codes"}, "-h")
	h.help("software help shows staged install", []string{"--install", "Homebrew formulae", fixtureID}, "software", "-h")
	h.help("skill help lists both bundles", []string{"software-install", extSkill, "extension:walkthrough-tools@0.1.0"}, "skill", "-h")
	h.help("help wins over install", []string{"Usage: managed-machine software"}, "software", "ruff", "--install", "--user", "-h")

	h.query("discover available software", false, func(r []record) string {
		for _, want := range []string{"git", "ruff", "typescript", fixtureID} {
			if !has(r, "software", want) {
				return "missing " + want
			}
		}
		return ""
	}, "software", "--available")
	h.query("external local definition by category", true, func(r []record) string {
		if len(r) != 1 || str(r[0], "software") != fixtureID {
			return fmt.Sprintf("category %s returns %v", fixtureCat, r)
		}
		return ""
	}, "software", "--available", "--category", fixtureCat)
	h.query("filter by provider", false, func(r []record) string {
		if !has(r, "software", fixtureID) {
			return "provider filter omits the external definition"
		}
		return all(r, func(x record) string {
			if str(x, "provider") != "download" {
				return "non-download record " + str(x, "software")
			}
			return ""
		})
	}, "software", "--available", "--provider", "download")
	h.query("filter by absent custom root", false, func(r []record) string {
		return all(r, func(x record) string {
			if !hasTarget(x, "custom") {
				return str(x, "software") + " does not support custom"
			}
			return ""
		})
	}, "software", "--available", "--custom", h.l.custom)
	h.query("inspect a chosen recipe", true, func(r []record) string {
		if len(r) != 1 || str(r[0], "recipe") != "uv-tool" || str(r[0], "provider") != "uv" {
			return fmt.Sprintf("ruff --recipe uv-tool returns %v", r)
		}
		return ""
	}, "software", "ruff", "--recipe", "uv-tool")
	h.help("inspect recipes via -h", []string{"ruff", "uv"}, "software", "ruff", "-h")

	h.query("list skills", true, func(r []record) string {
		if !has(r, "name", "software-install") || !has(r, "name", extSkill) {
			return "skill list omits the core or extension skill"
		}
		return ""
	}, "skill")
	for _, k := range []string{"summary", "contract", "procedure", "example"} {
		h.query("skill "+k, true, caveatFirst("software-install", k, "MM_SOFTWARE_INSTALL_UNAVAILABLE"), "skill", "software-install", "--kind", k)
	}
	h.query("skill reference", true, func(r []record) string {
		if len(r) != 1 || str(r[0], "kind") != "reference" || str(r[0], "content") == "" {
			return fmt.Sprintf("reference returns %v", r)
		}
		return ""
	}, "skill", "provider:npm", "--kind", "reference")
	h.query("pointer search", true, func(r []record) string {
		return all(r, func(x record) string {
			if _, ok := x["next"]; ok {
				return ""
			}
			if str(x, "pointer") == "" || x["content"] != nil {
				return "search record is not pointer-only: " + str(x, "name")
			}
			return ""
		})
	}, "skill", "--search", "install")
	h.continuation()

	// The download fixture has no executor: it is refused on every target
	// before any state is written, so the walkthrough never changes the runner.
	h.reject("install is unavailable (user)", 2, "MM_SOFTWARE_INSTALL_UNAVAILABLE", "software", fixtureID, "--install", "--user")
	h.reject("install is unavailable (absent custom root)", 2, "MM_SOFTWARE_INSTALL_UNAVAILABLE", "software", fixtureID, "--install", "--custom", h.l.custom)
	h.reject("install is unavailable (human)", 2, "MM_SOFTWARE_INSTALL_UNAVAILABLE", "software", fixtureID, "--install", "--user", "--human")
	h.reject("install needs one target", 2, "MM_USAGE", "software", fixtureID, "--install")
	h.reject("install has no global fixture recipe", 2, "MM_SOFTWARE_UNSUPPORTED_COMBINATION", "software", fixtureID, "--install", "--global")
	h.reject("unknown option", 2, "MM_USAGE", "software", "--bogus")
	h.reject("conflicting modes", 2, "MM_USAGE", "skill", "--json", "--human")
	h.reject("unknown category", 2, "MM_SOFTWARE_UNKNOWN_CATEGORY", "software", "--available", "--category", "no-such-category")
	h.reject("unknown skill", 2, "MM_SKILL_UNKNOWN_NAME", "skill", "no-such-skill")
	h.argumentBoundary()
	h.caseSensitiveSource()
}

// caveatFirst checks the first response carries the skill's caveats ahead
// of the content.
func caveatFirst(name, kind, caveat string) func([]record) string {
	return func(r []record) string {
		if len(r) != 1 || str(r[0], "name") != name || str(r[0], "kind") != kind {
			return fmt.Sprintf("want one %s %s record, got %d", name, kind, len(r))
		}
		c := str(r[0], "content")
		if !strings.HasPrefix(c, "Caveats:") || !strings.Contains(c, caveat) {
			return "caveats are not first in the response"
		}
		return ""
	}
}

// continuation reads the oversized extension procedure page by page: each
// response is within the bound, the caveat comes first, every handle
// advances and the last page ends the chain with every step present.
func (h *harness) continuation() {
	args := []string{"skill", extSkill, "--kind", "procedure"}
	var content strings.Builder
	seen := map[string]bool{}
	pages := 0
	for {
		code, o, e := h.exec(args...)
		recs, err := parseJSONL(o)
		problem := ""
		next := ""
		switch {
		case code != 0 || len(e) != 0:
			problem = fmt.Sprintf("exit %d, stderr %q", code, e)
		case err != nil:
			problem = err.Error()
		case len(o) > skill.MaxResponseBytes:
			problem = fmt.Sprintf("response is %d bytes, over %d", len(o), skill.MaxResponseBytes)
		case pages == 0 && (len(recs) == 0 || !strings.Contains(str(recs[0], "content"), extCaveat)):
			problem = "the first page does not carry the caveat"
		}
		for _, r := range recs {
			content.WriteString(str(r, "content"))
			if n := str(r, "next"); n != "" {
				next = n
			}
		}
		if next != "" && seen[next] {
			problem = "continuation handle repeats: " + next
		}
		seen[next] = true
		pages++
		h.record(fmt.Sprintf("continuation page %d", pages), "default", args, code, o, e, len(recs), true, problem)
		if problem != "" || next == "" {
			break
		}
		if pages > 50 {
			h.ev.Failures = append(h.ev.Failures, "continuation does not terminate")
			return
		}
		args = []string{"skill", "--next", next}
	}
	text := content.String()
	missing := 0
	for i := 1; i <= stepCount; i++ {
		if !strings.Contains(text, fmt.Sprintf("Step %03d:", i)) {
			missing++
		}
	}
	switch {
	case pages < 2:
		h.ev.Failures = append(h.ev.Failures, "the oversized procedure fit one page; continuation was not exercised")
	case missing > 0:
		h.ev.Failures = append(h.ev.Failures, fmt.Sprintf("continuation lost %d of %d steps", missing, stepCount))
	}
}

func (h *harness) guards(before, after map[string]entry) {
	g := &h.ev.Guards
	g.FilesystemChanges = diff(before, after)
	if g.FilesystemChanges == nil {
		g.FilesystemChanges = []string{}
	}
	if len(g.FilesystemChanges) > 0 {
		h.ev.Failures = append(h.ev.Failures, fmt.Sprintf("the harness changed: %v", g.FilesystemChanges))
	}
	g.SentinelInvocations = []string{}
	if f, err := os.Open(h.sentinelLog); err == nil {
		s := bufio.NewScanner(f)
		for s.Scan() {
			g.SentinelInvocations = append(g.SentinelInvocations, h.sanitize([]string{s.Text()})[0])
		}
		f.Close()
		h.ev.Failures = append(h.ev.Failures, fmt.Sprintf("an S1 command started %v", g.SentinelInvocations))
	}
	if _, err := os.Stat(h.marker); err == nil {
		g.ExtensionExecuted = true
		h.ev.Failures = append(h.ev.Failures, "serving an extension bundle ran the extension")
	}
	if _, err := os.Stat(h.l.custom); errors.Is(err, os.ErrNotExist) {
		g.CustomRootAbsent = true
	} else {
		h.ev.Failures = append(h.ev.Failures, "the custom root was created")
	}
	g.StateDirUntouched = true
	for _, c := range g.FilesystemChanges {
		if strings.Contains(c, ".local/state") {
			g.StateDirUntouched = false
		}
	}
}

func (h *harness) finish() {
	if h.ev.Compat.Status == "fail" || h.ev.Compat.Status == "missing" {
		h.ev.Failures = append(h.ev.Failures, "F01 compatibility: "+h.ev.Compat.Status)
	}
	h.ev.Result = "pass"
	if len(h.ev.Failures) > 0 {
		h.ev.Result = "fail"
	}
}

func osVersion() string {
	var out []byte
	switch runtime.GOOS {
	case "darwin":
		out, _ = exec.Command("sw_vers", "-productVersion").Output()
		return "macOS " + strings.TrimSpace(string(out))
	case "linux":
		b, _ := os.ReadFile("/etc/os-release")
		if m := regexp.MustCompile(`(?m)^PRETTY_NAME="?([^"\n]*)`).FindSubmatch(b); m != nil {
			return string(m[1])
		}
	case "windows":
		out, _ = exec.Command("cmd", "/c", "ver").Output()
		return strings.TrimSpace(string(out))
	}
	return "unknown"
}

var compatLine = regexp.MustCompile(`^(read-only|host-state|catalog) characterization: ([0-9]+) cases match goldens$`)

const fleetCutoverCompatLine = "fleet cutover characterization: all refusals and read-only checks passed"

// readCompat embeds the F01 harness pass lines. All four suites (read-only,
// host-state, catalog and fleet cutover) must report their full case counts;
// anything else fails the gate.
func readCompat(path, tgt string) Compat {
	if !strings.HasPrefix(tgt, "darwin/") {
		return Compat{Status: "not applicable: legacy Bash routes are macOS-only under compatibility B"}
	}
	b, err := os.ReadFile(path)
	if path == "" || err != nil {
		return Compat{Status: "missing"}
	}
	want := map[string]bool{"read-only": true, "host-state": true, "catalog": true, "fleet-cutover": true}
	c := Compat{Status: "pass"}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		line = strings.TrimSpace(line)
		if line == fleetCutoverCompatLine {
			c.Results = append(c.Results, line)
			delete(want, "fleet-cutover")
			continue
		}
		m := compatLine.FindStringSubmatch(line)
		if m == nil {
			c.Status = "fail"
			c.Results = append(c.Results, line)
			continue
		}
		c.Results = append(c.Results, line)
		switch m[1] {
		case "read-only":
			if m[2] == "17" {
				delete(want, "read-only")
			}
		case "host-state":
			if m[2] == "18" || m[2] == "20" {
				delete(want, "host-state")
			}
		case "catalog":
			if m[2] == "33" {
				delete(want, "catalog")
			}
		}
	}
	if len(want) > 0 {
		c.Status = "fail"
	}
	return c
}

// aggregateCmd checks the five evidence records belong together: one per
// first-release target, the same source revision, every walkthrough passed,
// and the platform-independent steps produced identical bytes everywhere.
func aggregateCmd(args []string) error {
	fl := flag.NewFlagSet("aggregate", flag.ContinueOnError)
	rev := fl.String("source-revision", "", "source revision every record must carry")
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *rev == "" {
		return errors.New("aggregate needs -source-revision")
	}
	var problems []string
	byTarget := map[string]Evidence{}
	for _, p := range fl.Args() {
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var ev Evidence
		if err := json.Unmarshal(b, &ev); err != nil {
			return fmt.Errorf("%s: %v", p, err)
		}
		if _, dup := byTarget[ev.Target]; dup {
			problems = append(problems, "two records for "+ev.Target)
		}
		byTarget[ev.Target] = ev
		if ev.SourceRevision != *rev {
			problems = append(problems, fmt.Sprintf("%s ran %q, not %s", ev.Target, ev.SourceRevision, *rev))
		}
		if ev.Result != "pass" || ev.Probe.Platform != ev.Target || ev.Probe.Emulated || ev.Runtime != ev.Target {
			problems = append(problems, ev.Target+" did not pass natively")
		}
	}
	var portable map[string]string
	var ref string
	for _, t := range target.All() {
		ev, ok := byTarget[string(t)]
		if !ok {
			problems = append(problems, "no evidence for "+string(t))
			continue
		}
		steps := map[string]string{}
		for _, s := range ev.Steps {
			if s.Portable {
				steps[s.Name+"/"+s.Mode+"/"+strings.Join(s.Args, " ")] = s.StdoutSHA256
			}
		}
		if portable == nil {
			portable, ref = steps, string(t)
			continue
		}
		for k, v := range portable {
			if steps[k] != v {
				problems = append(problems, fmt.Sprintf("%s differs from %s at %s", t, ref, k))
			}
		}
	}
	if len(byTarget) != len(target.All()) {
		problems = append(problems, fmt.Sprintf("%d records, want %d", len(byTarget), len(target.All())))
	}
	sort.Strings(problems)
	fmt.Printf("| target | OS | steps | binary sha256 | result |\n|---|---|---|---|---|\n")
	for _, t := range target.All() {
		if ev, ok := byTarget[string(t)]; ok {
			fmt.Printf("| %s | %s | %d | `%.12s` | %s |\n", t, ev.OSVersion, len(ev.Steps), ev.BinarySHA256, ev.Result)
		}
	}
	fmt.Printf("\nsource revision `%s`; %d platform-independent outputs identical across targets\n", *rev, len(portable))
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Println("FAIL:", p)
		}
		return errors.New("aggregate failed")
	}
	return nil
}

// argumentBoundary passes one argument with spaces, quotes and non-ASCII
// text and checks it reaches the core intact as a single argument (T04).
func (h *harness) argumentBoundary() {
	arg := `no such "ü 日本" category`
	args := []string{"software", "--available", "--category", arg}
	code, o, e := h.exec(args...)
	problem := ""
	var d struct{ Diagnostic, Message string }
	switch {
	case code != 2 || len(o) != 0:
		problem = fmt.Sprintf("exit %d, stdout %q", code, o)
	case json.Unmarshal(e, &d) != nil || d.Diagnostic != "MM_SOFTWARE_UNKNOWN_CATEGORY":
		problem = "unexpected stderr " + string(e)
	case !strings.Contains(d.Message, strconv.Quote(arg)[1:len(strconv.Quote(arg))-1]) && !strings.Contains(d.Message, arg):
		problem = "the argument did not arrive intact: " + d.Message
	}
	h.record("argument boundary", "default", args, code, o, e, 0, true, problem)
}

// caseSensitiveSource names the local source with different letter case.
// Linux paths are case-sensitive, so the source must not be found there
// (T02); macOS and Windows default volumes fold case and are not checked.
func (h *harness) caseSensitiveSource() {
	if !strings.HasPrefix(h.ev.Target, "linux/") {
		return
	}
	env := make([]string, 0, len(h.env))
	for _, kv := range h.env {
		if strings.HasPrefix(kv, "MANAGED_MACHINE_SOURCE_DIR=") {
			kv = strings.Replace(kv, definitionsDir, strings.ToUpper(definitionsDir), 1)
		}
		env = append(env, kv)
	}
	args := []string{"software", "--available"}
	code, o, e := h.execEnv(env, args...)
	problem := ""
	if code != 2 || !strings.HasPrefix(string(e), `{"diagnostic":"MM_SOFTWARE_SOURCE_INVALID"`) {
		problem = fmt.Sprintf("a case-variant source path was accepted: exit %d, stderr %s", code, e)
	}
	h.record("case-sensitive source path", "default", []string{"MANAGED_MACHINE_SOURCE_DIR=<case variant>", "software", "--available"}, code, o, e, 0, false, problem)
}
