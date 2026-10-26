package legacy_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/compose"
	"github.com/qwts/managed-machine/internal/config"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/fleet"
	"github.com/qwts/managed-machine/internal/fleet/directory"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/skill"
	"github.com/qwts/managed-machine/internal/software"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/statedir"
	"github.com/qwts/managed-machine/internal/status"
	"github.com/qwts/managed-machine/internal/target"
)

func TestStaticRouteTableCompleteness(t *testing.T) {
	if len(legacy.LegacyStatusSections) != 2 || legacy.LegacyStatusSections[0] != "bootstrap" || legacy.LegacyStatusSections[1] != "update" {
		t.Fatalf("LegacyStatusSections = %v, want [bootstrap update]", legacy.LegacyStatusSections)
	}
	// 10 base routes (skill path moved to Core in S8 D2) + 2 kept status
	// section routes + 17 removed status section routes = 29 entries.
	if len(legacy.Routes) != 29 {
		t.Fatalf("Routes count = %d, want 29", len(legacy.Routes))
	}
	seen := map[string]bool{}
	for _, r := range legacy.Routes {
		if r.Name == "" || r.Command == "" {
			t.Errorf("empty Name or Command in Route %+v", r)
		}
		if r.Name == "skill path" || r.Command == "skill" {
			t.Errorf("skill path must not appear in legacy.Routes: %+v", r)
		}
		if seen[r.Name] {
			t.Errorf("duplicate Route.Name %q", r.Name)
		}
		seen[r.Name] = true
	}
	if _, ok := legacy.Match([]string{"skill", "path"}); ok {
		t.Errorf("legacy.Match matched 'skill path'; Core owns skill path in S8 D2")
	}
	if _, ok := legacy.Match([]string{"skill", "path", "--json"}); ok {
		t.Errorf("legacy.Match matched 'skill path --json'; Core owns skill path in S8 D2")
	}
	for _, legacySkillArgs := range [][]string{{"skill", "--json", "path"}, {"skill", "--kind", "x", "path"}} {
		if r, ok := legacy.Match(legacySkillArgs); !ok || r.Name != "skill path" || r.RemovedIn != "" {
			t.Errorf("legacy.Match(%v) = (%+v, %v), want skill path route", legacySkillArgs, r, ok)
		}
	}
	for _, helpArgs := range [][]string{{"setup", "--help"}, {"setup", "-h"}} {
		if _, ok := legacy.Match(helpArgs); ok {
			t.Errorf("legacy.Match matched %v; Core owns setup --help/-h in S8 D3", helpArgs)
		}
	}
}

