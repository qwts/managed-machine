package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/target"
)

// env reads a fixed environment.
func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

// The desired file of a target is the explicit one when it is set, the
// platform configuration directory for the user and global targets, and the
// custom root's own file for a custom target. A custom root never reads
// the invoking user's file.
func TestDesiredPath(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "home")
	appdata := filepath.Join(base, "appdata")
	xdg := filepath.Join(base, "xdg")
	root := filepath.Join(base, "soul")
	explicit := filepath.Join(base, "elsewhere.yaml")
	cases := []struct {
		name   string
		env    map[string]string
		goos   string
		target string
		root   string
		want   string
	}{
		{"linux without xdg", map[string]string{"HOME": home}, "linux", TargetUser, "", filepath.Join(home, ".config", "managed-machine", "desired.yaml")},
		{"linux with xdg", map[string]string{"HOME": home, "XDG_CONFIG_HOME": xdg}, "linux", TargetUser, "", filepath.Join(xdg, "managed-machine", "desired.yaml")},
		{"macos", map[string]string{"HOME": home}, "darwin", TargetGlobal, "", filepath.Join(home, ".config", "managed-machine", "desired.yaml")},
		{"windows", map[string]string{"APPDATA": appdata}, "windows", TargetUser, "", filepath.Join(appdata, "managed-machine", "desired.yaml")},
		{"custom root", map[string]string{"HOME": home}, "linux", TargetCustom, root, filepath.Join(root, ".managed-machine", "desired.yaml")},
		{"explicit file", map[string]string{DesiredEnv: explicit, "HOME": home}, "linux", TargetUser, "", explicit},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := DesiredPath(env(c.env), c.goos, c.target, c.root)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Errorf("path %q, want %q", got, c.want)
			}
		})
	}
}

// Without a home the platform directory cannot be found, and the command
// says so instead of guessing one.
func TestDesiredPathWithoutAHome(t *testing.T) {
	if _, err := DesiredPath(env(nil), "linux", TargetUser, ""); err == nil {
		t.Error("accepted an environment without a home")
	}
	if _, err := DesiredPath(env(nil), "windows", TargetUser, ""); err == nil {
		t.Error("accepted an environment without a configuration directory")
	}
}

// The desired file is data: it names values and paths, and nothing in it
// is executed.
func TestLoadDesired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "desired.yaml")
	rc := filepath.Join(dir, ".bashrc")
	write(t, path, `target: user
config:
  shell:
    inputs:
      editor: emacs
    destinations:
      shell-rc: '`+rc+`'
software:
  ripgrep:
    preference: brew
flavors: [work]
`)
	d, err := LoadDesired(path)
	if err != nil {
		t.Fatal(err)
	}
	if d.Target != "user" || len(d.Flavors) != 1 || d.Flavors[0] != "work" {
		t.Errorf("desired %+v", d)
	}
	if d.Config["shell"].Inputs["editor"] != "emacs" || d.Config["shell"].Destinations["shell-rc"] != rc {
		t.Errorf("desired config %+v", d.Config["shell"])
	}
	if d.Software["ripgrep"].Preference != "brew" {
		t.Errorf("desired software %+v", d.Software)
	}
}

// A missing file, a malformed file and a relative destination are all
// desired errors.
func TestLoadDesiredRejects(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadDesired(filepath.Join(dir, "absent.yaml")); err == nil {
		t.Error("accepted a missing desired file")
	}
	bad := filepath.Join(dir, "bad.yaml")
	write(t, bad, "config:\n  shell:\n    destinations:\n      shell-rc: relative/path\n")
	if _, err := LoadDesired(bad); err == nil {
		t.Error("accepted a relative destination")
	}
	malformed := filepath.Join(dir, "malformed.yaml")
	write(t, malformed, "config:\n  shell:\n    unknown: field\n")
	if _, err := LoadDesired(malformed); err == nil {
		t.Error("accepted an unknown field")
	}
}

