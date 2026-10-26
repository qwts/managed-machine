package skill

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/target"
)

var update = flag.Bool("update", false, "rewrite testdata goldens")

func app(open func(target.Target) (Store, error)) (*cli.App, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return &cli.App{Name: "managed-machine", Stdout: &out, Stderr: &errb,
		Detect:   func() (target.Host, error) { return target.Host{Target: target.LinuxAMD64}, nil },
		Commands: []cli.Command{Command(open)}}, &out, &errb
}

func invoke(t *testing.T, st Store, args ...string) (string, string, int) {
	t.Helper()
	a, out, errb := app(func(target.Target) (Store, error) { return st, nil })
	code := a.Run(append([]string{"skill"}, args...))
	return out.String(), errb.String(), code
}

func core(t *testing.T) Store {
	t.Helper()
	b, err := Core()
	if err != nil {
		t.Fatal(err)
	}
	return Store{Bundles: []Bundle{b}}
}

// bundleFS builds a bundle with one skill per entry of bodies (kind
// procedure) plus a summary for each.
func bundleFS(name, version string, caveat string, bodies map[string]string) fstest.MapFS {
	var y strings.Builder
	fmt.Fprintf(&y, "schema_version: 1\nname: %s\nversion: %s\nskills:\n", name, version)
	fs := fstest.MapFS{}
	for skill, body := range bodies {
		fmt.Fprintf(&y, "  - name: %s\n    kinds: [summary, procedure]\n", skill)
		if caveat != "" {
			fmt.Fprintf(&y, "    caveats: [%q]\n", caveat)
		}
		fs["skills/"+skill+"/summary.md"] = &fstest.MapFile{Data: []byte("Summary of " + skill + ".\n")}
		fs["skills/"+skill+"/procedure.md"] = &fstest.MapFile{Data: []byte(body)}
	}
	fs[BundleFile] = &fstest.MapFile{Data: []byte(y.String())}
	return fs
}

func longBody(paragraphs int) string {
	var b strings.Builder
	for i := 1; i <= paragraphs; i++ {
		fmt.Fprintf(&b, "%d. Step %d explains one action in plain words, with \"quotes\", <angles> & ampersands, and ünïcödé text.\n\n", i, i)
	}
	return b.String()
}

func mustLoad(t *testing.T, fs fstest.MapFS) Bundle {
	t.Helper()
	b, err := Load(fs, "test")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Every --next page advances, stays within the bound in both modes,
// carries provenance, and the last one has no next; the pages reassemble
// the exact item, caveats first.
func TestChunkingAdvancesAndTerminates(t *testing.T) {
	body := longBody(120)
	b := mustLoad(t, bundleFS("long", "2.0.0", "Read the caveat first.", map[string]string{"big": body}))
	st := Store{Bundles: []Bundle{b}}
	var got strings.Builder
	args := []string{"big", "--kind", "procedure"}
	pages := 0
	for {
		out, errOut, code := invoke(t, st, args...)
		if code != 0 {
			t.Fatalf("page %d: exit %d %s", pages+1, code, errOut)
		}
		if len(out) > MaxResponseBytes {
			t.Fatalf("page %d is %d bytes", pages+1, len(out))
		}
		jsonOut, _, _ := invoke(t, st, append(args, "--json")...)
		if len(jsonOut) > MaxResponseBytes || !json.Valid([]byte(jsonOut)) {
			t.Fatalf("page %d --json: %d bytes, valid %v", pages+1, len(jsonOut), json.Valid([]byte(jsonOut)))
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(out), &rec); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"bundle", "version", "revision", "tokens_est"} {
			if _, ok := rec[k]; !ok {
				t.Fatalf("page %d lacks %s", pages+1, k)
			}
		}
		got.WriteString(rec["content"].(string))
		pages++
		next, ok := rec["next"].(string)
		if !ok {
			break
		}
		if pages > 100 {
			t.Fatal("cursor does not terminate")
		}
		args = []string{"--next", next}
	}
	want := "Caveats:\n- Read the caveat first.\n\n" + strings.TrimRight(body, "\n")
	if got.String() != want {
		t.Errorf("pages do not reassemble the item (%d vs %d bytes)", got.Len(), len(want))
	}
	if pages < 3 {
		t.Errorf("only %d pages for %d bytes", pages, len(body))
	}
}

