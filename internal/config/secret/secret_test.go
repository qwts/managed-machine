package secret

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestEnvVarMappingAndResolve(t *testing.T) {
	cases := []struct {
		name    string
		refName string
		wantEnv string
		val     string
	}{
		{"plain lowercase", "token", "MM_SECRET_TOKEN", "sec-plain-1"},
		{"hyphen", "api-key", "MM_SECRET_API_KEY", "sec-hyphen-2"},
		{"slash", "work/github/token", "MM_SECRET_WORK_GITHUB_TOKEN", "sec-slash-3"},
		{"dot", "registry.npmjs.org", "MM_SECRET_REGISTRY_NPMJS_ORG", "sec-dot-4"},
		{"combined separators and digits", "org/service-v2.api_key", "MM_SECRET_ORG_SERVICE_V2_API_KEY", "sec-combo-5"},
		{"already uppercase", "PATH", "MM_SECRET_PATH", "sec-path-6"},
		{"other punctuation", "db:prod@us-east+1", "MM_SECRET_DB_PROD_US_EAST_1", "sec-punct-7"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EnvVar(c.refName); got != c.wantEnv {
				t.Fatalf("EnvVar(%q) = %q, want %q", c.refName, got, c.wantEnv)
			}
			// Even when PATH and the raw refName are set in the environment,
			// only MM_SECRET_<NAME> is consulted.
			env := map[string]string{
				"PATH":    "/usr/bin:/bin",
				c.refName: "wrong-unprefixed-value",
				c.wantEnv: c.val,
			}
			var lookedUp []string
			r := NewResolver(func(k string) string {
				lookedUp = append(lookedUp, k)
				return env[k]
			})
			got, err := r.Resolve(context.Background(), c.refName)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.refName, err)
			}
			if got != c.val {
				t.Errorf("Resolve(%q) = %q, want %q", c.refName, got, c.val)
			}
			if len(lookedUp) != 1 || lookedUp[0] != c.wantEnv {
				t.Errorf("looked up %v, want [%q]", lookedUp, c.wantEnv)
			}
		})
	}
}

func TestSecretPathDoesNotReadProcessVar(t *testing.T) {
	env := map[string]string{
		"PATH": "/usr/local/bin:/usr/bin",
		"HOME": "/Users/alice",
	}
	r := NewResolver(func(k string) string { return env[k] })
	for _, name := range []string{"PATH", "HOME", " editor "} {
		_, err := r.Resolve(context.Background(), name)
		if err == nil {
			t.Fatalf("Resolve(%q) succeeded without MM_SECRET_*", name)
		}
		var ue *UnresolvedError
		if !errors.As(err, &ue) {
			t.Fatalf("Resolve(%q) error = %T (%v), want *UnresolvedError", name, err, err)
		}
		if strings.Contains(err.Error(), "/usr") || strings.Contains(err.Error(), "alice") {
			t.Errorf("error leaked environment value: %q", err.Error())
		}
	}
}

func TestResolverOrderingAndFallbackSeam(t *testing.T) {
	const envSecret = "from-env-sentinel"
	const fbSecret = "from-fallback-sentinel"

	fallbackCalls := 0
	r := NewResolver(
		func(k string) string {
			if k == "MM_SECRET_FIRST" {
				return envSecret
			}
			return ""
		},
		func(_ context.Context, name string) (string, bool, error) {
			fallbackCalls++
			if name == "first" || name == "second" {
				return fbSecret, true, nil
			}
			return "", false, nil
		},
	)

	// 1. MM_SECRET_<NAME> wins without invoking the fallback source.
	got, err := r.Resolve(context.Background(), "first")
	if err != nil || got != envSecret {
		t.Fatalf("first: got %q (%v), want %q", got, err, envSecret)
	}
	if fallbackCalls != 0 {
		t.Errorf("fallback called %d times when MM_SECRET_FIRST was set", fallbackCalls)
	}

	// 2. When MM_SECRET_<NAME> is unset, the fallback seam resolves the secret.
	got, err = r.Resolve(context.Background(), "second")
	if err != nil || got != fbSecret {
		t.Fatalf("second: got %q (%v), want %q", got, err, fbSecret)
	}
	if fallbackCalls != 1 {
		t.Errorf("fallback called %d times, want 1", fallbackCalls)
	}

	// 3. When neither resolves, UnresolvedError names only the reference.
	_, err = r.Resolve(context.Background(), "third")
	if err == nil {
		t.Fatal("third: expected error")
	}
	if err.Error() != "secret:third: could not be resolved" {
		t.Errorf("third error = %q", err.Error())
	}

	// 4. Empty reference name is rejected without consulting sources.
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Error("empty name: expected error")
	}
}

