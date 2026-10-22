package status_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/config"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/fleet"
	"github.com/qwts/managed-machine/core/internal/fleet/directory"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/plan"
	"github.com/qwts/managed-machine/core/internal/provider"
	"github.com/qwts/managed-machine/core/internal/statedir"
	"github.com/qwts/managed-machine/core/internal/status"
	"github.com/qwts/managed-machine/core/internal/target"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

type fleetFixtureKey struct {
	line        string
	blobB64     string
	fingerprint string
	machineID   string
}

func makeFleetEd25519Key(t *testing.T, seedByte byte, comment string) fleetFixtureKey {
	t.Helper()
	seed := bytes.Repeat([]byte{seedByte}, ed25519.SeedSize)
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	var blob bytes.Buffer
	var lenBuf [4]byte
	typeBytes := []byte(directory.KeyTypeEd25519)
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(typeBytes)))
	blob.Write(lenBuf[:])
	blob.Write(typeBytes)
	binary.BigEndian.PutUint32(lenBuf[:], uint32(len(pub)))
	blob.Write(lenBuf[:])
	blob.Write(pub)

	b64 := base64.StdEncoding.EncodeToString(blob.Bytes())
	line := directory.KeyTypeEd25519 + " " + b64 + " " + comment
	pk, err := directory.ParsePublicKey(line)
	if err != nil {
		t.Fatalf("ParsePublicKey(%q): %v", comment, err)
	}
	return fleetFixtureKey{
		line:        line,
		blobB64:     b64,
		fingerprint: pk.Fingerprint,
		machineID:   pk.MachineID,
	}
}

type fleetFixtures struct {
	aliceLow        fleetFixtureKey
	aliceHigh       fleetFixtureKey
	aliceUnapproved fleetFixtureKey
	bobValid        fleetFixtureKey
	bobMismatchRec  fleetFixtureKey
	bobMismatchAct  fleetFixtureKey
	carolUnapproved fleetFixtureKey

	aliceYAML string
	bobYAML   string
	carolYAML string
}

func newFleetFixtures(t *testing.T) fleetFixtures {
	t.Helper()
	kA := makeFleetEd25519Key(t, 0x11, "alice@workstation-a")
	kB := makeFleetEd25519Key(t, 0x12, "alice@workstation-b")
	low, high := kA, kB
	if low.fingerprint > high.fingerprint {
		low, high = high, low
	}
	unapproved := makeFleetEd25519Key(t, 0x13, "alice@unapproved-laptop")
	bobValid := makeFleetEd25519Key(t, 0x14, "bob@desktop")
	bobMismatchRec := makeFleetEd25519Key(t, 0x15, "bob@recorded")
	bobMismatchAct := makeFleetEd25519Key(t, 0x16, "bob@actual")
	carolUnapproved := makeFleetEd25519Key(t, 0x17, "carol@unapproved")

	aliceYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: alice
keys:
  - fingerprint: %s
    key: %s
    label: alice-high
    published_at: "2026-10-22T01:00:00Z"
    revision: 2
  - fingerprint: %s
    key: %s
    label: alice-low
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: alice-unapproved
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		high.fingerprint, high.line,
		low.fingerprint, low.line,
		unapproved.fingerprint, unapproved.line,
	)

	bobYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: bob
keys:
  - fingerprint: %s
    key: %s
    label: bob-workstation
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
  - fingerprint: %s
    key: %s
    label: bob-bad-fp
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		bobValid.fingerprint, bobValid.line,
		bobMismatchRec.fingerprint, bobMismatchAct.line,
	)

	carolYAML := fmt.Sprintf(`schema_version: 1
kind: key-directory
id: carol
keys:
  - fingerprint: %s
    key: %s
    label: carol-laptop
    published_at: "2026-10-22T01:00:00Z"
    revision: 1
`,
		carolUnapproved.fingerprint, carolUnapproved.line,
	)

	return fleetFixtures{
		aliceLow:        low,
		aliceHigh:       high,
		aliceUnapproved: unapproved,
		bobValid:        bobValid,
		bobMismatchRec:  bobMismatchRec,
		bobMismatchAct:  bobMismatchAct,
		carolUnapproved: carolUnapproved,
		aliceYAML:       aliceYAML,
		bobYAML:         bobYAML,
		carolYAML:       carolYAML,
	}
}

type fleetTestEnv struct {
	t         *testing.T
	base      string
	homeDir   string
	stateDir  string
	policyDir string
	keyDir    string
	binDir    string
	sentinel  string
	env       map[string]string
	goos      string
	username  string
	now       time.Time
	client    *http.Client
	planSeq   int
}

func newFleetTestEnv(t *testing.T, fx fleetFixtures, withCutover bool) *fleetTestEnv {
	t.Helper()
	base := t.TempDir()
	homeDir := filepath.Join(base, "home")
	stateDir := filepath.Join(base, "state")
	policyDir := filepath.Join(base, "policy")
	keyDir := filepath.Join(base, "key-directory")
	binDir := filepath.Join(base, "bin")
	sentinel := filepath.Join(base, "sentinel-invoked")

	for _, d := range []string{homeDir, stateDir, binDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tool := range []string{
		"git", "ssh-keygen", "ssh-add", "ssh", "gh", "osascript", "sudo", "security", "dscl",
		"brew", "uv", "npm", "cargo", "go", "winget", "pass", "gpg",
	} {
		writeSentinelTool(t, binDir, tool, sentinel)
	}

	writeFleetKeyDir(t, keyDir, fx)
	writeFleetPolicyDir(t, policyDir, "path: '"+filepath.ToSlash(keyDir)+"'\n", fx, withCutover)

	return &fleetTestEnv{
		t:         t,
		base:      base,
		homeDir:   homeDir,
		stateDir:  stateDir,
		policyDir: policyDir,
		keyDir:    keyDir,
		binDir:    binDir,
		sentinel:  sentinel,
		env: map[string]string{
			fleet.PolicyEnv:   policyDir,
			statedir.Override: stateDir,
			"HOME":            homeDir,
			"USERPROFILE":     homeDir,
			"PATH":            binDir,
			"PATHEXT":         ".COM;.EXE;.BAT;.CMD",
		},
		goos:     "darwin",
		username: "dev",
		now:      time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC),
	}
}

