package config

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/install"
	"github.com/qwts/managed-machine/core/internal/registry"
	"github.com/qwts/managed-machine/core/internal/software"
	"github.com/qwts/managed-machine/core/internal/source"
	"github.com/qwts/managed-machine/core/internal/target"
)

// TestFlavorSourceInvalid proves that flavor definitions in a source are
// validated and that a flavor entry naming a custom target with a root or an
// unknown target fails with MM_CONFIG_SOURCE_INVALID (exit 2).
func TestFlavorSourceInvalid(t *testing.T) {
	cases := []struct {
		name   string
		flavor string
		want   string
	}{
		{
			name: "software custom target with colon root",
			flavor: `kind: flavor
schema_version: 1
id: work
description: Invalid flavor.
software:
  - id: ripgrep
    target: custom:/opt/soul
`,
			want: "must not name a root",
		},
		{
			name: "software custom target with root field",
			flavor: `kind: flavor
schema_version: 1
id: work
description: Invalid flavor.
software:
  - id: ripgrep
    target: custom
    root: /opt/soul
`,
			want: "must not name a root",
		},
		{
			name: "config custom target with colon root",
			flavor: `kind: flavor
schema_version: 1
id: work
description: Invalid flavor.
config:
  - id: first
    target: custom:/opt/soul
`,
			want: "must not name a root",
		},
		{
			name: "config custom target with root field",
			flavor: `kind: flavor
schema_version: 1
id: work
description: Invalid flavor.
config:
  - id: first
    target: custom
    root: /opt/soul
`,
			want: "must not name a root",
		},
		{
			name: "unknown target on flavor entry",
			flavor: `kind: flavor
schema_version: 1
id: work
description: Invalid flavor.
config:
  - id: first
    target: system
`,
			want: `unknown installation target \"system\"`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newExecEnv(t)
			e.writeSource("files/a", "alpha\n")
			e.writeSource("first.yaml", `kind: config
schema_version: 1
id: first
description: First config.
targets: [user]
operations:
  - id: main
    provider: file
    destination: dest
    source: files/a
`)
			e.writeSource("work.yaml", c.flavor)
			dest := filepath.Join(e.homeDir, ".rc")
			e.writeDesired(fmt.Sprintf("target: user\nflavors: [work]\nconfig:\n  first:\n    destinations:\n      dest: %s\n", filepath.ToSlash(dest)))

			out, errOut, code := e.run("first", "--plan", "--user")
			if code != 2 || out != "" || !strings.Contains(errOut, "MM_CONFIG_SOURCE_INVALID") || !strings.Contains(errOut, c.want) {
				t.Fatalf("got exit %d stdout=%q stderr=%q, want MM_CONFIG_SOURCE_INVALID mentioning %q", code, out, errOut, c.want)
			}
		})
	}
}

// TestUnknownFlavorInDesired proves that an unknown flavor name in
// desired.flavors fails with MM_CONFIG_DESIRED_INVALID (exit 2).
func TestUnknownFlavorInDesired(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("files/a", "alpha\n")
	e.writeSource("first.yaml", `kind: config
schema_version: 1
id: first
description: First config.
targets: [user]
operations:
  - id: main
    provider: file
    destination: dest
    source: files/a
`)
	dest := filepath.Join(e.homeDir, ".rc")
	e.writeDesired(fmt.Sprintf("target: user\nflavors: [no-such-flavor]\nconfig:\n  first:\n    destinations:\n      dest: %s\n", filepath.ToSlash(dest)))

	for _, action := range []string{"--plan", "--apply", "--check"} {
		out, errOut, code := e.run("first", action, "--user")
		if code != 2 || out != "" || !strings.Contains(errOut, "MM_CONFIG_DESIRED_INVALID") || !strings.Contains(errOut, `unknown flavor \"no-such-flavor\"`) {
			t.Errorf("%s with unknown flavor: exit %d stdout=%q stderr=%q", action, code, out, errOut)
		}
	}
}

