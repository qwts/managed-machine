package structured

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/config/ledger"
	"github.com/qwts/managed-machine/core/internal/config/provider/file"
	"github.com/qwts/managed-machine/core/internal/config/render"
	"github.com/qwts/managed-machine/core/internal/plan"
)

func TestRoundTripPreservesUnrelatedKeysAndOrder(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		initial string
		keys    string
		owned   []string
		want    string
	}{
		{
			name:   "json preserves unrelated keys nested objects and arrays in order",
			format: FormatJSON,
			initial: "{\n" +
				"  \"zebra\": 1,\n" +
				"  \"nested\": {\n" +
				"    \"second\": \"keep-second\",\n" +
				"    \"target\": \"old\",\n" +
				"    \"first\": [\n" +
				"      \"a\",\n" +
				"      \"b\"\n" +
				"    ]\n" +
				"  },\n" +
				"  \"alpha\": true\n" +
				"}\n",
			keys:  `{"nested.target":"new","nested.added":["x","y"]}`,
			owned: []string{"nested.target"},
			want: "{\n" +
				"  \"zebra\": 1,\n" +
				"  \"nested\": {\n" +
				"    \"second\": \"keep-second\",\n" +
				"    \"target\": \"new\",\n" +
				"    \"first\": [\n" +
				"      \"a\",\n" +
				"      \"b\"\n" +
				"    ],\n" +
				"    \"added\": [\n" +
				"      \"x\",\n" +
				"      \"y\"\n" +
				"    ]\n" +
				"  },\n" +
				"  \"alpha\": true\n" +
				"}\n",
		},
		{
			name:   "toml preserves unrelated keys nested tables and arrays in order and drops comments",
			format: FormatTOML,
			initial: "# leading comment is not preserved\n" +
				"zebra = \"first\"\n" +
				"alpha = 42 # inline comment\n" +
				"items = [\"one\", \"two\"]\n" +
				"\n" +
				"[server]\n" +
				"port = 8080\n" +
				"host = \"127.0.0.1\"\n" +
				"\n" +
				"[server.tls]\n" +
				"enabled = false\n" +
				"protocols = [\"TLSv1.2\", \"TLSv1.3\"]\n" +
				"\n" +
				"[[routes]]\n" +
				"path = \"/api\"\n" +
				"backend = \"svc\"\n",
			keys:  `{"server.port":9090,"server.tls.enabled":true,"server.tls.ciphers":["AES128","AES256"]}`,
			owned: []string{"server.port", "server.tls.enabled"},
			want: "zebra = \"first\"\n" +
				"alpha = 42\n" +
				"items = [\"one\", \"two\"]\n" +
				"\n" +
				"[server]\n" +
				"port = 9090\n" +
				"host = \"127.0.0.1\"\n" +
				"\n" +
				"[server.tls]\n" +
				"enabled = true\n" +
				"protocols = [\"TLSv1.2\", \"TLSv1.3\"]\n" +
				"ciphers = [\"AES128\", \"AES256\"]\n" +
				"\n" +
				"[[routes]]\n" +
				"path = \"/api\"\n" +
				"backend = \"svc\"\n",
		},
	}

	p := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "config."+tc.format)
			if err := os.WriteFile(dest, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			var seed []ledger.Record
			for _, k := range tc.owned {
				seed = append(seed, ledger.Record{
					SchemaVersion: ledger.SchemaVersion,
					Kind:          ledger.KindKey,
					Path:          dest,
					ID:            k,
					Format:        tc.format,
					SHA256:        "00",
					Config:        "app",
					Operation:     "settings",
					Target:        "user",
					Account:       "tester",
					Plan:          "plan-20261021T120000Z-0001",
					At:            "2026-10-21T12:00:00Z",
					State:         ledger.Owned,
				})
			}
			op := plan.Operation{
				ID:        "settings",
				Operation: "settings",
				Target:    "user",
				Provider:  Name,
				Params: map[string]string{
					"config":      "app",
					"destination": "settings",
					"path":        dest,
					"format":      tc.format,
					"keys":        tc.keys,
				},
			}
			_, _, ops, err := p.Plan(nil, "", op, render.Values{}, ledger.NewSnapshot(seed))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(ops) != 1 || ops[0].Action != "update" {
				t.Fatalf("expected 1 update op, got %+v", ops)
			}
			st, err := p.Step(ops[0], nil)
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if _, err := st.Apply(context.Background()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if _, err := st.Verify(context.Background()); err != nil {
				t.Fatalf("Verify: %v", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("output mismatch:\ngot:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

func TestMalformedFixturesRejectedWithoutRewriting(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		content string
	}{
		{
			name:    "json with line comment",
			format:  FormatJSON,
			content: "{\n  // comment is not valid JSON\n  \"a\": 1\n}\n",
		},
		{
			name:    "json with block comment",
			format:  FormatJSON,
			content: "{\n  /* block comment */\n  \"a\": 1\n}\n",
		},
		{
			name:    "json with trailing comment",
			format:  FormatJSON,
			content: "{\"a\": 1} // trailing\n",
		},
		{
			name:    "json with trailing comma",
			format:  FormatJSON,
			content: "{\"a\": 1,}\n",
		},
		{
			name:    "json with array root",
			format:  FormatJSON,
			content: "[\"not\", \"an\", \"object\"]\n",
		},
		{
			name:    "toml with syntax error",
			format:  FormatTOML,
			content: "[unclosed\na = 1\n",
		},
		{
			name:    "toml with duplicate key",
			format:  FormatTOML,
			content: "a = 1\na = 2\n",
		},
	}

	p := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "bad."+tc.format)
			if err := os.WriteFile(dest, []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			op := plan.Operation{
				ID:        "settings",
				Operation: "settings",
				Target:    "user",
				Provider:  Name,
				Params: map[string]string{
					"config":      "app",
					"destination": "settings",
					"path":        dest,
					"format":      tc.format,
					"keys":        `{"editor.tabSize":4}`,
				},
			}
			if _, _, _, err := p.Plan(nil, "", op, render.Values{}, ledger.NewSnapshot(nil)); err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Errorf("Plan: got %v, want malformed error", err)
			}
			if _, err := p.Check(nil, "", op, render.Values{}); err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Errorf("Check: got %v, want malformed error", err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, []byte(tc.content)) {
				t.Errorf("file was modified: got %q, want %q", got, tc.content)
			}
		})
	}
}