func writeFleetKeyDir(t *testing.T, keyDir string, fx fleetFixtures) {
	t.Helper()
	keysSub := filepath.Join(keyDir, "keys")
	if err := os.MkdirAll(keysSub, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"alice.yaml": fx.aliceYAML,
		"bob.yaml":   fx.bobYAML,
		"carol.yaml": fx.carolYAML,
	} {
		if err := os.WriteFile(filepath.Join(keysSub, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func writeFleetPolicyDir(t *testing.T, policyDir, kdSourceLine string, fx fleetFixtures, withCutover bool) {
	t.Helper()
	groupsDir := filepath.Join(policyDir, "fleet", "access-groups")
	if err := os.MkdirAll(groupsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	kdYAML := "schema_version: 1\nkind: key-directory-source\n" + kdSourceLine
	if err := os.WriteFile(filepath.Join(policyDir, "fleet", "key-directory.yaml"), []byte(kdYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	groupAdmins := fmt.Sprintf(`schema_version: 1
kind: access-group
id: admins
description: Core administrators.
maintained_by: [alice]
accounts: [dev, root_admin]
approved_fingerprints:
  - %s
  - %s
  - %s
`,
		fx.aliceHigh.fingerprint,
		fx.bobValid.fingerprint,
		fx.bobMismatchRec.fingerprint,
	)
	groupWorkstations := fmt.Sprintf(`schema_version: 1
kind: access-group
id: workstations
description: Developer workstations.
maintained_by: [alice, bob]
accounts: [dev]
approved_fingerprints:
  - %s
  - %s
`,
		fx.aliceLow.fingerprint,
		fx.bobValid.fingerprint,
	)
	if err := os.WriteFile(filepath.Join(groupsDir, "admins.yaml"), []byte(groupAdmins), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(groupsDir, "workstations.yaml"), []byte(groupWorkstations), 0o644); err != nil {
		t.Fatal(err)
	}
	cutoverPath := filepath.Join(policyDir, "fleet", "cutover.yaml")
	if withCutover {
		const cutoverYAML = `schema_version: 1
kind: fleet-cutover
cut_over_at: "2026-10-22T00:00:00Z"
reviewed_by: [alice]
note: S5 fleet cutover.
`
		if err := os.WriteFile(cutoverPath, []byte(cutoverYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = os.Remove(cutoverPath)
	}
}

func (fe *fleetTestEnv) detectHost() (target.Host, error) {
	switch fe.goos {
	case "windows":
		return target.Host{Target: target.WindowsAMD64}, nil
	case "linux":
		return target.Host{Target: target.LinuxAMD64}, nil
	default:
		return target.Host{Target: target.DarwinARM64}, nil
	}
}

func (fe *fleetTestEnv) runStatus(args ...string) (string, string, int) {
	fe.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &stdout,
		Stderr: &stderr,
		Detect: fe.detectHost,
		Commands: []cli.Command{
			status.Command(status.Deps{
				Getenv:   func(k string) string { return fe.env[k] },
				GOOS:     fe.goos,
				StateDir: func() (string, error) { return fe.stateDir, nil },
				Username: func() (string, error) { return fe.username, nil },
				Home:     func() (string, error) { return fe.homeDir, nil },
				Now:      func() time.Time { return fe.now },
			}),
		},
	}
	code := app.Run(append([]string{"status"}, args...))
	if data, err := os.ReadFile(fe.sentinel); err == nil {
		fe.t.Fatalf("status spawned sentinel process: %s", string(data))
	}
	return stdout.String(), stderr.String(), code
}

func (fe *fleetTestEnv) runFleet(args ...string) (string, string, int) {
	fe.t.Helper()
	var stdout, stderr bytes.Buffer
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &stdout,
		Stderr: &stderr,
		Detect: fe.detectHost,
		Commands: []cli.Command{
			fleet.Command(fleet.Deps{
				Getenv:   func(k string) string { return fe.env[k] },
				GOOS:     fe.goos,
				StateDir: func() (string, error) { return fe.stateDir, nil },
				Username: func() (string, error) { return fe.username, nil },
				Home:     func() (string, error) { return fe.homeDir, nil },
				Now:      func() time.Time { return fe.now },
				Client:   fe.client,
				Machine:  func(context.Context) (string, error) { return "machine-test", nil },
				NewPlanID: func(now time.Time) (string, error) {
					fe.planSeq++
					return fmt.Sprintf("plan-%s-%04x", now.UTC().Format("20060102T150405Z"), fe.planSeq), nil
				},
				Runner: func(ctx context.Context, exe string, argv, env []string) (provider.Result, error) {
					return provider.Result{}, fmt.Errorf("unexpected process execution: %s %v", exe, argv)
				},
			}),
		},
	}
	code := app.Run(append([]string{"fleet"}, args...))
	if data, err := os.ReadFile(fe.sentinel); err == nil {
		fe.t.Fatalf("fleet spawned sentinel process: %s", string(data))
	}
	return stdout.String(), stderr.String(), code
}

type statusGistServer struct {
	srv      *httptest.Server
	client   *http.Client
	hits     int64
	gistID   string
	revision string
	files    map[string]map[string]any
}

func newStatusGistServer(t *testing.T, gistID, revision string, files map[string]string) *statusGistServer {
	t.Helper()
	gs := &statusGistServer{
		gistID:   gistID,
		revision: revision,
		files:    make(map[string]map[string]any, len(files)),
	}
	for name, content := range files {
		gs.files[name] = map[string]any{
			"filename":  name,
			"size":      len(content),
			"truncated": false,
			"content":   content,
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&gs.hits, 1)
		switch r.URL.Path {
		case "/gists/" + gs.gistID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"history": []map[string]string{{"version": gs.revision}},
			})
		case "/gists/" + gs.gistID + "/" + gs.revision:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"files": gs.files,
			})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	gs.srv = srv
	gs.client = srv.Client()
	return gs
}

func TestFleetSectionPathDirectoryAllThreeModesAndNameFilters(t *testing.T) {
	fx := newFleetFixtures(t)
	fe := newFleetTestEnv(t, fx, true)

	// 1. Plan and apply fleet authorized-keys at 01:20:00Z.
	fe.now = time.Date(2026, 10, 22, 1, 20, 0, 0, time.UTC)
	if _, errOut, code := fe.runFleet("--plan", "--user"); code != 0 {
		t.Fatalf("fleet --plan: exit %d %q", code, errOut)
	}
	if _, errOut, code := fe.runFleet("--apply", "--user"); code != 0 {
		t.Fatalf("fleet --apply: exit %d %q", code, errOut)
	}

	// 2. Write an enrollment record via `fleet --publish` at 01:30:00Z.
	fe.now = time.Date(2026, 10, 22, 1, 30, 0, 0, time.UTC)
	pubKeyPath := filepath.Join(fe.base, "alice-low.pub")
	if err := os.WriteFile(pubKeyPath, []byte(fx.aliceLow.line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := fe.runFleet("--publish", "--owner", "alice", "--key", pubKeyPath, "--label", "alice-low"); code != 0 {
		t.Fatalf("fleet --publish: exit %d %q", code, errOut)
	}

	// 3. Advance status clock to 02:00:00Z and run `status --section fleet`.
	fe.now = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	out, errOut, code := fe.runStatus("--section", "fleet")
	if code != 0 || errOut != "" {
		t.Fatalf("status --section fleet: exit=%d stderr=%q", code, errOut)
	}
	assertKeyOrder(t, out)

	recs, err := output.ReadCompact([]byte(out))
	if err != nil {
		t.Fatalf("ReadCompact: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("expected 4 fleet records, got %d:\n%s", len(recs), out)
	}

	dirLoaded, err := directory.LoadPath(fe.keyDir)
	if err != nil {
		t.Fatal(err)
	}
	wantMachineID := fx.aliceLow.machineID

	// Verify resource 0: directory (present / observed for a path: directory).
	rDir := recs[0]
	if s, _ := rDir.Get("section"); s != "fleet" {
		t.Errorf("rDir.section = %v, want fleet", s)
	}
	if r, _ := rDir.Get("resource"); r != "directory" {
		t.Errorf("rDir.resource = %v, want directory", r)
	}
	if tg, _ := rDir.Get("target"); tg != "user" {
		t.Errorf("rDir.target = %v, want user", tg)
	}
	if st, _ := rDir.Get("state"); st != "present" {
		t.Errorf("rDir.state = %v, want present", st)
	}
	if ev, _ := rDir.Get("evidence"); ev != "observed" {
		t.Errorf("rDir.evidence = %v, want observed", ev)
	}
	if at, _ := rDir.Get("at"); at != "2026-10-22T02:00:00Z" {
		t.Errorf("rDir.at = %v, want 2026-10-22T02:00:00Z", at)
	}
	wantDirLine := fmt.Sprintf(
		`{"section":"fleet","resource":"directory","target":"user","state":"present","facts":{"invalid":"1","keys":"6","owners":"3","source":%q},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`,
		dirLoaded.Identity,
	)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != wantDirLine {
		t.Errorf("line 0:\ngot  %s\nwant %s", lines[0], wantDirLine)
	}

	// Verify resource 1: policy (present / observed, cutover: present).
	wantPolLine := `{"section":"fleet","resource":"policy","target":"user","state":"present","facts":{"accounts":"2","cutover":"present","groups":"2"},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	if lines[1] != wantPolLine {
		t.Errorf("line 1:\ngot  %s\nwant %s", lines[1], wantPolLine)
	}

	// Verify resource 2: authorized-keys (present / observed, legacy_block: absent).
	wantAuthLine := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"present","facts":{"account":"dev","authorized":"3","legacy_block":"absent"},"evidence":"observed","at":"2026-10-22T01:20:00Z"}`
	if lines[2] != wantAuthLine {
		t.Errorf("line 2:\ngot  %s\nwant %s", lines[2], wantAuthLine)
	}

	// Verify resource 3: enrollment (present / recorded, at = enrollment.At).
	wantEnrollLine := fmt.Sprintf(
		`{"section":"fleet","resource":"enrollment","target":"user","state":"present","facts":{"fingerprint":%q,"machine_id":%q,"outcome":"written","owner":"alice"},"evidence":"recorded","at":"2026-10-22T01:30:00Z"}`,
		fx.aliceLow.fingerprint,
		wantMachineID,
	)
	if lines[3] != wantEnrollLine {
		t.Errorf("line 3:\ngot  %s\nwant %s", lines[3], wantEnrollLine)
	}

	// `--user` flag produces identical output.
	outUser, errUser, codeUser := fe.runStatus("--section", "fleet", "--user")
	if codeUser != 0 || errUser != "" || outUser != out {
		t.Errorf("status --section fleet --user differs: code=%d err=%q", codeUser, errUser)
	}

	// `--json` mode preserves key order and emits all 4 records.
	outJSON, errJSON, codeJSON := fe.runStatus("--section", "fleet", "--json")
	if codeJSON != 0 || errJSON != "" {
		t.Fatalf("status --section fleet --json: exit=%d err=%q", codeJSON, errJSON)
	}
	var jsonDoc struct {
		Records []*output.Record `json:"records"`
	}
	if err := json.Unmarshal([]byte(outJSON), &jsonDoc); err != nil {
		t.Fatalf("unmarshal --json: %v", err)
	}
	if len(jsonDoc.Records) != 4 {
		t.Fatalf("--json record count = %d, want 4", len(jsonDoc.Records))
	}

	// `--human` mode renders the D7 columns and all 4 resource rows.
	outHuman, errHuman, codeHuman := fe.runStatus("--section", "fleet", "--human")
	if codeHuman != 0 || errHuman != "" {
		t.Fatalf("status --section fleet --human: exit=%d err=%q", codeHuman, errHuman)
	}
	header := strings.Split(outHuman, "\n")[0]
	for _, col := range []string{"SECTION", "RESOURCE", "TARGET", "STATE", "FACTS", "EVIDENCE", "AT"} {
		if !strings.Contains(header, col) {
			t.Errorf("human header %q missing column %s", header, col)
		}
	}
	for _, resName := range []string{"directory", "policy", "authorized-keys", "enrollment"} {
		if !strings.Contains(outHuman, resName) {
			t.Errorf("human output missing resource %q:\n%s", resName, outHuman)
		}
	}

	// Single-resource positional filter `[name]`.
	for idx, resName := range []string{"directory", "policy", "authorized-keys", "enrollment"} {
		singleOut, singleErr, singleCode := fe.runStatus("--section", "fleet", resName)
		if singleCode != 0 || singleErr != "" {
			t.Fatalf("status --section fleet %s: exit=%d err=%q", resName, singleCode, singleErr)
		}
		assertKeyOrder(t, singleOut)
		if strings.TrimSpace(singleOut) != lines[idx] {
			t.Errorf("status --section fleet %s:\ngot  %s\nwant %s", resName, strings.TrimSpace(singleOut), lines[idx])
		}
	}

	// Removing `fleet/cutover.yaml` flips `facts.cutover` to `absent`.
	if err := os.Remove(filepath.Join(fe.policyDir, "fleet", "cutover.yaml")); err != nil {
		t.Fatal(err)
	}
	polOut, polErr, polCode := fe.runStatus("--section", "fleet", "policy")
	if polCode != 0 || polErr != "" {
		t.Fatalf("status policy without cutover: exit=%d err=%q", polCode, polErr)
	}
	wantPolNoCutover := `{"section":"fleet","resource":"policy","target":"user","state":"present","facts":{"accounts":"2","cutover":"absent","groups":"2"},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	if strings.TrimSpace(polOut) != wantPolNoCutover {
		t.Errorf("policy without cutover:\ngot  %s\nwant %s", strings.TrimSpace(polOut), wantPolNoCutover)
	}
}

func TestFleetSectionCachedGistDirectoryAndUncachedZeroHits(t *testing.T) {
	fx := newFleetFixtures(t)
	fe := newFleetTestEnv(t, fx, false)

	const gistID = "0123456789abcdef0123456789abcdef"
	const rev = "0123456789abcdef0123456789abcdef01234567"

	gs := newStatusGistServer(t, gistID, rev, map[string]string{
		"alice.yaml": fx.aliceYAML,
		"bob.yaml":   fx.bobYAML,
		"carol.yaml": fx.carolYAML,
	})
	fe.client = gs.client
	fe.env[fleet.GitHubAPIEnv] = gs.srv.URL
	writeFleetPolicyDir(t, fe.policyDir, "gist: "+gistID+"\n", fx, false)

	// 1. Before `fleet --refresh`, `directory` is `absent`/`observed` and zero HTTP requests occur.
	outBefore, errBefore, codeBefore := fe.runStatus("--section", "fleet")
	if codeBefore != 0 || errBefore != "" {
		t.Fatalf("uncached gist status: exit=%d err=%q", codeBefore, errBefore)
	}
	if gotHits := atomic.LoadInt64(&gs.hits); gotHits != 0 {
		t.Fatalf("status made %d HTTP requests, want 0", gotHits)
	}
	assertKeyOrder(t, outBefore)
	linesBefore := strings.Split(strings.TrimSpace(outBefore), "\n")
	if len(linesBefore) != 4 {
		t.Fatalf("expected 4 records, got %d:\n%s", len(linesBefore), outBefore)
	}
	wantAbsentDir := `{"section":"fleet","resource":"directory","target":"user","state":"absent","facts":{},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	if linesBefore[0] != wantAbsentDir {
		t.Errorf("uncached directory:\ngot  %s\nwant %s", linesBefore[0], wantAbsentDir)
	}
	wantAbsentEnroll := `{"section":"fleet","resource":"enrollment","target":"user","state":"absent","facts":{},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	if linesBefore[3] != wantAbsentEnroll {
		t.Errorf("absent enrollment:\ngot  %s\nwant %s", linesBefore[3], wantAbsentEnroll)
	}

	// 2. Run `fleet --refresh` at 01:15:00Z.
	fe.now = time.Date(2026, 10, 22, 1, 15, 0, 0, time.UTC)
	if _, errRefresh, codeRefresh := fe.runFleet("--refresh"); codeRefresh != 0 {
		t.Fatalf("fleet --refresh: exit=%d err=%q", codeRefresh, errRefresh)
	}
	hitsAfterRefresh := atomic.LoadInt64(&gs.hits)
	if hitsAfterRefresh != 2 {
		t.Fatalf("refresh hits = %d, want 2", hitsAfterRefresh)
	}

	// 3. Stage a publication against the gist directory at 01:45:00Z.
	fe.now = time.Date(2026, 10, 22, 1, 45, 0, 0, time.UTC)
	pubKeyPath := filepath.Join(fe.base, "alice-low.pub")
	if err := os.WriteFile(pubKeyPath, []byte(fx.aliceLow.line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errPub, codePub := fe.runFleet("--publish", "--owner", "alice", "--key", pubKeyPath); codePub != 0 {
		t.Fatalf("fleet --publish staged: exit=%d err=%q", codePub, errPub)
	}

	// 4. Advance status clock to 02:30:00Z and run `status --section fleet`.
	// `directory` must report `present`/`recorded` with `at` = 2026-10-22T01:15:00Z
	// (the cached snapshot fetch time) and make zero additional network requests.
	fe.now = time.Date(2026, 10, 22, 2, 30, 0, 0, time.UTC)
	outAfter, errAfter, codeAfter := fe.runStatus("--section", "fleet")
	if codeAfter != 0 || errAfter != "" {
		t.Fatalf("cached gist status: exit=%d err=%q", codeAfter, errAfter)
	}
	if gotHits := atomic.LoadInt64(&gs.hits) - hitsAfterRefresh; gotHits != 0 {
		t.Fatalf("status made %d HTTP requests, want 0", gotHits)
	}
	assertKeyOrder(t, outAfter)
	linesAfter := strings.Split(strings.TrimSpace(outAfter), "\n")
	wantCachedDir := fmt.Sprintf(
		`{"section":"fleet","resource":"directory","target":"user","state":"present","facts":{"invalid":"1","keys":"6","owners":"3","source":%q},"evidence":"recorded","at":"2026-10-22T01:15:00Z"}`,
		"gist:"+gistID+"@"+rev,
	)
	if linesAfter[0] != wantCachedDir {
		t.Errorf("cached directory:\ngot  %s\nwant %s", linesAfter[0], wantCachedDir)
	}
	wantMachineID := fx.aliceLow.machineID
	wantStagedEnroll := fmt.Sprintf(
		`{"section":"fleet","resource":"enrollment","target":"user","state":"present","facts":{"fingerprint":%q,"machine_id":%q,"outcome":"staged","owner":"alice"},"evidence":"recorded","at":"2026-10-22T01:45:00Z"}`,
		fx.aliceLow.fingerprint,
		wantMachineID,
	)
	if linesAfter[3] != wantStagedEnroll {
		t.Errorf("staged enrollment:\ngot  %s\nwant %s", linesAfter[3], wantStagedEnroll)
	}
}

func TestFleetSectionAuthorizedKeysStatesAndLegacyBlock(t *testing.T) {
	fx := newFleetFixtures(t)
	fe := newFleetTestEnv(t, fx, true)

	fe.now = time.Date(2026, 10, 22, 1, 20, 0, 0, time.UTC)
	if _, errOut, code := fe.runFleet("--plan", "--user"); code != 0 {
		t.Fatalf("fleet --plan: exit %d %q", code, errOut)
	}
	if _, errOut, code := fe.runFleet("--apply", "--user"); code != 0 {
		t.Fatalf("fleet --apply: exit %d %q", code, errOut)
	}

	fe.now = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	authPath := filepath.Join(fe.homeDir, ".ssh", "authorized_keys")
	origBytes, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Matching block with a legacy `# BEGIN managed-machine` block appended -> present/observed, legacy_block: present.
	withLegacy := string(origBytes) + "# BEGIN managed-machine\n" + fx.carolUnapproved.line + "\n# END managed-machine\n"
	if err := os.WriteFile(authPath, []byte(withLegacy), 0o600); err != nil {
		t.Fatal(err)
	}
	out1, err1, code1 := fe.runStatus("--section", "fleet", "authorized-keys")
	if code1 != 0 || err1 != "" {
		t.Fatalf("present with legacy block: exit=%d err=%q", code1, err1)
	}
	want1 := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"present","facts":{"account":"dev","authorized":"3","legacy_block":"present"},"evidence":"observed","at":"2026-10-22T01:20:00Z"}`
	if strings.TrimSpace(out1) != want1 {
		t.Errorf("present with legacy block:\ngot  %s\nwant %s", strings.TrimSpace(out1), want1)
	}

	// 2. Modified managed block + `# BEGIN home-bin new-machine` legacy block -> absent/observed, legacy_block: present.
	modified := strings.Replace(string(origBytes), "alice", "tampered", 1) +
		"# BEGIN home-bin new-machine\n" + fx.carolUnapproved.line + "\n# END home-bin new-machine\n"
	if err := os.WriteFile(authPath, []byte(modified), 0o600); err != nil {
		t.Fatal(err)
	}
	out2, err2, code2 := fe.runStatus("--section", "fleet", "authorized-keys")
	if code2 != 0 || err2 != "" {
		t.Fatalf("modified block: exit=%d err=%q", code2, err2)
	}
	want2 := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"absent","facts":{"account":"dev","authorized":"0","legacy_block":"present"},"evidence":"observed","at":"2026-10-22T01:20:00Z"}`
	if strings.TrimSpace(out2) != want2 {
		t.Errorf("modified block:\ngot  %s\nwant %s", strings.TrimSpace(out2), want2)
	}

	// 3. Managed block deleted entirely -> absent/observed, legacy_block: absent.
	if err := os.WriteFile(authPath, []byte("# user key only\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out3, err3, code3 := fe.runStatus("--section", "fleet", "authorized-keys")
	if code3 != 0 || err3 != "" {
		t.Fatalf("deleted block: exit=%d err=%q", code3, err3)
	}
	want3 := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"absent","facts":{"account":"dev","authorized":"0","legacy_block":"absent"},"evidence":"observed","at":"2026-10-22T01:20:00Z"}`
	if strings.TrimSpace(out3) != want3 {
		t.Errorf("deleted block:\ngot  %s\nwant %s", strings.TrimSpace(out3), want3)
	}

	// 4. Destination file removed -> absent/observed.
	if err := os.Remove(authPath); err != nil {
		t.Fatal(err)
	}
	out4, err4, code4 := fe.runStatus("--section", "fleet", "authorized-keys")
	if code4 != 0 || err4 != "" {
		t.Fatalf("removed destination: exit=%d err=%q", code4, err4)
	}
	if strings.TrimSpace(out4) != want3 {
		t.Errorf("removed destination:\ngot  %s\nwant %s", strings.TrimSpace(out4), want3)
	}

	// 5. Destination replaced by a directory (portable unreadable file across Darwin, Linux and Windows)
	// -> unknown/recorded (never rendered as absent).
	if err := os.MkdirAll(authPath, 0o755); err != nil {
		t.Fatal(err)
	}
	out5, err5, code5 := fe.runStatus("--section", "fleet", "authorized-keys")
	if code5 != 0 || err5 != "" {
		t.Fatalf("unreadable destination: exit=%d err=%q", code5, err5)
	}
	want5 := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"unknown","facts":{"account":"dev","authorized":"0","legacy_block":"absent"},"evidence":"recorded","at":"2026-10-22T01:20:00Z"}`
	if strings.TrimSpace(out5) != want5 {
		t.Errorf("unreadable destination:\ngot  %s\nwant %s", strings.TrimSpace(out5), want5)
	}
	if strings.Contains(out5, `"state":"absent"`) {
		t.Fatal("unreadable destination must never be rendered as absent")
	}

	// 6. Windows DOMAIN\username stripping and desired.yaml destination override when no ledger exists.
	feWin := newFleetTestEnv(t, fx, true)
	feWin.goos = "windows"
	feWin.username = `CORP\dev`
	customAuth := filepath.Join(feWin.base, "custom-ssh", "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(customAuth), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customAuth, []byte("# BEGIN managed-machine\n"+fx.aliceLow.line+"\n# END managed-machine\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	desiredPath := filepath.Join(feWin.base, "desired.yaml")
	desiredYAML := fmt.Sprintf("target: user\nfleet:\n  destinations:\n    authorized_keys: '%s'\n", filepath.ToSlash(customAuth))
	if err := os.WriteFile(desiredPath, []byte(desiredYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	feWin.env[config.DesiredEnv] = desiredPath

	outWin, errWin, codeWin := feWin.runStatus("--section", "fleet", "authorized-keys")
	if codeWin != 0 || errWin != "" {
		t.Fatalf("windows desired override: exit=%d err=%q", codeWin, errWin)
	}
	wantWin := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"absent","facts":{"account":"dev","authorized":"0","legacy_block":"present"},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	if strings.TrimSpace(outWin) != wantWin {
		t.Errorf("windows desired override:\ngot  %s\nwant %s", strings.TrimSpace(outWin), wantWin)
	}
}

func TestFleetSectionUsageErrorsAndMissingPolicyDir(t *testing.T) {
	fx := newFleetFixtures(t)
	fe := newFleetTestEnv(t, fx, false)
	customDir := filepath.Join(fe.base, "soul-root")

	usageCases := []struct {
		name       string
		args       []string
		wantSubstr []string
	}{
		{
			name:       "global_after_section",
			args:       []string{"--section", "fleet", "--global"},
			wantSubstr: []string{"--user is the only target", "OS accounts", "Souls", "sshd"},
		},
		{
			name:       "global_before_section",
			args:       []string{"--global", "--section", "fleet"},
			wantSubstr: []string{"--user is the only target", "OS accounts", "Souls", "sshd"},
		},
		{
			name:       "custom_with_path",
			args:       []string{"--section", "fleet", "--custom", customDir},
			wantSubstr: []string{"--user is the only target", "OS accounts", "Souls", "sshd"},
		},
		{
			name:       "custom_equals_path",
			args:       []string{"--section", "fleet", "--custom=" + customDir},
			wantSubstr: []string{"--user is the only target", "OS accounts", "Souls", "sshd"},
		},
		{
			name:       "custom_without_path",
			args:       []string{"--section", "fleet", "--custom"},
			wantSubstr: []string{"--user is the only target", "OS accounts", "Souls", "sshd"},
		},
		{
			name:       "custom_before_section",
			args:       []string{"--custom", customDir, "--section", "fleet"},
			wantSubstr: []string{"--user is the only target", "OS accounts", "Souls", "sshd"},
		},
		{
			name:       "unknown_fleet_resource",
			args:       []string{"--section", "fleet", "bogus"},
			wantSubstr: []string{"unknown fleet resource", "bogus"},
		},
		{
			name:       "comma_section_list",
			args:       []string{"--section", "fleet,config"},
			wantSubstr: []string{"comma list"},
		},
		{
			name:       "comma_section_equals",
			args:       []string{"--section=fleet,software"},
			wantSubstr: []string{"comma list"},
		},
	}

	for _, tc := range usageCases {
		t.Run(tc.name, func(t *testing.T) {
			out, errOut, code := fe.runStatus(tc.args...)
			if code != diag.ExitUsage {
				t.Fatalf("args %v: exit = %d, want %d; stdout=%q stderr=%q", tc.args, code, diag.ExitUsage, out, errOut)
			}
			if out != "" {
				t.Errorf("args %v: expected empty stdout, got %q", tc.args, out)
			}
			if !strings.Contains(errOut, `"diagnostic":"MM_USAGE"`) {
				t.Errorf("args %v: expected MM_USAGE on stderr, got %q", tc.args, errOut)
			}
			for _, sub := range tc.wantSubstr {
				if !strings.Contains(errOut, sub) {
					t.Errorf("args %v: stderr %q missing %q", tc.args, errOut, sub)
				}
			}
		})
	}

	// Missing policy directory yields `policy` `absent` (and `directory` `absent`) and still exits 0.
	fe.env[fleet.PolicyEnv] = filepath.Join(fe.base, "missing-policy-checkout")
	outMissing, errMissing, codeMissing := fe.runStatus("--section", "fleet")
	if codeMissing != 0 || errMissing != "" {
		t.Fatalf("missing policy directory: exit=%d err=%q", codeMissing, errMissing)
	}
	assertKeyOrder(t, outMissing)
	lines := strings.Split(strings.TrimSpace(outMissing), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 records when policy directory is missing, got %d:\n%s", len(lines), outMissing)
	}
	wantDirAbsent := `{"section":"fleet","resource":"directory","target":"user","state":"absent","facts":{},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	wantPolAbsent := `{"section":"fleet","resource":"policy","target":"user","state":"absent","facts":{},"evidence":"observed","at":"2026-10-22T02:00:00Z"}`
	if lines[0] != wantDirAbsent {
		t.Errorf("missing policy dir line 0:\ngot  %s\nwant %s", lines[0], wantDirAbsent)
	}
	if lines[1] != wantPolAbsent {
		t.Errorf("missing policy dir line 1:\ngot  %s\nwant %s", lines[1], wantPolAbsent)
	}

	// Also verify default `<state>/managed-machine-config` when MANAGED_MACHINE_POLICY_DIR is unset and absent.
	delete(fe.env, fleet.PolicyEnv)
	outDefaultAbsent, errDefaultAbsent, codeDefaultAbsent := fe.runStatus("--section", "fleet", "policy")
	if codeDefaultAbsent != 0 || errDefaultAbsent != "" || strings.TrimSpace(outDefaultAbsent) != wantPolAbsent {
		t.Errorf("default absent policy dir: exit=%d out=%q err=%q", codeDefaultAbsent, outDefaultAbsent, errDefaultAbsent)
	}
}

func TestFleetSectionUnreadableLedgerPartialExit4(t *testing.T) {
	fx := newFleetFixtures(t)
	fe := newFleetTestEnv(t, fx, true)

	// Populate enrollment so all other resources are present.
	fe.now = time.Date(2026, 10, 22, 1, 30, 0, 0, time.UTC)
	pubKeyPath := filepath.Join(fe.base, "alice-low.pub")
	if err := os.WriteFile(pubKeyPath, []byte(fx.aliceLow.line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := fe.runFleet("--publish", "--owner", "alice", "--key", pubKeyPath); code != 0 {
		t.Fatalf("fleet --publish: exit %d %q", code, errOut)
	}
	fe.now = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)

	fleetLedgerPath := fleet.FleetLedgerPath(fe.stateDir)
	if err := os.MkdirAll(filepath.Dir(fleetLedgerPath), 0o755); err != nil {
		t.Fatal(err)
	}

	// Case 1: Corrupt fleet ledger file -> exits 4 with MM_STATUS_PARTIAL while all four resources print
	// and authorized-keys is unknown/recorded (never absent).
	if err := os.WriteFile(fleetLedgerPath, []byte("{corrupt-fleet-ledger\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errOut, code := fe.runStatus("--section", "fleet")
	if code != diag.ExitIncomplete {
		t.Fatalf("corrupt fleet ledger: exit = %d, want %d; stdout=%q stderr=%q", code, diag.ExitIncomplete, out, errOut)
	}
	if !strings.Contains(errOut, `"diagnostic":"MM_STATUS_PARTIAL"`) {
		t.Errorf("expected MM_STATUS_PARTIAL on stderr, got: %s", errOut)
	}
	assertKeyOrder(t, out)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected all 4 fleet resources to print on partial report, got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], `"resource":"directory","target":"user","state":"present"`) {
		t.Errorf("expected directory present on partial report, got: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"resource":"policy","target":"user","state":"present"`) {
		t.Errorf("expected policy present on partial report, got: %s", lines[1])
	}
	wantUnknownAuth := `{"section":"fleet","resource":"authorized-keys","target":"user","state":"unknown","facts":{"account":"dev","authorized":"0","legacy_block":"absent"},"evidence":"recorded","at":"2026-10-22T02:00:00Z"}`
	if lines[2] != wantUnknownAuth {
		t.Errorf("authorized-keys on corrupt ledger:\ngot  %s\nwant %s", lines[2], wantUnknownAuth)
	}
	if !strings.Contains(lines[3], `"resource":"enrollment","target":"user","state":"present"`) {
		t.Errorf("expected enrollment present on partial report, got: %s", lines[3])
	}

	// Case 2: Fleet ledger path replaced with a directory (portable unreadable file across Darwin, Linux and Windows).
	if err := os.Remove(fleetLedgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fleetLedgerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	outDir, errDir, codeDir := fe.runStatus("--section", "fleet")
	if codeDir != diag.ExitIncomplete || !strings.Contains(errDir, `"diagnostic":"MM_STATUS_PARTIAL"`) {
		t.Fatalf("directory fleet ledger: exit=%d out=%q err=%q", codeDir, outDir, errDir)
	}
	linesDir := strings.Split(strings.TrimSpace(outDir), "\n")
	if len(linesDir) != 4 || linesDir[2] != wantUnknownAuth {
		t.Errorf("unexpected output with directory fleet ledger:\n%s", outDir)
	}
}

func TestFleetSectionNoLockNoWriteNoNetworkNoProcessGuards(t *testing.T) {
	fx := newFleetFixtures(t)
	fe := newFleetTestEnv(t, fx, true)

	const gistID = "0123456789abcdef0123456789abcdef"
	const rev = "0123456789abcdef0123456789abcdef01234567"

	gs := newStatusGistServer(t, gistID, rev, map[string]string{
		"alice.yaml": fx.aliceYAML,
		"bob.yaml":   fx.bobYAML,
		"carol.yaml": fx.carolYAML,
	})
	fe.client = gs.client
	fe.env[fleet.GitHubAPIEnv] = gs.srv.URL
	writeFleetPolicyDir(t, fe.policyDir, "gist: "+gistID+"\n", fx, true)

	fe.now = time.Date(2026, 10, 22, 1, 15, 0, 0, time.UTC)
	if _, errOut, code := fe.runFleet("--refresh"); code != 0 {
		t.Fatalf("fleet --refresh: exit=%d err=%q", code, errOut)
	}
	fe.now = time.Date(2026, 10, 22, 1, 20, 0, 0, time.UTC)
	if _, errOut, code := fe.runFleet("--plan", "--user"); code != 0 {
		t.Fatalf("fleet --plan: exit=%d err=%q", code, errOut)
	}
	if _, errOut, code := fe.runFleet("--apply", "--user"); code != 0 {
		t.Fatalf("fleet --apply: exit=%d err=%q", code, errOut)
	}
	fe.now = time.Date(2026, 10, 22, 1, 30, 0, 0, time.UTC)
	pubKeyPath := filepath.Join(fe.base, "alice-low.pub")
	if err := os.WriteFile(pubKeyPath, []byte(fx.aliceLow.line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := fe.runFleet("--publish", "--owner", "alice", "--key", pubKeyPath); code != 0 {
		t.Fatalf("fleet --publish: exit=%d err=%q", code, errOut)
	}

	// Hold the plan-store lock open while status runs to prove status never acquires .lock.
	heldStore, err := plan.Open(fe.stateDir, func() time.Time { return fe.now })
	if err != nil {
		t.Fatal(err)
	}
	defer heldStore.Close()

	fe.now = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
	hitsBefore := atomic.LoadInt64(&gs.hits)
	beforeState := snapshotDir(t, fe.stateDir)
	beforeHome := snapshotDir(t, fe.homeDir)
	beforePolicy := snapshotDir(t, fe.policyDir)

	invocations := [][]string{
		{"--section", "fleet"},
		{"--section", "fleet", "--user"},
		{"--section", "fleet", "--json"},
		{"--section", "fleet", "--human"},
		{"--section", "fleet", "directory"},
		{"--section", "fleet", "policy"},
		{"--section", "fleet", "authorized-keys"},
		{"--section", "fleet", "enrollment"},
	}
	for _, args := range invocations {
		out, errOut, code := fe.runStatus(args...)
		if code != 0 || errOut != "" || out == "" {
			t.Fatalf("args %v: exit=%d out=%q err=%q", args, code, out, errOut)
		}
	}

	if gotHits := atomic.LoadInt64(&gs.hits) - hitsBefore; gotHits != 0 {
		t.Errorf("status performed %d HTTP requests, want 0", gotHits)
	}
	if afterState := snapshotDir(t, fe.stateDir); afterState != beforeState {
		t.Errorf("stateDir mutated during status!\nbefore:\n%s\nafter:\n%s", beforeState, afterState)
	}
	if afterHome := snapshotDir(t, fe.homeDir); afterHome != beforeHome {
		t.Errorf("homeDir mutated during status!\nbefore:\n%s\nafter:\n%s", beforeHome, afterHome)
	}
	if afterPolicy := snapshotDir(t, fe.policyDir); afterPolicy != beforePolicy {
		t.Errorf("policyDir mutated during status!\nbefore:\n%s\nafter:\n%s", beforePolicy, afterPolicy)
	}
}

func TestFleetStatusGoldens(t *testing.T) {
	fx := newFleetFixtures(t)
	const gistID = "0123456789abcdef0123456789abcdef"
	const rev = "0123456789abcdef0123456789abcdef01234567"

	setupPopulatedGist := func(t *testing.T) *fleetTestEnv {
		fe := newFleetTestEnv(t, fx, true)
		gs := newStatusGistServer(t, gistID, rev, map[string]string{
			"alice.yaml": fx.aliceYAML,
			"bob.yaml":   fx.bobYAML,
			"carol.yaml": fx.carolYAML,
		})
		fe.client = gs.client
		fe.env[fleet.GitHubAPIEnv] = gs.srv.URL
		writeFleetPolicyDir(t, fe.policyDir, "gist: "+gistID+"\n", fx, true)

		fe.now = time.Date(2026, 10, 22, 1, 15, 0, 0, time.UTC)
		if _, errOut, code := fe.runFleet("--refresh"); code != 0 {
			t.Fatalf("seed refresh: exit %d %q", code, errOut)
		}
		fe.now = time.Date(2026, 10, 22, 1, 20, 0, 0, time.UTC)
		if _, errOut, code := fe.runFleet("--plan", "--user"); code != 0 {
			t.Fatalf("seed plan: exit %d %q", code, errOut)
		}
		if _, errOut, code := fe.runFleet("--apply", "--user"); code != 0 {
			t.Fatalf("seed apply: exit %d %q", code, errOut)
		}
		fe.now = time.Date(2026, 10, 22, 1, 30, 0, 0, time.UTC)
		pubKeyPath := filepath.Join(fe.base, "alice-low.pub")
		if err := os.WriteFile(pubKeyPath, []byte(fx.aliceLow.line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, errOut, code := fe.runFleet("--publish", "--owner", "alice", "--key", pubKeyPath); code != 0 {
			t.Fatalf("seed publish: exit %d %q", code, errOut)
		}
		fe.now = time.Date(2026, 10, 22, 2, 0, 0, 0, time.UTC)
		return fe
	}

	cases := []struct {
		name      string
		populated bool
		args      []string
	}{
		{"fleet-default", true, []string{"--section", "fleet"}},
		{"fleet-json", true, []string{"--section", "fleet", "--json"}},
		{"fleet-human", true, []string{"--section", "fleet", "--human"}},
		{"fleet-policy-only", true, []string{"--section", "fleet", "policy"}},
		{"fleet-absent", false, []string{"--section", "fleet"}},
		{"fleet-target-global", false, []string{"--section", "fleet", "--global"}},
		{"fleet-unknown-resource", false, []string{"--section", "fleet", "bogus"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fe *fleetTestEnv
			if tc.populated {
				fe = setupPopulatedGist(t)
			} else {
				fe = newFleetTestEnv(t, fx, false)
				fe.env[fleet.PolicyEnv] = filepath.Join(fe.base, "missing-policy")
			}
			out, errOut, code := fe.runStatus(tc.args...)
			got := "exit " + strconv.Itoa(code) + "\n--- stdout\n" + out + "--- stderr\n" + errOut
			goldenPath := filepath.Join("testdata", tc.name+".golden")
			if *update {
				if err := os.MkdirAll("testdata", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v", goldenPath, err)
			}
			if got != string(want) {
				t.Errorf("differs from %s:\ngot:\n%s\nwant:\n%s", goldenPath, got, string(want))
			}
		})
	}
}
