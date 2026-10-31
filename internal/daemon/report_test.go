package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReceiptsOmitEvidence(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plans"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "journal"), 0o700); err != nil {
		t.Fatal(err)
	}
	plan := `{"plan":"plan-1","software":"ripgrep","command":"software","state":"applied"}`
	if err := os.WriteFile(filepath.Join(dir, "plans", "plan-1.json"), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := "{\"action\":\"receipt\",\"outcome\":\"applied\",\"at\":\"2026-10-30T00:00:00Z\",\"evidence\":{\"output\":\"secret-command-output\"}}\n"
	if err := os.WriteFile(filepath.Join(dir, "journal", "plan-1.jsonl"), []byte(journal), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := receiptsBody(dir)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if strings.Contains(text, "secret-command-output") || strings.Contains(text, "evidence") {
		t.Fatalf("evidence leaked: %s", text)
	}
	if !strings.Contains(text, "plan-1") || !strings.Contains(text, "applied") {
		t.Fatalf("missing receipt fields: %s", text)
	}
	_, statusJSON, err := statusBody(dir)
	if err != nil {
		t.Fatal(err)
	}
	statusText := string(statusJSON)
	if !strings.Contains(statusText, "plan-1") || !strings.Contains(statusText, `"name":"ripgrep"`) || strings.Contains(statusText, "secret-command-output") {
		t.Fatalf("status receipts: %s", statusText)
	}
}
