package legacy_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	if len(legacy.SetupCutover) != 22 {
		t.Fatalf("SetupCutover count = %d, want 22", len(legacy.SetupCutover))
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

		if sr.Software == "" {
			t.Errorf("%s: empty Software", sr.Name)
		}
		if sr.Target != "global" && sr.Target != "user" {
			t.Errorf("%s: Target = %q, want global or user", sr.Name, sr.Target)
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
}

func TestSetupCutoverMatchesEmittedFixtureCatalog(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "tests", "fixtures", "apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	reg := registry.Builtin()
	rep, err := legacy.Classify(data, reg)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "software")
	if err := legacy.Emit(rep, dir); err != nil {
		t.Fatal(err)
	}
	checked, err := legacy.CheckConverted(rep, dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	if checked.Counts[legacy.Converted] != len(legacy.SetupCutover) || checked.Counts[legacy.Drift] != 0 {
		t.Fatalf("counts = %+v, want %d converted and 0 drift", checked.Counts, len(legacy.SetupCutover))
	}

	var wantTable []legacy.SetupRoute
	for _, r := range rep.Rows {
		if !r.Convertible() {
			continue
		}
		var aliases []string
		if len(r.Aliases) > 0 {
			aliases = append([]string(nil), r.Aliases...)
		}
		wantTable = append(wantTable, legacy.SetupRoute{
			Name:     r.Name,
			Aliases:  aliases,
			Software: r.Software.ID,
			Target:   r.Software.Recipes[0].Targets[0],
		})
	}
	if !reflect.DeepEqual(legacy.SetupCutover, wantTable) {
		t.Fatalf("SetupCutover does not match convertible rows of tests/fixtures/apps.json:\ngot:  %+v\nwant: %+v", legacy.SetupCutover, wantTable)
	}

	entries, _, err := software.LoadCatalog(func(k string) string {
		if k == catalog.SourceDirEnv {
			return dir
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]source.Entry{}
	for _, e := range entries {
		byID[e.Software.ID] = e
	}
	for _, sr := range legacy.SetupCutover {
		entry, ok := byID[sr.Software]
		if !ok {
			t.Errorf("cutover row %q references missing software definition %q", sr.Name, sr.Software)
			continue
		}
		if len(entry.Software.Recipes) == 0 {
			t.Errorf("software definition %q has no recipes", sr.Software)
		}
	}
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
	getenv := func(k string) string {
		if k == catalog.SourceDirEnv {
			return sourceDir
		}
		return ""
	}
	loadedEntries, loadedReg, err := software.LoadCatalog(getenv)
	if err != nil {
		t.Fatal(err)
	}

	type installCall struct {
		id       string
		target   string
		platform string
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

	// 1. Every SetupCutover row (bare, setup- prefixed, alias, setup-<alias>)
	// executes software <Software> --install --<Target> with identical output
	// and exit code in default, --json, and --human modes.
	for _, sr := range legacy.SetupCutover {
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

	// 2. Destination occupied (MM_SOFTWARE_DESTINATION_OCCUPIED, exit 1) and
	// unsupported platform (MM_SOFTWARE_UNSUPPORTED_PLATFORM, exit 2) match
	// software <id> --install --<target> across presentation modes.
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

	// 3. setup --list, unconverted names, missing name, or any extra argument
	// stay MM_LEGACY_ROUTE_UNSUPPORTED (exit 2) without invoking Install.
	legacyCases := [][]string{
		{"setup"},
		{"setup", "--list"},
		{"setup", "--list", "--json"},
		{"setup", "--json", "--list"},
		{"setup", "grok-build"},
		{"setup", "setup-grok-build"},
		{"setup", "grok"},
		{"setup", "qwen-desktop"},
		{"setup", "qwen-code-desktop"},
		{"setup", "bin"},
		{"setup", "setup-bin"},
		{"setup", "warp", "extra"},
		{"setup", "cursor", "--extra"},
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
