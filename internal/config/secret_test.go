package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/config/provider/file"
	"github.com/qwts/managed-machine/internal/config/render"
	"github.com/qwts/managed-machine/internal/config/secret"
	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/target"
)

type secretHarness struct {
	*execEnv
	extraEnv  map[string]string
	providers map[string]Provider
	outputs   []string
}

func newSecretHarness(t *testing.T) *secretHarness {
	t.Helper()
	return &secretHarness{
		execEnv:   newExecEnv(t),
		extraEnv:  map[string]string{},
		providers: map[string]Provider{},
	}
}

func (h *secretHarness) run(args ...string) (string, string, int) {
	h.t.Helper()
	var out, errb bytes.Buffer
	envMap := map[string]string{
		DesiredEnv: h.desired,
		"HOME":     h.homeDir,
		"PATH":     h.binDir,
		"PATHEXT":  ".EXE;.CMD;.BAT",
	}
	for k, v := range h.extraEnv {
		envMap[k] = v
	}
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &out,
		Stderr: &errb,
		Detect: func() (target.Host, error) { return target.Host{Target: h.platform}, nil },
		Commands: []cli.Command{Command(Deps{
			Deps: command.Deps{
				Getenv: func(k string) string { return envMap[k] },
				Environ: func() []string {
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
				},
				GOOS:     "darwin",
				StateDir: func() (string, error) { return h.stateDir, nil },
				Account:  func() (string, error) { return h.account, nil },
				Machine:  func(context.Context) (string, error) { return h.machine, nil },
				Now: func() time.Time {
					h.seq++
					return h.now.Add(time.Duration(h.seq) * time.Second)
				},
				NewPlanID: func(now time.Time) (string, error) {
					return fmt.Sprintf("plan-%s-%04x", now.UTC().Format("20060102T150405Z"), h.seq), nil
				},
				Writable: h.writable,
			},
			Catalog: func() ([]Entry, error) {
				snap, err := Load(source.Identity{Kind: "local", Location: "fixtures/local"}, os.DirFS(h.sourceDir), ".")
				if err != nil {
					return nil, err
				}
				return Catalog(snap)
			},
			Provider: func(name string) Provider {
				if p, ok := h.providers[name]; ok {
					return p
				}
				return DefaultProvider(name)
			},
			Secrets: h.secrets,
			Compose: h.compose,
		})},
	}
	code := app.Run(append([]string{"config"}, args...))
	h.outputs = append(h.outputs, out.String(), errb.String())
	return out.String(), errb.String(), code
}

