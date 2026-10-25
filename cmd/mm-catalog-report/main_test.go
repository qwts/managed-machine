package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	fixture     = "../../internal/legacy/testdata/apps.json"
	appsFixture = "../../tests/fixtures/apps.json"
)

func TestModes(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{fixture}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if n := strings.Count(out.String(), "\n"); n != 12 {
		t.Errorf("%d JSON Lines records, want 12", n)
	}
	if !strings.HasPrefix(out.String(), `{"name":"fixture-cask","kind":"signed-cask","verdict":"manual","candidate":`) {
		t.Errorf("first record: %.120s", out.String())
	}
	if !strings.Contains(out.String(), `"id":"brew-cask","provider":"brew"`) {
		t.Errorf("expected brew-cask recipe id in output: %.200s", out.String())
	}
	if got := errb.String(); got != "12 rows: 2 mappable, 6 manual, 4 unmappable; 2 alias collisions\n" {
		t.Errorf("summary %q", got)
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"--json", fixture}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "{\n  \"records\": [") {
		t.Errorf("--json: exit %d, %.40q", code, out.String())
	}
}

func TestEmitAndConvertedAgainstFixtures(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "software")
	var out, errb bytes.Buffer
	if code := run([]string{"--emit", dir, appsFixture}, &out, &errb); code != 0 {
		t.Fatalf("--emit exit %d: %s", code, errb.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 22 {
		t.Fatalf("emitted %d recipes, want 22", len(entries))
	}

	// Edit a description and recipe id to verify --converted ignores both.
	warpPath := filepath.Join(dir, "warp.yaml")
	raw, err := os.ReadFile(warpPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "description: warp", "description: Warp terminal", 1)
	edited = strings.Replace(edited, "id: brew-cask", "id: custom-cask", 1)
	if err := os.WriteFile(warpPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := run([]string{"--converted", dir, appsFixture}, &out, &errb); code != 0 {
		t.Fatalf("--converted exit %d: %s", code, errb.String())
	}
	if got := errb.String(); got != "43 rows: 22 converted, 0 drift, 4 mappable, 2 manual, 15 unmappable; 0 alias collisions\n" {
		t.Errorf("summary %q", got)
	}
	var convertedCount int
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var rec struct {
			Name      string `json:"name"`
			Kind      string `json:"kind"`
			Verdict   string `json:"verdict"`
			Candidate any    `json:"candidate"`
			Items     []struct {
				Field  string `json:"field"`
				Action string `json:"action"`
				Reason string `json:"reason"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal %q: %v", line, err)
		}
		switch rec.Verdict {
		case "converted":
			convertedCount++
			if rec.Candidate == nil {
				t.Errorf("%s: converted row has no candidate", rec.Name)
			}
		case "drift":
			t.Errorf("%s: unexpected drift", rec.Name)
		default:
			hasReason := false
			for _, it := range rec.Items {
				if (it.Action == "blocked" || it.Action == "deferred") && it.Reason != "" {
					hasReason = true
				}
			}
			if !hasReason {
				t.Errorf("%s (%s): unconverted row without reason item: %+v", rec.Name, rec.Kind, rec.Items)
			}
		}
	}
	if convertedCount != 22 {
		t.Errorf("convertedCount=%d, want 22", convertedCount)
	}

	// Human mode works with --converted too.
	out.Reset()
	errb.Reset()
	if code := run([]string{"--converted", dir, "--human", appsFixture}, &out, &errb); code != 0 {
		t.Fatalf("--converted --human exit %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "converted") {
		t.Errorf("expected converted in human table:\n%s", out.String())
	}

	// Introduce drift and verify it is reported as drift.
	drifted := strings.Replace(edited, "team_id: 2BBY89MBSN", "team_id: DRIFTED000", 1)
	if err := os.WriteFile(warpPath, []byte(drifted), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"--converted", dir, appsFixture}, &out, &errb); code != 0 {
		t.Fatalf("--converted after drift exit %d: %s", code, errb.String())
	}
	if got := errb.String(); got != "43 rows: 21 converted, 1 drift, 4 mappable, 2 manual, 15 unmappable; 0 alias collisions\n" {
		t.Errorf("drift summary %q", got)
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"--json", "--human", fixture},
		{"--bogus", fixture},
		{"--emit"},
		{"--converted"},
		{fixture, fixture},
	} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"missing.json"}, &out, &errb); code != 1 {
		t.Errorf("unreadable file: exit %d", code)
	}
	if code := run([]string{"--converted", filepath.Join(t.TempDir(), "missing"), fixture}, &out, &errb); code != 1 {
		t.Errorf("missing converted dir: exit %d", code)
	}
	for _, args := range [][]string{{"-h"}, {"--bogus", "--json", "--human", "--help"}} {
		out.Reset()
		if code := run(args, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "usage:") {
			t.Errorf("%v: help must win, got exit %d", args, code)
		}
	}
}