func TestForeignKeyConflictEvenWhenValueMatches(t *testing.T) {
	cases := []struct {
		name    string
		format  string
		initial string
		keys    string
		wantKey string
	}{
		{
			name:    "json matching value without ledger entry",
			format:  FormatJSON,
			initial: "{\n  \"editor\": {\n    \"tabSize\": 4\n  }\n}\n",
			keys:    `{"editor.tabSize":4}`,
			wantKey: "editor.tabSize",
		},
		{
			name:    "json differing value without ledger entry",
			format:  FormatJSON,
			initial: "{\n  \"editor\": {\n    \"tabSize\": 2\n  }\n}\n",
			keys:    `{"editor.tabSize":4}`,
			wantKey: "editor.tabSize",
		},
		{
			name:    "toml matching value without ledger entry",
			format:  FormatTOML,
			initial: "[editor]\ntabSize = 4\n",
			keys:    `{"editor.tabSize":4}`,
			wantKey: "editor.tabSize",
		},
		{
			name:    "toml deep path without ledger entry",
			format:  FormatTOML,
			initial: "[a.b]\nc = [\"one\", \"two\"]\n",
			keys:    `{"a.b.c":["one","two"]}`,
			wantKey: "a.b.c",
		},
	}

	p := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "cfg."+tc.format)
			if err := os.WriteFile(dest, []byte(tc.initial), 0o644); err != nil {
				t.Fatal(err)
			}
			op := plan.Operation{
				ID:        "settings",
				Operation: "settings",
				Target:    "user",
				Provider:  Name,
				Params: map[string]string{
					"config":      "app",
					"destination": "settings",
					"path":        dest,
					"format":      tc.format,
					"keys":        tc.keys,
				},
			}
			_, _, ops, err := p.Plan(nil, "", op, render.Values{}, ledger.NewSnapshot(nil))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			wantAction := "conflict foreign-key " + dest + ":" + tc.wantKey
			if len(ops) != 1 || ops[0].Action != wantAction {
				t.Fatalf("got ops %+v, want action %q", ops, wantAction)
			}
			got, _ := os.ReadFile(dest)
			if string(got) != tc.initial {
				t.Fatalf("file changed during conflict plan: %q", got)
			}
		})
	}
}

