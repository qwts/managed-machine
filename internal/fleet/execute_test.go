package fleet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/config/ledger"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/status"
	"github.com/qwts/managed-machine/internal/target"
)

const validCutoverYAML = `schema_version: 1
kind: fleet-cutover
cut_over_at: "2026-10-22T00:00:00Z"
reviewed_by: [alice]
note: S5 fleet cutover.
`

type execHarness struct {
	*testHarness
	homeDir   string
	planSeq   int
	writable  func(string) bool
	machineID string
}

func newExecHarness(t *testing.T, fx fixtureSet, withCutover bool) *execHarness {
	t.Helper()
	base := newHarness(t, fx)
	homeDir := filepath.Join(base.base, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	base.env["HOME"] = homeDir
	base.env["USERPROFILE"] = homeDir
	if withCutover {
		writeCutover(t, base.policyDir)
	}
	return &execHarness{
		testHarness: base,
		homeDir:     homeDir,
		machineID:   "machine-test",
	}
}

func writeCutover(t *testing.T, policyDir string) {
	t.Helper()
	p := filepath.Join(policyDir, "fleet", "cutover.yaml")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(validCutoverYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (eh *execHarness) run(args ...string) (string, string, int) {
	eh.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &stdout,
		Stderr: &stderr,
		Detect: func() (target.Host, error) {
			switch eh.goos {
			case "windows":
				return target.Host{Target: target.WindowsAMD64}, nil
			case "linux":
				return target.Host{Target: target.LinuxAMD64}, nil
			default:
				return target.Host{Target: target.DarwinARM64}, nil
			}
		},
		Commands: []cli.Command{
			fleet.Command(fleet.Deps{
				Getenv:   func(k string) string { return eh.env[k] },
				GOOS:     eh.goos,
				Username: func() (string, error) { return eh.username, nil },
				Home:     func() (string, error) { return eh.homeDir, nil },
				Now:      func() time.Time { return eh.now },
				Client:   eh.client,
				Machine:  func(context.Context) (string, error) { return eh.machineID, nil },
				NewPlanID: func(now time.Time) (string, error) {
					eh.planSeq++
					return fmt.Sprintf("plan-%s-%04x", now.UTC().Format("20060102T150405Z"), eh.planSeq), nil
				},
				Writable: eh.writable,
				Runner: func(ctx context.Context, exe string, argv, env []string) (provider.Result, error) {
					atomic.AddInt64(&eh.runnerHits, 1)
					return provider.Result{}, fmt.Errorf("unexpected process execution: %s %v", exe, argv)
				},
			}),
		},
	}
	code := app.Run(append([]string{"fleet"}, args...))
	if data, err := os.ReadFile(eh.sentinel); err == nil {
		eh.t.Fatalf("external binary invoked: %s", string(data))
	}
	if got := atomic.LoadInt64(&eh.runnerHits); got != 0 {
		eh.t.Fatalf("runner seam called %d times, want 0", got)
	}
	return stdout.String(), stderr.String(), code
}

// Acceptance 1: `--plan --user` saves a plan with `command: "fleet --apply"`,
// the top-level `fleet: <account>` field, `source_revision` = directory
// identity, `bindings.assets` holding `policy:<path>` digests for every policy
// document and `directory:<identity>`, and the `policy fleet-cutover`
// precondition; it prints the plan record, one operation record, `legacy`
// records when applicable and the `access` records in order; `--apply`
// consumes it through the existing `Select` rule and prints apply records;
// `--from-plan` mismatches are `MM_FLEET_PLAN_MISMATCH`; editing any policy
// document after planning rejects the plan with `MM_FLEET_PLAN_REJECTED`
// naming the asset.
func TestPlanAndApplyDocumentAssetsSelectMismatchAndPolicyRejection(t *testing.T) {
	fx := newFixtureSet(t)
	eh := newExecHarness(t, fx, true)

	// 1. Run `fleet --plan --user` and inspect output records and saved plan document.
	planOut, planErr, planCode := eh.run("--plan", "--user")
	if planCode != 0 || planErr != "" {
		t.Fatalf("fleet --plan --user: exit=%d stderr=%q", planCode, planErr)
	}
	recs, err := output.ReadCompact([]byte(planOut))
	if err != nil {
		t.Fatalf("ReadCompact: %v", err)
	}
	// Expected: 1 plan record + 1 operation record + 3 authorized access records = 5 records.
	if len(recs) != 5 {
		t.Fatalf("got %d plan records, want 5:\n%s", len(recs), planOut)
	}

	assertFieldOrder(t, "plan[0]", recs[0].Fields(),
		[]string{"plan", "fleet", "target", "source_revision", "created_at", "state"})
	planIDVal, _ := recs[0].Get("plan")
	planID := fmt.Sprint(planIDVal)
	if fl, _ := recs[0].Get("fleet"); fl != "dev" {
		t.Errorf("plan.fleet = %v, want dev", fl)
	}
	if tgt, _ := recs[0].Get("target"); tgt != "user" {
		t.Errorf("plan.target = %v, want user", tgt)
	}
	srcRevVal, _ := recs[0].Get("source_revision")
	srcRev := fmt.Sprint(srcRevVal)
	if !strings.HasPrefix(srcRev, "file:") {
		t.Errorf("plan.source_revision = %q, want file:<sha256>", srcRev)
	}
	if st, _ := recs[0].Get("state"); st != "saved" {
		t.Errorf("plan.state = %v, want saved", st)
	}

	assertFieldOrder(t, "operation[0]", recs[1].Fields(),
		[]string{"id", "operation", "target", "action"})
	if id, _ := recs[1].Get("id"); id != "fleet" {
		t.Errorf("op.id = %v, want fleet", id)
	}
	if opName, _ := recs[1].Get("operation"); opName != "authorized-keys" {
		t.Errorf("op.operation = %v, want authorized-keys", opName)
	}
	if act, _ := recs[1].Get("action"); act != "create" {
		t.Errorf("op.action = %v, want create", act)
	}

	// Trailing records are the 3 authorized access records in owner-then-fingerprint order.
	wantAccess := []struct {
		owner string
		fp    string
		group string
	}{
		{"alice", fx.aliceLow.fingerprint, "workstations"},
		{"alice", fx.aliceHigh.fingerprint, "admins"},
		{"bob", fx.bobValid.fingerprint, "admins"},
	}
	for i, w := range wantAccess {
		r := recs[2+i]
		assertFieldOrder(t, fmt.Sprintf("access[%d]", i), r.Fields(),
			[]string{"account", "fingerprint", "state", "owner", "group"})
		if acct, _ := r.Get("account"); acct != "dev" {
			t.Errorf("access[%d] account = %v, want dev", i, acct)
		}
		if fp, _ := r.Get("fingerprint"); fp != w.fp {
			t.Errorf("access[%d] fingerprint = %v, want %s", i, fp, w.fp)
		}
		if st, _ := r.Get("state"); st != "authorized" {
			t.Errorf("access[%d] state = %v, want authorized", i, st)
		}
		if ow, _ := r.Get("owner"); ow != w.owner {
			t.Errorf("access[%d] owner = %v, want %s", i, ow, w.owner)
		}
		if gr, _ := r.Get("group"); gr != w.group {
			t.Errorf("access[%d] group = %v, want %s", i, gr, w.group)
		}
	}

	// Load and inspect the saved plan JSON on disk.
	planPath := filepath.Join(eh.stateDir, "plans", planID+".json")
	rawPlan, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read saved plan: %v", err)
	}
	var doc plan.Document
	if err := json.Unmarshal(rawPlan, &doc); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("saved plan failed Validate: %v", err)
	}
	if doc.Command != "fleet --apply" {
		t.Errorf("doc.Command = %q, want %q", doc.Command, "fleet --apply")
	}
	if doc.Fleet != "dev" || doc.Software != "" || doc.Config != "" {
		t.Errorf("doc resource fields: fleet=%q software=%q config=%q", doc.Fleet, doc.Software, doc.Config)
	}
	if doc.Target != "user" || doc.Account != "dev" {
		t.Errorf("doc target=%q account=%q", doc.Target, doc.Account)
	}
	if doc.SourceRevision != srcRev {
		t.Errorf("doc.SourceRevision = %q, want %q", doc.SourceRevision, srcRev)
	}

	// Verify bindings.assets contains every policy document plus directory:<identity>.
	gotAssets := map[string]string{}
	for _, a := range doc.Bindings.Assets {
		gotAssets[a.Name] = a.SHA256
	}
	wantAssetNames := []string{
		"directory:" + srcRev,
		"policy:fleet/access-groups/admins.yaml",
		"policy:fleet/access-groups/workstations.yaml",
		"policy:fleet/cutover.yaml",
		"policy:fleet/key-directory.yaml",
	}
	if len(gotAssets) != len(wantAssetNames) {
		t.Fatalf("doc.Bindings.Assets = %v, want %v", gotAssets, wantAssetNames)
	}
	for _, name := range wantAssetNames {
		if sum, ok := gotAssets[name]; !ok || len(sum) != 64 {
			t.Errorf("asset %q digest = %q (present=%v), want 64-hex sha256", name, sum, ok)
		}
	}

	// Verify preconditions include policy fleet-cutover, platform, and target.
	foundPolicyPre := false
	foundPlatformPre := false
	foundTargetPre := false
	for _, pre := range doc.Preconditions {
		switch pre.Kind {
		case "policy":
			if pre.Subject == "fleet-cutover" && pre.Requirement == "present" && pre.Observed == "present" {
				foundPolicyPre = true
			}
		case "platform":
			if pre.Subject == "host" && pre.Requirement == "darwin/arm64" {
				foundPlatformPre = true
			}
		case "target":
			if pre.Requirement == "absent" && pre.Observed == "absent" {
				foundTargetPre = true
			}
		}
	}
	if !foundPolicyPre || !foundPlatformPre || !foundTargetPre {
		t.Errorf("preconditions = %+v (policy=%v platform=%v target=%v)",
			doc.Preconditions, foundPolicyPre, foundPlatformPre, foundTargetPre)
	}

	// 2. Presentation modes for `--plan` (--json and --human).
	eh.now = eh.now.Add(time.Second)
	jsonPlanOut, jsonPlanErr, jsonPlanCode := eh.run("--plan", "--user", "--json")
	if jsonPlanCode != 0 || jsonPlanErr != "" {
		t.Fatalf("fleet --plan --json: exit=%d stderr=%q", jsonPlanCode, jsonPlanErr)
	}
	var jsonDoc struct {
		Records []*output.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(jsonPlanOut), &jsonDoc); err != nil || len(jsonDoc.Records) != 5 {
		t.Fatalf("unmarshal --plan --json: err=%v len=%d", err, len(jsonDoc.Records))
	}
	eh.now = eh.now.Add(time.Second)
	humanPlanOut, humanPlanErr, humanPlanCode := eh.run("--plan", "--user", "--human")
	if humanPlanCode != 0 || humanPlanErr != "" {
		t.Fatalf("fleet --plan --human: exit=%d stderr=%q", humanPlanCode, humanPlanErr)
	}
	for _, col := range []string{"PLAN", "FLEET", "SOURCE_REVISION", "OPERATION", "ACTION", "ACCOUNT", "FINGERPRINT", "GROUP"} {
		if !strings.Contains(humanPlanOut, col) {
			t.Errorf("--plan --human missing column %q:\n%s", col, humanPlanOut)
		}
	}
	latestPlanRecs, err := output.ReadCompact([]byte(planOut))
	if err != nil {
		t.Fatal(err)
	}
	_ = latestPlanRecs

	// 3. `--apply` consumes the latest saved plan via Select and prints apply records.
	applyOut, applyErr, applyCode := eh.run("--apply", "--user")
	if applyCode != 0 || applyErr != "" {
		t.Fatalf("fleet --apply --user: exit=%d stderr=%q", applyCode, applyErr)
	}
	applyRecs, err := output.ReadCompact([]byte(applyOut))
	if err != nil || len(applyRecs) != 1 {
		t.Fatalf("apply records: err=%v len=%d out=%q", err, len(applyRecs), applyOut)
	}
	assertFieldOrder(t, "apply[0]", applyRecs[0].Fields(),
		[]string{"id", "operation", "target", "action", "verified", "plan"})
	if v, _ := applyRecs[0].Get("id"); v != "fleet" {
		t.Errorf("apply.id = %v, want fleet", v)
	}
	if v, _ := applyRecs[0].Get("operation"); v != "authorized-keys" {
		t.Errorf("apply.operation = %v, want authorized-keys", v)
	}
	if v, _ := applyRecs[0].Get("action"); v != "create" {
		t.Errorf("apply.action = %v, want create", v)
	}
	if v, _ := applyRecs[0].Get("verified"); v != true {
		t.Errorf("apply.verified = %v, want true", v)
	}

	// Older superseded plans were marked superseded when the latest plan was applied.
	rawOldPlan, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	var oldDoc plan.Document
	if err := json.Unmarshal(rawOldPlan, &oldDoc); err != nil {
		t.Fatal(err)
	}
	if oldDoc.State != plan.Superseded {
		t.Errorf("older plan state = %q, want %q", oldDoc.State, plan.Superseded)
	}

	// 4. `--from-plan` mismatches return MM_FLEET_PLAN_MISMATCH (exit 2).
	eh.now = eh.now.Add(time.Second)
	pOut, pErr, pCode := eh.run("--plan", "--user")
	if pCode != 0 || pErr != "" {
		t.Fatalf("plan for mismatch test: exit=%d stderr=%q", pCode, pErr)
	}
	pRecs, _ := output.ReadCompact([]byte(pOut))
	mismatchPlanIDVal, _ := pRecs[0].Get("plan")
	mismatchPlanID := fmt.Sprint(mismatchPlanIDVal)

	// Switch account so the plan's fleet/account/inputs_digest mismatches.
	eh.username = "root_admin"
	mOut, mErr, mCode := eh.run("--apply", "--from-plan", mismatchPlanID)
	if mCode != 2 || mOut != "" || !strings.Contains(mErr, `"diagnostic":"MM_FLEET_PLAN_MISMATCH"`) {
		t.Errorf("--from-plan account mismatch: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_PLAN_MISMATCH", mCode, mOut, mErr)
	}
	// Restore account and test a plan file for a different command (config).
	eh.username = "dev"
	otherCmdPlan := filepath.Join(t.TempDir(), "config-plan.json")
	cfgDoc := doc
	cfgDoc.Command = "config"
	cfgDoc.Config = "shell"
	cfgDoc.Fleet = ""
	rawCfgPlan, err := json.Marshal(cfgDoc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherCmdPlan, rawCfgPlan, 0o644); err != nil {
		t.Fatal(err)
	}
	mOut, mErr, mCode = eh.run("--apply", "--from-plan", otherCmdPlan)
	if mCode != 2 || mOut != "" || !strings.Contains(mErr, `"diagnostic":"MM_FLEET_PLAN_MISMATCH"`) {
		t.Errorf("--from-plan command mismatch: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_PLAN_MISMATCH", mCode, mOut, mErr)
	}

	// 5. Editing any policy document after planning rejects the plan with
	// MM_FLEET_PLAN_REJECTED (exit 1) naming the modified policy:<path> asset.
	for _, policyRel := range []string{
		"fleet/access-groups/admins.yaml",
		"fleet/access-groups/workstations.yaml",
		"fleet/cutover.yaml",
	} {
		t.Run("policy edit rejects "+policyRel, func(t *testing.T) {
			sub := newExecHarness(t, fx, true)
			out, errOut, code := sub.run("--plan", "--user")
			if code != 0 || errOut != "" {
				t.Fatalf("--plan: exit=%d stderr=%q", code, errOut)
			}
			r, _ := output.ReadCompact([]byte(out))
			pidVal, _ := r[0].Get("plan")
			pid := fmt.Sprint(pidVal)

			// Edit the policy document after planning (adding a valid comment
			// or updating description/note so policy remains valid).
			targetFile := filepath.Join(sub.policyDir, filepath.FromSlash(policyRel))
			orig, err := os.ReadFile(targetFile)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(targetFile, append(orig, []byte("# post-plan edit\n")...), 0o644); err != nil {
				t.Fatal(err)
			}

			aOut, aErr, aCode := sub.run("--apply", "--from-plan", pid)
			if aCode != 1 || aOut != "" || !strings.Contains(aErr, `"diagnostic":"MM_FLEET_PLAN_REJECTED"`) {
				t.Fatalf("after editing %s: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_PLAN_REJECTED",
					policyRel, aCode, aOut, aErr)
			}
			wantAsset := "policy:" + policyRel
			if !strings.Contains(aErr, wantAsset) {
				t.Errorf("rejection diagnostic %q does not name asset %q", aErr, wantAsset)
			}
		})
	}
}

