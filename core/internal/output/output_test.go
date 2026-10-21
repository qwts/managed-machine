package output

import (
	"bytes"
	"strings"
	"testing"
)

func statusRecords() []*Record {
	return []*Record{
		New().Set("section", "software").Set("resource", "ruff").Set("target", "user").
			Set("state", "present").SetOptional("facts", map[string]string{"version": "0.6.9", "provider": "uv"}).
			Set("evidence", "observed").SetOptional("at", "2026-10-20T09:15:00Z"),
		New().Set("section", "config").Set("resource", "zsh").Set("target", CustomTarget(`C:\souls\bob`)).
			Set("state", "unknown").SetOptional("facts", map[string]string{}).
			Set("evidence", "none").SetOptional("at", ""),
	}
}

func TestCompactBytesAreDeterministicAndOmitAbsentOptionalFields(t *testing.T) {
	want := `{"section":"software","resource":"ruff","target":"user","state":"present","facts":{"provider":"uv","version":"0.6.9"},"evidence":"observed","at":"2026-10-20T09:15:00Z"}` + "\n" +
		`{"section":"config","resource":"zsh","target":"custom:C:\\souls\\bob","state":"unknown","evidence":"none"}` + "\n"
	for i := 0; i < 3; i++ {
		var b bytes.Buffer
		if err := Write(&b, Compact, statusRecords()); err != nil {
			t.Fatal(err)
		}
		if b.String() != want {
			t.Fatalf("run %d:\n%s\nwant\n%s", i, b.String(), want)
		}
	}
	if strings.Contains(want, "null") {
		t.Fatal("test fixture contains null")
	}
}

func TestCompactRoundTripsToIdenticalBytes(t *testing.T) {
	var b bytes.Buffer
	if err := WriteCompact(&b, statusRecords()); err != nil {
		t.Fatal(err)
	}
	records, err := ReadCompact(b.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := WriteCompact(&again, records); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.Bytes(), again.Bytes()) {
		t.Errorf("round trip changed bytes:\n%s\n%s", b.String(), again.String())
	}
}

func TestReadCompactRejectsBlankLines(t *testing.T) {
	if _, err := ReadCompact([]byte("{\"a\":1}\n\n{\"b\":2}\n")); err == nil {
		t.Error("blank line accepted")
	}
}

func TestJSONDocumentShapes(t *testing.T) {
	var one bytes.Buffer
	if err := Write(&one, JSON, statusRecords()[:1]); err != nil {
		t.Fatal(err)
	}
	wantOne := "{\n  \"section\": \"software\",\n  \"resource\": \"ruff\",\n  \"target\": \"user\",\n  \"state\": \"present\",\n  \"facts\": {\n    \"provider\": \"uv\",\n    \"version\": \"0.6.9\"\n  },\n  \"evidence\": \"observed\",\n  \"at\": \"2026-10-20T09:15:00Z\"\n}\n"
	if one.String() != wantOne {
		t.Errorf("single record --json:\n%s\nwant\n%s", one.String(), wantOne)
	}
	var many bytes.Buffer
	if err := Write(&many, JSON, statusRecords()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(many.String(), "{\n  \"records\": [\n") || !strings.HasSuffix(many.String(), "  ]\n}\n") {
		t.Errorf("multi-record --json is not a records document:\n%s", many.String())
	}
	var none bytes.Buffer
	if err := Write(&none, JSON, nil); err != nil {
		t.Fatal(err)
	}
	if none.String() != "{\n  \"records\": []\n}\n" {
		t.Errorf("empty --json = %q", none.String())
	}
}

func TestHumanTable(t *testing.T) {
	var b bytes.Buffer
	if err := Write(&b, Human, statusRecords()); err != nil {
		t.Fatal(err)
	}
	want := "SECTION   RESOURCE  TARGET               STATE    FACTS                       EVIDENCE  AT\n" +
		"software  ruff      user                 present  provider=uv, version=0.6.9  observed  2026-10-20T09:15:00Z\n" +
		"config    zsh       custom:C:\\souls\\bob  unknown                              none\n"
	if b.String() != want {
		t.Errorf("human table:\n%s\nwant\n%s", b.String(), want)
	}
	var empty bytes.Buffer
	if err := Write(&empty, Human, nil); err != nil || empty.Len() != 0 {
		t.Errorf("empty human output = %q, %v", empty.String(), err)
	}
}

func TestSelectMode(t *testing.T) {
	if m, err := SelectMode(false, false); err != nil || m != Compact {
		t.Errorf("default = %v, %v", m, err)
	}
	if m, err := SelectMode(true, false); err != nil || m != JSON {
		t.Errorf("--json = %v, %v", m, err)
	}
	if m, err := SelectMode(false, true); err != nil || m != Human {
		t.Errorf("--human = %v, %v", m, err)
	}
	if _, err := SelectMode(true, true); err != ErrConflictingModes {
		t.Errorf("--json --human = %v, want ErrConflictingModes", err)
	}
}

func TestSetReplacesInPlaceAndNonASCIIIsPreserved(t *testing.T) {
	r := New().Set("a", "1").Set("b", "ünïcödé — ✓").Set("a", "2")
	var b bytes.Buffer
	if err := WriteCompact(&b, []*Record{r}); err != nil {
		t.Fatal(err)
	}
	if b.String() != `{"a":"2","b":"ünïcödé — ✓"}`+"\n" {
		t.Errorf("got %q", b.String())
	}
}
