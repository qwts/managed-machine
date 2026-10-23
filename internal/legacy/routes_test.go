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
	if len(legacy.LegacyStatusSections) != 19 {
		t.Fatalf("LegacyStatusSections count = %d, want 19", len(legacy.LegacyStatusSections))
	}
	// 11 base routes + 19 status section routes = 30 entries.
	if len(legacy.Routes) != 30 {
		t.Fatalf("Routes count = %d, want 30", len(legacy.Routes))
	}
	seen := map[string]bool{}
	for _, r := range legacy.Routes {
		if r.Name == "" || r.Command == "" {
			t.Errorf("empty Name or Command in Route %+v", r)
		}
		if seen[r.Name] {
			t.Errorf("duplicate Route.Name %q", r.Name)
		}
		seen[r.Name] = true
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
		Stdout:  &stdout,
		Stderr:  &stderr,
		Detect:  func() (target.Host, error) { return target.Host{Target: tgt}, nil },
		Commands: []cli.Command{
			cli.Platform(),
			cli.Version("0.9.0"),
			software.Command(software.Deps{
				Catalog: func() ([]source.Entry, *registry.Registry, error) {
					return software.LoadCatalog(func(string) string { return "" })
				},
				Install: func(ctx context.Context, r install.Request) (install.Result, error) {
					atomic.AddInt64(&h.runnerHits, 1)
					return install.Result{}, errors.New("unexpected install")
				},
				Getenv: getenv,
			}),
			skill.Command(func(t target.Target) (skill.Store, error) {
				return skill.Open(getenv, t)
			}),
			config.Command(config.Deps{
				Catalog:  func() ([]config.Entry, error) { return config.LoadCatalog(func(string) string { return "" }) },
				Getenv:   getenv,
				GOOS:     goos,
				StateDir: func() (string, error) { return h.stateDir, nil },
				Now:      now,
			}),
			status.Command(status.Deps{
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
				Getenv:    getenv,
				GOOS:      goos,
				StateDir:  func() (string, error) { return h.stateDir, nil },
				PolicyDir: func() (string, error) { return h.policyDir, nil },
				Username:  func() (string, error) { return "dev", nil },
				Home:      func() (string, error) { return h.homeDir, nil },
				Now:       now,
			}),
			fleet.Command(fleet.Deps{
				Getenv:    getenv,
				GOOS:      goos,
				StateDir:  func() (string, error) { return h.stateDir, nil },
				PolicyDir: func() (string, error) { return h.policyDir, nil },
				Username:  func() (string, error) { return "dev", nil },
				Home:      func() (string, error) { return h.homeDir, nil },
				Now:       now,
				Runner:    countingRunner,
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
	cases := []routeCase{
		{args: []string{"setup", "--list"}, wantRoute: "setup"},
		{args: []string{"catalog", "list"}, wantRoute: "catalog"},
		{args: []string{"adopt"}, wantRoute: "adopt"},
		{args: []string{"add-agent", "x"}, wantRoute: "add-agent"},
		{args: []string{"account", "doctor", "x"}, wantRoute: "account"},
		{args: []string{"ssh", "status"}, wantRoute: "ssh"},
		{args: []string{"--bootstrap"}, wantRoute: "--bootstrap"},
		{args: []string{"--update"}, wantRoute: "--update"},
		{args: []string{"fleet", "list"}, wantRoute: "fleet list"},
		{args: []string{"fleet", "remove", "sha256-x", "--yes"}, wantRoute: "fleet remove"},
		{args: []string{"skill", "path"}, wantRoute: "skill path"},
	}
	for _, sec := range legacy.LegacyStatusSections {
		cases = append(cases, routeCase{
			args:      []string{"status", "--section", sec},
			wantRoute: "status --section " + sec,
		})
	}

	for _, tc := range cases {
		t.Run(strings.Join(tc.args, "_"), func(t *testing.T) {
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
