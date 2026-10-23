package policy

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/schema"
)

var update = flag.Bool("update", false, "rewrite golden files under testdata/")

const (
	fpAlpha = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	fpBravo = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	fpDelta = "SHA256:DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD"
)

const validAccessGroupYAML = `schema_version: 1
kind: access-group
id: workstations
description: Developer workstations and personal machines.
maintained_by:
  - bob
  - alice
accounts:
  - user_two.dev
  - alice-admin
approved_fingerprints:
  - SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
  - SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
`

func TestAccessGroupNormalizesToDeterministicJSON(t *testing.T) {
	g1, err := ParseAccessGroup("fleet/access-groups/workstations.yaml", []byte(validAccessGroupYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(g1.MaintainedBy, ","); got != "alice,bob" {
		t.Errorf("maintained_by not sorted: %q", got)
	}
	if got := strings.Join(g1.Accounts, ","); got != "alice-admin,user_two.dev" {
		t.Errorf("accounts not sorted: %q", got)
	}
	if got := strings.Join(g1.ApprovedFingerprints, ","); got != fpAlpha+","+fpBravo {
		t.Errorf("approved_fingerprints not sorted: %q", got)
	}

	reorderedYAML := `kind: access-group
schema_version: 1
id: workstations
description: Developer workstations and personal machines.
maintained_by: [alice, bob]
accounts: [alice-admin, user_two.dev]
approved_fingerprints:
  - SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
  - SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB
`
	g2, err := ParseAccessGroup("fleet/access-groups/workstations.yaml", []byte(reorderedYAML))
	if err != nil {
		t.Fatal(err)
	}
	b1, err := g1.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	b2, err := g2.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("normalized JSON differs by declaration order:\n%s\n%s", b1, b2)
	}

	goldenPath := filepath.Join("testdata", "access-group.golden.json")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, b1, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b1, want) {
		t.Errorf("normalized access-group differs from golden:\ngot:  %s\nwant: %s", b1, want)
	}
}

func TestAccessGroupRejectsInvalidDocuments(t *testing.T) {
	cases := []struct {
		name      string
		edit      func(string) string
		wantKind  schema.Kind
		wantField string
	}{
		{
			name:      "missing kind",
			edit:      sub("kind: access-group\n", ""),
			wantKind:  schema.Invalid,
			wantField: "kind",
		},
		{
			name:      "wrong kind",
			edit:      sub("kind: access-group", "kind: config"),
			wantKind:  schema.Invalid,
			wantField: "kind",
		},
		{
			name:      "fingerprint without SHA256 prefix",
			edit:      sub(fpBravo, "MD5:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"),
			wantKind:  schema.Invalid,
			wantField: "approved_fingerprints",
		},
		{
			name:      "fingerprint wrong length (42 chars)",
			edit:      sub(fpBravo, "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"),
			wantKind:  schema.Invalid,
			wantField: "approved_fingerprints",
		},
		{
			name:      "fingerprint with padding equals sign",
			edit:      sub(fpBravo, "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="),
			wantKind:  schema.Invalid,
			wantField: "approved_fingerprints",
		},
		{
			name:      "fingerprint with base64url char",
			edit:      sub(fpBravo, "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB_"),
			wantKind:  schema.Invalid,
			wantField: "approved_fingerprints",
		},
		{
			name:      "duplicate fingerprint in one group",
			edit:      sub(fpBravo, fpAlpha),
			wantKind:  schema.Invalid,
			wantField: "approved_fingerprints",
		},
		{
			name:      "account starting with hyphen",
			edit:      sub("alice-admin", "-alice"),
			wantKind:  schema.Invalid,
			wantField: "accounts",
		},
		{
			name:      "account starting with dot",
			edit:      sub("alice-admin", ".alice"),
			wantKind:  schema.Invalid,
			wantField: "accounts",
		},
		{
			name:      "account containing space",
			edit:      sub("alice-admin", "alice admin"),
			wantKind:  schema.Invalid,
			wantField: "accounts",
		},
		{
			name:      "account exceeding 64 chars",
			edit:      sub("alice-admin", "a"+strings.Repeat("b", 64)),
			wantKind:  schema.Invalid,
			wantField: "accounts",
		},
		{
			name:      "duplicate account in one group",
			edit:      sub("user_two.dev", "alice-admin"),
			wantKind:  schema.Invalid,
			wantField: "accounts",
		},
		{
			name:      "empty maintained_by",
			edit:      sub("maintained_by:\n  - bob\n  - alice\n", "maintained_by: []\n"),
			wantKind:  schema.Invalid,
			wantField: "maintained_by",
		},
		{
			name:      "missing maintained_by",
			edit:      sub("maintained_by:\n  - bob\n  - alice\n", ""),
			wantKind:  schema.Invalid,
			wantField: "maintained_by",
		},
		{
			name:      "invalid maintained_by owner id (uppercase)",
			edit:      sub("  - bob", "  - Bob"),
			wantKind:  schema.Invalid,
			wantField: "maintained_by",
		},
		{
			name:      "invalid maintained_by owner id (leading hyphen)",
			edit:      sub("  - bob", "  - -bob"),
			wantKind:  schema.Invalid,
			wantField: "maintained_by",
		},
		{
			name:      "invalid group id",
			edit:      sub("id: workstations", "id: Work_Stations"),
			wantKind:  schema.Invalid,
			wantField: "id",
		},
		{
			name:      "missing schema_version",
			edit:      sub("schema_version: 1\n", ""),
			wantKind:  schema.InvalidVersion,
			wantField: "schema_version",
		},
		{
			name:      "unsupported schema_version",
			edit:      sub("schema_version: 1", "schema_version: 2"),
			wantKind:  schema.InvalidVersion,
			wantField: "schema_version",
		},
		{
			name:      "unknown field",
			edit:      sub("id: workstations", "id: workstations\nextra_field: nope"),
			wantKind:  schema.UnknownField,
			wantField: "extra_field",
		},
		{
			name:      "yaml anchor",
			edit:      sub("  - bob", "  - &a bob"),
			wantKind:  schema.Invalid,
			wantField: "anchors",
		},
	}

	docName := "fleet/access-groups/workstations.yaml"
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			edited := tc.edit(validAccessGroupYAML)
			if edited == validAccessGroupYAML {
				t.Fatal("edit did not change fixture")
			}
			_, err := ParseAccessGroup(docName, []byte(edited))
			if err == nil {
				t.Fatalf("expected error, got nil for:\n%s", edited)
			}
			if !errors.Is(err, schema.Err(tc.wantKind)) {
				t.Errorf("error kind = %v, want %s", err, tc.wantKind)
			}
			if got := Diagnostic(err); got != diag.FleetPolicyInvalid {
				t.Errorf("Diagnostic = %q, want %q", got, diag.FleetPolicyInvalid)
			}
			if !strings.HasPrefix(err.Error(), docName+": ") {
				t.Errorf("error %q does not name document %q", err.Error(), docName)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not mention field %q", err.Error(), tc.wantField)
			}
		})
	}
}