func TestUsageHeredocMatchesLegacyRoutes(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "legacy", "bin", "managed-machine"))
	if err != nil {
		t.Fatalf("read legacy/bin/managed-machine: %v", err)
	}
	script := strings.ReplaceAll(string(raw), "\r\n", "\n")

	for _, removedFn := range []string{"usage", "setup_usage", "usage_legacy_body", "cmd_version", "cmd_skill", "exec_new_core", "is_new_core_route"} {
		if strings.Contains(script, "\n"+removedFn+"() {") {
			t.Errorf("legacy/bin/managed-machine still defines %s()", removedFn)
		}
	}
	if !strings.Contains(script, "print_setup_names refresh >&2") {
		t.Errorf("legacy/bin/managed-machine missing 'print_setup_names refresh >&2' on unknown-setup-name error path")
	}

	renderHelp := func(args ...string) string {
		var stdout, stderr bytes.Buffer
		app := &cli.App{
			Name:    "managed-machine",
			Version: "0.10.0",
			Stdout:  &stdout,
			Stderr:  &stderr,
		}
		if code := app.Run(args); code != diag.ExitOK || stderr.Len() != 0 {
			t.Fatalf("Run(%v): code=%d stderr=%q", args, code, stderr.String())
		}
		return stdout.String()
	}

	heredoc := strings.TrimSuffix(renderHelp("--help"), "\n")
	for _, helpArgs := range [][]string{nil, {"-h"}, {"help"}, {"setup", "--help"}, {"setup", "-h"}} {
		if got := strings.TrimSuffix(renderHelp(helpArgs...), "\n"); got != heredoc {
			t.Errorf("Run(%v) help mismatch with --help:\n--- got ---\n%s\n--- want ---\n%s", helpArgs, got, heredoc)
		}
	}

	lines := strings.Split(heredoc, "\n")
	if len(lines) == 0 || lines[0] != "managed-machine - machine setup and orchestration" {
		t.Fatalf("first help line = %q, want banner", lines[0])
	}

	coreIdx := strings.Index(heredoc, "Portable core commands (all five targets):\n")
	legacyIdx := strings.Index(heredoc, "Legacy commands (macOS only; Homebrew install):\n")
	compatIdx := strings.Index(heredoc, "Compatibility (ADR-0016, ADR-0017):\n")
	pointerLine := "Run 'managed-machine setup --list' for the setup names and their install state."
	pointerIdx := strings.Index(heredoc, pointerLine)
	if coreIdx < 0 || legacyIdx < 0 || compatIdx < 0 || pointerIdx < 0 {
		t.Fatalf("core help missing required section headers (core=%d legacy=%d compat=%d pointer=%d):\n%s",
			coreIdx, legacyIdx, compatIdx, pointerIdx, heredoc)
	}
	if !(coreIdx < legacyIdx && legacyIdx < compatIdx && compatIdx < pointerIdx) {
		t.Errorf("section order invalid: core=%d legacy=%d compat=%d pointer=%d", coreIdx, legacyIdx, compatIdx, pointerIdx)
	}
	if strings.Contains(heredoc, "Available setup names") {
		t.Errorf("core help contains setup-name listing")
	}

	// Verify one line per new-core command in the Portable core commands block.
	coreSection := heredoc[coreIdx:legacyIdx]
	var coreLines []string
	for _, ln := range strings.Split(strings.TrimSpace(coreSection), "\n")[1:] {
		if strings.TrimSpace(ln) != "" {
			coreLines = append(coreLines, ln)
		}
	}
	wantCorePrefixes := []string{
		"  managed-machine software ",
		"  managed-machine config ",
		"  managed-machine status --section software|config|flavor|fleet ",
		"  managed-machine fleet ",
		"  managed-machine skill ",
		"  managed-machine platform ",
		"  managed-machine version ",
		"  managed-machine --version ",
		"  managed-machine --help ",
	}
	if len(coreLines) != len(wantCorePrefixes) {
		t.Fatalf("core command line count = %d, want %d:\n%s", len(coreLines), len(wantCorePrefixes), strings.Join(coreLines, "\n"))
	}
	for i, wantPrefix := range wantCorePrefixes {
		if !strings.HasPrefix(coreLines[i], wantPrefix) {
			t.Errorf("core line %d = %q, want prefix %q", i, coreLines[i], wantPrefix)
		}
	}

	// Derive the ordered surviving base entries from legacy.Routes: the
	// r.Section == "" && r.RemovedIn == "" entries followed by the single
	// r.Command entry representing the surviving sectioned status routes.
	type baseEntry struct {
		Name        string
		Replacement string
		Sectioned   bool
	}
	var baseRoutes []baseEntry
	statusAdded := false
	for _, r := range legacy.Routes {
		if r.RemovedIn != "" {
			continue
		}
		if r.Section == "" {
			baseRoutes = append(baseRoutes, baseEntry{
				Name:        r.Name,
				Replacement: r.Replacement,
			})
			continue
		}
		if r.Command != "status" {
			t.Fatalf("unexpected sectioned route %+v", r)
		}
		if !statusAdded {
			baseRoutes = append(baseRoutes, baseEntry{
				Name:        r.Command,
				Replacement: r.Replacement,
				Sectioned:   true,
			})
			statusAdded = true
		}
	}

	var legacyLines []string
	for _, ln := range lines {
		if strings.Contains(ln, "[legacy]") {
			legacyLines = append(legacyLines, ln)
		}
	}
	if len(legacyLines) != len(baseRoutes) {
		t.Fatalf("[legacy] line count = %d, want %d:\n%s", len(legacyLines), len(baseRoutes), strings.Join(legacyLines, "\n"))
	}

	lineNamesRoute := func(line, routeName string) bool {
		const prefix = "  [legacy] "
		if !strings.HasPrefix(line, prefix) {
			return false
		}
		rest := strings.TrimPrefix(line, prefix)
		return rest == routeName || strings.HasPrefix(rest, routeName+" ")
	}

	// Every surviving base entry derived from legacy.Routes must appear on
	// exactly one [legacy] line, in legacy.Routes table order, and removed
	// routes must not appear.
	prevLineIdx := -1
	for _, br := range baseRoutes {
		matches := 0
		matchedIdx := -1
		var matchedLine string
		for idx, ln := range legacyLines {
			if lineNamesRoute(ln, br.Name) {
				matches++
				matchedIdx = idx
				matchedLine = ln
			}
		}
		if matches != 1 {
			t.Errorf("base route %q matched %d [legacy] lines, want 1", br.Name, matches)
			continue
		}
		if matchedIdx <= prevLineIdx {
			t.Errorf("base route %q at [legacy] line %d out of legacy.Routes order (previous=%d)", br.Name, matchedIdx, prevLineIdx)
		}
		prevLineIdx = matchedIdx
		if br.Sectioned {
			wantStatus := "  [legacy] status --section " + strings.Join(legacy.LegacyStatusSections, "|")
			if matchedLine != wantStatus {
				t.Errorf("status [legacy] line = %q, want %q", matchedLine, wantStatus)
			}
		}
	}
	for _, r := range legacy.Routes {
		if r.RemovedIn == "" {
			continue
		}
		for _, ln := range legacyLines {
			if lineNamesRoute(ln, r.Name) {
				t.Errorf("removed route %q still listed on [legacy] line %q", r.Name, ln)
			}
		}
	}
}