// assertZeroHits greps every file under stateDir and every captured stdout and
// stderr stream for each sentinel value and fails when any appears.
func (h *secretHarness) assertZeroHits(sentinels ...string) {
	h.t.Helper()
	for _, stream := range h.outputs {
		for _, s := range sentinels {
			if strings.Contains(stream, s) {
				h.t.Fatalf("captured output leaked secret sentinel")
			}
		}
	}
	if _, err := os.Stat(h.stateDir); os.IsNotExist(err) {
		return
	}
	err := filepath.WalkDir(h.stateDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, s := range sentinels {
			if bytes.Contains(data, []byte(s)) {
				h.t.Fatalf("artifact %s leaked secret sentinel", filepath.Base(p))
			}
		}
		return nil
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

// TestSecretEnvResolutionAssetsRejectionAndLeakage proves:
//   - secret:<name> in a desired input resolves from MM_SECRET_<NAME> via the
//     injected Getenv seam using the D5 name mapping (incl. '-', '/', '.').
//   - bindings.assets carries {"name":"secret:<name>","sha256":...} with
//     sha256(name || 0x00 || value), and inputs_digest changes when the secret
//     value changes.
//   - Stale plans are rejected by --apply --from-plan with
//     MM_CONFIG_PLAN_REJECTED naming "asset secret:<name>".
//   - Template and structured operations with secrets apply and verify by
//     digest, and --check drift detail reports actual=sha256:... desired=sha256:...,
//     never the values.
//   - No plan document, journal, ledger, stdout or diagnostic ever contains a
//     fixture secret value.
func TestSecretEnvResolutionAssetsRejectionAndLeakage(t *testing.T) {
	const (
		secValV1      = "fixture-secret-v1-alpha-918273"
		secValV2      = "fixture-secret-v2-bravo-645342"
		structSecVal  = "fixture-secret-struct-charlie-112233"
		driftedActual = "fixture-drifted-on-disk-value-998877"
	)

	h := newSecretHarness(t)
	h.providers[ProviderStructured] = &stubStructuredProvider{}

	h.writeSource("templates/npmrc.tmpl", "//registry.npmjs.org/:_authToken={{ inputs.npm-token }}\n; header={{ settings.banner }}\n")
	h.writeSource("creds.yaml", `kind: config
schema_version: 1
id: creds
description: Credentials configuration.
targets: [user]
inputs:
  npm-token: ""
  api-token: ""
operations:
  - id: npmrc
    provider: template
    destination: npmrc
    source: templates/npmrc.tmpl
    settings:
      banner: "token-for-{{ inputs.npm-token }}"
  - id: settings
    provider: structured
    destination: settings
    format: json
    keys:
      auth.token: "{{ inputs.api-token }}"
activation:
  npmrc: "{{ destinations.npmrc }}"
`)

	npmrcDest := filepath.Join(h.homeDir, ".npmrc")
	settingsDest := filepath.Join(h.homeDir, ".config", "app", "settings.json")
	h.writeDesired(fmt.Sprintf(`target: user
config:
  creds:
    inputs:
      npm-token: secret:registry.npmjs.org/auth-token
      api-token: secret:work/api-key.v1
    destinations:
      npmrc: %s
      settings: %s
`, filepath.ToSlash(npmrcDest), filepath.ToSlash(settingsDest)))

	// 1. Set both secrets via MM_SECRET_<NAME>.
	h.extraEnv["MM_SECRET_REGISTRY_NPMJS_ORG_AUTH_TOKEN"] = secValV1
	h.extraEnv["MM_SECRET_WORK_API_KEY_V1"] = structSecVal

	out, errOut, code := h.run("creds", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan v1: exit %d %q %q", code, out, errOut)
	}
	plan1ID := extractPlanID(t, out)
	p1 := readPlan(t, h.stateDir, plan1ID)

	wantAsset1 := secret.Asset("registry.npmjs.org/auth-token", secValV1)
	wantAsset2 := secret.Asset("work/api-key.v1", structSecVal)
	found1, found2 := false, false
	for _, a := range p1.Bindings.Assets {
		if a == wantAsset1 {
			found1 = true
		}
		if a == wantAsset2 {
			found2 = true
		}
	}
	if !found1 || !found2 {
		t.Fatalf("bindings.assets missing secret assets: %+v", p1.Bindings.Assets)
	}

	// 2. Rotate MM_SECRET_REGISTRY_NPMJS_ORG_AUTH_TOKEN to v2 before applying plan1.
	h.extraEnv["MM_SECRET_REGISTRY_NPMJS_ORG_AUTH_TOKEN"] = secValV2

	// Without --from-plan, inputs_digest changed so plan1 is not eligible -> MM_CONFIG_NO_PLAN (exit 2).
	if _, errOut, code := h.run("creds", "--apply", "--user"); code != 2 || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Fatalf("apply without --from-plan after secret change: exit %d %q", code, errOut)
	}

	// With --from-plan, executor re-derives assets and rejects plan1 with MM_CONFIG_PLAN_REJECTED naming "asset secret:registry.npmjs.org/auth-token".
	_, errOut, code = h.run("creds", "--apply", "--user", "--from-plan", plan1ID)
	if code != 1 || !strings.Contains(errOut, "MM_CONFIG_PLAN_REJECTED") || !strings.Contains(errOut, "asset secret:registry.npmjs.org/auth-token") {
		t.Fatalf("apply stale plan: exit %d %q", code, errOut)
	}
	if p1After := readPlan(t, h.stateDir, plan1ID); p1After.State != plan.Rejected {
		t.Fatalf("plan1 state = %q, want rejected", p1After.State)
	}

	// 3. Re-plan with v2; inputs_digest must differ from plan1.
	out, errOut, code = h.run("creds", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan v2: exit %d %q %q", code, out, errOut)
	}
	plan2ID := extractPlanID(t, out)
	p2 := readPlan(t, h.stateDir, plan2ID)
	if p2.Bindings.InputsDigest == p1.Bindings.InputsDigest {
		t.Errorf("inputs_digest did not change when secret value changed: %s", p2.Bindings.InputsDigest)
	}

	// 4. Apply plan2; both template and structured destinations are written and verified by digest.
	out, errOut, code = h.run("creds", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply v2: exit %d %q %q", code, out, errOut)
	}
	gotNpmrc, err := os.ReadFile(npmrcDest)
	if err != nil || !strings.Contains(string(gotNpmrc), secValV2) {
		t.Fatalf("npmrc not written with secret: %v", err)
	}
	gotSettings, err := os.ReadFile(settingsDest)
	if err != nil || !strings.Contains(string(gotSettings), structSecVal) {
		t.Fatalf("settings not written with secret: %v", err)
	}

	// 5. --check passes with 0 differences when destinations match.
	out, errOut, code = h.run("creds", "--check", "--user")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("check clean: exit %d %q %q", code, out, errOut)
	}

	// 6. Modify both destinations on disk; --check exits 3 and reports actual=sha256:... desired=sha256:..., never the values.
	if err := os.WriteFile(npmrcDest, []byte(driftedActual+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(settingsDest, []byte(fmt.Sprintf(`{"auth.token":%q}`+"\n", driftedActual)), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = h.run("creds", "--check", "--user")
	if code != 3 || !strings.Contains(errOut, "MM_CONFIG_DRIFT_FOUND") {
		t.Fatalf("check drifted: exit %d %q %q", code, out, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 drift records, got %d: %s", len(lines), out)
	}
	for _, line := range lines {
		var rec struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(rec.Detail, "actual=sha256:") || !strings.Contains(rec.Detail, " desired=sha256:") {
			t.Errorf("drift detail is not digest-only: %q", rec.Detail)
		}
	}

	// 7. Grep every artifact (plans, journals, ownership ledger, stdouts, diagnostics) for all secret and drifted values.
	h.assertZeroHits(secValV1, secValV2, structSecVal, driftedActual)
}

// TestSecretUnresolvedAtPlanCheckAndApply proves that an unresolvable
// secret:<name> fails with MM_CONFIG_SECRET_UNRESOLVED (exit 1) at --plan,
// --check and --apply, naming only the reference.
func TestSecretUnresolvedAtPlanCheckAndApply(t *testing.T) {
	const sentinel = "never-leak-this-value-554433"
	h := newSecretHarness(t)
	h.extraEnv["PATH"] = h.binDir + string(os.PathListSeparator) + sentinel

	h.writeSource("templates/rc.tmpl", "TOKEN={{ inputs.token }}\n")
	h.writeSource("app.yaml", `kind: config
schema_version: 1
id: app
description: App configuration.
targets: [user]
inputs:
  token: ""
operations:
  - id: rc
    provider: template
    destination: rc
    source: templates/rc.tmpl
`)
	dest := filepath.Join(h.homeDir, ".apprc")
	// Reference secret:PATH to verify it does not read PATH, and fails naming only secret:PATH.
	h.writeDesired(fmt.Sprintf(`target: user
config:
  app:
    inputs:
      token: secret:PATH
    destinations:
      rc: %s
`, filepath.ToSlash(dest)))

	for _, action := range []string{"--plan", "--check", "--apply"} {
		out, errOut, code := h.run("app", action, "--user")
		if code != 1 || out != "" {
			t.Fatalf("%s with unresolved secret: exit %d out=%q err=%q", action, code, out, errOut)
		}
		if !strings.Contains(errOut, "MM_CONFIG_SECRET_UNRESOLVED") || !strings.Contains(errOut, "secret:PATH") {
			t.Errorf("%s stderr = %q, want MM_CONFIG_SECRET_UNRESOLVED naming secret:PATH", action, errOut)
		}
	}
	h.assertZeroHits(sentinel)
}

// stubStructuredProvider is a minimal structured provider used to test C05
// secret handling for structured key values (digest verification and drift
// detail redaction) alongside the built-in template provider.
type stubStructuredProvider struct{}

func (*stubStructuredProvider) Name() string    { return ProviderStructured }
func (*stubStructuredProvider) Version() string { return "1" }

func (p *stubStructuredProvider) renderKeys(op plan.Operation, v render.Values) (map[string]string, []byte, error) {
	var raw map[string]any
	if err := json.Unmarshal([]byte(op.Params["keys"]), &raw); err != nil {
		return nil, nil, err
	}
	rendered := make(map[string]string, len(raw))
	for k, val := range raw {
		s, err := render.Render(fmt.Sprint(val), v)
		if err != nil {
			return nil, nil, err
		}
		rendered[k] = s
	}
	b, err := json.Marshal(rendered)
	if err != nil {
		return nil, nil, err
	}
	return rendered, append(b, '\n'), nil
}

func (p *stubStructuredProvider) Plan(_ fs.FS, _ string, op plan.Operation, v render.Values, owned *ledger.Snapshot) ([]plan.Asset, []plan.Precondition, []plan.Operation, error) {
	_, data, err := p.renderKeys(op, v)
	if err != nil {
		return nil, nil, nil, err
	}
	pre, ops, err := file.PlanBytes(op, data, owned)
	return nil, pre, ops, err
}

func (p *stubStructuredProvider) Check(_ fs.FS, _ string, op plan.Operation, v render.Values) ([]ledger.Drift, error) {
	wantMap, _, err := p.renderKeys(op, v)
	if err != nil {
		return nil, err
	}
	curBytes, err := os.ReadFile(op.Params["path"])
	if err != nil {
		return nil, err
	}
	var curMap map[string]string
	if err := json.Unmarshal(curBytes, &curMap); err != nil {
		return nil, err
	}
	var drifts []ledger.Drift
	for k, wantVal := range wantMap {
		if gotVal := curMap[k]; gotVal != wantVal {
			// Return raw values in Detail so checkRun's secret-redaction path is exercised.
			drifts = append(drifts, ledger.Drift{
				Subject: k,
				Detail:  "actual=" + gotVal + " desired=" + wantVal,
			})
		}
	}
	return drifts, nil
}

func (p *stubStructuredProvider) Step(op plan.Operation, record func(ledger.Record) error) (execute.Step, error) {
	return file.StepFor(ProviderStructured, op, record)
}

func extractPlanID(t *testing.T, out string) string {
	t.Helper()
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	var rec struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(first), &rec); err != nil || rec.Plan == "" {
		t.Fatalf("extract plan id from %q: %v", first, err)
	}
	return rec.Plan
}

// TestSecretStructuredPlanCarriesNoEscapedValue proves that the real
// structured provider's planned values never reach the saved plan, even when
// a secret only appears there in its JSON-escaped form.
func TestSecretStructuredPlanCarriesNoEscapedValue(t *testing.T) {
	const sec = `fixture-"quoted"\back-445566`
	escaped, err := json.Marshal(sec)
	if err != nil {
		t.Fatal(err)
	}
	h := newSecretHarness(t)
	h.writeSource("app.yaml", `kind: config
schema_version: 1
id: app
description: Structured secret configuration.
targets: [user]
inputs:
  api-token: ""
operations:
  - id: settings
    provider: structured
    destination: settings
    format: json
    keys:
      auth.token: "{{ inputs.api-token }}"
`)
	settingsDest := filepath.Join(h.homeDir, ".config", "app", "settings.json")
	h.writeDesired(fmt.Sprintf(`target: user
config:
  app:
    inputs:
      api-token: secret:work/api-key
    destinations:
      settings: %s
`, filepath.ToSlash(settingsDest)))
	h.extraEnv["MM_SECRET_WORK_API_KEY"] = sec

	if out, errOut, code := h.run("app", "--plan", "--user"); code != 0 || errOut != "" {
		t.Fatalf("plan: exit %d %q %q", code, out, errOut)
	}
	if out, errOut, code := h.run("app", "--apply", "--user"); code != 0 || errOut != "" {
		t.Fatalf("apply: exit %d %q %q", code, out, errOut)
	}
	h.assertZeroHits(sec, string(escaped[1:len(escaped)-1]))
}

// TestSecretPassFixtureScriptOrderingAndZeroLeakage places a fixture `pass`
// script (.cmd on Windows) first on an injected PATH and proves the three D5
// cases (pass-only name, env-only name, neither) natively through `config`,
// then greps the state directory and all outputs for both fixture values,
// pass's second stdout line and pass's stderr (zero hits).
func TestSecretPassFixtureScriptOrderingAndZeroLeakage(t *testing.T) {
	const (
		passVal        = "fixture-pass-secret-delta-314159"
		passSecondLine = "fixture-pass-second-line-metadata-265358"
		envVal         = "fixture-env-secret-epsilon-979323"
		passShadowVal  = "fixture-pass-shadowed-value-846264"
		passStderr     = "fixture-pass-stderr-never-echo-338327"
	)

	h := newSecretHarness(t)
	firstBin := filepath.Join(h.dir, "first-bin")
	logFile := filepath.Join(h.dir, "pass-invocations.log")
	writeFixturePass(t, firstBin, logFile, passStderr, map[string]string{
		"work/pass-only": passVal + "\n" + passSecondLine,
		"work/env-only":  passShadowVal + "\n" + passSecondLine,
	})
	// Place firstBin first on the injected PATH; the host's PATH is never included.
	h.extraEnv["PATH"] = firstBin + string(os.PathListSeparator) + h.binDir
	h.extraEnv["MM_SECRET_WORK_ENV_ONLY"] = envVal

	h.writeSource("templates/rc.tmpl", "PASS_TOKEN={{ inputs.pass-tok }}\nENV_TOKEN={{ inputs.env-tok }}\n")
	h.writeSource("creds.yaml", `kind: config
schema_version: 1
id: creds
description: Credentials from Pass and environment.
targets: [user]
inputs:
  pass-tok: ""
  env-tok: ""
operations:
  - id: rc
    provider: template
    destination: rc
    source: templates/rc.tmpl
  - id: settings
    provider: structured
    destination: settings
    format: json
    keys:
      auth.pass_token: "{{ inputs.pass-tok }}"
      auth.env_token: "{{ inputs.env-tok }}"
`)

	rcDest := filepath.Join(h.homeDir, ".credsrc")
	settingsDest := filepath.Join(h.homeDir, ".config", "creds", "settings.json")

	// 1. Third name present in neither MM_SECRET_* nor Pass -> MM_CONFIG_SECRET_UNRESOLVED (exit 1)
	//    at --plan, --check and --apply; pass stderr is never echoed.
	h.writeDesired(fmt.Sprintf(`target: user
config:
  creds:
    inputs:
      pass-tok: secret:work/pass-only
      env-tok: secret:work/neither
    destinations:
      rc: %s
      settings: %s
`, filepath.ToSlash(rcDest), filepath.ToSlash(settingsDest)))

	for _, action := range []string{"--plan", "--check", "--apply"} {
		out, errOut, code := h.run("creds", action, "--user")
		if code != 1 || out != "" {
			t.Fatalf("%s with unresolved pass secret: exit %d out=%q err=%q", action, code, out, errOut)
		}
		if !strings.Contains(errOut, "MM_CONFIG_SECRET_UNRESOLVED") || !strings.Contains(errOut, "secret:work/neither") {
			t.Errorf("%s stderr = %q, want MM_CONFIG_SECRET_UNRESOLVED naming secret:work/neither", action, errOut)
		}
	}

	// 2. Configure the pass-only name (work/pass-only) and env-only name (work/env-only).
	//    Remove logFile first so we observe only the successful lifecycle calls.
	_ = os.Remove(logFile)
	h.writeDesired(fmt.Sprintf(`target: user
config:
  creds:
    inputs:
      pass-tok: secret:work/pass-only
      env-tok: secret:work/env-only
    destinations:
      rc: %s
      settings: %s
`, filepath.ToSlash(rcDest), filepath.ToSlash(settingsDest)))

	out, errOut, code := h.run("creds", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan: exit %d %q %q", code, out, errOut)
	}
	// Verify pass was called ONLY for work/pass-only, never for work/env-only (env wins).
	if calls := readPassLog(t, logFile); !reflect.DeepEqual(calls, []string{"show work/pass-only"}) {
		t.Fatalf("pass calls during --plan = %v, want [show work/pass-only]", calls)
	}

	out, errOut, code = h.run("creds", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply: exit %d %q %q", code, out, errOut)
	}
	gotRC, err := os.ReadFile(rcDest)
	if err != nil {
		t.Fatal(err)
	}
	wantRC := "PASS_TOKEN=" + passVal + "\nENV_TOKEN=" + envVal + "\n"
	if string(gotRC) != wantRC {
		t.Errorf("rc destination:\ngot:  %q\nwant: %q", string(gotRC), wantRC)
	}
	gotSettings, err := os.ReadFile(settingsDest)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotSettings), passVal) || !strings.Contains(string(gotSettings), envVal) {
		t.Errorf("settings destination missing resolved values: %s", gotSettings)
	}
	if strings.Contains(string(gotSettings), passSecondLine) || strings.Contains(string(gotSettings), passShadowVal) {
		t.Errorf("settings destination leaked second line or shadowed pass value: %s", gotSettings)
	}

	// --check passes cleanly when destinations match.
	out, errOut, code = h.run("creds", "--check", "--user")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("check clean: exit %d %q %q", code, out, errOut)
	}

	// Grep stateDir (plans, journals, ownership ledger) and all captured stdouts/diagnostics: zero hits.
	h.assertZeroHits(passVal, passSecondLine, envVal, passShadowVal, passStderr)
}