// Acceptance 2: The destination follows D4: `desired.yaml`
// `fleet.destinations.authorized_keys` when declared, else
// `<HOME>/.ssh/authorized_keys` through an injected home on all three OS
// families; an absent `.ssh` directory is created; after apply the Unix
// directory mode is 0700 and the file 0600 and the receipt evidence carries
// `mode` (asserted on darwin and linux only); the block content is
// `<type> <blob> <owner>` lines in owner-then-fingerprint order inside the
// `fleet-authorized-keys` markers and every line outside the markers is
// byte-identical (golden diffs, CRLF preserved on a CRLF file).
func TestDestinationResolutionModesOrderingPreservationAndCRLF(t *testing.T) {
	fx := newFixtureSet(t)

	// 1. Default `<HOME>/.ssh/authorized_keys` across darwin, linux, and windows,
	// creating an absent `.ssh` directory, setting 0700/0600 on Unix, and
	// recording `mode` in the receipt evidence.
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run("default home on "+goos, func(t *testing.T) {
			eh := newExecHarness(t, fx, true)
			eh.goos = goos
			sshDir := filepath.Join(eh.homeDir, ".ssh")
			destPath := filepath.Join(sshDir, "authorized_keys")

			if _, err := os.Stat(sshDir); !os.IsNotExist(err) {
				t.Fatalf("expected .ssh to be absent before apply: %v", err)
			}
			pOut, pErr, pCode := eh.run("--plan", "--user")
			if pCode != 0 || pErr != "" {
				t.Fatalf("%s --plan: exit=%d stderr=%q", goos, pCode, pErr)
			}
			pRecs, _ := output.ReadCompact([]byte(pOut))
			planID := fmt.Sprint(mustGet(t, pRecs[0], "plan"))

			// Planning is read-only: .ssh still does not exist.
			if _, err := os.Stat(sshDir); !os.IsNotExist(err) {
				t.Fatalf("%s --plan created .ssh", goos)
			}

			aOut, aErr, aCode := eh.run("--apply", "--user")
			if aCode != 0 || aErr != "" || aOut == "" {
				t.Fatalf("%s --apply: exit=%d stdout=%q stderr=%q", goos, aCode, aOut, aErr)
			}

			gotBytes, err := os.ReadFile(destPath)
			if err != nil {
				t.Fatalf("%s read %s: %v", goos, destPath, err)
			}
			wantBlock := fmt.Sprintf("# managed-machine:begin fleet-authorized-keys\n"+
				"ssh-ed25519 %s alice\n"+
				"ssh-ed25519 %s alice\n"+
				"ssh-ed25519 %s bob\n"+
				"# managed-machine:end fleet-authorized-keys\n",
				fx.aliceLow.blobB64,
				fx.aliceHigh.blobB64,
				fx.bobValid.blobB64,
			)
			if string(gotBytes) != wantBlock {
				t.Errorf("%s authorized_keys:\ngot:\n%s\nwant:\n%s", goos, string(gotBytes), wantBlock)
			}

			if goos != "windows" && runtime.GOOS != "windows" {
				dirInfo, err := os.Stat(sshDir)
				if err != nil {
					t.Fatal(err)
				}
				if gotPerm := dirInfo.Mode().Perm(); gotPerm != 0o700 {
					t.Errorf("%s .ssh perm = %04o, want 0700", goos, gotPerm)
				}
				fileInfo, err := os.Stat(destPath)
				if err != nil {
					t.Fatal(err)
				}
				if gotPerm := fileInfo.Mode().Perm(); gotPerm != 0o600 {
					t.Errorf("%s authorized_keys perm = %04o, want 0600", goos, gotPerm)
				}

				// Inspect journal receipt evidence for `mode: "0600"`.
				journalPath := filepath.Join(eh.stateDir, "journal", planID+".jsonl")
				rawJournal, err := os.ReadFile(journalPath)
				if err != nil {
					t.Fatalf("read journal: %v", err)
				}
				lines := strings.Split(strings.TrimSpace(string(rawJournal)), "\n")
				var receipt plan.Record
				if err := json.Unmarshal([]byte(lines[len(lines)-1]), &receipt); err != nil {
					t.Fatalf("unmarshal receipt: %v", err)
				}
				if receipt.Action != plan.ActionReceipt || receipt.Outcome != plan.OutcomeApplied {
					t.Fatalf("unexpected last journal record: %+v", receipt)
				}
				if receipt.Evidence["mode"] != "0600" {
					t.Errorf("%s receipt evidence mode = %v, want 0600 (evidence=%+v)", goos, receipt.Evidence["mode"], receipt.Evidence)
				}
			}
		})
	}

	// 2. `desired.yaml` `fleet.destinations.authorized_keys` overrides `<HOME>/.ssh/authorized_keys`.
	t.Run("desired.yaml override", func(t *testing.T) {
		eh := newExecHarness(t, fx, true)
		customDest := filepath.Join(eh.base, "custom-ssh", "authorized_keys_override")
		desiredFile := filepath.Join(eh.homeDir, ".config", "managed-machine", "desired.yaml")
		if err := os.MkdirAll(filepath.Dir(desiredFile), 0o755); err != nil {
			t.Fatal(err)
		}
		desiredYAML := "target: user\nfleet:\n  destinations:\n    authorized_keys: '" + filepath.ToSlash(customDest) + "'\n"
		if err := os.WriteFile(desiredFile, []byte(desiredYAML), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, errOut, code := eh.run("--plan", "--user"); code != 0 || errOut != "" {
			t.Fatalf("--plan with desired.yaml: exit=%d stderr=%q", code, errOut)
		}
		if _, errOut, code := eh.run("--apply", "--user"); code != 0 || errOut != "" {
			t.Fatalf("--apply with desired.yaml: exit=%d stderr=%q", code, errOut)
		}
		if _, err := os.Stat(customDest); err != nil {
			t.Fatalf("expected custom destination %s to exist: %v", customDest, err)
		}
		if _, err := os.Stat(filepath.Join(eh.homeDir, ".ssh", "authorized_keys")); !os.IsNotExist(err) {
			t.Errorf("default ~/.ssh/authorized_keys should not have been created when desired.yaml overrides it")
		}
	})

	// 3. Preserving lines outside markers byte-identically (LF and CRLF golden files).
	t.Run("preserve outside lines LF and CRLF", func(t *testing.T) {
		personalKey := makeEd25519Key(t, 0x31, "user@personal-yubikey")
		backupKey := makeEd25519Key(t, 0x32, "user@backup-key")

		for _, tc := range []struct {
			name       string
			eol        string
			goldenName string
		}{
			{"lf", "\n", "authorized-keys-preserved-lf.golden"},
			{"crlf", "\r\n", "authorized-keys-preserved-crlf.golden"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				eh := newExecHarness(t, fx, true)
				destPath := filepath.Join(eh.homeDir, ".ssh", "authorized_keys")
				if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
					t.Fatal(err)
				}
				prefix := strings.Join([]string{
					"# Personal keys managed by hand",
					`environment="FOO=bar",no-agent-forwarding ` + personalKey.line,
					"",
				}, tc.eol) + tc.eol
				suffix := tc.eol + strings.Join([]string{
					"# Trailing personal key",
					backupKey.line,
				}, tc.eol) + tc.eol

				// Seed an initial owned block between prefix and suffix so update
				// replaces in place and preserves both prefix and suffix verbatim.
				initialBlock := strings.Join([]string{
					"# managed-machine:begin fleet-authorized-keys",
					"ssh-ed25519 " + fx.aliceLow.blobB64 + " alice",
					"# managed-machine:end fleet-authorized-keys",
				}, tc.eol) + tc.eol
				initialFile := []byte(prefix + initialBlock + suffix)
				if err := os.WriteFile(destPath, initialFile, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := ledger.Append(fleet.FleetLedgerPath(eh.stateDir), ledger.Record{
					SchemaVersion: ledger.SchemaVersion,
					Kind:          ledger.KindBlock,
					Path:          destPath,
					ID:            fleet.BlockID,
					SHA256:        "0000000000000000000000000000000000000000000000000000000000000000",
					Config:        fleet.FleetID,
					Operation:     fleet.OperationAuthorizedKeys,
					Target:        fleet.TargetUser,
					State:         ledger.Owned,
					Account:       "dev",
					Plan:          "plan-seed",
					At:            "2026-10-22T01:00:00Z",
				}); err != nil {
					t.Fatal(err)
				}

				if _, errOut, code := eh.run("--plan", "--user"); code != 0 || errOut != "" {
					t.Fatalf("%s --plan: exit=%d stderr=%q", tc.name, code, errOut)
				}
				if _, errOut, code := eh.run("--apply", "--user"); code != 0 || errOut != "" {
					t.Fatalf("%s --apply: exit=%d stderr=%q", tc.name, code, errOut)
				}

				gotBytes, err := os.ReadFile(destPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.HasPrefix(gotBytes, []byte(prefix)) {
					t.Errorf("%s prefix was modified:\ngot:\n%q\nwant prefix:\n%q", tc.name, string(gotBytes), prefix)
				}
				if !bytes.HasSuffix(gotBytes, []byte(suffix)) {
					t.Errorf("%s suffix was modified:\ngot:\n%q\nwant suffix:\n%q", tc.name, string(gotBytes), suffix)
				}
				goldenBytes := gotBytes
				if tc.eol == "\r\n" {
					for i := 0; i < len(gotBytes); i++ {
						if gotBytes[i] == '\n' && (i == 0 || gotBytes[i-1] != '\r') {
							t.Fatalf("CRLF file acquired bare LF at byte %d:\n%q", i, string(gotBytes))
						}
					}
					// Encode CRLF visibly in the golden file so .gitattributes (* text=auto eol=lf)
					// keeps golden files byte-identical across git checkouts on all platforms.
					goldenBytes = []byte(strings.ReplaceAll(string(gotBytes), "\r\n", "<CRLF>\n"))
				}

				assertGoldenBytes(t, tc.goldenName, goldenBytes)
			})
		}
	})
}