func TestDigestAndAsset(t *testing.T) {
	sum := sha256.Sum256([]byte("work/token\x00s3cr3t-val"))
	wantHex := hex.EncodeToString(sum[:])

	if got := Digest("work/token", "s3cr3t-val"); got != wantHex {
		t.Errorf("Digest = %q, want %q", got, wantHex)
	}
	asset := Asset("work/token", "s3cr3t-val")
	if asset.Name != "secret:work/token" || asset.SHA256 != wantHex {
		t.Errorf("Asset = %+v", asset)
	}
	if strings.Contains(asset.Name, "s3cr3t-val") || strings.Contains(asset.SHA256, "s3cr3t-val") {
		t.Errorf("Asset leaked secret value: %+v", asset)
	}
	// Null separator prevents boundary collisions between (name, value) pairs.
	if Digest("ab", "c") == Digest("a", "bc") {
		t.Error("Digest collided across name/value boundary")
	}
	// Different reference names that map to the same MM_SECRET_* variable still have distinct asset digests.
	if Digest("a-b", "v") == Digest("a/b", "v") {
		t.Error("Digest collided across distinct reference names")
	}
}

type passCall struct {
	path string
	args []string
	env  []string
}

type fakePassRunner struct {
	entries map[string]string
	stderr  string
	calls   []passCall
	runErr  error
}

func (f *fakePassRunner) run(_ context.Context, path string, args, env []string) (Result, error) {
	cpArgs := append([]string(nil), args...)
	cpEnv := append([]string(nil), env...)
	f.calls = append(f.calls, passCall{path: path, args: cpArgs, env: cpEnv})
	if f.runErr != nil {
		return Result{Stderr: []byte(f.stderr)}, f.runErr
	}
	if len(args) != 2 || args[0] != "show" {
		return Result{Exit: 1, Stderr: []byte("usage: pass show <name>\n")}, nil
	}
	out, ok := f.entries[args[1]]
	if !ok {
		return Result{
			Exit:   1,
			Stdout: []byte("partial-stdout-on-error-never-use\n"),
			Stderr: []byte(f.stderr),
		}, nil
	}
	return Result{
		Exit:   0,
		Stdout: []byte(out),
		Stderr: []byte(f.stderr),
	}, nil
}

