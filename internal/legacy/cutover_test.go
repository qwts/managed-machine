package legacy_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwts/managed-machine/internal/catalog"
	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/command"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/registry"
	"github.com/qwts/managed-machine/internal/software"
	"github.com/qwts/managed-machine/internal/source"
	"github.com/qwts/managed-machine/internal/target"
)

func TestSetupCutoverFragmentByteEqual(t *testing.T) {
	want := []byte(legacy.RenderSetupCutoverSh())
	path := filepath.Join("..", "..", "lib", "setup-cutover.sh")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is out of date with legacy.SetupCutover; run 'go generate ./internal/legacy'\n--- got ---\n%s--- want ---\n%s", path, got, want)
	}
}

func TestSetupCutoverUniquenessAndInvariants(t *testing.T) {
	if len(legacy.SetupCutover) != 19 {
		t.Fatalf("SetupCutover count = %d, want 19", len(legacy.SetupCutover))
	}

	routeTokens := map[string]string{}
	for _, r := range legacy.Routes {
		for _, tok := range []string{r.Name, r.Command, r.Subcommand, r.Section} {
			if tok != "" {
				routeTokens[tok] = r.Name
			}
		}
	}

	seen := map[string]string{}
	prevName := ""
	for _, sr := range legacy.SetupCutover {
		if sr.Name <= prevName {
			t.Errorf("SetupCutover not strictly sorted by Name: %q after %q", sr.Name, prevName)
		}
		prevName = sr.Name

		if sr.Software != sr.Name {
			t.Errorf("%s: Software = %q, want %q", sr.Name, sr.Software, sr.Name)
		}
		if sr.Target != "global" {
			t.Errorf("%s: Target = %q, want \"global\"", sr.Name, sr.Target)
		}
		if norm, ok := legacy.NormalizeSetupName(sr.Software); !ok || norm != sr.Software {
			t.Errorf("%s: Software %q failed NormalizeSetupName (got %q, %v)", sr.Name, sr.Software, norm, ok)
		}

		tokens := append([]string{sr.Name}, sr.Aliases...)
		for _, tok := range tokens {
			if strings.HasPrefix(tok, "setup-") {
				t.Errorf("%s: token %q must not carry a setup- prefix", sr.Name, tok)
			}
			norm, ok := legacy.NormalizeSetupName(tok)
			if !ok || norm != tok {
				t.Errorf("%s: token %q failed NormalizeSetupName (got %q, %v)", sr.Name, tok, norm, ok)
			}
			if prev, exists := seen[tok]; exists {
				t.Errorf("duplicate setup cutover token %q in %q and %q", tok, prev, sr.Name)
			}
			seen[tok] = sr.Name
			if routeName, conflict := routeTokens[tok]; conflict {
				t.Errorf("setup cutover token %q collides with legacy Route %q", tok, routeName)
			}
		}
	}

	// Binary-artifact casks refused by the brew cask provider's app-only rule
	// (MM_SOFTWARE_CASK_REFUSED) stay out of SetupCutover in S7.
	for _, removed := range []string{
		"antigravity-ide",
		"cursor",
		"devin-app", "devin-desktop",
		"docker", "docker-desktop",
		"kiro",
		"kiro-cli",
		"vscode", "visual-studio-code",
		"vscode-insiders", "code-insiders",
	} {
		for _, spelling := range []string{removed, "setup-" + removed} {
			if sr, ok := legacy.LookupSetupCutover(spelling); ok {
				t.Errorf("LookupSetupCutover(%q) matched %+v; binary-artifact cask must stay legacy", spelling, sr)
			}
			if sr, ok := legacy.MatchSetupCutover([]string{"setup", spelling}); ok {
				t.Errorf("MatchSetupCutover([setup %q]) matched %+v; binary-artifact cask must stay legacy", spelling, sr)
			}
		}
	}
}

