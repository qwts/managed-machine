package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/config/ledger"
)

// TestStructuredProviderLifecycleAndFileConflict proves the end-to-end
// --plan, --apply, --check and --status lifecycle for JSON and TOML
// structured operations:
//   - an absent destination is created with only the managed keys;
//   - the ledger records only `key` entries, never a `file` entry, so a later
//     `file` operation on the same path plans `conflict replace-file <path>`;
//   - `--check` reports per-key drift with `actual=... desired=...` detail;
//   - reapply with unchanged keys produces a zero-operation plan;
//   - removing an owned structured operation plans `delete`, removes the
//     owned key while keeping unrelated keys, and records `released` in the
//     ledger.
func TestStructuredProviderLifecycleAndFileConflict(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("settings.yaml", `kind: config
schema_version: 1
id: settings
description: Editor and tool structured settings.
targets: [user]
operations:
  - id: json-cfg
    provider: structured
    destination: json-cfg
    format: json
    keys:
      editor.tabSize: 4
      editor.rulers: ["80", "120"]
      telemetry.enabled: false
  - id: toml-cfg
    provider: structured
    destination: toml-cfg
    format: toml
    keys:
      ui.theme.name: solarized
      ui.font.ligatures: true
      ui.features: ["lsp", "format"]
`)
	jsonDest := filepath.Join(e.homeDir, ".config", "app", "settings.json")
	tomlDest := filepath.Join(e.homeDir, ".config", "app", "config.toml")
	e.writeDesired(fmt.Sprintf(`target: user
config:
  settings:
    destinations:
      json-cfg: %s
      toml-cfg: %s
`, filepath.ToSlash(jsonDest), filepath.ToSlash(tomlDest)))

	// 1. Plan on absent destinations creates both operations.
	out, errOut, code := e.run("settings", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan: exit %d %q %q", code, out, errOut)
	}
	if !strings.Contains(out, `"operation":"json-cfg","target":"user","action":"create"`) ||
		!strings.Contains(out, `"operation":"toml-cfg","target":"user","action":"create"`) {
		t.Fatalf("unexpected plan output:\n%s", out)
	}

	// 2. Apply creates both files containing only the managed keys.
	out, errOut, code = e.run("settings", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply: exit %d %q %q", code, out, errOut)
	}
	gotJSON, err := os.ReadFile(jsonDest)
	if err != nil {
		t.Fatal(err)
	}
	wantJSON := "{\n" +
		"  \"editor\": {\n" +
		"    \"rulers\": [\n" +
		"      \"80\",\n" +
		"      \"120\"\n" +
		"    ],\n" +
		"    \"tabSize\": 4\n" +
		"  },\n" +
		"  \"telemetry\": {\n" +
		"    \"enabled\": false\n" +
		"  }\n" +
		"}\n"
	if string(gotJSON) != wantJSON {
		t.Errorf("json content:\ngot:\n%s\nwant:\n%s", gotJSON, wantJSON)
	}
	gotTOML, err := os.ReadFile(tomlDest)
	if err != nil {
		t.Fatal(err)
	}
	wantTOML := "[ui]\n" +
		"features = [\"lsp\", \"format\"]\n" +
		"\n" +
		"[ui.font]\n" +
		"ligatures = true\n" +
		"\n" +
		"[ui.theme]\n" +
		"name = \"solarized\"\n"
	if string(gotTOML) != wantTOML {
		t.Errorf("toml content:\ngot:\n%s\nwant:\n%s", gotTOML, wantTOML)
	}

	// 3. The ownership ledger holds only `key` records (6 keys total), never a `file` record.
	ledgerPath := filepath.Join(e.stateDir, ledger.FileName)
	recs, err := ledger.Read(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 6 {
		t.Fatalf("expected 6 key ledger records, got %d: %+v", len(recs), recs)
	}
	for _, r := range recs {
		if r.Kind != ledger.KindKey || r.State != ledger.Owned || r.ID == "" {
			t.Errorf("unexpected ledger record: %+v", r)
		}
	}

	// 4. Reapply with unchanged keys yields a zero-operation plan.
	out, errOut, code = e.run("settings", "--plan", "--user")
	if code != 0 || errOut != "" || strings.Count(out, "\n") != 1 {
		t.Fatalf("reapply no-op plan: exit %d %q %q", code, out, errOut)
	}
	out, errOut, code = e.run("settings", "--apply", "--user")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("reapply no-op apply: exit %d %q %q", code, out, errOut)
	}

	// 5. --check exits 0 when keys match, and exits 3 with per-key actual=... desired=... detail on drift.
	if out, errOut, code := e.run("settings", "--check", "--user"); code != 0 || out != "" || errOut != "" {
		t.Fatalf("check clean: exit %d %q %q", code, out, errOut)
	}
	driftedJSON := "{\n" +
		"  \"unrelated\": \"kept\",\n" +
		"  \"editor\": {\n" +
		"    \"tabSize\": 2\n" +
		"  },\n" +
		"  \"telemetry\": {\n" +
		"    \"enabled\": false\n" +
		"  }\n" +
		"}\n"
	if err := os.WriteFile(jsonDest, []byte(driftedJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	out, errOut, code = e.run("settings", "--check", "--user")
	if code != 3 || !strings.Contains(errOut, "MM_CONFIG_DRIFT_FOUND") || !strings.Contains(errOut, "2 differences") {
		t.Fatalf("check drift: exit %d %q %q", code, out, errOut)
	}
	wantDriftRulers := `{"id":"settings","operation":"json-cfg","target":"user","drift":"editor.rulers","detail":"actual=absent desired=[\"80\",\"120\"]"}`
	wantDriftTab := `{"id":"settings","operation":"json-cfg","target":"user","drift":"editor.tabSize","detail":"actual=2 desired=4"}`
	if !strings.Contains(out, wantDriftRulers) || !strings.Contains(out, wantDriftTab) {
		t.Errorf("drift records:\ngot:\n%s\nwant lines:\n%s\n%s", out, wantDriftRulers, wantDriftTab)
	}

	// 6. Re-planning and applying after drift restores the drifted keys while preserving the new unrelated key.
	if _, errOut, code := e.run("settings", "--plan", "--user"); code != 0 {
		t.Fatalf("plan after drift: exit %d %q", code, errOut)
	}
	if _, errOut, code := e.run("settings", "--apply", "--user"); code != 0 {
		t.Fatalf("apply after drift: exit %d %q", code, errOut)
	}
	gotJSONAfter, _ := os.ReadFile(jsonDest)
	if !strings.HasPrefix(string(gotJSONAfter), "{\n  \"unrelated\": \"kept\",\n") {
		t.Errorf("unrelated key or order lost:\n%s", gotJSONAfter)
	}

	// 7. A later `file` operation targeting jsonDest plans `conflict replace-file <path>` because the ledger has only `key` entries.
	e.writeSource("files/raw.json", "{}\n")
	e.writeSource("whole.yaml", `kind: config
schema_version: 1
id: whole
description: Whole-file config targeting the structured path.
targets: [user]
operations:
  - id: overwrite
    provider: file
    destination: json-cfg
    source: files/raw.json
`)
	e.writeDesired(fmt.Sprintf(`target: user
config:
  settings:
    destinations:
      json-cfg: %s
      toml-cfg: %s
  whole:
    destinations:
      json-cfg: %s
`, filepath.ToSlash(jsonDest), filepath.ToSlash(tomlDest), filepath.ToSlash(jsonDest)))

	out, errOut, code = e.run("whole", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"conflict replace-file `) {
		t.Fatalf("whole-file plan on structured destination: exit %d %q %q", code, out, errOut)
	}
	if _, errOut, code := e.run("whole", "--apply", "--user"); code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") {
		t.Fatalf("whole-file apply on structured destination: exit %d %q", code, errOut)
	}
	if got, _ := os.ReadFile(jsonDest); string(got) != string(gotJSONAfter) {
		t.Fatalf("conflicting file apply modified structured file")
	}

	// 8. Even if `settings` itself switches `json-cfg` from `structured` to `file`, it still plans `conflict replace-file <path>`.
	e.writeSource("settings.yaml", `kind: config
schema_version: 1
id: settings
description: Switched json-cfg to whole-file provider.
targets: [user]
operations:
  - id: json-cfg
    provider: file
    destination: json-cfg
    source: files/raw.json
  - id: toml-cfg
    provider: structured
    destination: toml-cfg
    format: toml
    keys:
      ui.theme.name: solarized
      ui.font.ligatures: true
      ui.features: ["lsp", "format"]
`)
	out, errOut, code = e.run("settings", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"action":"conflict replace-file `) {
		t.Fatalf("same-config file op on key-owned path: exit %d %q %q", code, out, errOut)
	}

	// 9. Removing `toml-cfg` from `settings` plans delete operations for its owned keys, removes them from disk, and records released.
	e.writeSource("settings.yaml", `kind: config
schema_version: 1
id: settings
description: Only json-cfg remains.
targets: [user]
operations:
  - id: json-cfg
    provider: structured
    destination: json-cfg
    format: json
    keys:
      editor.tabSize: 4
      editor.rulers: ["80", "120"]
      telemetry.enabled: false
`)
	e.writeDesired(fmt.Sprintf(`target: user
config:
  settings:
    destinations:
      json-cfg: %s
`, filepath.ToSlash(jsonDest)))

	out, errOut, code = e.run("settings", "--plan", "--user")
	if code != 0 || !strings.Contains(out, `"operation":"toml-cfg","target":"user","action":"delete"`) {
		t.Fatalf("plan delete orphaned structured op: exit %d %q %q", code, out, errOut)
	}
	out, errOut, code = e.run("settings", "--apply", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("apply delete orphaned structured op: exit %d %q %q", code, out, errOut)
	}
	gotTOMLAfterDelete, err := os.ReadFile(tomlDest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotTOMLAfterDelete)) != "" {
		t.Errorf("expected empty TOML after deleting all managed keys, got:\n%s", gotTOMLAfterDelete)
	}
}