// TestPassRunnerOrderingFirstLineAndFailures proves through a fake
// provider.Runner that:
//   - MM_SECRET_<NAME> wins when both exist without invoking the runner
//     (call count 0).
//   - When MM_SECRET_<NAME> is unset, the resolver runs argv ["show", <name>]
//     with the inherited environment and takes only the first stdout line
//     (both LF and CRLF).
//   - Nonzero exit, empty stdout, or pass missing from PATH falls through to
//     UnresolvedError naming only the reference, and stderr is never echoed.
//   - The recorded asset digest is identical whether the secret value came
//     from the environment or Pass.
func TestPassRunnerOrderingFirstLineAndFailures(t *testing.T) {
	const (
		envVal         = "env-winner-secret-111"
		passShadowVal  = "pass-shadowed-secret-222"
		passFirstLine  = "pass-first-line-secret-333"
		passSecondLine = "username: alice-never-use-444"
		passCRLFLine   = "pass-crlf-secret-555"
		stderrSentinel = "Error: gpg stderr sentinel 666"
		sharedVal      = "shared-digest-secret-777"
	)

	binDir := t.TempDir()
	fakeExe := writePassExecutable(t, binDir, "", "", nil)
	inheritedEnv := []string{
		"PATH=" + binDir,
		"PATHEXT=.COM;.EXE;.BAT;.CMD",
		"PASSWORD_STORE_DIR=" + filepath.Join(binDir, "store"),
	}

	fp := &fakePassRunner{
		entries: map[string]string{
			"work/both":      passShadowVal + "\n",
			"work/multiline": passFirstLine + "\n" + passSecondLine + "\nurl: https://example.com\n",
			"work/crlf":      passCRLFLine + "\r\n" + passSecondLine + "\r\n",
			"work/empty":     "\n" + passSecondLine + "\n",
			"work/shared":    sharedVal + "\n",
		},
		stderr: stderrSentinel + "\n",
	}

	envVars := map[string]string{
		"MM_SECRET_WORK_BOTH": envVal,
	}
	r := NewResolver(
		func(k string) string { return envVars[k] },
		Pass(fp.run, func() []string { return inheritedEnv }),
	)

	// 1. Both env and pass exist: env wins and fake runner call count stays 0.
	got, err := r.Resolve(context.Background(), "work/both")
	if err != nil || got != envVal {
		t.Fatalf("work/both: got %q (%v), want %q", got, err, envVal)
	}
	if len(fp.calls) != 0 {
		t.Fatalf("runner call count = %d when MM_SECRET_WORK_BOTH was set, want 0", len(fp.calls))
	}

	// 2. Env unset: runner is called once with argv ["show", "work/multiline"] and inherited env;
	//    only the first stdout line is returned.
	got, err = r.Resolve(context.Background(), "work/multiline")
	if err != nil || got != passFirstLine {
		t.Fatalf("work/multiline: got %q (%v), want %q", got, err, passFirstLine)
	}
	if len(fp.calls) != 1 {
		t.Fatalf("runner call count = %d, want 1", len(fp.calls))
	}
	if fp.calls[0].path != fakeExe {
		t.Errorf("runner path = %q, want %q", fp.calls[0].path, fakeExe)
	}
	if !reflect.DeepEqual(fp.calls[0].args, []string{"show", "work/multiline"}) {
		t.Errorf("runner args = %v, want [show work/multiline]", fp.calls[0].args)
	}
	if !reflect.DeepEqual(fp.calls[0].env, inheritedEnv) {
		t.Errorf("runner env = %v, want %v", fp.calls[0].env, inheritedEnv)
	}

	// 3. CRLF stdout: trailing \r is stripped from the first line.
	got, err = r.Resolve(context.Background(), "work/crlf")
	if err != nil || got != passCRLFLine {
		t.Fatalf("work/crlf: got %q (%v), want %q", got, err, passCRLFLine)
	}

	// 4. Nonzero exit from pass: falls through to UnresolvedError and never echoes stderr or partial stdout.
	_, err = r.Resolve(context.Background(), "work/missing")
	if err == nil {
		t.Fatal("work/missing: expected error")
	}
	var ue *UnresolvedError
	if !errors.As(err, &ue) || ue.Name != "work/missing" {
		t.Fatalf("work/missing error = %T (%v), want *UnresolvedError{Name: work/missing}", err, err)
	}
	if err.Error() != "secret:work/missing: could not be resolved" {
		t.Errorf("work/missing error = %q", err.Error())
	}
	if strings.Contains(err.Error(), stderrSentinel) || strings.Contains(err.Error(), "partial-stdout") {
		t.Errorf("error leaked pass output: %q", err.Error())
	}

	// 5. Empty first stdout line from pass is treated as unresolved.
	if _, err := r.Resolve(context.Background(), "work/empty"); err == nil {
		t.Fatal("work/empty: expected error for empty first line")
	}

	// 6. pass missing from PATH (empty directory on PATH): runner is not invoked, returns UnresolvedError.
	prevCalls := len(fp.calls)
	emptyBin := t.TempDir()
	missingOnPath := NewResolver(
		func(string) string { return "" },
		Pass(fp.run, func() []string { return []string{"PATH=" + emptyBin, "PATHEXT=.COM;.EXE;.BAT;.CMD"} }),
	)
	if _, err := missingOnPath.Resolve(context.Background(), "work/multiline"); err == nil {
		t.Fatal("missing pass on PATH: expected error")
	}
	if len(fp.calls) != prevCalls {
		t.Errorf("runner was called when pass was absent from PATH: %d -> %d", prevCalls, len(fp.calls))
	}

	// 7. LookPath returning ErrNotFound or Runner returning an exec error also falls through cleanly.
	missingLookPath := Resolver{
		Getenv:   func(string) string { return "" },
		Run:      fp.run,
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
	}
	if _, err := missingLookPath.Resolve(context.Background(), "work/multiline"); err == nil {
		t.Fatal("LookPath ErrNotFound: expected error")
	}
	fp.runErr = errors.New("runner failed sentinel")
	if _, err := r.Resolve(context.Background(), "work/multiline"); err == nil || strings.Contains(err.Error(), "runner failed") {
		t.Fatalf("runner error: got %v, want clean UnresolvedError", err)
	}
	fp.runErr = nil

	// 8. Asset digest is identical whether the secret resolved from MM_SECRET_<NAME> or Pass.
	valFromPass, err := r.Resolve(context.Background(), "work/shared")
	if err != nil {
		t.Fatalf("resolve work/shared from pass: %v", err)
	}
	envVars["MM_SECRET_WORK_SHARED"] = sharedVal
	valFromEnv, err := r.Resolve(context.Background(), "work/shared")
	if err != nil {
		t.Fatalf("resolve work/shared from env: %v", err)
	}
	if Asset("work/shared", valFromEnv) != Asset("work/shared", valFromPass) {
		t.Errorf("asset mismatch: env=%+v pass=%+v", Asset("work/shared", valFromEnv), Asset("work/shared", valFromPass))
	}
}

