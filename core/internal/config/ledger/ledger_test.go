package ledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPathLocations(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	soul := filepath.Join(base, "soul")

	for _, tg := range []string{"user", "global"} {
		got, err := Path(state, tg)
		if err != nil {
			t.Fatalf("%s: %v", tg, err)
		}
		if want := filepath.Join(state, FileName); got != want {
			t.Errorf("%s: got %q, want %q", tg, got, want)
		}
	}
	got, err := Path(state, "custom:"+soul)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(soul, ".managed-machine", FileName); got != want {
		t.Errorf("custom: got %q, want %q", got, want)
	}
	if _, err := Path("", "user"); err == nil {
		t.Error("accepted empty state dir for user")
	}
	if _, err := Path(state, "custom:"); err == nil {
		t.Error("accepted empty custom root")
	}
	if _, err := Path(state, "system"); err == nil {
		t.Error("accepted unknown target")
	}
}

func TestRecordKeyOrderAndLatestWins(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, FileName)
	dest := filepath.Join(dir, "gitconfig")

	// Reading an absent ledger creates nothing.
	snap, err := Load(p)
	if err != nil || len(snap.Records()) != 0 {
		t.Fatalf("absent load: %+v %v", snap, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("Load created the ledger file")
	}

	r1 := Record{
		SchemaVersion: SchemaVersion,
		Kind:          KindFile,
		Path:          dest,
		SHA256:        "aa",
		Config:        "gitconfig",
		Operation:     "config",
		Target:        "user",
		Account:       "tester",
		Plan:          "plan-20261021T120000Z-0001",
		At:            "2026-10-21T12:00:00Z",
		State:         Owned,
	}
	if err := Append(p, r1); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, `{"schema_version":1,"kind":"file","path":`) ||
		!strings.HasSuffix(line, `"config":"gitconfig","operation":"config","target":"user","account":"tester","plan":"plan-20261021T120000Z-0001","at":"2026-10-21T12:00:00Z","state":"owned"}`) {
		t.Errorf("unexpected serialized record:\n%s", line)
	}

	snap, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.IsOwnedBy("gitconfig", "user", dest, KindFile, "") {
		t.Error("expected destination to be owned by gitconfig")
	}
	if snap.IsOwnedBy("other", "user", dest, KindFile, "") {
		t.Error("destination reported as owned by another config")
	}

	// Releasing supersedes the owned record for the same (target, path, kind, id).
	r2 := Record{
		SchemaVersion: SchemaVersion,
		Kind:          KindFile,
		Path:          dest,
		Config:        "gitconfig",
		Operation:     "config",
		Target:        "user",
		Account:       "tester",
		Plan:          "plan-20261021T120001Z-0002",
		At:            "2026-10-21T12:00:01Z",
		State:         Released,
	}
	if err := Append(p, r2); err != nil {
		t.Fatal(err)
	}
	snap, err = Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if snap.IsOwnedBy("gitconfig", "user", dest, KindFile, "") {
		t.Error("released record still reported as owned")
	}
	if len(snap.OwnedBy("gitconfig", "user")) != 0 {
		t.Errorf("OwnedBy after release = %+v", snap.OwnedBy("gitconfig", "user"))
	}
	if last := snap.LatestByOperation("gitconfig", "user")["config"]; last.State != Released {
		t.Errorf("LatestByOperation = %+v", last)
	}
}