func TestScalarsStringListsDeepPathsAndCRLFTable(t *testing.T) {
	cases := []struct {
		name     string
		format   string
		initial  string
		keys     string
		wantSub  []string
		wantCRLF bool
	}{
		{
			name:     "json absent file scalars string list and depth 4 path",
			format:   FormatJSON,
			initial:  "",
			keys:     `{"a.b.c.d":"deep-val","count":42,"enabled":true,"ratio":1.5,"tags":["alpha","beta"]}`,
			wantSub:  []string{`"d": "deep-val"`, `"count": 42`, `"enabled": true`, `"ratio": 1.5`, `"alpha"`, `"beta"`},
			wantCRLF: false,
		},
		{
			name:     "toml absent file scalars string list and depth 4 path",
			format:   FormatTOML,
			initial:  "",
			keys:     `{"a.b.c.d":"deep-val","count":42,"enabled":true,"ratio":1.5,"tags":["alpha","beta"]}`,
			wantSub:  []string{`[a.b.c]`, `d = "deep-val"`, `count = 42`, `enabled = true`, `ratio = 1.5`, `tags = ["alpha", "beta"]`},
			wantCRLF: false,
		},
		{
			name:     "json windows crlf preserved on update and create",
			format:   FormatJSON,
			initial:  "{\r\n  \"keep\": \"yes\",\r\n  \"a\": {\r\n    \"b\": {\r\n      \"c\": \"old\"\r\n    }\r\n  }\r\n}\r\n",
			keys:     `{"a.b.c":"new","a.b.d":["one","two"]}`,
			wantSub:  []string{"\"keep\": \"yes\",\r\n", "\"c\": \"new\",\r\n", "\"d\": [\r\n"},
			wantCRLF: true,
		},
		{
			name:     "toml windows crlf preserved on update and create",
			format:   FormatTOML,
			initial:  "keep = \"yes\"\r\n\r\n[a.b]\r\nc = \"old\"\r\n",
			keys:     `{"a.b.c":"new","a.b.d":["one","two"]}`,
			wantSub:  []string{"keep = \"yes\"\r\n", "[a.b]\r\n", "c = \"new\"\r\n", "d = [\"one\", \"two\"]\r\n"},
			wantCRLF: true,
		},
	}

	p := New()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "target."+tc.format)
			var seed []ledger.Record
			if tc.initial != "" {
				if err := os.WriteFile(dest, []byte(tc.initial), 0o644); err != nil {
					t.Fatal(err)
				}
				seed = append(seed, ledger.Record{
					SchemaVersion: ledger.SchemaVersion,
					Kind:          ledger.KindKey,
					Path:          dest,
					ID:            "a.b.c",
					Format:        tc.format,
					SHA256:        "00",
					Config:        "app",
					Operation:     "op1",
					Target:        "user",
					Account:       "tester",
					Plan:          "plan-20261021T120000Z-0001",
					At:            "2026-10-21T12:00:00Z",
					State:         ledger.Owned,
				})
			}

			op := plan.Operation{
				ID:        "op1",
				Operation: "op1",
				Target:    "user",
				Provider:  Name,
				Params: map[string]string{
					"config":      "app",
					"destination": "dest",
					"path":        dest,
					"format":      tc.format,
					"keys":        tc.keys,
				},
			}
			_, _, ops, err := p.Plan(nil, "", op, render.Values{}, ledger.NewSnapshot(seed))
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			if len(ops) != 1 {
				t.Fatalf("expected 1 planned op, got %+v", ops)
			}

			var recorded []ledger.Record
			recFn := func(r ledger.Record) error {
				r.SchemaVersion = ledger.SchemaVersion
				r.Account = "tester"
				r.Plan = "plan-20261021T120001Z-0002"
				r.At = "2026-10-21T12:00:01Z"
				recorded = append(recorded, r)
				return nil
			}
			st, err := p.Step(ops[0], recFn)
			if err != nil {
				t.Fatalf("Step: %v", err)
			}
			if _, err := st.Apply(context.Background()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if _, err := st.Verify(context.Background()); err != nil {
				t.Fatalf("Verify: %v", err)
			}

			gotBytes, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			got := string(gotBytes)
			for _, sub := range tc.wantSub {
				if !strings.Contains(got, sub) {
					t.Errorf("output missing %q in:\n%q", sub, got)
				}
			}
			if tc.wantCRLF {
				if !strings.Contains(got, "\r\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
					t.Errorf("expected pure CRLF line endings, got %q", got)
				}
			} else if strings.Contains(got, "\r\n") {
				t.Errorf("unexpected CRLF in output: %q", got)
			}

			for _, r := range recorded {
				if r.Kind != ledger.KindKey || r.ID == "" || r.Format != tc.format || r.State != ledger.Owned {
					t.Errorf("unexpected ledger record: %+v", r)
				}
			}

			// Reapply with unchanged keys plans zero operations.
			snap := ledger.NewSnapshot(append(seed, recorded...))
			_, _, noopOps, err := p.Plan(nil, "", op, render.Values{}, snap)
			if err != nil || len(noopOps) != 0 {
				t.Errorf("reapply no-op: ops=%+v err=%v", noopOps, err)
			}

			// Check reports zero drift.
			drifts, err := p.Check(nil, "", op, render.Values{})
			if err != nil || len(drifts) != 0 {
				t.Errorf("check clean: drifts=%+v err=%v", drifts, err)
			}
		})
	}
}