func loadLocalCatalog(t *testing.T, dir string) ([]source.Entry, *registry.Registry) {
	t.Helper()
	// The built-in catalog joins as in production: cutover software it
	// defines (ripgrep, taplo) routes without a local recipe.
	entries, reg, err := software.LoadCatalog(func(k string) string {
		if k == catalog.SourceDirEnv {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("loading built-in and local catalog from %s: %v", dir, err)
	}
	return entries, reg
}

func TestSetupCutoverCoreExecutionAndRefusals(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.Builtin()
	rep, err := legacy.Classify(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	sourceDir := filepath.Join(t.TempDir(), "software")
	if err := legacy.Emit(rep, sourceDir); err != nil {
		t.Fatal(err)
	}
	emptyDir := filepath.Join(t.TempDir(), "empty-software")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	type installCall struct {
		id       string
		target   string
		platform string
	}

	var (
		activeSourceDir string
		loadedEntries   []source.Entry
		loadedReg       *registry.Registry
	)
	getenv := func(k string) string {
		if k == catalog.SourceDirEnv {
			return activeSourceDir
		}
		return ""
	}

	runApp := func(tgt target.Target, installFn func(ctx context.Context, r install.Request) (install.Result, error), args ...string) (string, string, int, []installCall) {
		var stdout, stderr bytes.Buffer
		var calls []installCall
		app := &cli.App{
			Name:    "managed-machine",
			Version: "0.9.0",
			Stdout:  &stdout,
			Stderr:  &stderr,
			Detect:  func() (target.Host, error) { return target.Host{Target: tgt}, nil },
			Getenv:  getenv,
			Commands: []cli.Command{
				software.Command(software.Deps{
					Deps: command.Deps{Getenv: getenv},
					Catalog: func() ([]source.Entry, *registry.Registry, error) {
						return loadedEntries, loadedReg, nil
					},
					Install: func(ctx context.Context, r install.Request) (install.Result, error) {
						calls = append(calls, installCall{
							id:       r.Entry.Software.ID,
							target:   r.Target,
							platform: r.Platform,
						})
						if installFn != nil {
							return installFn(ctx, r)
						}
						return install.Result{
							Plan:            "plan-" + r.Entry.Software.ID,
							Outcome:         "installed",
							ResolvedVersion: "1.0.0",
						}, nil
					},
				}),
			},
		}
		code := app.Run(args)
		return stdout.String(), stderr.String(), code, calls
	}

	// 1. Before writing minimal recipes for the names not in tests/fixtures/apps.json:
	//    - the 14 SetupCutover names covered by the fixture have <software>.yaml in sourceDir, and the
	//      2 the built-in catalog defines (ripgrep, taplo) need none; all 16 route to the core;
	//    - the 3 remaining names lack <software>.yaml and stay on MM_LEGACY_ROUTE_UNSUPPORTED (exit 2).
	activeSourceDir = sourceDir
	loadedEntries, loadedReg = loadLocalCatalog(t, sourceDir)
	emittedCount := 0
	missingCount := 0
	for _, sr := range legacy.SetupCutover {
		spellings := []string{sr.Name, "setup-" + sr.Name}
		for _, alias := range sr.Aliases {
			spellings = append(spellings, alias, "setup-"+alias)
		}
		if legacy.HasSetupRecipe(getenv, sr.Software) {
			emittedCount++
			for _, spelling := range spellings {
				gotOut, gotErr, gotCode, gotCalls := runApp(target.DarwinARM64, nil, "setup", spelling)
				if gotCode != diag.ExitOK || gotErr != "" || len(gotCalls) != 1 || gotCalls[0].id != sr.Software {
					t.Fatalf("fixture-covered setup %s: code=%d out=%q err=%q calls=%+v", spelling, gotCode, gotOut, gotErr, gotCalls)
				}
			}
		} else {
			missingCount++
			for _, spelling := range spellings {
				for _, modeFlag := range [][]string{nil, {"--json"}, {"--human"}} {
					args := append([]string{"setup", spelling}, modeFlag...)
					gotOut, gotErr, gotCode, gotCalls := runApp(target.DarwinARM64, nil, args...)
					if gotCode != diag.ExitUsage || gotOut != "" || len(gotCalls) != 0 {
						t.Fatalf("missing-recipe %v: code=%d out=%q err=%q calls=%+v, want exit 2 and no install calls", args, gotCode, gotOut, gotErr, gotCalls)
					}
					if !strings.Contains(gotErr, string(diag.LegacyRouteUnsupported)) {
						t.Fatalf("missing-recipe %v: stderr=%q, want %s", args, gotErr, diag.LegacyRouteUnsupported)
					}
				}
			}
		}
	}
	if emittedCount != 16 || missingCount != 3 {
		t.Fatalf("fixture coverage: emitted=%d missing=%d, want 16 routed and 3 missing", emittedCount, missingCount)
	}

	// 2. When MANAGED_MACHINE_SOURCE_DIR is unset or points to an empty directory,
	//    every SetupCutover row and alias stays on MM_LEGACY_ROUTE_UNSUPPORTED,
	//    except the software the built-in catalog defines.
	for _, dir := range []string{"", emptyDir} {
		activeSourceDir = dir
		for _, sr := range legacy.SetupCutover {
			if catalog.BuiltinDefines(sr.Software) {
				if !legacy.HasSetupRecipe(getenv, sr.Software) {
					t.Fatalf("HasSetupRecipe(%q, %q) = false for built-in software", dir, sr.Software)
				}
				continue
			}
			if legacy.HasSetupRecipe(getenv, sr.Software) {
				t.Fatalf("HasSetupRecipe(%q, %q) = true, want false", dir, sr.Software)
			}
			spellings := []string{sr.Name}
			spellings = append(spellings, sr.Aliases...)
			for _, spelling := range spellings {
				gotOut, gotErr, gotCode, gotCalls := runApp(target.DarwinARM64, nil, "setup", spelling)
				if gotCode != diag.ExitUsage || gotOut != "" || len(gotCalls) != 0 || !strings.Contains(gotErr, string(diag.LegacyRouteUnsupported)) {
					t.Fatalf("sourceDir=%q setup %s: code=%d out=%q err=%q calls=%+v", dir, spelling, gotCode, gotOut, gotErr, gotCalls)
				}
			}
		}
	}

	// 3. Write minimal recipes for the remaining 5 SetupCutover names not covered by
	//    tests/fixtures/apps.json (plus vscode-insiders so all 8 removed binary-artifact
	//    casks also have recipe files on disk), reload the local catalog, and verify all
	//    19 SetupCutover rows (bare, setup- prefixed, alias, setup-<alias>) route to the
	//    core and match software <Software> --install --<Target> across default, --json,
	//    and --human modes.
	activeSourceDir = sourceDir
	for _, id := range []string{"ast-grep", "fd", "ripgrep", "taplo", "uv", "vscode-insiders"} {
		if legacy.HasSetupRecipe(getenv, id) {
			continue
		}
		minYAML := fmt.Sprintf(
			"schema_version: 1\nid: %s\ndescription: %s (minimal test recipe)\nrecipes:\n  - id: brew\n    provider: brew\n    package: %s\n    platforms:\n      - darwin/arm64\n    targets:\n      - global\n    options:\n      formula: %s\n",
			id, id, id, id,
		)
		if err := os.WriteFile(filepath.Join(sourceDir, id+".yaml"), []byte(minYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	loadedEntries, loadedReg = loadLocalCatalog(t, sourceDir)

	for _, sr := range legacy.SetupCutover {
		if !legacy.HasSetupRecipe(getenv, sr.Software) {
			t.Fatalf("HasSetupRecipe(%q) = false after writing all recipes", sr.Software)
		}
		spellings := []string{sr.Name, "setup-" + sr.Name}
		for _, alias := range sr.Aliases {
			spellings = append(spellings, alias, "setup-"+alias)
		}
		for _, spelling := range spellings {
			for _, modeFlag := range [][]string{nil, {"--json"}, {"--human"}} {
				setupArgs := append([]string{"setup", spelling}, modeFlag...)
				swArgs := append([]string{"software", sr.Software, "--install", "--" + sr.Target}, modeFlag...)

				gotOut, gotErr, gotCode, gotCalls := runApp(target.DarwinARM64, nil, setupArgs...)
				wantOut, wantErr, wantCode, _ := runApp(target.DarwinARM64, nil, swArgs...)

				if gotCode != diag.ExitOK || gotCode != wantCode {
					t.Fatalf("%v: exit=%d, want %d (stderr=%q)", setupArgs, gotCode, wantCode, gotErr)
				}
				if gotOut != wantOut || gotErr != wantErr {
					t.Fatalf("%v output mismatch with %v:\ngot:  stdout=%q stderr=%q\nwant: stdout=%q stderr=%q",
						setupArgs, swArgs, gotOut, gotErr, wantOut, wantErr)
				}
				if len(gotCalls) != 1 || gotCalls[0].id != sr.Software || gotCalls[0].target != sr.Target || gotCalls[0].platform != string(target.DarwinARM64) {
					t.Fatalf("%v: install calls = %+v, want [{id:%s target:%s platform:darwin/arm64}]",
						setupArgs, gotCalls, sr.Software, sr.Target)
				}
			}
		}
	}

	// 4. Destination occupied (MM_SOFTWARE_DESTINATION_OCCUPIED, exit 1) and
	//    unsupported platform (MM_SOFTWARE_UNSUPPORTED_COMBINATION, exit 2) match
	//    software <id> --install --<target> across presentation modes.
	occupiedFn := func(ctx context.Context, r install.Request) (install.Result, error) {
		return install.Result{}, fmt.Errorf("%w: /Applications/Warp.app already exists and is not managed by Homebrew cask warp", install.ErrOccupied)
	}
	for _, modeFlag := range [][]string{nil, {"--json"}, {"--human"}} {
		setupArgs := append([]string{"setup", "warp-terminal"}, modeFlag...)
		swArgs := append([]string{"software", "warp", "--install", "--global"}, modeFlag...)

		gotOut, gotErr, gotCode, _ := runApp(target.DarwinARM64, occupiedFn, setupArgs...)
		wantOut, wantErr, wantCode, _ := runApp(target.DarwinARM64, occupiedFn, swArgs...)
		if gotCode != diag.ExitFailure || gotCode != wantCode || gotOut != wantOut || gotErr != wantErr {
			t.Fatalf("occupied %v: code=%d/%d out=%q/%q err=%q/%q",
				setupArgs, gotCode, wantCode, gotOut, wantOut, gotErr, wantErr)
		}
		if !strings.Contains(gotErr, string(diag.SoftwareDestinationOccupied)) {
			t.Errorf("occupied %v: stderr %q missing MM_SOFTWARE_DESTINATION_OCCUPIED", setupArgs, gotErr)
		}

		// Unsupported platform on linux/amd64: legacy catalog recipes claim darwin/arm64 only.
		gotOut, gotErr, gotCode, calls := runApp(target.LinuxAMD64, nil, setupArgs...)
		wantOut, wantErr, wantCode, _ = runApp(target.LinuxAMD64, nil, swArgs...)
		if gotCode != diag.ExitUsage || gotCode != wantCode || gotOut != wantOut || gotErr != wantErr {
			t.Fatalf("linux %v: code=%d/%d out=%q/%q err=%q/%q",
				setupArgs, gotCode, wantCode, gotOut, wantOut, gotErr, wantErr)
		}
		if len(calls) != 0 {
			t.Errorf("linux %v: install seam called %d times", setupArgs, len(calls))
		}
		if !strings.Contains(gotErr, string(diag.SoftwareUnsupportedCombo)) {
			t.Errorf("linux %v: stderr %q missing %s", setupArgs, gotErr, diag.SoftwareUnsupportedCombo)
		}
	}

	// 5. Even when a recipe file exists on disk for the 8 binary-artifact casks
	//    dropped from SetupCutover, invoking `setup <name|alias>` stays
	//    MM_LEGACY_ROUTE_UNSUPPORTED (exit 2) without invoking Install, as do
	//    setup --list, unconverted names, missing name, or any extra argument.
	for _, removedID := range []string{
		"antigravity-ide", "cursor", "devin-app", "docker",
		"kiro", "kiro-cli", "vscode", "vscode-insiders",
	} {
		if !legacy.HasSetupRecipe(getenv, removedID) {
			t.Fatalf("HasSetupRecipe(%q) = false, want true so test proves recipe presence does not cut over removed binary-artifact casks", removedID)
		}
	}
	legacyCases := [][]string{
		{"setup"},
		{"setup", "--list"},
		{"setup", "--list", "--json"},
		{"setup", "--json", "--list"},
		{"setup", "antigravity-ide"},
		{"setup", "setup-antigravity-ide"},
		{"setup", "cursor"},
		{"setup", "setup-cursor"},
		{"setup", "cursor", "--json"},
		{"setup", "devin-app"},
		{"setup", "devin-desktop"},
		{"setup", "docker"},
		{"setup", "docker-desktop"},
		{"setup", "kiro"},
		{"setup", "kiro-cli"},
		{"setup", "vscode"},
		{"setup", "visual-studio-code"},
		{"setup", "vscode-insiders"},
		{"setup", "code-insiders"},
		{"setup", "grok-build"},
		{"setup", "setup-grok-build"},
		{"setup", "grok"},
		{"setup", "qwen"},
		{"setup", "qwen-code"},
		{"setup", "qwen-desktop"},
		{"setup", "qwen-code-desktop"},
		{"setup", "bin"},
		{"setup", "setup-bin"},
		{"setup", "warp", "extra"},
		{"setup", "lmstudio", "--extra"},
		{"setup", "warp", "--user"},
		{"setup", "warp", "--global"},
		{"setup", "../warp"},
		{"setup", "setup-setup-warp"},
	}
	for _, args := range legacyCases {
		for _, tgt := range target.All() {
			out, errOut, code, calls := runApp(tgt, nil, args...)
			if code != diag.ExitUsage || out != "" {
				t.Fatalf("%s %v: code=%d out=%q err=%q", tgt, args, code, out, errOut)
			}
			if !strings.Contains(errOut, `"diagnostic":"MM_LEGACY_ROUTE_UNSUPPORTED"`) {
				t.Errorf("%s %v: stderr=%q, want MM_LEGACY_ROUTE_UNSUPPORTED", tgt, args, errOut)
			}
			if len(calls) != 0 {
				t.Errorf("%s %v: install seam called %d times", tgt, args, len(calls))
			}
		}
	}
}
