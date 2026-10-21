package legacy

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/source"
)

var update = flag.Bool("update", false, "rewrite testdata/report.golden.json")

func fixture(t *testing.T) (Report, []map[string]any) {
	t.Helper()
	data, err := os.ReadFile("testdata/apps.json")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Classify(data, registry.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Apps []map[string]any }
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return rep, doc.Apps
}

func TestReportGolden(t *testing.T) {
	rep, _ := fixture(t)
	got, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *update {
		os.WriteFile("testdata/report.golden.json", got, 0o644)
	}
	want, err := os.ReadFile("testdata/report.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("report differs from golden (run with -update and review the diff)\n%s", got)
	}
}

// Every input row is classified exactly once and every legacy kind in the
// fixture is exercised.
func TestEveryRowClassified(t *testing.T) {
	rep, apps := fixture(t)
	if len(rep.Rows) != len(apps) || rep.Counts["rows"] != len(apps) {
		t.Fatalf("%d rows in, %d classified", len(apps), len(rep.Rows))
	}
	kinds := map[string]bool{}
	for _, r := range rep.Rows {
		kinds[r.Kind] = true
		if r.Verdict != Mappable && r.Verdict != Manual && r.Verdict != Unmappable {
			t.Errorf("%s: verdict %q", r.Name, r.Verdict)
		}
		if r.Verdict == Unmappable && r.Software != nil || r.Verdict != Unmappable && r.Software == nil {
			t.Errorf("%s: verdict %s with candidate %v", r.Name, r.Verdict, r.Software != nil)
		}
		if r.Verdict != Mappable && !hasAction(r, Blocked) {
			t.Errorf("%s: %s without a blocked item saying why", r.Name, r.Verdict)
		}
	}
	for k := range knownFields {
		if !kinds[k] {
			t.Errorf("fixture does not exercise legacy kind %s", k)
		}
	}
}

// Every field of every row is accounted for by an item, and every trust
// field is carried with its exact value or explicitly retained.
func TestNoFieldSilentlyDropped(t *testing.T) {
	rep, apps := fixture(t)
	byName := map[string]Row{}
	for _, r := range rep.Rows {
		byName[r.Name] = r
	}
	for _, app := range apps {
		r := byName[app["name"].(string)]
		for field, value := range app {
			if field == "name" || field == "kind" && r.Kind != "mas-app" {
				continue
			}
			it, ok := item(r, field)
			if !ok {
				t.Errorf("%s: field %s has no item", r.Name, field)
				continue
			}
			switch field {
			case "team_id", "url_hosts", "homepage_hosts", "sha256", "allow_rolling_url":
				if it.Action == Retained {
					continue
				}
				if it.Action != Carried {
					t.Errorf("%s: trust field %s is %s", r.Name, field, it.Action)
					continue
				}
				got, _ := json.Marshal(r.Software.Recipes[0].Options[field])
				want, _ := json.Marshal(value)
				if !bytes.Equal(got, want) {
					t.Errorf("%s: %s carried as %s, source %s", r.Name, field, got, want)
				}
			}
		}
	}
}

// A pinned version is desired input and never appears in a candidate.
func TestVersionsAreDesiredInput(t *testing.T) {
	rep, apps := fixture(t)
	for _, app := range apps {
		v, ok := app["version"].(string)
		if !ok {
			continue
		}
		for _, r := range rep.Rows {
			if r.Name != app["name"] {
				continue
			}
			if r.Desired["version"] != v {
				t.Errorf("%s: desired version %q, source %q", r.Name, r.Desired["version"], v)
			}
			cand, _ := json.Marshal(r.Software)
			if bytes.Contains(cand, []byte(v)) {
				t.Errorf("%s: version %s embedded in the candidate %s", r.Name, v, cand)
			}
		}
	}
}

func TestAliasCollisionsAndAuto(t *testing.T) {
	rep, _ := fixture(t)
	if len(rep.Collisions) != 2 {
		t.Fatalf("collisions %+v", rep.Collisions)
	}
	// An alias equal to another row's name is a collision as much as an
	// alias two rows share.
	if c := rep.Collisions[0]; c.Alias != "fixture-npm" || strings.Join(c.Rows, ",") != "fixture-cask,fixture-npm" {
		t.Errorf("name collision %+v", c)
	}
	if c := rep.Collisions[1]; c.Alias != "shared-alias" || strings.Join(c.Rows, ",") != "fixture-cask,fixture-formula" {
		t.Errorf("alias collision %+v", c)
	}
	for _, r := range rep.Rows {
		want := "bootstrap"
		if r.Name == "fixture-cask-rolling" {
			want = "setup-only"
		}
		if r.Auto != want {
			t.Errorf("%s: auto %s, want %s", r.Name, r.Auto, want)
		}
	}
}

// When MM_PRIVATE_CATALOG names the private apps.json (local runs only),
// the report covers its 52 rows and no private value appears in any
// committed fixture of this package.
func TestPrivateCatalogCoverage(t *testing.T) {
	path := os.Getenv("MM_PRIVATE_CATALOG")
	if path == "" {
		t.Skip("MM_PRIVATE_CATALOG not set; the private catalog is checked on the owner's machine only")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Classify(data, registry.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	if rep.Counts["rows"] != 52 {
		t.Errorf("%d rows classified, the pinned revision has 52", rep.Counts["rows"])
	}
	var doc struct{ Apps []map[string]any }
	json.Unmarshal(data, &doc)
	files, _ := filepath.Glob("testdata/*")
	for _, f := range files {
		pub, _ := os.ReadFile(f)
		for _, app := range doc.Apps {
			for field, v := range app {
				for _, s := range stringsOf(v) {
					// Kind names are public (the F01 inventory lists them;
					// two rows are named after their kind) and so are JSON
					// literals.
					_, isKind := knownFields[s]
					if field != "kind" && !isKind && s != "true" && s != "false" && len(s) >= 4 && bytes.Contains(pub, []byte(s)) {
						t.Errorf("%s contains a private %s value", f, field)
					}
				}
			}
		}
	}
}

func stringsOf(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, stringsOf(e)...)
		}
		return out
	case map[string]any:
		var out []string
		for k, e := range x {
			out = append(out, k)
			out = append(out, stringsOf(e)...)
		}
		return out
	}
	return nil
}

func item(r Row, field string) (Item, bool) {
	for _, it := range r.Items {
		if it.Field == field {
			return it, true
		}
	}
	return Item{}, false
}

func hasAction(r Row, action string) bool {
	for _, it := range r.Items {
		if it.Action == action {
			return true
		}
	}
	return false
}

// The representative converted fixture: candidates written as F04 files.
// Each committed file must parse with the F04 loader to the candidate.
func TestConvertedFixture(t *testing.T) {
	rep, _ := fixture(t)
	n := 0
	for _, r := range rep.Rows {
		if r.Software == nil || r.Verdict != Mappable && r.Name != "fixture-cask" {
			continue
		}
		n++
		got, err := CandidateYAML(*r.Software)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "converted", r.Name+".yaml")
		if *update {
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, got, 0o644)
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from its candidate:\n%s", path, got)
		}
	}
	if n != 3 {
		t.Errorf("%d converted fixtures, want 3", n)
	}
}

func TestConvertedFixtureLoadsAsASource(t *testing.T) {
	snap, err := source.Load(source.Identity{Kind: "local", Location: "legacy/converted"}, os.DirFS("testdata"), "converted", registry.Builtin())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Entries) != 3 {
		t.Errorf("%d entries", len(snap.Entries))
	}
}