func TestTwoGroupsSharingIDFailWithPolicyInvalid(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	kdYAML := "schema_version: 1\nkind: key-directory-source\npath: '" + filepath.ToSlash(dir) + "'\n"
	fsys := fstest.MapFS{
		"fleet/key-directory.yaml":       {Data: []byte(kdYAML)},
		"fleet/access-groups/first.yaml": {Data: []byte(validAccessGroupYAML)},
		"fleet/access-groups/second.yaml": {Data: []byte(strings.Replace(
			validAccessGroupYAML,
			"Description: Developer workstations and personal machines.",
			"Description: Duplicate group id.",
			1,
		))},
	}
	_, err := LoadFS(fsys)
	if err == nil {
		t.Fatal("accepted two access groups sharing an id")
	}
	if !errors.Is(err, schema.Err(schema.Conflict)) {
		t.Errorf("got %v, want schema.Conflict", err)
	}
	if got := Diagnostic(err); got != diag.FleetPolicyInvalid {
		t.Errorf("Diagnostic = %q, want %q", got, diag.FleetPolicyInvalid)
	}
	if !strings.Contains(err.Error(), "fleet/access-groups/second.yaml: id:") {
		t.Errorf("error %q does not name document and field id", err.Error())
	}
}

func TestKeyDirectorySourceValidation(t *testing.T) {
	absPath := filepath.ToSlash(filepath.Join(t.TempDir(), "key-dir"))

	validGist := "schema_version: 1\nkind: key-directory-source\ngist: 0123456789abcdef0123456789abcdef\n"
	sGist, err := ParseKeyDirectorySource(KeyDirectoryRelPath, []byte(validGist))
	if err != nil {
		t.Fatalf("valid gist source failed: %v", err)
	}
	if sGist.Gist != "0123456789abcdef0123456789abcdef" || sGist.Path != "" {
		t.Errorf("unexpected gist source: %+v", sGist)
	}

	validPath := "schema_version: 1\nkind: key-directory-source\npath: '" + absPath + "'\n"
	sPath, err := ParseKeyDirectorySource(KeyDirectoryRelPath, []byte(validPath))
	if err != nil {
		t.Fatalf("valid path source failed: %v", err)
	}
	if sPath.Path != absPath || sPath.Gist != "" {
		t.Errorf("unexpected path source: %+v", sPath)
	}

	cases := []struct {
		name      string
		yaml      string
		wantField string
	}{
		{
			name:      "neither gist nor path",
			yaml:      "schema_version: 1\nkind: key-directory-source\n",
			wantField: "gist",
		},
		{
			name:      "both gist and path",
			yaml:      "schema_version: 1\nkind: key-directory-source\ngist: deadbeef\npath: '" + absPath + "'\n",
			wantField: "gist",
		},
		{
			name:      "relative path",
			yaml:      "schema_version: 1\nkind: key-directory-source\npath: relative/keys\n",
			wantField: "path",
		},
		{
			name:      "non-hex gist (uppercase)",
			yaml:      "schema_version: 1\nkind: key-directory-source\ngist: DEADBEEF\n",
			wantField: "gist",
		},
		{
			name:      "non-hex gist (invalid characters)",
			yaml:      "schema_version: 1\nkind: key-directory-source\ngist: not-a-hex-id!\n",
			wantField: "gist",
		},
		{
			name:      "missing kind",
			yaml:      "schema_version: 1\ngist: deadbeef\n",
			wantField: "kind",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKeyDirectorySource(KeyDirectoryRelPath, []byte(tc.yaml))
			if err == nil {
				t.Fatalf("accepted invalid key-directory-source:\n%s", tc.yaml)
			}
			if got := Diagnostic(err); got != diag.FleetPolicyInvalid {
				t.Errorf("Diagnostic = %q, want %q", got, diag.FleetPolicyInvalid)
			}
			if !strings.HasPrefix(err.Error(), KeyDirectoryRelPath+": ") {
				t.Errorf("error %q does not name document %q", err.Error(), KeyDirectoryRelPath)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name field %q", err.Error(), tc.wantField)
			}
		})
	}
}