func TestOversizedItem(t *testing.T) {
	b := mustLoad(t, bundleFS("huge", "1.0.0", "", map[string]string{"huge": strings.Repeat("x", MaxItemBytes+1)}))
	_, errOut, code := invoke(t, Store{Bundles: []Bundle{b}}, "huge", "--kind", "procedure")
	if code != 1 || !strings.Contains(errOut, "MM_SKILL_CONTENT_OVERSIZED") || !strings.Contains(errOut, "out of band") {
		t.Errorf("exit %d %s", code, errOut)
	}
}

func TestCaveatsThatCannotFitAreRejectedAtLoad(t *testing.T) {
	_, err := Load(bundleFS("cav", "1.0.0", strings.Repeat("caveat ", 700), map[string]string{"x": "body\n"}), "test")
	if err == nil || !strings.Contains(err.Error(), "mandatory caveats need") {
		t.Errorf("got %v", err)
	}
}

func TestSearchPagesAndCursorsBind(t *testing.T) {
	bodies := map[string]string{}
	for i := 0; i < 60; i++ {
		bodies[fmt.Sprintf("topic-%02d", i)] = "Do it.\n"
	}
	b := mustLoad(t, bundleFS("many", "1.0.0", "", bodies))
	st := Store{Bundles: []Bundle{b}}
	out, _, code := invoke(t, st, "--search", "TOPIC", "--kind", "summary")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if code != 0 || len(out) > MaxResponseBytes {
		t.Fatalf("exit %d, %d bytes", code, len(out))
	}
	var last map[string]any
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	next, ok := last["next"].(string)
	if !ok || last["content"] != nil {
		t.Fatalf("no continuation record: %s", lines[len(lines)-1])
	}
	for _, l := range lines[:len(lines)-1] {
		if strings.Contains(l, `"content"`) || !strings.Contains(l, `"pointer"`) {
			t.Fatalf("search returned a non-pointer record: %s", l)
		}
	}
	seen := len(lines) - 1
	for i := 0; i < 20 && next != ""; i++ {
		out, errOut, code := invoke(t, st, "--search", "TOPIC", "--kind", "summary", "--next", next)
		if code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		lines = strings.Split(strings.TrimRight(out, "\n"), "\n")
		last = map[string]any{}
		json.Unmarshal([]byte(lines[len(lines)-1]), &last)
		next, _ = last["next"].(string)
		if next != "" {
			seen += len(lines) - 1
		} else {
			seen += len(lines)
		}
	}
	if seen != 60 {
		t.Errorf("search pages returned %d hits, want 60", seen)
	}
	firstNext := firstCursor(t, st, "--search", "TOPIC", "--kind", "summary")
	for name, args := range map[string][]string{
		"different query":  {"--search", "topic-0", "--kind", "summary", "--next", firstNext},
		"different filter": {"--search", "TOPIC", "--kind", "procedure", "--next", firstNext},
		"listing cursor":   {"--next", firstNext},
	} {
		if _, errOut, code := invoke(t, st, args...); code != 2 || !strings.Contains(errOut, "MM_SKILL_CURSOR_MISMATCH") {
			t.Errorf("%s: exit %d %s", name, code, errOut)
		}
	}
	// A changed snapshot (new revision) invalidates the cursor.
	changed := bundleFS("many", "1.0.0", "", bodies)
	changed["skills/topic-00/procedure.md"] = &fstest.MapFile{Data: []byte("Do it differently.\n")}
	if _, errOut, code := invoke(t, Store{Bundles: []Bundle{mustLoad(t, changed)}}, "--search", "TOPIC", "--kind", "summary", "--next", firstNext); code != 2 || !strings.Contains(errOut, "MM_SKILL_CURSOR_MISMATCH") {
		t.Errorf("changed snapshot: exit %d %s", code, errOut)
	}
}