type coreHarness struct {
	t          *testing.T
	base       string
	stateDir   string
	policyDir  string
	homeDir    string
	runnerHits int64
}

func newCoreHarness(t *testing.T) *coreHarness {
	t.Helper()
	base := t.TempDir()
	return &coreHarness{
		t:         t,
		base:      base,
		stateDir:  filepath.Join(base, "state"),
		policyDir: filepath.Join(base, "policy"),
		homeDir:   filepath.Join(base, "home"),
	}
}

func (h *coreHarness) seedFleetFixture() {
	h.t.Helper()
	seed := bytes.Repeat([]byte{0x21}, ed25519.SeedSize)
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
	line := directory.KeyTypeEd25519 + " " + base64.StdEncoding.EncodeToString(blob.Bytes()) + " alice@workstation"
	pk, err := directory.ParsePublicKey(line)
	if err != nil {
		h.t.Fatal(err)
	}

	keyDir := filepath.Join(h.base, "key-directory")
	if err := os.MkdirAll(filepath.Join(keyDir, "keys"), 0o755); err != nil {
		h.t.Fatal(err)
	}
	aliceYAML := fmt.Sprintf("schema_version: 1\nkind: key-directory\nid: alice\nkeys:\n  - fingerprint: %s\n    key: %s\n    label: alice-main\n    published_at: \"2026-10-22T01:00:00Z\"\n    revision: 1\n", pk.Fingerprint, line)
	if err := os.WriteFile(filepath.Join(keyDir, "keys", "alice.yaml"), []byte(aliceYAML), 0o644); err != nil {
		h.t.Fatal(err)
	}
	groupsDir := filepath.Join(h.policyDir, "fleet", "access-groups")
	if err := os.MkdirAll(groupsDir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	kdYAML := "schema_version: 1\nkind: key-directory-source\npath: '" + filepath.ToSlash(keyDir) + "'\n"
	if err := os.WriteFile(filepath.Join(h.policyDir, "fleet", "key-directory.yaml"), []byte(kdYAML), 0o644); err != nil {
		h.t.Fatal(err)
	}
	groupYAML := fmt.Sprintf("schema_version: 1\nkind: access-group\nid: admins\ndescription: Admins\nmaintained_by: [alice]\naccounts: [dev]\napproved_fingerprints:\n  - %s\n", pk.Fingerprint)
	if err := os.WriteFile(filepath.Join(groupsDir, "admins.yaml"), []byte(groupYAML), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *coreHarness) runOnTarget(tgt target.Target, args ...string) (string, string, int) {
	h.t.Helper()
	var stdout, stderr bytes.Buffer
	goos := "darwin"
	if i := strings.IndexByte(string(tgt), '/'); i > 0 {
		goos = string(tgt[:i])
	}
	getenv := func(k string) string {
		switch k {
		case statedir.Override:
			return h.stateDir
		case fleet.PolicyEnv:
			return h.policyDir
		case "HOME", "USERPROFILE":
			return h.homeDir
		}
		return ""
	}
	countingRunner := func(ctx context.Context, exe string, argv, env []string) (provider.Result, error) {
		atomic.AddInt64(&h.runnerHits, 1)
		return provider.Result{}, fmt.Errorf("unexpected runner call: %s %v", exe, argv)
	}
	now := func() time.Time { return time.Date(2026, 10, 22, 3, 0, 0, 0, time.UTC) }

	app := &cli.App{
		Name:    "managed-machine",
		Version: "0.9.0",
		GOOS:    goos,
		Stdout:  &stdout,
		Stderr:  &stderr,
		Detect:  func() (target.Host, error) { return target.Host{Target: tgt}, nil },
		Commands: []cli.Command{
			cli.Platform(),
			cli.Version("0.9.0"),
			software.Command(software.Deps{
				Deps: command.Deps{
					Getenv: getenv,
				},
				Catalog: func() ([]source.Entry, *registry.Registry, error) {
					return software.LoadCatalog(func(string) string { return "" })
				},
				Install: func(ctx context.Context, r install.Request) (install.Result, error) {
					atomic.AddInt64(&h.runnerHits, 1)
					return install.Result{}, errors.New("unexpected install")
				},
			}),
			skill.Command(func(t target.Target) (skill.Store, error) {
				return skill.Open(getenv, t)
			}),
			config.Command(config.Deps{
				Deps: command.Deps{
					Getenv:   getenv,
					GOOS:     goos,
					StateDir: func() (string, error) { return h.stateDir, nil },
					Now:      now,
				},
				Catalog: func() ([]config.Entry, error) { return config.LoadCatalog(func(string) string { return "" }) },
			}),
			status.Command(status.Deps{
				Deps: command.Deps{
					Getenv:    getenv,
					GOOS:      goos,
					StateDir:  func() (string, error) { return h.stateDir, nil },
					PolicyDir: func() (string, error) { return h.policyDir, nil },
					Username:  func() (string, error) { return "dev", nil },
					Home:      func() (string, error) { return h.homeDir, nil },
					Now:       now,
				},
				SoftwareCatalog: func() ([]source.Entry, *registry.Registry, error) {
					return software.LoadCatalog(func(string) string { return "" })
				},
				ConfigCatalog: func() ([]config.Entry, error) {
					return []config.Entry{{
						Definition: config.Definition{
							SchemaVersion: 1,
							ID:            "gitconfig",
							Description:   "Git user config",
							Targets:       []string{"user"},
						},
					}}, nil
				},
				Flavors: []compose.Flavor{{
					SchemaVersion: compose.SchemaVersion,
					ID:            "workstation",
					Description:   "Workstation",
					Config:        []compose.ConfigEntry{{ID: "gitconfig", Target: "user"}},
				}},
			}),
			fleet.Command(fleet.Deps{
				Deps: command.Deps{
					Getenv:    getenv,
					GOOS:      goos,
					StateDir:  func() (string, error) { return h.stateDir, nil },
					PolicyDir: func() (string, error) { return h.policyDir, nil },
					Username:  func() (string, error) { return "dev", nil },
					Home:      func() (string, error) { return h.homeDir, nil },
					Now:       now,
				},
				Runner: countingRunner,
			}),
		},
	}
	code := app.Run(args)
	return stdout.String(), stderr.String(), code
}

func TestLegacyRouteRefusalAndPreservedNewCoreRoutes(t *testing.T) {
	type routeCase struct {
		args      []string
		wantRoute string
	}
	survivingCases := []routeCase{
		{args: []string{"setup", "--list"}, wantRoute: "setup"},
		{args: []string{"catalog", "list"}, wantRoute: "catalog"},
		{args: []string{"adopt"}, wantRoute: "adopt"},
		{args: []string{"add-agent", "x"}, wantRoute: "add-agent"},
		{args: []string{"account", "doctor", "x"}, wantRoute: "account"},
		{args: []string{"ssh", "status"}, wantRoute: "ssh"},
		{args: []string{"--bootstrap"}, wantRoute: "--bootstrap"},
		{args: []string{"--update"}, wantRoute: "--update"},
		{args: []string{"fleet", "remove", "sha256-x", "--yes"}, wantRoute: "fleet remove"},
		{args: []string{"skill", "--json", "path"}, wantRoute: "skill path"},
		{args: []string{"skill", "--kind", "x", "path"}, wantRoute: "skill path"},
	}
	for _, sec := range legacy.LegacyStatusSections {
		survivingCases = append(survivingCases, routeCase{
			args:      []string{"status", "--section", sec},
			wantRoute: "status --section " + sec,
		})
	}

	for _, tc := range survivingCases {
		t.Run("surviving_"+strings.Join(tc.args, "_"), func(t *testing.T) {
			for _, tgt := range target.All() {
				h := newCoreHarness(t)
				stdout, stderr, code := h.runOnTarget(tgt, tc.args...)
				if code != diag.ExitUsage {
					t.Fatalf("%s %v: exit = %d, want 2 (stderr=%q)", tgt, tc.args, code, stderr)
				}
				if stdout != "" {
					t.Errorf("%s %v: stdout = %q, want empty", tgt, tc.args, stdout)
				}
				if !strings.Contains(stderr, `"diagnostic":"MM_LEGACY_ROUTE_UNSUPPORTED"`) {
					t.Errorf("%s %v: stderr = %q, want MM_LEGACY_ROUTE_UNSUPPORTED", tgt, tc.args, stderr)
				}
				if !strings.Contains(stderr, tc.wantRoute) {
					t.Errorf("%s %v: stderr %q does not name route %q", tgt, tc.args, stderr, tc.wantRoute)
				}
				if !strings.Contains(stderr, "Homebrew-installed managed-machine on macOS") {
					t.Errorf("%s %v: stderr %q missing compatibility statement", tgt, tc.args, stderr)
				}
				if _, err := os.Stat(h.stateDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s %v: state directory was created (%v)", tgt, tc.args, err)
				}
				if got := atomic.LoadInt64(&h.runnerHits); got != 0 {
					t.Errorf("%s %v: runner seam count = %d, want 0", tgt, tc.args, got)
				}
			}
		})
	}

	type removedCase struct {
		args            []string
		wantRoute       string
		wantReplacement string
	}
	const noRep = "no replacement"
	removedCases := []removedCase{
		{args: []string{"fleet", "list"}, wantRoute: "fleet list", wantReplacement: "managed-machine fleet --keys"},
		{args: []string{"status", "--section", "managed-machine"}, wantRoute: "status --section managed-machine", wantReplacement: "managed-machine version"},
		{args: []string{"status", "--section", "machine"}, wantRoute: "status --section machine", wantReplacement: noRep},
		{args: []string{"status", "--section", "ssh"}, wantRoute: "status --section ssh", wantReplacement: "managed-machine ssh status"},
		{args: []string{"status", "--section", "agent-accounts"}, wantRoute: "status --section agent-accounts", wantReplacement: "managed-machine account doctor"},
		{args: []string{"status", "--section", "local-bin"}, wantRoute: "status --section local-bin", wantReplacement: noRep},
		{args: []string{"status", "--section", "zsh-functions"}, wantRoute: "status --section zsh-functions", wantReplacement: noRep},
		{args: []string{"status", "--section", "pins"}, wantRoute: "status --section pins", wantReplacement: noRep},
		{args: []string{"status", "--section", "toolchain"}, wantRoute: "status --section toolchain", wantReplacement: noRep},
		{args: []string{"status", "--section", "homebrew"}, wantRoute: "status --section homebrew", wantReplacement: noRep},
		{args: []string{"status", "--section", "gh"}, wantRoute: "status --section gh", wantReplacement: noRep},
		{args: []string{"status", "--section", "nvm"}, wantRoute: "status --section nvm", wantReplacement: noRep},
		{args: []string{"status", "--section", "node"}, wantRoute: "status --section node", wantReplacement: noRep},
		{args: []string{"status", "--section", "hostname"}, wantRoute: "status --section hostname", wantReplacement: noRep},
		{args: []string{"status", "--section", "rustup"}, wantRoute: "status --section rustup", wantReplacement: noRep},
		{args: []string{"status", "--section", "rustc"}, wantRoute: "status --section rustc", wantReplacement: noRep},
		{args: []string{"status", "--section", "cargo"}, wantRoute: "status --section cargo", wantReplacement: noRep},
		{args: []string{"status", "--section", "catalog"}, wantRoute: "status --section catalog", wantReplacement: "managed-machine setup --list and managed-machine software --available"},
	}
	if len(removedCases) != 18 {
		t.Fatalf("removedCases count = %d, want 18 (fleet list + 17 removed status sections)", len(removedCases))
	}
	for _, tc := range removedCases {
		t.Run("removed_"+strings.Join(tc.args, "_"), func(t *testing.T) {
			for _, tgt := range target.All() {
				h := newCoreHarness(t)
				stdout, stderr, code := h.runOnTarget(tgt, tc.args...)
				if code != diag.ExitUsage {
					t.Fatalf("%s %v: exit = %d, want 2 (stderr=%q)", tgt, tc.args, code, stderr)
				}
				if stdout != "" {
					t.Errorf("%s %v: stdout = %q, want empty", tgt, tc.args, stdout)
				}
				if !strings.Contains(stderr, `"diagnostic":"MM_LEGACY_ROUTE_REMOVED"`) {
					t.Errorf("%s %v: stderr = %q, want MM_LEGACY_ROUTE_REMOVED", tgt, tc.args, stderr)
				}
				for _, substr := range []string{
					tc.wantRoute,
					tc.wantReplacement,
					legacy.CompatibilityRemovalVersion,
					"docs/plans/f01-caller-characterization.md",
				} {
					if !strings.Contains(stderr, substr) {
						t.Errorf("%s %v: stderr %q missing %q", tgt, tc.args, stderr, substr)
					}
				}
				if _, err := os.Stat(h.stateDir); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s %v: state directory was created (%v)", tgt, tc.args, err)
				}
				if got := atomic.LoadInt64(&h.runnerHits); got != 0 {
					t.Errorf("%s %v: runner seam count = %d, want 0", tgt, tc.args, got)
				}
			}
		})
	}

	// An unknown command is still MM_USAGE.
	t.Run("unknown_command_stays_MM_USAGE", func(t *testing.T) {
		h := newCoreHarness(t)
		stdout, stderr, code := h.runOnTarget(target.DarwinARM64, "no-such-command")
		if code != diag.ExitUsage || stdout != "" {
			t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		if !strings.Contains(stderr, `"diagnostic":"MM_USAGE"`) {
			t.Errorf("stderr = %q, want MM_USAGE", stderr)
		}
		if _, err := os.Stat(h.stateDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("state directory was created (%v)", err)
		}
	})

	// Bare `fleet` still means `--keys`.
	t.Run("bare_fleet_still_means_keys", func(t *testing.T) {
		h := newCoreHarness(t)
		h.seedFleetFixture()
		bareOut, bareErr, bareCode := h.runOnTarget(target.DarwinARM64, "fleet")
		keysOut, keysErr, keysCode := h.runOnTarget(target.DarwinARM64, "fleet", "--keys")
		if bareCode != 0 || bareErr != "" || keysCode != 0 || keysErr != "" {
			t.Fatalf("bare=(%d,%q) keys=(%d,%q)", bareCode, bareErr, keysCode, keysErr)
		}
		if bareOut == "" || bareOut != keysOut {
			t.Errorf("bare fleet stdout != fleet --keys:\nbare:\n%s\nkeys:\n%s", bareOut, keysOut)
		}
		if _, err := os.Stat(h.stateDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("state directory was created (%v)", err)
		}
		if got := atomic.LoadInt64(&h.runnerHits); got != 0 {
			t.Errorf("runner seam count = %d, want 0", got)
		}
	})

	// Bare `status` still reports the new-core sections.
	t.Run("bare_status_reports_new_core_sections", func(t *testing.T) {
		h := newCoreHarness(t)
		h.seedFleetFixture()
		out, errOut, code := h.runOnTarget(target.DarwinARM64, "status")
		if code != 0 || errOut != "" {
			t.Fatalf("bare status: exit=%d stderr=%q", code, errOut)
		}
		recs, err := output.ReadCompact([]byte(out))
		if err != nil {
			t.Fatal(err)
		}
		sections := map[string]bool{}
		for _, r := range recs {
			if s, ok := r.Get("section"); ok {
				sections[fmt.Sprint(s)] = true
			}
		}
		if !sections["flavor"] {
			t.Errorf("bare status missing new-core sections in %v (out=%s)", sections, out)
		}
		for _, legacySec := range legacy.LegacyStatusSections {
			if sections[legacySec] {
				t.Errorf("bare status unexpectedly reported legacy section %q", legacySec)
			}
		}
		if _, err := os.Stat(h.stateDir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("state directory was created (%v)", err)
		}
		if got := atomic.LoadInt64(&h.runnerHits); got != 0 {
			t.Errorf("runner seam count = %d, want 0", got)
		}
	})
}