func TestCutoverValidation(t *testing.T) {
	validCutover := `schema_version: 1
kind: fleet-cutover
cut_over_at: "2026-10-22T01:00:00Z"
reviewed_by:
  - bob
  - alice
note: Cut over to S5 fleet management.
`
	c, err := ParseCutover(CutoverRelPath, []byte(validCutover))
	if err != nil {
		t.Fatalf("valid cutover failed: %v", err)
	}
	if c.CutOverAt != "2026-10-22T01:00:00Z" || strings.Join(c.ReviewedBy, ",") != "alice,bob" {
		t.Errorf("unexpected cutover: %+v", c)
	}

	cases := []struct {
		name      string
		yaml      string
		wantField string
	}{
		{
			name:      "missing cut_over_at",
			yaml:      "schema_version: 1\nkind: fleet-cutover\nreviewed_by: [alice]\n",
			wantField: "cut_over_at",
		},
		{
			name:      "non-UTC cut_over_at",
			yaml:      "schema_version: 1\nkind: fleet-cutover\ncut_over_at: \"2026-10-22T01:00:00-05:00\"\nreviewed_by: [alice]\n",
			wantField: "cut_over_at",
		},
		{
			name:      "malformed cut_over_at",
			yaml:      "schema_version: 1\nkind: fleet-cutover\ncut_over_at: \"2026-10-22\"\nreviewed_by: [alice]\n",
			wantField: "cut_over_at",
		},
		{
			name:      "empty reviewed_by",
			yaml:      "schema_version: 1\nkind: fleet-cutover\ncut_over_at: \"2026-10-22T01:00:00Z\"\nreviewed_by: []\n",
			wantField: "reviewed_by",
		},
		{
			name:      "missing reviewed_by",
			yaml:      "schema_version: 1\nkind: fleet-cutover\ncut_over_at: \"2026-10-22T01:00:00Z\"\n",
			wantField: "reviewed_by",
		},
		{
			name:      "invalid reviewer owner id",
			yaml:      "schema_version: 1\nkind: fleet-cutover\ncut_over_at: \"2026-10-22T01:00:00Z\"\nreviewed_by: [Alice_Bad]\n",
			wantField: "reviewed_by",
		},
		{
			name:      "duplicate reviewer",
			yaml:      "schema_version: 1\nkind: fleet-cutover\ncut_over_at: \"2026-10-22T01:00:00Z\"\nreviewed_by: [alice, alice]\n",
			wantField: "reviewed_by",
		},
		{
			name:      "missing kind",
			yaml:      "schema_version: 1\ncut_over_at: \"2026-10-22T01:00:00Z\"\nreviewed_by: [alice]\n",
			wantField: "kind",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCutover(CutoverRelPath, []byte(tc.yaml))
			if err == nil {
				t.Fatalf("accepted invalid cutover:\n%s", tc.yaml)
			}
			if got := Diagnostic(err); got != diag.FleetPolicyInvalid {
				t.Errorf("Diagnostic = %q, want %q", got, diag.FleetPolicyInvalid)
			}
			if !strings.HasPrefix(err.Error(), CutoverRelPath+": ") {
				t.Errorf("error %q does not name document %q", err.Error(), CutoverRelPath)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Errorf("error %q does not name field %q", err.Error(), tc.wantField)
			}
		})
	}
}

