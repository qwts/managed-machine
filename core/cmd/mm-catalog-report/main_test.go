package main

import (
	"bytes"
	"strings"
	"testing"
)

const fixture = "../../internal/legacy/testdata/apps.json"

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
	if got := errb.String(); got != "12 rows: 2 mappable, 5 manual, 5 unmappable; 2 alias collisions\n" {
		t.Errorf("summary %q", got)
	}
	out.Reset()
	errb.Reset()
	if code := run([]string{"--json", fixture}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "{\n  \"records\": [") {
		t.Errorf("--json: exit %d, %.40q", code, out.String())
	}
}

func TestUsage(t *testing.T) {
	for _, args := range [][]string{{}, {"--json", "--human", fixture}, {"--bogus", fixture}, {fixture, fixture}} {
		var out, errb bytes.Buffer
		if code := run(args, &out, &errb); code != 2 || out.Len() != 0 {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
	var out, errb bytes.Buffer
	if code := run([]string{"missing.json"}, &out, &errb); code != 1 {
		t.Errorf("unreadable file: exit %d", code)
	}
	for _, args := range [][]string{{"-h"}, {"--bogus", "--json", "--human", "--help"}} {
		out.Reset()
		if code := run(args, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "usage:") {
			t.Errorf("%v: help must win, got exit %d", args, code)
		}
	}
}