// Acceptance 3: Ownership lives in `<state>/fleet/ownership.jsonl`; markers
// with no fleet-ledger entry plan `conflict foreign-block
// <path>:fleet-authorized-keys`, `--apply` refuses with `MM_FLEET_CONFLICT`
// and the file is byte-identical; `status --section config` never lists the
// fleet block (test over a shared state directory).
func TestFleetOwnershipLedgerForeignBlockConflictAndStatusIsolation(t *testing.T) {
	fx := newFixtureSet(t)

	// 1. Unowned `fleet-authorized-keys` markers in destination plan
	// `conflict foreign-block <path>:fleet-authorized-keys` and `--apply`
	// fails with MM_FLEET_CONFLICT (exit 1), leaving the file byte-identical.
	t.Run("foreign-block conflict", func(t *testing.T) {
		eh := newExecHarness(t, fx, true)
		destPath := filepath.Join(eh.homeDir, ".ssh", "authorized_keys")
		if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
			t.Fatal(err)
		}
		foreignContent := []byte("# hand-crafted file\n" +
			"# managed-machine:begin fleet-authorized-keys\n" +
			"ssh-ed25519 " + fx.aliceLow.blobB64 + " someone\n" +
			"# managed-machine:end fleet-authorized-keys\n")
		if err := os.WriteFile(destPath, foreignContent, 0o600); err != nil {
			t.Fatal(err)
		}

		// Also verify that even if `<state>/ownership.jsonl` (the config ledger)
		// claims the block, the fleet command consults `<state>/fleet/ownership.jsonl`
		// and still treats it as a foreign-block!
		configLedger := filepath.Join(eh.stateDir, ledger.FileName)
		if err := ledger.Append(configLedger, ledger.Record{
			SchemaVersion: ledger.SchemaVersion,
			Kind:          ledger.KindBlock,
			Path:          destPath,
			ID:            fleet.BlockID,
			SHA256:        "1111111111111111111111111111111111111111111111111111111111111111",
			Config:        "ssh-custom",
			Operation:     "keys",
			Target:        "user",
			State:         ledger.Owned,
			Account:       "dev",
			Plan:          "plan-cfg",
			At:            "2026-10-22T01:00:00Z",
		}); err != nil {
			t.Fatal(err)
		}

		pOut, pErr, pCode := eh.run("--plan", "--user")
		if pCode != 0 || pErr != "" {
			t.Fatalf("--plan on foreign block: exit=%d stderr=%q", pCode, pErr)
		}
		pRecs, err := output.ReadCompact([]byte(pOut))
		if err != nil || len(pRecs) < 2 {
			t.Fatalf("plan records: err=%v len=%d", err, len(pRecs))
		}
		wantAction := "conflict foreign-block " + destPath + ":" + fleet.BlockID
		if gotAct := fmt.Sprint(mustGet(t, pRecs[1], "action")); gotAct != wantAction {
			t.Errorf("planned action = %q, want %q", gotAct, wantAction)
		}

		aOut, aErr, aCode := eh.run("--apply", "--user")
		if aCode != 1 || aOut != "" || !strings.Contains(aErr, `"diagnostic":"MM_FLEET_CONFLICT"`) {
			t.Fatalf("--apply on foreign block: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_CONFLICT", aCode, aOut, aErr)
		}
		afterBytes, err := os.ReadFile(destPath)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(afterBytes, foreignContent) {
			t.Errorf("authorized_keys was modified on MM_FLEET_CONFLICT:\ngot:\n%s\nwant:\n%s", string(afterBytes), string(foreignContent))
		}
	})

	// 2. Shared state directory: `fleet --apply` writes `<state>/fleet/ownership.jsonl`
	// and never `<state>/ownership.jsonl`, so `status --section config` never lists
	// the fleet block; and a `config` definition targeting `fleet-authorized-keys`
	// sees `foreign-block` from the other ledger.
	t.Run("shared state directory status and config isolation", func(t *testing.T) {
		eh := newExecHarness(t, fx, true)
		if _, errOut, code := eh.run("--plan", "--user"); code != 0 || errOut != "" {
			t.Fatalf("fleet --plan: exit=%d stderr=%q", code, errOut)
		}
		if _, errOut, code := eh.run("--apply", "--user"); code != 0 || errOut != "" {
			t.Fatalf("fleet --apply: exit=%d stderr=%q", code, errOut)
		}

		// Verify `<state>/fleet/ownership.jsonl` exists and records ownership.
		fleetSnap, err := ledger.Load(fleet.FleetLedgerPath(eh.stateDir))
		if err != nil {
			t.Fatalf("load fleet ledger: %v", err)
		}
		destPath := filepath.Join(eh.homeDir, ".ssh", "authorized_keys")
		if !fleetSnap.IsOwnedBy(fleet.FleetID, fleet.TargetUser, destPath, ledger.KindBlock, fleet.BlockID) {
			t.Fatalf("expected fleet ledger to own %s:%s", destPath, fleet.BlockID)
		}

		// Verify `<state>/ownership.jsonl` does not exist after fleet --apply.
		if _, err := os.Stat(filepath.Join(eh.stateDir, ledger.FileName)); !os.IsNotExist(err) {
			t.Fatalf("expected config ownership.jsonl to be absent after fleet --apply: %v", err)
		}

		// Run `status --section config` over the same state directory -> 0 records.
		var stOut, stErr bytes.Buffer
		stApp := &cli.App{
			Name:   "managed-machine",
			Stdout: &stOut,
			Stderr: &stErr,
			Detect: func() (target.Host, error) { return target.Host{Target: target.DarwinARM64}, nil },
			Commands: []cli.Command{
				status.Command(status.Deps{
					ConfigCatalog:   func() ([]config.Entry, error) { return nil, nil },
					SoftwareCatalog: func() ([]source.Entry, *registry.Registry, error) { return nil, nil, nil },
					Getenv:          func(k string) string { return eh.env[k] },
					GOOS:            eh.goos,
					StateDir:        func() (string, error) { return eh.stateDir, nil },
					Now:             func() time.Time { return eh.now },
				}),
			},
		}
		if code := stApp.Run([]string{"status", "--section", "config"}); code != 0 || stErr.String() != "" || stOut.String() != "" {
			t.Fatalf("status --section config: exit=%d stdout=%q stderr=%q, want 0 and empty",
				code, stOut.String(), stErr.String())
		}
		stOut.Reset()
		stErr.Reset()
		if code := stApp.Run([]string{"status"}); code != 0 || stErr.String() != "" || stOut.String() != "" {
			t.Fatalf("status: exit=%d stdout=%q stderr=%q, want 0 and empty",
				code, stOut.String(), stErr.String())
		}
	})
}

