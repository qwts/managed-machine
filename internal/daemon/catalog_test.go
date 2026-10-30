package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	if diagID = p.Apply(id, "", "", "", "iron", "cursor"); diagID != "" {
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
	if !strings.Contains(text, id) || !strings.Contains(text, "applied") || !strings.Contains(text, "admin") || !strings.Contains(text, "iron") || !strings.Contains(text, "cursor") {
		t.Fatalf("receipt %s", text)
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

func TestApplyIgnoresRequestFields(t *testing.T) {
	var calls int
	p := testPlanner(t, func(Item, string, string) error { calls++; return nil })
	id, diagID := p.Plan("ripgrep", "14.1.1", "user")
	if diagID != "" {
		t.Fatal(diagID)
	}
	if got := p.Apply(id, "docker", "", "", "", ""); got != "MM_DAEMON_UNDECLARED" {
		t.Fatalf("got %s", got)
	}
	if calls != 0 {
		t.Fatal("apply with extra fields started a process")
	}
	if _, err := os.Stat(filepath.Join(p.StateDir, "plans", id+".json")); err != nil {
		t.Fatal(err)
	}
}
