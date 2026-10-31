package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/provider"
)

func testPlanner(t *testing.T, exec func(Item, string, string) error) Planner {
	t.Helper()
	return Planner{
		Catalog: Catalog{Items: []Item{
			{Name: "ripgrep", Options: []string{"14.1.1"}, Targets: []string{"user"}},
			{Name: "docker", Options: []string{"desktop"}, Targets: []string{"user"}, PrivilegeRequired: true},
		}},
		StateDir: t.TempDir(),
		Account:  "admin",
		Exec:     exec,
		Now:      func() time.Time { return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC) },
	}
}

func TestPlanDeclaredOption(t *testing.T) {
	var calls int
	p := testPlanner(t, func(Item, string, string) error { calls++; return nil })
	id, diagID := p.Plan("ripgrep", "14.1.1", "user")
	if diagID != "" || id == "" {
		t.Fatalf("plan %s %s", id, diagID)
	}
	if calls != 0 {
		t.Fatal("plan started a process")
	}
	if diagID = p.Apply(id, "", "", "", "iron", "cursor", "ses_018f2c3a-7b4e-7d2a-8c11-6a5b4c3d2e1f"); diagID != "" {
		t.Fatal(diagID)
	}
	if calls != 1 {
		t.Fatalf("exec calls %d", calls)
	}
	body, err := receiptsBody(p.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "evidence") {
		t.Fatalf("evidence leaked: %s", text)
	}
	if !strings.Contains(text, id) || !strings.Contains(text, "applied") || !strings.Contains(text, "admin") || !strings.Contains(text, "iron") || !strings.Contains(text, "cursor") || !strings.Contains(text, "ses_018f2c3a-7b4e-7d2a-8c11-6a5b4c3d2e1f") {
		t.Fatalf("receipt %s", text)
	}
	if got := p.Apply(id, "", "", "", "iron", "cursor", ""); got != diag.DaemonReplay {
		t.Fatalf("second apply got %s", got)
	}
	if calls != 1 {
		t.Fatal("replayed plan started a process")
	}
}

func TestPlanRefusesBeforeExec(t *testing.T) {
	var calls int
	p := testPlanner(t, func(Item, string, string) error { calls++; return nil })
	cases := []struct {
		name, option, target, want string
	}{
		{"missing", "14.1.1", "user", "MM_DAEMON_NOT_IN_CATALOG"},
		{"ripgrep", "9.9.9", "user", "MM_DAEMON_UNDECLARED"},
		{"ripgrep", "14.1.1", "/tmp/evil", "MM_DAEMON_UNDECLARED"},
		{"docker", "desktop", "user", "MM_DAEMON_PRIVILEGE_REQUIRED"},
	}
	for _, tc := range cases {
		t.Run(tc.name+" "+tc.option, func(t *testing.T) {
			id, got := p.Plan(tc.name, tc.option, tc.target)
			if got != tc.want || id != "" {
				t.Fatalf("got %s %s", id, got)
			}
		})
	}
	if calls != 0 {
		t.Fatal("a refused plan started a process")
	}
}

func TestApplyRefusesAMissingProvider(t *testing.T) {
	p := testPlanner(t, func(Item, string, string) error {
		return fmt.Errorf("%w: /home/linuxbrew/.linuxbrew/bin/brew", provider.ErrMissing)
	})
	id, diagID := p.Plan("ripgrep", "14.1.1", "user")
	if diagID != "" || id == "" {
		t.Fatalf("plan %s %s", id, diagID)
	}
	if got := p.Apply(id, "", "", "", "iron", "terminal", ""); got != diag.DaemonProviderMissing {
		t.Fatalf("missing provider got %s", got)
	}
	privPlanner := testPlanner(t, func(Item, string, string) error {
		return provider.ErrPrivilege
	})
	privID, diagID := privPlanner.Plan("ripgrep", "14.1.1", "user")
	if diagID != "" || privID == "" {
		t.Fatalf("plan %s %s", privID, diagID)
	}
	if got := privPlanner.Apply(privID, "", "", "", "iron", "terminal", ""); got != diag.DaemonPrivilegeRequired {
		t.Fatalf("privilege error got %s", got)
	}
}

func TestBrewInstalledRefusesAnAbsentPrefix(t *testing.T) {
	if err := brewInstalled(""); err == nil || !errors.Is(err, provider.ErrMissing) {
		t.Fatal(err)
	}
	dir := t.TempDir()
	err := brewInstalled(dir)
	if err == nil || !errors.Is(err, provider.ErrMissing) {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "bin", "brew")) {
		t.Fatal(err)
	}
}

func TestApplyIgnoresRequestFields(t *testing.T) {
	var calls int
	p := testPlanner(t, func(Item, string, string) error { calls++; return nil })
	id, diagID := p.Plan("ripgrep", "14.1.1", "user")
	if diagID != "" {
		t.Fatal(diagID)
	}
	if got := p.Apply(id, "docker", "", "", "", "", ""); got != "MM_DAEMON_UNDECLARED" {
		t.Fatalf("got %s", got)
	}
	if calls != 0 {
		t.Fatal("apply with extra fields started a process")
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, "plans", id+".json")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(p.StateDir, "plans", id+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(string(raw), `"14.1.1"`, `""`, 1)
	if err := os.WriteFile(path, []byte(tampered), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := p.Apply(id, "", "", "", "", "", ""); got != diag.DaemonPlanFailed {
		t.Fatalf("tampered digest got %s", got)
	}
	if calls != 0 {
		t.Fatal("tampered plan started a process")
	}
}