func mustRun(t *testing.T, st Store, args ...string) string {
	out, errOut, code := invoke(t, st, args...)
	if code != 0 {
		t.Fatalf("%v: exit %d %s", args, code, errOut)
	}
	return out
}

func firstCursor(t *testing.T, st Store, args ...string) string {
	out := mustRun(t, st, args...)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var last map[string]any
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	return last["next"].(string)
}

func TestDiagnostics(t *testing.T) {
	st := core(t)
	b := st.Bundles[0]
	stale := fmt.Sprintf("core:software-install:procedure@%s:000000000000#2", b.Version)
	for _, c := range []struct {
		id   string
		code int
		args []string
	}{
		{"MM_SKILL_UNKNOWN_NAME", 2, []string{"nothing-here"}},
		{"MM_SKILL_UNKNOWN_KIND", 2, []string{"software-install", "--kind", "tutorial"}},
		{"MM_SKILL_UNKNOWN_BUNDLE", 2, []string{"--bundle", "cicd"}},
		{"MM_SKILL_UNKNOWN_VERSION", 2, []string{"--version", "9.9.9"}},
		{"MM_SKILL_UNKNOWN_HANDLE", 2, []string{"--next", "garbage"}},
		{"MM_SKILL_UNKNOWN_HANDLE", 2, []string{"--next", stale}},
		{"MM_SKILL_UNKNOWN_HANDLE", 2, []string{"--next", "core:software-install:procedure@1.0.0:" + b.Revision + "#9"}},
		{"MM_SKILL_UNSUPPORTED_KIND", 2, []string{"provider:brew", "--kind", "procedure"}},
		{"MM_SKILL_CURSOR_MISMATCH", 2, []string{"--search", "x", "--next", "list@0123456789abcdef#2"}},
		{"MM_USAGE", 2, []string{"software-install", "--search", "x"}},
		{"MM_USAGE", 2, []string{"--json", "--human"}},
	} {
		_, errOut, code := invoke(t, st, c.args...)
		if code != c.code || !strings.Contains(errOut, `"diagnostic":"`+c.id+`"`) {
			t.Errorf("%v: exit %d %s, want %s", c.args, code, errOut, c.id)
		}
	}
	a, _, errb := app(func(target.Target) (Store, error) { return Store{}, errors.New("bundle core: unreadable") })
	if code := a.Run([]string{"skill"}); code != 1 || !strings.Contains(errb.String(), "MM_SKILL_BUNDLE_UNREADABLE") {
		t.Errorf("unreadable: exit %d %s", code, errb.String())
	}
}

// The core bundle's guidance identifies installation as unavailable and
// never claims a read authorizes action, in the first chunk of every kind.
func TestCoreCaveatsReport(t *testing.T) {
	b := core(t).Bundles[0]
	var rep strings.Builder
	fmt.Fprintf(&rep, "core %s: response bound %d bytes, item bound %d bytes\n", b.Version, MaxResponseBytes, MaxItemBytes)
	for _, s := range b.Skills {
		for _, k := range s.Kinds {
			cs, err := chunks(b, s, k)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&rep, "%s %s: caveat floor %d bytes, %d chunk(s), %d content bytes\n", s.Name, k, firstChunkFloor(b, s, k), len(cs), len(s.Content[k]))
			if s.Name == "software-install" {
				if !strings.Contains(cs[0], "MM_SOFTWARE_INSTALL_UNAVAILABLE") || !strings.Contains(cs[0], "does not authorize any action") {
					t.Errorf("%s %s: first chunk lacks the mandatory caveats", s.Name, k)
				}
			}
		}
	}
	golden(t, "caveats-report.txt", rep.String())
}