// A resolution is exact: every destination an operation uses is mapped,
// every mapping is used, and the file agrees with the command.
func TestResolve(t *testing.T) {
	def, err := Parse("shell.yaml", []byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	mapped := Desired{
		Target: TargetUser,
		Config: map[string]DesiredConfig{"shell": {
			Inputs:       map[string]string{"editor": "emacs"},
			Destinations: map[string]string{"shell-rc": filepath.Join(home, ".bashrc")},
		}},
	}
	req := Request{Target: TargetUser, Platform: target.DarwinARM64}
	resolved, err := Resolve(def, mapped, req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Inputs["editor"] != "emacs" {
		t.Errorf("the desired input did not win: %+v", resolved.Inputs)
	}
	if resolved.Destinations["shell-rc"] != filepath.Join(home, ".bashrc") {
		t.Errorf("destination %+v", resolved.Destinations)
	}
	// An input falls back to the definition's default.
	def.Inputs["editor"] = "vim"
	resolved, err = Resolve(def, Desired{Config: map[string]DesiredConfig{"shell": {Destinations: mapped.Config["shell"].Destinations}}}, req)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Inputs["editor"] != "vim" {
		t.Errorf("the default input did not fall back: %+v", resolved.Inputs)
	}

	missing := Desired{Config: map[string]DesiredConfig{"shell": {Destinations: map[string]string{}}}}
	extra := Desired{Config: map[string]DesiredConfig{"shell": {Destinations: map[string]string{
		"shell-rc": filepath.Join(home, ".bashrc"), "other-rc": filepath.Join(home, ".other")}}}}
	undeclared := Desired{Config: map[string]DesiredConfig{"shell": {
		Inputs:       map[string]string{"nope": "x"},
		Destinations: mapped.Config["shell"].Destinations}}}
	cases := []struct {
		name    string
		desired Desired
		req     Request
		want    string
	}{
		{"a missing mapping", missing, req, `the destination "shell-rc" is not mapped`},
		{"an extra mapping", extra, req, `no operation for destination "other-rc"`},
		{"an undeclared input", undeclared, req, `declares no input "nope"`},
		{"no configuration", Desired{}, req, `names no configuration "shell"`},
		{"a target mismatch", mapped, Request{Target: TargetGlobal, Platform: target.DarwinARM64}, `names target "user"`},
		{"a root mismatch", Desired{Root: "/other/root", Config: mapped.Config}, Request{Target: TargetCustom, Root: "/soul/root"}, `names root "/other/root"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Resolve(def, c.desired, c.req)
			if err == nil {
				t.Fatal("accepted")
			}
			var de *DesiredError
			if !errors.As(err, &de) {
				t.Fatalf("got %v, want a desired error", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("message %q does not mention %q", err, c.want)
			}
		})
	}
}

// A definition that does not declare the requested target or the host
// platform is an unsupported combination, not a desired error.
func TestResolveUnsupportedCombination(t *testing.T) {
	def, err := Parse("shell.yaml", []byte(validConfig))
	if err != nil {
		t.Fatal(err)
	}
	mapped := Desired{Config: map[string]DesiredConfig{"shell": {Destinations: map[string]string{"shell-rc": "/home/user/.bashrc"}}}}
	for _, c := range []struct {
		name string
		req  Request
	}{
		{"another target", Request{Target: TargetGlobal, Platform: target.DarwinARM64}},
		{"another platform", Request{Target: TargetUser, Platform: target.WindowsAMD64}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := Resolve(def, mapped, c.req)
			var ue *UnsupportedError
			if !errors.As(err, &ue) {
				t.Fatalf("got %v, want an unsupported combination", err)
			}
		})
	}
}

// Every error of this package names the diagnostic it belongs to.
func TestDiagnostic(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"a desired error", &DesiredError{Field: "config.shell", Message: "no mapping"}, diag.ConfigDesiredInvalid},
		{"an unsupported combination", &UnsupportedError{Target: TargetUser, Platform: "darwin/arm64"}, diag.ConfigUnsupportedCombo},
		{"a source error", schema.Errorf(schema.Invalid, "doc.yaml", "id", "invalid"), diag.ConfigSourceInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Diagnostic(c.err); got != c.want {
				t.Errorf("diagnostic %q, want %q", got, c.want)
			}
		})
	}
}

// write writes a fixture file inside the test's own directory.
func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
