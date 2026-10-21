package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/core/internal/target"
)

// The isolated harness. Every directory name carries spaces and non-ASCII
// characters (T01-T05 common acceptance), and the custom root is configured
// on the command line but never created.
const (
	homeName       = "home dir ünï 日本"
	definitionsDir = "definitions é"
	extensionsDir  = "extensions ø"
	customRootRel  = "custom root ✓/opt"
	fixtureID      = "mm-walkthrough-tool"
	fixtureCat     = "walkthrough"
	extBundle      = "walkthrough"
	extSkill       = "walkthrough-guide"
	extCaveat      = "Fixture guidance from an explicitly configured extension; reading it runs nothing."
	stepCount      = 120
)

type layout struct {
	root, home, state, definitions, extensions, custom string
}

func newLayout(root string) layout {
	home := filepath.Join(root, homeName)
	return layout{
		root:        root,
		home:        home,
		state:       filepath.Join(home, ".local", "state", "managed-machine"),
		definitions: filepath.Join(home, definitionsDir),
		extensions:  filepath.Join(home, extensionsDir),
		custom:      filepath.Join(home, filepath.FromSlash(customRootRel)),
	}
}

// fixtureDefinition is an external local definition with a recipe for every
// first-release target, so it is discoverable on all five runners. Its
// provider, download, has no executor, so installing it is refused on every
// target before anything runs or is written.
func fixtureDefinition() string {
	var p []string
	for _, t := range target.All() {
		p = append(p, string(t))
	}
	return "schema_version: 1\nid: " + fixtureID + "\ndescription: Walkthrough fixture tool.\ncategories: [" + fixtureCat + "]\n" +
		"recipes:\n  - id: download\n    provider: download\n    package: " + fixtureID + "\n    platforms: [" + strings.Join(p, ", ") + "]\n" +
		"    targets: [user, custom]\n    requires: []\n    options:\n      url: https://example.invalid/" + fixtureID + ".tar.gz\n      url_hosts: [example.invalid]\n"
}

// longProcedure is larger than one 4096-byte response, so serving it needs
// continuation; each numbered step lets the walkthrough check completeness.
func longProcedure() string {
	var b strings.Builder
	for i := 1; i <= stepCount; i++ {
		fmt.Fprintf(&b, "Step %03d: read the next instruction, with ünïcödé text and \"quotes\".\n\n", i)
	}
	return b.String()
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func write(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// build creates the harness. The extension's executable is a script that
// would leave a marker if anything ran it; serving skills must not.
func (l layout) build(marker string) error {
	for _, d := range []string{l.state, l.definitions, l.extensions, filepath.Dir(l.custom)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := write(filepath.Join(l.definitions, fixtureID+".yaml"), fixtureDefinition()); err != nil {
		return err
	}
	ext := filepath.Join(l.extensions, "walkthrough-tools")
	files := map[string]string{
		"bin/mm-walkthrough-ext": "#!/bin/sh\n: > '" + marker + "'\n",
		"bundles/" + extBundle + "/bundle.yaml": "schema_version: 1\nname: " + extBundle + "\nversion: 0.1.0\nskills:\n  - name: " + extSkill +
			"\n    kinds: [summary, procedure]\n    caveats: [\"" + extCaveat + "\"]\n",
		"bundles/" + extBundle + "/skills/" + extSkill + "/summary.md":   "Walk through the first slice.\n",
		"bundles/" + extBundle + "/skills/" + extSkill + "/procedure.md": longProcedure(),
	}
	var assets, exes strings.Builder
	for _, p := range []string{"bundles/" + extBundle + "/bundle.yaml", "bundles/" + extBundle + "/skills/" + extSkill + "/summary.md", "bundles/" + extBundle + "/skills/" + extSkill + "/procedure.md"} {
		fmt.Fprintf(&assets, "    - path: %s\n      sha256: %s\n", p, sum([]byte(files[p])))
	}
	for p, c := range files {
		if err := write(filepath.Join(ext, filepath.FromSlash(p)), c); err != nil {
			return err
		}
	}
	for _, t := range target.All() {
		fmt.Fprintf(&exes, "  - platform: %s\n    path: bin/mm-walkthrough-ext\n    sha256: %s\n", t, sum([]byte(files["bin/mm-walkthrough-ext"])))
	}
	manifest := "schema_version: 1\nname: walkthrough-tools\nversion: 0.1.0\nprotocol: mm-extension-1\ncapabilities: [provider, skills]\nexecutables:\n" + exes.String() +
		"provides:\n  providers:\n    - name: walkthrough\n  skill_bundles:\n    - id: " + extBundle + "\n      covers: [walkthrough]\n  assets:\n" + assets.String()
	return write(filepath.Join(ext, "manifest.yaml"), manifest)
}
