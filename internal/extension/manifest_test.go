package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/qwts/managed-machine/internal/protocol"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/schema"
	"github.com/qwts/managed-machine/internal/target"
)

// TestMain doubles as the test-only extension: when MM_EXTENSION_FIXTURE
// is set, the test binary answers one mm-extension-1 request and exits, so
// protocol compatibility is proven without building or installing anything.
func TestMain(m *testing.M) {
	if mode := os.Getenv("MM_EXTENSION_FIXTURE"); mode != "" {
		os.Exit(fixture(mode))
	}
	os.Exit(m.Run())
}

func fixture(mode string) int {
	var req protocol.Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil || req.Protocol != protocol.Version || req.Method != "describe" {
		fmt.Printf(`{"protocol":"mm-extension-1","id":%q,"error":{"code":"bad-request","message":"unexpected request"}}`+"\n", req.ID)
		return 0
	}
	version := "0.3.0"
	if mode == "wrong-version" {
		version = "0.4.0"
	}
	out, _ := json.Marshal(map[string]any{"protocol": protocol.Version, "id": req.ID,
		"result": map[string]any{"name": "cargo-tools", "version": version, "providers": []string{"cargo-tools"}}})
	fmt.Println(string(out))
	return 0
}

func fixtureEnv(mode string) []string {
	env := []string{"MM_EXTENSION_FIXTURE=" + mode}
	if v := os.Getenv("SYSTEMROOT"); v != "" {
		env = append(env, "SYSTEMROOT="+v) // Windows processes need it to start
	}
	return env
}

func load(t *testing.T) Manifest {
	t.Helper()
	m, err := Discover(os.DirFS("testdata/cargo-tools"), "cargo-tools")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestDiscoverAndVerifyNeverExecute(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "executed")
	t.Setenv("MM_FIXTURE_MARKER", marker)
	m := load(t)
	if m.Name != "cargo-tools" || m.Version != "0.3.0" || len(m.Executables) != 2 || m.Provides.Providers[0].Name != "cargo-tools" {
		t.Fatalf("manifest %+v", m)
	}
	if _, err := Verify(os.DirFS("testdata/cargo-tools"), "cargo-tools", m, target.DarwinARM64); err != nil {
		t.Fatal(err)
	}
	reg := registry.Builtin()
	if err := RegisterProviders(reg, m); err != nil {
		t.Fatal(err)
	}
	if p, ok := reg.Lookup("cargo-tools"); !ok || p.Origin != "extension:cargo-tools@0.3.0" {
		t.Errorf("cargo-tools provider %+v", p)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discovery or verification executed the extension binary")
	}
}

func TestVerifyRejectsTamperingAndMissingTargets(t *testing.T) {
	m := load(t)
	exe, _ := os.ReadFile("testdata/cargo-tools/bin/mm-cargo")
	asset, _ := os.ReadFile("testdata/cargo-tools/share/notes.txt")
	tampered := fstest.MapFS{"bin/mm-cargo": {Data: append(exe, '#')}, "share/notes.txt": {Data: asset}}
	if _, err := Verify(tampered, "x", m, target.DarwinARM64); !errors.Is(err, schema.Err(schema.DigestMismatch)) {
		t.Errorf("tampered executable: %v", err)
	}
	badAsset := fstest.MapFS{"bin/mm-cargo": {Data: exe}, "share/notes.txt": {Data: []byte("x")}}
	if _, err := Verify(badAsset, "x", m, target.LinuxAMD64); !errors.Is(err, schema.Err(schema.DigestMismatch)) {
		t.Errorf("tampered asset: %v", err)
	}
	if _, err := Verify(os.DirFS("testdata/cargo-tools"), "x", m, target.WindowsARM64); err == nil || !strings.Contains(err.Error(), "no executable for windows/arm64") {
		t.Errorf("target without an executable: %v", err)
	}
}

func TestParseRejects(t *testing.T) {
	base, err := os.ReadFile("testdata/cargo-tools/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	sum := strings.Repeat("a", 64)
	cases := []struct {
		name, old, new string
		kind           schema.Kind
	}{
		{"short version", "version: 0.3.0", "version: 0.3", schema.InvalidVersion},
		{"v-prefixed version", "version: 0.3.0", "version: v0.3.0", schema.InvalidVersion},
		{"build metadata", "version: 0.3.0", "version: 0.3.0+abc", schema.InvalidVersion},
		{"leading zero", "version: 0.3.0", "version: 0.03.0", schema.InvalidVersion},
		{"future protocol", "protocol: mm-extension-1", "protocol: mm-extension-2", schema.InvalidVersion},
		{"future schema", "schema_version: 1", "schema_version: 2", schema.InvalidVersion},
		{"unknown field", "protocol: mm-extension-1", "protocol: mm-extension-1\nentrypoint: bin/mm-cargo", schema.UnknownField},
		{"unknown capability", "[provider]", "[provider, shell]", schema.Invalid},
		{"capability without providers", "[provider]", "[provider, skills]", schema.Invalid},
		{"duplicate platform", "platform: linux/amd64", "platform: darwin/arm64", schema.Conflict},
		{"platform alias", "platform: linux/amd64", "platform: linux/x86_64", schema.Invalid},
		{"escaping path", "path: share/notes.txt", "path: ../notes.txt", schema.Invalid},
		{"absolute path", "path: share/notes.txt", "path: /etc/passwd", schema.Invalid},
		{"unclean path", "path: share/notes.txt", "path: share/./notes.txt", schema.Invalid},
		{"uppercase digest", "sha256: 5495eb", "sha256: 5495EB", schema.Invalid},
		{"unknown option type", "locked: {type: bool}", "locked: {type: int}", schema.Invalid},
		{"invalid provider name", "- name: cargo-tools", "- name: Cargo-tools", schema.Invalid},
		{"short digest", "sha256: 5495ebeaf22917a469df9a9753de145b2a354562f2043093a3ab958395caeced", "sha256: " + sum[:63], schema.Invalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc := strings.Replace(string(base), c.old, c.new, 1)
			if doc == string(base) {
				t.Fatal("edit did not apply")
			}
			if _, err := Parse("m", []byte(doc)); !errors.Is(err, schema.Err(c.kind)) {
				t.Errorf("got %v, want %s", err, c.kind)
			}
		})
	}
}

func TestProviderConflictWithBuiltin(t *testing.T) {
	m := load(t)
	m.Provides.Providers[0].Name = "brew"
	if err := RegisterProviders(registry.Builtin(), m); !errors.Is(err, schema.Err(schema.Conflict)) {
		t.Errorf("extension provider named brew: %v", err)
	}
}

func TestDescribeMatchesManifest(t *testing.T) {
	m := load(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d, err := Describe(context.Background(), []string{self}, fixtureEnv("ok"), m)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != "cargo-tools" || d.Version != "0.3.0" {
		t.Errorf("description %+v", d)
	}
	if _, err := Describe(context.Background(), []string{self}, fixtureEnv("wrong-version"), m); err == nil || !strings.Contains(err.Error(), "does not match manifest") {
		t.Errorf("version drift between manifest and binary: %v", err)
	}
}