// TestFlavorCompositionConflictsAndDedupe proves that:
//  1. Without desired.flavors, planning and applying `first` succeeds even
//     when `second` maps to the same destination in desired.yaml.
//  2. With desired.flavors composing `first` and `second` for the target and
//     resolving to one destination with different values, `config first --plan`
//     emits a `conflict` action naming `config:<id>@<target>:<operation>`,
//     `--apply` refuses with MM_CONFIG_CONFLICT (exit 1), and `--check`
//     reports the conflict with MM_CONFIG_CONFLICT (exit 1).
//  3. When `first` and `second` resolve to one destination with identical
//     values, the pair deduplicates and `--plan`, `--apply` and `--check`
//     succeed without conflict.
//  4. Flavor entries for other targets do not collide with the command's
//     target, and `target: custom` in a flavor takes its custom root from
//     desired.yaml.
func TestFlavorCompositionConflictsAndDedupe(t *testing.T) {
	e := newExecEnv(t)
	e.writeSource("templates/rc.tmpl", "EDITOR={{ inputs.editor }}\n")
	e.writeSource("first.yaml", `kind: config
schema_version: 1
id: first
description: First editor config.
targets: [global, user, custom]
inputs:
  editor: vim
operations:
  - id: main
    provider: template
    destination: dest
    source: templates/rc.tmpl
`)
	e.writeSource("second.yaml", `kind: config
schema_version: 1
id: second
description: Second editor config.
targets: [global, user, custom]
inputs:
  editor: emacs
operations:
  - id: main
    provider: template
    destination: dest
    source: templates/rc.tmpl
`)
	e.writeSource("work.yaml", `kind: flavor
schema_version: 1
id: work
description: Work flavor composing first and second.
software:
  - id: ripgrep
    target: user
    version: 14.1.0
config:
  - id: first
    target: user
  - id: second
    target: user
  - id: second
    target: global
`)

	sharedDest := filepath.Join(e.homeDir, ".editorrc")

	// 1. Without flavors in desired.yaml, behavior is unchanged: first plans create.
	e.writeDesired(fmt.Sprintf(`target: user
config:
  first:
    inputs:
      editor: vim
    destinations:
      dest: %s
  second:
    inputs:
      editor: emacs
    destinations:
      dest: %s
`, filepath.ToSlash(sharedDest), filepath.ToSlash(sharedDest)))

	out, errOut, code := e.run("first", "--plan", "--user")
	if code != 0 || errOut != "" || !strings.Contains(out, `"action":"create"`) {
		t.Fatalf("plan without flavors: exit %d stdout=%q stderr=%q", code, out, errOut)
	}

	// 2. With desired.flavors: [work] and differing rendered values (vim vs emacs)
	// for sharedDest:
	// - --plan emits a conflict action naming config:first@user:main and config:second@user:main
	// - --apply refuses with MM_CONFIG_CONFLICT (exit 1) and writes nothing
	// - --check reports the conflict action with MM_CONFIG_CONFLICT (exit 1)
	e.writeDesired(fmt.Sprintf(`target: user
flavors: [work]
config:
  first:
    inputs:
      editor: vim
    destinations:
      dest: %s
  second:
    inputs:
      editor: emacs
    destinations:
      dest: %s
`, filepath.ToSlash(sharedDest), filepath.ToSlash(sharedDest)))

	out, errOut, code = e.run("first", "--plan", "--user")
	if code != 0 || errOut != "" {
		t.Fatalf("plan with conflicting flavor: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	if !strings.Contains(out, `"operation":"main","target":"user","action":"conflict `) ||
		!strings.Contains(out, "config:first@user:main") ||
		!strings.Contains(out, "config:second@user:main") {
		t.Fatalf("plan conflict output missing expected identities:\n%s", out)
	}

	out, errOut, code = e.run("first", "--apply", "--user")
	if code != 1 || out != "" || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") {
		t.Fatalf("apply with conflicting flavor: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	if _, err := os.Stat(sharedDest); !os.IsNotExist(err) {
		t.Fatal("conflicting flavor apply created destination on disk")
	}

	out, errOut, code = e.run("first", "--check", "--user")
	if code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") || !strings.Contains(out, `"action":"conflict `) {
		t.Fatalf("check with conflicting flavor: exit %d stdout=%q stderr=%q", code, out, errOut)
	}

	// 3. When both configs resolve to the same destination with identical values
	// (both vim), identical destination-and-value pairs dedupe: --plan plans create,
	// --apply writes the file, and --check exits 0.
	e.writeDesired(fmt.Sprintf(`target: user
flavors: [work, work]
config:
  first:
    inputs:
      editor: vim
    destinations:
      dest: %s
  second:
    inputs:
      editor: vim
    destinations:
      dest: %s
`, filepath.ToSlash(sharedDest), filepath.ToSlash(sharedDest)))

	out, errOut, code = e.run("first", "--plan", "--user")
	if code != 0 || errOut != "" || !strings.Contains(out, `"action":"create"`) || strings.Contains(out, "conflict") {
		t.Fatalf("plan with identical flavored values: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	out, errOut, code = e.run("first", "--apply", "--user")
	if code != 0 || errOut != "" || !strings.Contains(out, `"state":"applied"`) {
		t.Fatalf("apply with identical flavored values: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	out, errOut, code = e.run("first", "--check", "--user")
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("check after apply with identical flavored values: exit %d stdout=%q stderr=%q", code, out, errOut)
	}

	// 4. Custom target in a flavor (`target: custom` without root in flavor YAML,
	// root supplied by <customRoot>/.managed-machine/desired.yaml).
	e.writeSource("custom-flavor.yaml", `kind: flavor
schema_version: 1
id: custom-flavor
description: Custom target flavor.
config:
  - id: first
    target: custom
  - id: second
    target: custom
`)
	customRoot := filepath.Join(e.dir, "soul")
	customDest := filepath.Join(customRoot, "etc", "editorrc")
	customDesired := filepath.Join(customRoot, ".managed-machine", "desired.yaml")
	if err := os.MkdirAll(filepath.Dir(customDesired), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(customDesired, []byte(fmt.Sprintf(`target: custom
root: %s
flavors: [custom-flavor]
config:
  first:
    inputs:
      editor: vim
    destinations:
      dest: %s
  second:
    inputs:
      editor: emacs
    destinations:
      dest: %s
`, filepath.ToSlash(customRoot), filepath.ToSlash(customDest), filepath.ToSlash(customDest))), 0o644); err != nil {
		t.Fatal(err)
	}
	e.desired = ""

	out, errOut, code = e.run("first", "--plan", "--custom", customRoot)
	if code != 0 || errOut != "" || !strings.Contains(out, "config:first@custom:main") || !strings.Contains(out, "config:second@custom:main") {
		t.Fatalf("custom target flavor plan: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	if _, errOut, code = e.run("first", "--apply", "--custom", customRoot); code != 1 || !strings.Contains(errOut, "MM_CONFIG_CONFLICT") {
		t.Fatalf("custom target flavor apply: exit %d stderr=%q", code, errOut)
	}
}

// TestSoftwareInstallDoesNotConsultFlavorSeam proves that `software --install`
// does not invoke the flavor composition seam or read desired.flavors (call
// count zero through the existing seams).
func TestSoftwareInstallDoesNotConsultFlavorSeam(t *testing.T) {
	composeCalls := 0
	desiredCalls := 0
	installCalls := 0
	var out, errb bytes.Buffer
	app := &cli.App{
		Name:   "managed-machine",
		Stdout: &out,
		Stderr: &errb,
		Detect: func() (target.Host, error) { return target.Host{Target: target.DarwinARM64}, nil },
		Commands: []cli.Command{
			software.Command(software.Deps{
				Catalog: func() ([]source.Entry, *registry.Registry, error) {
					return software.LoadCatalog(func(k string) string {
						if k == DesiredEnv {
							desiredCalls++
						}
						return ""
					})
				},
				Install: func(_ context.Context, _ install.Request) (install.Result, error) {
					installCalls++
					return install.Result{ResolvedVersion: "14.1.1", Outcome: "ok", Plan: "plan-20261021T120000Z-3f9c"}, nil
				},
				Getenv: func(k string) string {
					if k == DesiredEnv {
						desiredCalls++
					}
					return ""
				},
			}),
			Command(Deps{
				Compose: func(def Definition, _ Desired) (Definition, error) {
					composeCalls++
					return def, nil
				},
			}),
		},
	}

	code := app.Run([]string{"software", "ripgrep", "--install", "--global"})
	if code != 0 || errb.String() != "" {
		t.Fatalf("software --install: exit %d stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	if composeCalls != 0 || desiredCalls != 0 {
		t.Fatalf("composeCalls=%d desiredCalls=%d, want 0 and 0", composeCalls, desiredCalls)
	}
	if installCalls != 1 {
		t.Fatalf("installCalls=%d, want 1", installCalls)
	}
}

// TestFlavorComposesSecretInputsFromTheEnvironment proves that a config
// composed through desired.flavors resolves its secret inputs the same way the
// planned config does, and that the conflict it causes names no secret value.
func TestFlavorComposesSecretInputsFromTheEnvironment(t *testing.T) {
	const sec = "fixture-flavor-secret-778899"
	h := newSecretHarness(t)
	h.writeSource("templates/rc.tmpl", "EDITOR={{ inputs.editor }}\n")
	for _, id := range []string{"first", "second"} {
		h.writeSource(id+".yaml", fmt.Sprintf(`kind: config
schema_version: 1
id: %s
description: Editor config.
targets: [user]
inputs:
  editor: vim
operations:
  - id: main
    provider: template
    destination: dest
    source: templates/rc.tmpl
`, id))
	}
	h.writeSource("work.yaml", `kind: flavor
schema_version: 1
id: work
description: Work flavor composing first and second.
config:
  - id: first
    target: user
  - id: second
    target: user
`)
	dest := filepath.Join(h.homeDir, ".editorrc")
	h.writeDesired(fmt.Sprintf(`target: user
flavors: [work]
config:
  first:
    destinations:
      dest: %s
  second:
    inputs:
      editor: secret:work/editor
    destinations:
      dest: %s
`, filepath.ToSlash(dest), filepath.ToSlash(dest)))
	h.extraEnv["MM_SECRET_WORK_EDITOR"] = sec

	out, errOut, code := h.run("first", "--plan", "--user")
	if code != 0 || errOut != "" || !strings.Contains(out, "config:second@user:main") {
		t.Fatalf("plan: exit %d stdout=%q stderr=%q", code, out, errOut)
	}
	h.assertZeroHits(sec)
}