// Acceptance 4: Unchanged authorization yields a zero-operation plan whose
// apply changes nothing; an empty authorized set plans `delete` of an owned
// block; applying again without a new plan is `MM_FLEET_NO_PLAN` (exit 2);
// `--check` exits 0 silently, 3 with `MM_FLEET_DRIFT_FOUND` and digest-only
// detail after an external edit, 3 with `drift: legacy-block` when a legacy
// block exists, and 4 with `MM_FLEET_CHECK_INCOMPLETE` when the destination is
// a directory; an unwritable destination directory is
// `MM_FLEET_PRIVILEGE_REQUIRED`; `ErrBusy` maps to `MM_FLEET_BUSY`.
func TestNoOpReapplyEmptyDeleteNoPlanCheckDriftIncompletePrivilegeAndBusy(t *testing.T) {
	fx := newFixtureSet(t)
	eh := newExecHarness(t, fx, true)
	destPath := filepath.Join(eh.homeDir, ".ssh", "authorized_keys")

	// Initial plan + apply.
	if _, errOut, code := eh.run("--plan", "--user"); code != 0 || errOut != "" {
		t.Fatalf("initial --plan: exit=%d stderr=%q", code, errOut)
	}
	if _, errOut, code := eh.run("--apply", "--user"); code != 0 || errOut != "" {
		t.Fatalf("initial --apply: exit=%d stderr=%q", code, errOut)
	}
	initialBytes, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Applying again without a new plan is MM_FLEET_NO_PLAN (exit 2).
	noPlanOut, noPlanErr, noPlanCode := eh.run("--apply", "--user")
	if noPlanCode != 2 || noPlanOut != "" || !strings.Contains(noPlanErr, `"diagnostic":"MM_FLEET_NO_PLAN"`) {
		t.Fatalf("reapply without new plan: exit=%d stdout=%q stderr=%q, want 2 MM_FLEET_NO_PLAN",
			noPlanCode, noPlanOut, noPlanErr)
	}

	// 2. `--check` when in sync exits 0 silently.
	chkOut, chkErr, chkCode := eh.run("--check", "--user")
	if chkCode != 0 || chkOut != "" || chkErr != "" {
		t.Fatalf("--check in sync: exit=%d stdout=%q stderr=%q, want 0 silent", chkCode, chkOut, chkErr)
	}

	// 3. Unchanged authorization yields a zero-operation plan whose apply changes nothing.
	eh.now = eh.now.Add(time.Second)
	noopPlanOut, noopPlanErr, noopPlanCode := eh.run("--plan", "--user")
	if noopPlanCode != 0 || noopPlanErr != "" {
		t.Fatalf("unchanged --plan: exit=%d stderr=%q", noopPlanCode, noopPlanErr)
	}
	noopPlanRecs, err := output.ReadCompact([]byte(noopPlanOut))
	if err != nil {
		t.Fatal(err)
	}
	// 1 plan record + 0 operation records + 3 access records = 4 records.
	if len(noopPlanRecs) != 4 {
		t.Fatalf("unchanged --plan records = %d, want 4:\n%s", len(noopPlanRecs), noopPlanOut)
	}
	for _, r := range noopPlanRecs {
		if _, hasOp := r.Get("operation"); hasOp {
			t.Fatalf("unchanged --plan unexpectedly emitted operation record: %v", r.Fields())
		}
	}
	noopApplyOut, noopApplyErr, noopApplyCode := eh.run("--apply", "--user")
	if noopApplyCode != 0 || noopApplyErr != "" || noopApplyOut != "" {
		t.Fatalf("unchanged --apply: exit=%d stdout=%q stderr=%q, want 0 and empty",
			noopApplyCode, noopApplyOut, noopApplyErr)
	}
	afterNoopBytes, err := os.ReadFile(destPath)
	if err != nil || !bytes.Equal(afterNoopBytes, initialBytes) {
		t.Fatalf("unchanged --apply modified destination")
	}

	// 4. `--check` after an external edit to the block exits 3 with
	// MM_FLEET_DRIFT_FOUND and digest-only detail (`actual=sha256:... desired=sha256:...`).
	tampered := strings.Replace(string(initialBytes), " alice\n", " tampered-comment\n", 1)
	if err := os.WriteFile(destPath, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	driftOut, driftErr, driftCode := eh.run("--check", "--user")
	if driftCode != 3 || !strings.Contains(driftErr, `"diagnostic":"MM_FLEET_DRIFT_FOUND"`) {
		t.Fatalf("--check after external edit: exit=%d stdout=%q stderr=%q, want 3 MM_FLEET_DRIFT_FOUND",
			driftCode, driftOut, driftErr)
	}
	if strings.Contains(driftOut, "tampered-comment") || strings.Contains(driftOut, fx.aliceLow.blobB64) {
		t.Errorf("--check leaked key material or comment in output: %q", driftOut)
	}
	driftRecs, err := output.ReadCompact([]byte(driftOut))
	if err != nil || len(driftRecs) != 1 {
		t.Fatalf("drift records: err=%v len=%d out=%q", err, len(driftRecs), driftOut)
	}
	assertFieldOrder(t, "drift[0]", driftRecs[0].Fields(),
		[]string{"id", "operation", "target", "drift", "detail"})
	if gotDrift := fmt.Sprint(mustGet(t, driftRecs[0], "drift")); gotDrift != "block" {
		t.Errorf("drift = %q, want block", gotDrift)
	}
	gotDetail := fmt.Sprint(mustGet(t, driftRecs[0], "detail"))
	if !strings.HasPrefix(gotDetail, "actual=sha256:") || !strings.Contains(gotDetail, " desired=sha256:") {
		t.Errorf("drift detail = %q, want actual=sha256:<hex> desired=sha256:<hex>", gotDetail)
	}

	// Restore initialBytes and add a legacy block: `--check` exits 3 with
	// `drift: legacy-block` both with and without `fleet/cutover.yaml`.
	withLegacy := string(initialBytes) + "# BEGIN managed-machine\n" + fx.aliceLow.line + "\n# END managed-machine\n"
	if err := os.WriteFile(destPath, []byte(withLegacy), 0o600); err != nil {
		t.Fatal(err)
	}
	legOut, legErr, legCode := eh.run("--check", "--user")
	if legCode != 3 || !strings.Contains(legErr, `"diagnostic":"MM_FLEET_DRIFT_FOUND"`) {
		t.Fatalf("--check with legacy block: exit=%d stdout=%q stderr=%q", legCode, legOut, legErr)
	}
	legRecs, err := output.ReadCompact([]byte(legOut))
	if err != nil || len(legRecs) != 1 {
		t.Fatalf("legacy drift records: err=%v len=%d out=%q", err, len(legRecs), legOut)
	}
	if gotDrift := fmt.Sprint(mustGet(t, legRecs[0], "drift")); gotDrift != "legacy-block" {
		t.Errorf("legacy drift = %q, want legacy-block", gotDrift)
	}
	if gotDet := fmt.Sprint(mustGet(t, legRecs[0], "detail")); gotDet != "# BEGIN managed-machine" {
		t.Errorf("legacy detail = %q, want %q", gotDet, "# BEGIN managed-machine")
	}

	// Remove cutover.yaml and verify `--check` STILL reports `drift: legacy-block` (D6).
	if err := os.Remove(filepath.Join(eh.policyDir, "fleet", "cutover.yaml")); err != nil {
		t.Fatal(err)
	}
	preCutOut, preCutErr, preCutCode := eh.run("--check", "--user")
	if preCutCode != 3 || preCutOut != legOut || !strings.Contains(preCutErr, `"diagnostic":"MM_FLEET_DRIFT_FOUND"`) {
		t.Fatalf("--check before cutover: exit=%d stdout=%q stderr=%q", preCutCode, preCutOut, preCutErr)
	}
	writeCutover(t, eh.policyDir)

	// 5. Empty authorized set plans `delete` of an owned block and apply removes
	// the block while preserving lines outside the markers.
	personalLine := "# personal line outside block\n"
	if err := os.WriteFile(destPath, append([]byte(personalLine), initialBytes...), 0o600); err != nil {
		t.Fatal(err)
	}
	// Switch to an account that no access group authorizes ("unlisted_user").
	// Wait: the ledger entry was recorded for config="fleet", target="user", path=destPath,
	// so emptying the access groups for "dev" tests deleting the owned block for "dev".
	if err := os.Remove(filepath.Join(eh.policyDir, "fleet", "access-groups", "admins.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(eh.policyDir, "fleet", "access-groups", "workstations.yaml")); err != nil {
		t.Fatal(err)
	}
	eh.now = eh.now.Add(time.Second)
	delPlanOut, delPlanErr, delPlanCode := eh.run("--plan", "--user")
	if delPlanCode != 0 || delPlanErr != "" {
		t.Fatalf("empty authorized set --plan: exit=%d stderr=%q", delPlanCode, delPlanErr)
	}
	delPlanRecs, err := output.ReadCompact([]byte(delPlanOut))
	if err != nil || len(delPlanRecs) != 2 {
		t.Fatalf("delete plan records: err=%v len=%d out=%q", err, len(delPlanRecs), delPlanOut)
	}
	if act := fmt.Sprint(mustGet(t, delPlanRecs[1], "action")); act != "delete" {
		t.Errorf("delete plan action = %q, want delete", act)
	}
	delApplyOut, delApplyErr, delApplyCode := eh.run("--apply", "--user")
	if delApplyCode != 0 || delApplyErr != "" {
		t.Fatalf("delete --apply: exit=%d stderr=%q", delApplyCode, delApplyErr)
	}
	delApplyRecs, err := output.ReadCompact([]byte(delApplyOut))
	if err != nil || len(delApplyRecs) != 1 {
		t.Fatalf("delete apply records: err=%v len=%d", err, len(delApplyRecs))
	}
	if act := fmt.Sprint(mustGet(t, delApplyRecs[0], "action")); act != "delete" {
		t.Errorf("delete apply action = %q, want delete", act)
	}
	afterDelBytes, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDelBytes) != personalLine {
		t.Errorf("after delete authorized_keys = %q, want %q", string(afterDelBytes), personalLine)
	}
	if chkOut, chkErr, chkCode := eh.run("--check", "--user"); chkCode != 0 || chkOut != "" || chkErr != "" {
		t.Fatalf("--check after delete: exit=%d stdout=%q stderr=%q", chkCode, chkOut, chkErr)
	}

	// 6. `--check` when destination is a directory exits 4 with MM_FLEET_CHECK_INCOMPLETE.
	if err := os.Remove(destPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(destPath, 0o700); err != nil {
		t.Fatal(err)
	}
	incOut, incErr, incCode := eh.run("--check", "--user")
	if incCode != 4 || incOut != "" || !strings.Contains(incErr, `"diagnostic":"MM_FLEET_CHECK_INCOMPLETE"`) {
		t.Fatalf("--check on directory destination: exit=%d stdout=%q stderr=%q, want 4 MM_FLEET_CHECK_INCOMPLETE",
			incCode, incOut, incErr)
	}
	if err := os.RemoveAll(destPath); err != nil {
		t.Fatal(err)
	}

	// 7. Unwritable destination directory -> MM_FLEET_PRIVILEGE_REQUIRED (exit 1).
	ehPriv := newExecHarness(t, fx, true)
	ehPriv.writable = func(string) bool { return false }
	privOut, privErr, privCode := ehPriv.run("--plan", "--user")
	if privCode != 1 || privOut != "" || !strings.Contains(privErr, `"diagnostic":"MM_FLEET_PRIVILEGE_REQUIRED"`) {
		t.Fatalf("unwritable --plan: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_PRIVILEGE_REQUIRED",
			privCode, privOut, privErr)
	}
	privOut, privErr, privCode = ehPriv.run("--apply", "--user")
	if privCode != 1 || privOut != "" || !strings.Contains(privErr, `"diagnostic":"MM_FLEET_PRIVILEGE_REQUIRED"`) {
		t.Fatalf("unwritable --apply: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_PRIVILEGE_REQUIRED",
			privCode, privOut, privErr)
	}

	// 8. `ErrBusy` -> MM_FLEET_BUSY (exit 1) when another handle holds the plan store lock.
	ehBusy := newExecHarness(t, fx, true)
	heldStore, err := plan.Open(ehBusy.stateDir, func() time.Time { return ehBusy.now })
	if err != nil {
		t.Fatalf("open held plan store: %v", err)
	}
	defer heldStore.Close()

	busyOut, busyErr, busyCode := ehBusy.run("--plan", "--user")
	if busyCode != 1 || busyOut != "" || !strings.Contains(busyErr, `"diagnostic":"MM_FLEET_BUSY"`) {
		t.Fatalf("busy --plan: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_BUSY",
			busyCode, busyOut, busyErr)
	}
	busyOut, busyErr, busyCode = ehBusy.run("--apply", "--user")
	if busyCode != 1 || busyOut != "" || !strings.Contains(busyErr, `"diagnostic":"MM_FLEET_BUSY"`) {
		t.Fatalf("busy --apply: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_BUSY",
			busyCode, busyOut, busyErr)
	}
}

// Acceptance 5: Without `fleet/cutover.yaml`, `--plan` and `--apply` exit 1
// with `MM_FLEET_NOT_CUT_OVER` before writing anything (plan store unchanged);
// with it, a destination carrying a user-owned line, a `# BEGIN
// managed-machine` block and a `# BEGIN home-bin new-machine` block plans one
// `update`, prints two `legacy` records with the right key counts, and apply
// leaves exactly the user-owned line plus the new block (golden), with
// `~/.ssh/allowed_signers` untouched.
func TestCutoverPreconditionAndLegacyBlockMigration(t *testing.T) {
	fx := newFixtureSet(t)

	// 1. Without `fleet/cutover.yaml`, `--plan` and `--apply` exit 1 with
	// MM_FLEET_NOT_CUT_OVER before touching the plan store.
	ehNoCut := newExecHarness(t, fx, false)
	for _, action := range []string{"--plan", "--apply"} {
		out, errOut, code := ehNoCut.run(action, "--user")
		if code != 1 || out != "" || !strings.Contains(errOut, `"diagnostic":"MM_FLEET_NOT_CUT_OVER"`) {
			t.Fatalf("%s without cutover.yaml: exit=%d stdout=%q stderr=%q, want 1 MM_FLEET_NOT_CUT_OVER",
				action, code, out, errOut)
		}
		if _, err := os.Stat(filepath.Join(ehNoCut.stateDir, "plans")); !os.IsNotExist(err) {
			t.Fatalf("%s without cutover.yaml touched plan store: %v", action, err)
		}
	}

	// 2. With `fleet/cutover.yaml`, migrate a destination carrying a user-owned
	// line, a `# BEGIN managed-machine` block (2 keys) and a `# BEGIN home-bin
	// new-machine` block (1 key), plus `~/.ssh/allowed_signers`.
	eh := newExecHarness(t, fx, true)
	sshDir := filepath.Join(eh.homeDir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	destPath := filepath.Join(sshDir, "authorized_keys")
	allowedSignersPath := filepath.Join(sshDir, "allowed_signers")

	personalKey := makeEd25519Key(t, 0x41, "dev@personal-key")
	legacyKey1 := makeEd25519Key(t, 0x42, "legacy-one@fleet")
	legacyKey2 := makeEd25519Key(t, 0x43, "legacy-two@fleet")
	legacyKey3 := makeEd25519Key(t, 0x44, "legacy-home-bin@fleet")

	initialAuthorizedKeys := strings.Join([]string{
		personalKey.line,
		"# BEGIN managed-machine",
		"# Managed by managed-machine; do not edit",
		legacyKey1.line,
		legacyKey2.line,
		"# END managed-machine",
		"# BEGIN home-bin new-machine",
		legacyKey3.line,
		"# END home-bin new-machine",
	}, "\n") + "\n"
	if err := os.WriteFile(destPath, []byte(initialAuthorizedKeys), 0o600); err != nil {
		t.Fatal(err)
	}

	allowedSignersContent := []byte("# BEGIN managed-machine\n* " + legacyKey1.line + "\n# END managed-machine\n")
	if err := os.WriteFile(allowedSignersPath, allowedSignersContent, 0o600); err != nil {
		t.Fatal(err)
	}

	planOut, planErr, planCode := eh.run("--plan", "--user")
	if planCode != 0 || planErr != "" {
		t.Fatalf("legacy migration --plan: exit=%d stderr=%q", planCode, planErr)
	}
	assertGoldenBytes(t, "plan-legacy-migration.golden",
		[]byte("exit "+strconv.Itoa(planCode)+"\n--- stdout\n"+planOut+"--- stderr\n"+planErr))

	recs, err := output.ReadCompact([]byte(planOut))
	if err != nil {
		t.Fatal(err)
	}
	// 1 plan + 1 operation (update) + 2 legacy + 3 access = 7 records.
	if len(recs) != 7 {
		t.Fatalf("got %d records, want 7:\n%s", len(recs), planOut)
	}
	if act := fmt.Sprint(mustGet(t, recs[1], "action")); act != "update" {
		t.Errorf("operation action = %q, want update", act)
	}

	assertFieldOrder(t, "legacy[0]", recs[2].Fields(), []string{"legacy_block", "keys", "action"})
	if b := fmt.Sprint(mustGet(t, recs[2], "legacy_block")); b != "# BEGIN managed-machine" {
		t.Errorf("legacy[0].legacy_block = %q, want # BEGIN managed-machine", b)
	}
	if k := fmt.Sprint(mustGet(t, recs[2], "keys")); k != "2" {
		t.Errorf("legacy[0].keys = %q, want 2", k)
	}
	if a := fmt.Sprint(mustGet(t, recs[2], "action")); a != "remove" {
		t.Errorf("legacy[0].action = %q, want remove", a)
	}

	assertFieldOrder(t, "legacy[1]", recs[3].Fields(), []string{"legacy_block", "keys", "action"})
	if b := fmt.Sprint(mustGet(t, recs[3], "legacy_block")); b != "# BEGIN home-bin new-machine" {
		t.Errorf("legacy[1].legacy_block = %q, want # BEGIN home-bin new-machine", b)
	}
	if k := fmt.Sprint(mustGet(t, recs[3], "keys")); k != "1" {
		t.Errorf("legacy[1].keys = %q, want 1", k)
	}
	if a := fmt.Sprint(mustGet(t, recs[3], "action")); a != "remove" {
		t.Errorf("legacy[1].action = %q, want remove", a)
	}

	applyOut, applyErr, applyCode := eh.run("--apply", "--user")
	if applyCode != 0 || applyErr != "" {
		t.Fatalf("legacy migration --apply: exit=%d stderr=%q", applyCode, applyErr)
	}
	assertGoldenBytes(t, "apply-legacy-migration.golden",
		[]byte("exit "+strconv.Itoa(applyCode)+"\n--- stdout\n"+applyOut+"--- stderr\n"+applyErr))

	gotAuthorizedKeys, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenBytes(t, "authorized-keys-after-legacy-migration.golden", gotAuthorizedKeys)

	// Verify ~/.ssh/allowed_signers was left completely untouched.
	gotAllowedSigners, err := os.ReadFile(allowedSignersPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotAllowedSigners, allowedSignersContent) {
		t.Errorf("allowed_signers was modified:\ngot:\n%s\nwant:\n%s",
			string(gotAllowedSigners), string(allowedSignersContent))
	}

	// Verify `--check` after migration exits 0 silently.
	if chkOut, chkErr, chkCode := eh.run("--check", "--user"); chkCode != 0 || chkOut != "" || chkErr != "" {
		t.Fatalf("--check after legacy migration: exit=%d stdout=%q stderr=%q", chkCode, chkOut, chkErr)
	}
}

func mustGet(t *testing.T, r *output.Record, key string) any {
	t.Helper()
	v, ok := r.Get(key)
	if !ok {
		t.Fatalf("record %v missing key %q", r.Fields(), key)
	}
	return v
}

func assertGoldenBytes(t *testing.T, name string, got []byte) {
	t.Helper()
	goldenPath := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s: %v", goldenPath, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("differs from %s:\ngot:\n%s\nwant:\n%s", goldenPath, string(got), string(want))
	}
}