func TestFormatRequiredAndNeverInferredFromPath(t *testing.T) {
	p := New()
	dest := filepath.Join(t.TempDir(), "settings.json")
	op := plan.Operation{
		ID:        "op1",
		Operation: "op1",
		Target:    "user",
		Provider:  Name,
		Params: map[string]string{
			"config":      "app",
			"destination": "settings",
			"path":        dest,
			"keys":        `{"a":"b"}`,
		},
	}
	if _, _, _, err := p.Plan(nil, "", op, render.Values{}, ledger.NewSnapshot(nil)); err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("missing format: got %v", err)
	}
	op.Params["format"] = "yaml"
	if _, _, _, err := p.Plan(nil, "", op, render.Values{}, ledger.NewSnapshot(nil)); err == nil || !strings.Contains(err.Error(), "unknown format") {
		t.Errorf("unknown format: got %v", err)
	}
}

func TestVerifyRejectsTamperedFileAndDoesNotRecordOwnership(t *testing.T) {
	p := New()
	dest := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(dest, []byte("{\"a\": \"tampered\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wantContent := "{\n  \"a\": \"expected\"\n}\n"
	var recorded []ledger.Record
	st, err := p.Step(plan.Operation{
		ID:        "op1",
		Operation: "op1",
		Target:    "user",
		Action:    "create",
		Provider:  Name,
		Params: map[string]string{
			"config": "app",
			"path":   dest,
			"kind":   ledger.KindKey,
			"format": FormatJSON,
			"keys":   `{"a":"expected"}`,
			"sha256": file.SHA256Hex([]byte(wantContent)),
		},
	}, func(r ledger.Record) error {
		recorded = append(recorded, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Verify(context.Background()); err == nil {
		t.Fatal("Verify accepted tampered file")
	}
	if len(recorded) != 0 {
		t.Fatalf("Verify recorded ownership on failure: %+v", recorded)
	}
}