// TestSecretAssetDigestIdenticalAcrossEnvAndPass proves that the recorded
// asset digest and inputs_digest are identical whether the value came from
// MM_SECRET_<NAME> or Pass, so a plan created when the secret came from the
// environment remains eligible and applies cleanly when the secret comes from
// Pass (and vice versa).
func TestSecretAssetDigestIdenticalAcrossEnvAndPass(t *testing.T) {
	const (
		sharedVal      = "fixture-shared-source-secret-602214"
		rotatedPassVal = "fixture-rotated-pass-secret-076000"
		passSecondLine = "fixture-pass-extra-line-999"
		passStderr     = "fixture-pass-stderr-888"
	)

	h := newSecretHarness(t)
	firstBin := filepath.Join(h.dir, "first-bin")
	logFile := filepath.Join(h.dir, "pass-invocations.log")
	writeFixturePass(t, firstBin, logFile, passStderr, map[string]string{
		"registry.npmjs.org/auth-token": sharedVal + "\n" + passSecondLine,
	})
	h.extraEnv["PATH"] = firstBin + string(os.PathListSeparator) + h.binDir

	h.writeSource("templates/npmrc.tmpl", "//registry.npmjs.org/:_authToken={{ inputs.npm-token }}\n")
	h.writeSource("creds.yaml", `kind: config
schema_version: 1
id: creds
description: NPM token config.
targets: [user]
inputs:
  npm-token: ""
operations:
  - id: npmrc
    provider: template
    destination: npmrc
    source: templates/npmrc.tmpl
`)
	npmrcDest := filepath.Join(h.homeDir, ".npmrc")
	h.writeDesired(fmt.Sprintf(`target: user
config:
  creds:
    inputs:
      npm-token: secret:registry.npmjs.org/auth-token
    destinations:
      npmrc: %s
`, filepath.ToSlash(npmrcDest)))

	// 1. Plan with MM_SECRET_REGISTRY_NPMJS_ORG_AUTH_TOKEN set in the environment.
	h.extraEnv["MM_SECRET_REGISTRY_NPMJS_ORG_AUTH_TOKEN"] = sharedVal
	out, errOut, code := h.run("creds", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan from env: exit %d %q %q", code, out, errOut)
	}
	planEnvID := extractPlanID(t, out)
	pEnv := readPlan(t, h.stateDir, planEnvID)
	if _, err := os.Stat(logFile); !os.IsNotExist(err) {
		t.Fatal("pass script was called when MM_SECRET_* was set")
	}

	// 2. Unset MM_SECRET_* so the same secret value now comes from `pass show`.
	//    Applying without --from-plan must select planEnvID as eligible and succeed.
	delete(h.extraEnv, "MM_SECRET_REGISTRY_NPMJS_ORG_AUTH_TOKEN")
	out, errOut, code = h.run("creds", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply env-planned plan using pass source: exit %d %q %q", code, out, errOut)
	}
	if calls := readPassLog(t, logFile); !reflect.DeepEqual(calls, []string{"show registry.npmjs.org/auth-token"}) {
		t.Fatalf("pass calls during apply = %v, want [show registry.npmjs.org/auth-token]", calls)
	}

	// 3. Plan again with the value coming from Pass; bindings.assets and inputs_digest must match pEnv.
	out, errOut, code = h.run("creds", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan from pass: exit %d %q %q", code, out, errOut)
	}
	planPassID := extractPlanID(t, out)
	pPass := readPlan(t, h.stateDir, planPassID)
	if pPass.Bindings.InputsDigest != pEnv.Bindings.InputsDigest {
		t.Errorf("inputs_digest differed by source: env=%s pass=%s", pEnv.Bindings.InputsDigest, pPass.Bindings.InputsDigest)
	}
	if !reflect.DeepEqual(pPass.Bindings.Assets, pEnv.Bindings.Assets) {
		t.Errorf("bindings.assets differed by source: env=%+v pass=%+v", pEnv.Bindings.Assets, pPass.Bindings.Assets)
	}

	// 4. Rotate the secret in Pass: planPassID is no longer eligible without --from-plan,
	//    and is rejected with MM_CONFIG_PLAN_REJECTED naming "asset secret:registry.npmjs.org/auth-token" with --from-plan.
	writeFixturePass(t, firstBin, logFile, passStderr, map[string]string{
		"registry.npmjs.org/auth-token": rotatedPassVal + "\n",
	})
	if _, errOut, code := h.run("creds", "--apply", "--user"); code != 2 || !strings.Contains(errOut, "MM_CONFIG_NO_PLAN") {
		t.Fatalf("apply after pass secret rotation without --from-plan: exit %d %q", code, errOut)
	}
	_, errOut, code = h.run("creds", "--apply", "--user", "--from-plan", planPassID)
	if code != 1 || !strings.Contains(errOut, "MM_CONFIG_PLAN_REJECTED") || !strings.Contains(errOut, "asset secret:registry.npmjs.org/auth-token") {
		t.Fatalf("apply stale pass plan with --from-plan: exit %d %q", code, errOut)
	}

	h.assertZeroHits(sharedVal, rotatedPassVal, passSecondLine, passStderr)
}

func writeFixturePass(t *testing.T, dir, logFile, stderrSentinel string, entries map[string]string) {
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
		if err := os.WriteFile(filepath.Join(dir, "pass.cmd"), []byte(b.String()), 0o755); err != nil {
			t.Fatal(err)
		}
		return
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
	if err := os.WriteFile(filepath.Join(dir, "pass"), []byte(b.String()), 0o755); err != nil {
		t.Fatal(err)
	}
}

func readPassLog(t *testing.T, logFile string) []string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("read pass log %s: %v", logFile, err)
	}
	var calls []string
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			calls = append(calls, line)
		}
	}
	return calls
}