// TestStructuredForeignKeyRefusal proves that an existing key path in a JSON
// or TOML file without a matching key ledger entry plans
// `conflict foreign-key <path>:<key>` even when the value already matches, and
// that `--apply` refuses with MM_CONFIG_CONFLICT leaving the file byte-identical.
func TestStructuredForeignKeyRefusal(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("app.yaml", `kind: config
schema_version: 1
id: app
description: Structured config.
targets: [user]
operations:
  - id: cfg
    provider: structured
    destination: cfg
    format: toml
    keys:
      server.port: 8080
`)
	dest := filepath.Join(e.homeDir, "app.toml")
	initial := "# existing user file\n[server]\nport = 8080\nhost = \"localhost\"\n"
	if err := os.WriteFile(dest, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	e.writeDesired(fmt.Sprintf(`target: user
config:
  app:
    destinations:
      cfg: %s
`, filepath.ToSlash(dest)))

	out, errOut, code := e.run("app", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan: exit %d %q %q", code, out, errOut)
	}
	wantConflict := "conflict foreign-key " + filepath.ToSlash(dest) + ":server.port"
	if !strings.Contains(out, wantConflict) {
		t.Fatalf("plan output missing %q:\n%s", wantConflict, out)
	}

	_, errOut, code = e.run("app", "--apply", "--user")
	if code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") || !strings.Contains(errOut, "foreign-key") {
		t.Fatalf("apply foreign-key conflict: exit %d %q", code, errOut)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != initial {
		t.Errorf("file modified on refused apply:\ngot:  %q\nwant: %q", got, initial)
	}
}