func TestKeyDirectoryDocumentUnderPolicyTreeIsRejected(t *testing.T) {
	absPath := filepath.ToSlash(filepath.Join(t.TempDir(), "key-dir"))
	kdSource := "schema_version: 1\nkind: key-directory-source\npath: '" + absPath + "'\n"
	keyDirDoc := `schema_version: 1
kind: key-directory
id: alice
keys: []
`
	for _, misplacedPath := range []string{
		"fleet/access-groups/alice.yaml",
		"fleet/alice.yaml",
		"fleet/keys/alice.yaml",
	} {
		t.Run(misplacedPath, func(t *testing.T) {
			fsys := fstest.MapFS{
				"fleet/key-directory.yaml": {Data: []byte(kdSource)},
				misplacedPath:              {Data: []byte(keyDirDoc)},
			}
			_, err := LoadFS(fsys)
			if err == nil {
				t.Fatalf("accepted key-directory document at %s in policy tree", misplacedPath)
			}
			if got := Diagnostic(err); got != diag.FleetPolicyInvalid {
				t.Errorf("Diagnostic = %q, want %q", got, diag.FleetPolicyInvalid)
			}
			if !strings.Contains(err.Error(), misplacedPath) || !strings.Contains(err.Error(), "kind") {
				t.Errorf("error %q does not name document %q and field kind", err.Error(), misplacedPath)
			}
		})
	}
}

func TestLoadFromTempDirAndUnavailableErrors(t *testing.T) {
	base := t.TempDir()
	missingDir := filepath.Join(base, "absent")
	if _, err := Load(missingDir); !errors.Is(err, ErrUnavailable) || Diagnostic(err) != diag.FleetPolicyUnavailable {
		t.Fatalf("missing policy dir: got %v (%s), want MM_FLEET_POLICY_UNAVAILABLE", err, Diagnostic(err))
	}

	emptyPolicy := filepath.Join(base, "empty")
	if err := os.MkdirAll(filepath.Join(emptyPolicy, "fleet"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(emptyPolicy); !errors.Is(err, ErrUnavailable) || Diagnostic(err) != diag.FleetPolicyUnavailable {
		t.Fatalf("policy dir without fleet/key-directory.yaml: got %v (%s), want MM_FLEET_POLICY_UNAVAILABLE", err, Diagnostic(err))
	}

	// Valid policy checkout with frozen legacy fleet/machines/*.toml record.
	checkout := filepath.Join(base, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "fleet", "access-groups"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(checkout, "fleet", "machines"), 0o755); err != nil {
		t.Fatal(err)
	}
	kdYAML := "schema_version: 1\nkind: key-directory-source\ngist: 0123456789abcdef\n"
	if err := os.WriteFile(filepath.Join(checkout, "fleet", "key-directory.yaml"), []byte(kdYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "fleet", "access-groups", "workstations.yaml"), []byte(validAccessGroupYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "fleet", "machines", "sha256-legacy.toml"), []byte("schema_version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := Load(checkout)
	if err != nil {
		t.Fatalf("Load valid checkout: %v", err)
	}
	if len(p.Groups) != 1 || p.Groups[0].ID != "workstations" {
		t.Errorf("groups = %+v", p.Groups)
	}
	if len(p.Assets) != 2 || p.Assets[0].Name != "policy:fleet/access-groups/workstations.yaml" || p.Assets[1].Name != "policy:fleet/key-directory.yaml" {
		t.Errorf("assets = %+v", p.Assets)
	}
	if err := p.RequireCutover(); !errors.Is(err, ErrNotCutOver) || Diagnostic(err) != diag.FleetNotCutOver {
		t.Errorf("RequireCutover without cutover.yaml: got %v (%s), want MM_FLEET_NOT_CUT_OVER", err, Diagnostic(err))
	}

	// Dir resolution uses MANAGED_MACHINE_POLICY_DIR when set, otherwise <state>/managed-machine-config.
	stateDir := filepath.Join(base, "state")
	gotDefault, err := Dir(func(string) string { return "" }, stateDir)
	if err != nil || gotDefault != filepath.Join(stateDir, DefaultSubdir) {
		t.Errorf("Dir default = %q, %v", gotDefault, err)
	}
	gotExplicit, err := Dir(func(k string) string {
		if k == PolicyEnv {
			return checkout
		}
		return ""
	}, stateDir)
	if err != nil || gotExplicit != checkout {
		t.Errorf("Dir explicit = %q, %v", gotExplicit, err)
	}
}

func sub(old, new string) func(string) string {
	return func(s string) string { return strings.Replace(s, old, new, 1) }
}