// TestPassFixtureScriptOnInjectedPath places a fixture pass script (.cmd on
// Windows) first on an injected PATH and exercises the three cases natively
// through the real Exec runner without touching the real pass.
func TestPassFixtureScriptOnInjectedPath(t *testing.T) {
	const (
		passOnlyVal    = "fixture-pass-only-secret-801"
		passSecondLine = "second-line-metadata-802"
		envOnlyVal     = "fixture-env-only-secret-803"
		envWinnerVal   = "fixture-env-winner-secret-804"
		passShadowVal  = "fixture-pass-shadowed-805"
		stderrSentinel = "fixture-pass-stderr-sentinel-806"
	)

	firstBin := filepath.Join(t.TempDir(), "first-bin")
	secondBin := filepath.Join(t.TempDir(), "second-bin")
	logFile := filepath.Join(t.TempDir(), "pass-calls.log")

	// Write the fixture pass script in firstBin and a failing shadow script in secondBin.
	writePassExecutable(t, firstBin, logFile, stderrSentinel, map[string]string{
		"work/pass-only": passOnlyVal + "\n" + passSecondLine,
		"work/both":      passShadowVal + "\n" + passSecondLine,
	})
	writePassExecutable(t, secondBin, "", "shadow-pass-should-not-run", nil)

	envMap := map[string]string{
		"PATH":                firstBin + string(os.PathListSeparator) + secondBin,
		"PATHEXT":             ".COM;.EXE;.BAT;.CMD",
		"MM_SECRET_WORK_ENV":  envOnlyVal,
		"MM_SECRET_WORK_BOTH": envWinnerVal,
	}
	environ := func() []string {
		env := []string{
			"PATH=" + envMap["PATH"],
			"PATHEXT=" + envMap["PATHEXT"],
		}
		if runtime.GOOS == "windows" {
			for _, k := range []string{"SystemRoot", "windir", "ComSpec"} {
				if v := os.Getenv(k); v != "" {
					env = append(env, k+"="+v)
				}
			}
		}
		return env
	}

	r := NewResolver(func(k string) string { return envMap[k] }, Pass(nil, environ))

	// Case 1: pass-only name resolves from the fixture pass script (first stdout line only).
	got, err := r.Resolve(context.Background(), "work/pass-only")
	if err != nil || got != passOnlyVal {
		t.Fatalf("work/pass-only: got %q (%v), want %q", got, err, passOnlyVal)
	}
	if strings.Contains(got, passSecondLine) || strings.Contains(got, stderrSentinel) {
		t.Fatalf("work/pass-only leaked extra lines or stderr: %q", got)
	}

	// Case 2: env-only name and env+pass name resolve from MM_SECRET_* without invoking pass.
	got, err = r.Resolve(context.Background(), "work/env")
	if err != nil || got != envOnlyVal {
		t.Fatalf("work/env: got %q (%v), want %q", got, err, envOnlyVal)
	}
	got, err = r.Resolve(context.Background(), "work/both")
	if err != nil || got != envWinnerVal {
		t.Fatalf("work/both: got %q (%v), want %q", got, err, envWinnerVal)
	}

	// Case 3: neither source has the name -> pass exits nonzero and Resolve returns UnresolvedError.
	_, err = r.Resolve(context.Background(), "work/neither")
	if err == nil {
		t.Fatal("work/neither: expected UnresolvedError")
	}
	if err.Error() != "secret:work/neither: could not be resolved" {
		t.Errorf("work/neither error = %q", err.Error())
	}
	if strings.Contains(err.Error(), stderrSentinel) {
		t.Errorf("work/neither error leaked pass stderr: %q", err.Error())
	}

	// Verify the fixture pass script was invoked only for work/pass-only and work/neither.
	logBytes, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read pass invocation log: %v", err)
	}
	var gotCalls []string
	for _, line := range strings.Split(strings.ReplaceAll(string(logBytes), "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			gotCalls = append(gotCalls, line)
		}
	}
	wantCalls := []string{"show work/pass-only", "show work/neither"}
	if !reflect.DeepEqual(gotCalls, wantCalls) {
		t.Errorf("pass script calls = %v, want %v", gotCalls, wantCalls)
	}
}

// writePassExecutable writes a fixture `pass` script (`pass.cmd` on Windows,
// `pass` on Unix) into dir that optionally logs its first two arguments to
// logFile, writes stderrSentinel to stderr, and prints entries[name] on
// `show <name>`.
func writePassExecutable(t *testing.T, dir, logFile, stderrSentinel string, entries map[string]string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if runtime.GOOS == "windows" {
		var b strings.Builder
		b.WriteString("@echo off\r\n")
		if logFile != "" {
			fmt.Fprintf(&b, "echo %%~1 %%~2>>%q\r\n", logFile)
		}
		if stderrSentinel != "" {
			fmt.Fprintf(&b, ">&2 echo %s\r\n", stderrSentinel)
		}
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
		p := filepath.Join(dir, "pass.cmd")
		if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}

	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	if logFile != "" {
		fmt.Fprintf(&b, "printf '%%s %%s\\n' \"$1\" \"$2\" >> %q\n", logFile)
	}
	if stderrSentinel != "" {
		fmt.Fprintf(&b, "printf '%%s\\n' %q >&2\n", stderrSentinel)
	}
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
	p := filepath.Join(dir, "pass")
	if err := os.WriteFile(p, []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