func TestGoldens(t *testing.T) {
	st := core(t)
	for name, args := range map[string][]string{
		"list":            {},
		"list-json":       {"--json"},
		"list-human":      {"--human"},
		"summary":         {"software-install"},
		"procedure-human": {"software-install", "--kind", "procedure", "--human"},
		"reference":       {"target:custom", "--kind", "reference"},
		"search":          {"--search", "install"},
		"help":            {"-h"},
		"help-reference":  {"-h", "--kind", "reference", "software-install"},
	} {
		out, errOut, code := invoke(t, st, args...)
		golden(t, name+".golden", fmt.Sprintf("exit %d\n--- stdout\n%s--- stderr\n%s", code, out, errOut))
	}
}

func golden(t *testing.T, name, got string) {
	t.Helper()
	p := filepath.Join("testdata", name)
	if *update {
		os.MkdirAll("testdata", 0o755)
		os.WriteFile(p, []byte(got), 0o644)
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("%s differs:\n%s", p, got)
	}
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// writeExtension lays out an installed extension that provides provider
// cicd and a skills bundle, pinned for every target. Its executable writes
// a marker if it is ever run.
func writeExtension(t *testing.T, dir string, tamper bool) {
	t.Helper()
	root := filepath.Join(dir, "cicd-tools")
	files := map[string]string{
		"bin/mm-cicd":              "#!/bin/sh\n: > \"${MM_FIXTURE_MARKER:-/dev/null}\"\n",
		"bundles/cicd/bundle.yaml": "schema_version: 1\nname: cicd\nversion: 0.2.0\nskills:\n  - name: cicd-pipelines\n    kinds: [summary]\n    caveats: [\"Guidance only; this extension runs nothing when read.\"]\n",
		"bundles/cicd/skills/cicd-pipelines/summary.md": "Describe the cicd provider's pipelines.\n",
	}
	var assets, exes strings.Builder
	for p, c := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(c), 0o644)
		if strings.HasPrefix(p, "bundles/") {
			fmt.Fprintf(&assets, "    - path: %s\n      sha256: %s\n", p, sum([]byte(c)))
		}
	}
	for _, tg := range target.All() {
		fmt.Fprintf(&exes, "  - platform: %s\n    path: bin/mm-cicd\n    sha256: %s\n", tg, sum([]byte(files["bin/mm-cicd"])))
	}
	manifest := "schema_version: 1\nname: cicd-tools\nversion: 0.2.0\nprotocol: mm-extension-1\ncapabilities: [provider, skills]\nexecutables:\n" + exes.String() +
		"provides:\n  providers:\n    - name: cicd\n  skill_bundles:\n    - id: cicd\n      covers: [cicd]\n  assets:\n" + assets.String()
	os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte(manifest), 0o644)
	if tamper {
		os.WriteFile(filepath.Join(root, "bundles/cicd/skills/cicd-pipelines/summary.md"), []byte("Changed after pinning.\n"), 0o644)
	}
}

func TestExtensionBundle(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	t.Setenv("MM_FIXTURE_MARKER", marker)
	writeExtension(t, dir, false)
	st, err := Open(func(k string) string { return map[string]string{ExtensionsDirEnv: dir}[k] }, target.LinuxAMD64)
	if err != nil {
		t.Fatal(err)
	}
	out := mustRun(t, st, "cicd-pipelines", "--bundle", "cicd")
	if !strings.Contains(out, `"bundle":"cicd","version":"0.2.0"`) || !strings.Contains(out, "runs nothing when read") {
		t.Errorf("extension skill: %s", out)
	}
	if h := mustRun(t, st, "-h"); !strings.Contains(h, "extension:cicd-tools@0.2.0") {
		t.Errorf("help does not show the extension bundle source:\n%s", h)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("serving an extension bundle ran the extension")
	}
	tampered := t.TempDir()
	writeExtension(t, tampered, true)
	if _, err := Open(func(string) string { return tampered }, target.LinuxAMD64); err == nil || !strings.Contains(err.Error(), "digest") && !strings.Contains(err.Error(), "sha256") {
		t.Errorf("tampered bundle served: %v", err)
	}
}

func TestReservedLegacyName(t *testing.T) {
	fs := bundleFS("x", "1.0.0", "", map[string]string{"path": "p\n"})
	if _, err := Load(fs, "test"); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Errorf("a bundle may not shadow `skill path`: %v", err)
	}
}

func TestSkillPath(t *testing.T) {
	rawBase := t.TempDir()
	base, err := filepath.EvalSymlinks(rawBase)
	if err != nil {
		base = rawBase
	}

	failingOpen := func(target.Target) (Store, error) {
		t.Fatal("skill path must not open the skill store")
		return Store{}, nil
	}
	failingDetect := func() (target.Host, error) {
		t.Fatal("skill path must not run the host probe")
		return target.Host{}, nil
	}

	// 1. MANAGED_MACHINE_BUNDLE_ROOT with valid 40-hex RELEASE_COMMIT across compact, --json, --human.
	overrideRoot := filepath.Join(base, "override-bundle")
	bundleDir := filepath.Join(overrideRoot, "skills", "managed-machine")
	skillFile := filepath.Join(bundleDir, "SKILL.md")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillFile, []byte("# managed-machine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	const wantCommit = "0123456789abcdef0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(overrideRoot, "RELEASE_COMMIT"), []byte(wantCommit+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	runWith := func(env map[string]string, exe string, args ...string) (string, string, int) {
		var out, errb bytes.Buffer
		a := &cli.App{
			Name:     "managed-machine",
			Stdout:   &out,
			Stderr:   &errb,
			Detect:   failingDetect,
			Getenv:   func(k string) string { return env[k] },
			Commands: []cli.Command{Command(failingOpen)},
		}
		if exe != "" {
			a.Executable = func() (string, error) { return exe, nil }
		}
		code := a.Run(append([]string{"skill"}, args...))
		return out.String(), errb.String(), code
	}

	env := map[string]string{BundleRootEnv: overrideRoot}
	out, errOut, code := runWith(env, "", "path")
	if code != 0 || errOut != "" {
		t.Fatalf("compact: exit=%d stderr=%q", code, errOut)
	}
	wantCompact := fmt.Sprintf(`{"command":"skill-path","name":"managed-machine","path":%q,"skill_file":%q,"commit":%q}`+"\n",
		bundleDir, skillFile, wantCommit)
	if out != wantCompact {
		t.Errorf("compact:\ngot:  %s\nwant: %s", out, wantCompact)
	}

	jOut, jErr, jCode := runWith(env, "", "path", "--json")
	if jCode != 0 || jErr != "" {
		t.Fatalf("--json: exit=%d stderr=%q", jCode, jErr)
	}
	var parsed map[string]string
	if err := json.Unmarshal([]byte(jOut), &parsed); err != nil {
		t.Fatalf("--json unmarshal: %v (%s)", err, jOut)
	}
	if parsed["command"] != "skill-path" || parsed["name"] != "managed-machine" ||
		parsed["path"] != bundleDir || parsed["skill_file"] != skillFile || parsed["commit"] != wantCommit {
		t.Errorf("--json fields = %+v", parsed)
	}

	hOut, hErr, hCode := runWith(env, "", "path", "--human")
	if hCode != 0 || hErr != "" {
		t.Fatalf("--human: exit=%d stderr=%q", hCode, hErr)
	}
	wantHuman := fmt.Sprintf("%s\ncommit %s\n", bundleDir, wantCommit)
	if hOut != wantHuman {
		t.Errorf("--human:\ngot:  %q\nwant: %q", hOut, wantHuman)
	}

	// 2. Default <directory of symlink-resolved executable>/../libexec resolution,
	// with missing or non-40-hex RELEASE_COMMIT falling back to "unknown".
	cellarBinDir := filepath.Join(base, "Cellar", "managed-machine", "0.11.0", "bin")
	cellarLibexec := filepath.Join(base, "Cellar", "managed-machine", "0.11.0", "libexec")
	cellarBundleDir := filepath.Join(cellarLibexec, "skills", "managed-machine")
	cellarSkillFile := filepath.Join(cellarBundleDir, "SKILL.md")
	if err := os.MkdirAll(cellarBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cellarBundleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realExe := filepath.Join(cellarBinDir, "managed-machine")
	if err := os.WriteFile(realExe, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cellarSkillFile, []byte("# skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	optBinDir := filepath.Join(base, "bin")
	if err := os.MkdirAll(optBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkExe := filepath.Join(optBinDir, "managed-machine")
	if err := os.Symlink(realExe, symlinkExe); err != nil {
		t.Fatal(err)
	}

	// Missing RELEASE_COMMIT -> "unknown"
	sOut, sErr, sCode := runWith(nil, symlinkExe, "path", "--human")
	if sCode != 0 || sErr != "" {
		t.Fatalf("symlink missing RELEASE_COMMIT: exit=%d stderr=%q", sCode, sErr)
	}
	if want := fmt.Sprintf("%s\ncommit unknown\n", cellarBundleDir); sOut != want {
		t.Errorf("symlink missing RELEASE_COMMIT:\ngot:  %q\nwant: %q", sOut, want)
	}

	// Non-40-hex RELEASE_COMMIT -> "unknown"
	for _, badCommit := range []string{"not-a-sha\n", "0123456789abcdef\n", "0123456789ABCDEF0123456789ABCDEF01234567\n"} {
		if err := os.WriteFile(filepath.Join(cellarLibexec, "RELEASE_COMMIT"), []byte(badCommit), 0o644); err != nil {
			t.Fatal(err)
		}
		bOut, bErr, bCode := runWith(nil, symlinkExe, "path", "--human")
		if bCode != 0 || bErr != "" || !strings.HasSuffix(bOut, "\ncommit unknown\n") {
			t.Errorf("bad RELEASE_COMMIT %q: exit=%d out=%q err=%q", badCommit, bCode, bOut, bErr)
		}
	}

	// 3. Missing <bundle dir>/SKILL.md -> MM_SKILL_BUNDLE_MISSING (exit 1).
	emptyRoot := filepath.Join(base, "empty-bundle")
	mOut, mErr, mCode := runWith(map[string]string{BundleRootEnv: emptyRoot}, "", "path")
	if mCode != 1 || mOut != "" || !strings.Contains(mErr, `"diagnostic":"MM_SKILL_BUNDLE_MISSING"`) {
		t.Errorf("missing bundle: exit=%d stdout=%q stderr=%q, want exit 1 with MM_SKILL_BUNDLE_MISSING", mCode, mOut, mErr)
	}

	// 4. Extra positional or flag after path (args[0] == "path") -> MM_USAGE (exit 2).
	for _, badArgs := range [][]string{
		{"path", "extra"},
		{"path", "--kind", "summary"},
		{"path", "--bundle", "core"},
		{"path", "--search", "x"},
		{"path", "--version", "1.0.0"},
		{"path", "--next", "h"},
		{"path", "--bogus"},
	} {
		uOut, uErr, uCode := runWith(env, "", badArgs...)
		if uCode != 2 || uOut != "" || !strings.Contains(uErr, `"diagnostic":"MM_USAGE"`) {
			t.Errorf("skill %v: exit=%d stdout=%q stderr=%q, want exit 2 with MM_USAGE", badArgs, uCode, uOut, uErr)
		}
	}

	// 5. path in any position other than args[0] == "path" (e.g. skill --json path,
	// skill --kind x path) preserves the pre-change MM_LEGACY_ROUTE_UNSUPPORTED (exit 2).
	for _, legacyArgs := range [][]string{
		{"--json", "path"},
		{"--kind", "x", "path"},
		{"--kind", "summary", "path"},
		{"--bundle", "core", "path"},
		{"--version", "1.0.0", "path"},
	} {
		lOut, lErr, lCode := runWith(env, "", legacyArgs...)
		if lCode != 2 || lOut != "" || !strings.Contains(lErr, `"diagnostic":"MM_LEGACY_ROUTE_UNSUPPORTED"`) {
			t.Errorf("skill %v: exit=%d stdout=%q stderr=%q, want exit 2 with MM_LEGACY_ROUTE_UNSUPPORTED", legacyArgs, lCode, lOut, lErr)
		}
	}
}
