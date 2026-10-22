package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/core/internal/schema"
	"github.com/qwts/managed-machine/core/internal/target"
)

// DesiredEnv names the desired-configuration file explicitly. It is read
// only when set, like the local definition source: a test or a gate names
// the file it means, and nothing else is read.
const DesiredEnv = "MANAGED_MACHINE_DESIRED_FILE"

// The installation targets a desired configuration belongs to.
const (
	TargetGlobal = "global"
	TargetUser   = "user"
	TargetCustom = "custom"
)

// Desired is the desired configuration of one target: the inputs and the
// destination mappings of every configuration definition, the software
// preferences and the flavors.
type Desired struct {
	// Target is the installation target the file declares, empty when it
	// declares none.
	Target string
	// Root is the custom root the file declares, empty when it declares
	// none.
	Root string
	// Config holds the desired configuration of every configuration
	// definition the file names.
	Config map[string]DesiredConfig
	// Software holds the preference of every software the file names.
	Software map[string]SoftwarePreference
	// Flavors are the flavors the file composes.
	Flavors []string
}

// DesiredConfig is the desired configuration of one configuration
// definition.
type DesiredConfig struct {
	// Inputs are the values of the definition's inputs.
	Inputs map[string]string
	// Destinations map a logical destination name to an absolute file
	// path.
	Destinations map[string]string
}

// SoftwarePreference is the preferred way to install one software.
type SoftwarePreference struct {
	Preference string `json:"preference"`
}

// DesiredError is a desired configuration that cannot be used: the file is
// missing, malformed, unmapped, or disagrees with the command. It carries
// the field that is wrong, so the diagnostic names it.
type DesiredError struct {
	Field   string
	Message string
}

func (e *DesiredError) Error() string {
	if e.Field == "" {
		return e.Message
	}
	return e.Field + ": " + e.Message
}

// UnsupportedError reports a definition that does not declare the
// requested installation target or the host platform.
type UnsupportedError struct {
	Target   string
	Platform string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("the definition does not declare target %q on platform %q", e.Target, e.Platform)
}

// Request is what a resolution is for: the installation target, the custom
// root and the host platform.
type Request struct {
	Target   string
	Root     string
	Platform target.Target
}

// Resolved is the desired state of one configuration definition: the
// values of its inputs and the paths of its destinations.
type Resolved struct {
	Inputs       map[string]string
	Destinations map[string]string
}

type rawDesired struct {
	Target   string                        `yaml:"target"`
	Root     string                        `yaml:"root"`
	Config   map[string]rawDesiredConfig   `yaml:"config"`
	Software map[string]rawDesiredSoftware `yaml:"software"`
	Flavors  []string                      `yaml:"flavors"`
}

type rawDesiredConfig struct {
	Inputs       map[string]string `yaml:"inputs"`
	Destinations map[string]string `yaml:"destinations"`
}

type rawDesiredSoftware struct {
	Preference string `yaml:"preference"`
}

// DesiredPath returns the desired-configuration file of one target: the
// explicit setting when it is set, the platform configuration directory
// for the user and global targets, and the custom root's own file for a
// custom target. A custom root never reads the invoking user's file, so a
// Soul describes its own layout.
func DesiredPath(getenv func(string) string, goos, installationTarget, root string) (string, error) {
	if p := strings.TrimSpace(getenv(DesiredEnv)); p != "" {
		return filepath.Clean(p), nil
	}
	if installationTarget == TargetCustom {
		if root == "" {
			return "", &DesiredError{Field: "target", Message: "a custom target needs a root"}
		}
		return filepath.Join(root, ".managed-machine", "desired.yaml"), nil
	}
	switch goos {
	case "windows":
		app := getenv("APPDATA")
		if app == "" {
			return "", &DesiredError{Field: "target", Message: "the environment names no configuration directory"}
		}
		return filepath.Join(app, "managed-machine", "desired.yaml"), nil
	default:
		if x := getenv("XDG_CONFIG_HOME"); x != "" && filepath.IsAbs(x) {
			return filepath.Join(x, "managed-machine", "desired.yaml"), nil
		}
		home := getenv("HOME")
		if home == "" {
			return "", &DesiredError{Field: "target", Message: "the environment names no home directory"}
		}
		return filepath.Join(home, ".config", "managed-machine", "desired.yaml"), nil
	}
}

// LoadDesired reads and validates the desired-configuration file at path.
// The file is data only: it names values and paths, and nothing in it is
// executed.
func LoadDesired(path string) (Desired, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Desired{}, &DesiredError{Field: "desired", Message: "no desired configuration at " + path}
		}
		return Desired{}, &DesiredError{Field: "desired", Message: err.Error()}
	}
	return parseDesired(path, data)
}

// parseDesired decodes and validates one desired-configuration document.
func parseDesired(doc string, data []byte) (Desired, error) {
	var raw rawDesired
	if err := schema.Decode(doc, data, &raw); err != nil {
		return Desired{}, &DesiredError{Field: "desired", Message: err.Error()}
	}
	out := Desired{
		Target:   strings.TrimSpace(raw.Target),
		Root:     strings.TrimSpace(raw.Root),
		Config:   map[string]DesiredConfig{},
		Software: map[string]SoftwarePreference{},
		Flavors:  []string{},
	}
	if out.Target != "" && !knownTarget(out.Target) {
		return Desired{}, &DesiredError{Field: "target", Message: fmt.Sprintf("unknown target %q; supported: global, user, custom", out.Target)}
	}
	for _, id := range sortedConfigKeys(raw.Config) {
		if !schema.ValidID(id) {
			return Desired{}, &DesiredError{Field: "config." + id, Message: fmt.Sprintf("invalid configuration id %q", id)}
		}
		c := raw.Config[id]
		cfg := DesiredConfig{Inputs: map[string]string{}, Destinations: map[string]string{}}
		for _, name := range sortedKeys(c.Inputs) {
			cfg.Inputs[name] = c.Inputs[name]
		}
		for _, name := range sortedKeys(c.Destinations) {
			p := c.Destinations[name]
			if !filepath.IsAbs(p) {
				return Desired{}, &DesiredError{Field: "config." + id + ".destinations." + name,
					Message: fmt.Sprintf("the path %q is not absolute", p)}
			}
			cfg.Destinations[name] = p
		}
		out.Config[id] = cfg
	}
	for _, id := range sortedSoftwareKeys(raw.Software) {
		if !schema.ValidID(id) {
			return Desired{}, &DesiredError{Field: "software." + id, Message: fmt.Sprintf("invalid software id %q", id)}
		}
		out.Software[id] = SoftwarePreference{Preference: raw.Software[id].Preference}
	}
	for i, f := range raw.Flavors {
		if !schema.ValidID(f) {
			return Desired{}, &DesiredError{Field: fmt.Sprintf("flavors[%d]", i), Message: fmt.Sprintf("invalid flavor name %q", f)}
		}
		out.Flavors = append(out.Flavors, f)
	}
	return out, nil
}

// Resolve validates the desired configuration of one definition for one
// target and returns the values of its inputs and the paths of its
// destinations. An input falls back to the definition's default; a
// destination has no default, so every logical name an operation uses must
// be mapped.
func Resolve(def Definition, d Desired, req Request) (Resolved, error) {
	if d.Target != "" && d.Target != req.Target {
		return Resolved{}, &DesiredError{Field: "target",
			Message: fmt.Sprintf("the desired configuration names target %q, not %q", d.Target, req.Target)}
	}
	if d.Root != "" {
		if req.Target != TargetCustom {
			return Resolved{}, &DesiredError{Field: "root",
				Message: fmt.Sprintf("the desired configuration names root %q, but the target is %q", d.Root, req.Target)}
		}
		if filepath.Clean(d.Root) != filepath.Clean(req.Root) {
			return Resolved{}, &DesiredError{Field: "root",
				Message: fmt.Sprintf("the desired configuration names root %q, not %q", d.Root, req.Root)}
		}
	}
	if !def.Supports(req.Target, req.Platform) {
		return Resolved{}, &UnsupportedError{Target: req.Target, Platform: string(req.Platform)}
	}
	cfg, ok := d.Config[def.ID]
	if !ok {
		return Resolved{}, &DesiredError{Field: "config." + def.ID,
			Message: fmt.Sprintf("the desired configuration names no configuration %q", def.ID)}
	}
	used := def.DestinationNames()
	for _, name := range sortedKeys(cfg.Destinations) {
		if !used[name] {
			return Resolved{}, &DesiredError{Field: "config." + def.ID + ".destinations." + name,
				Message: fmt.Sprintf("the definition has no operation for destination %q", name)}
		}
	}
	for _, name := range sortedNames(used) {
		if _, ok := cfg.Destinations[name]; !ok {
			return Resolved{}, &DesiredError{Field: "config." + def.ID + ".destinations." + name,
				Message: fmt.Sprintf("the destination %q is not mapped", name)}
		}
	}
	inputs := map[string]string{}
	for name, value := range def.Inputs {
		inputs[name] = value
	}
	for _, name := range sortedKeys(cfg.Inputs) {
		if _, ok := def.Inputs[name]; !ok {
			return Resolved{}, &DesiredError{Field: "config." + def.ID + ".inputs." + name,
				Message: fmt.Sprintf("the definition declares no input %q", name)}
		}
		inputs[name] = cfg.Inputs[name]
	}
	destinations := map[string]string{}
	for name, p := range cfg.Destinations {
		destinations[name] = p
	}
	return Resolved{Inputs: inputs, Destinations: destinations}, nil
}

// knownTarget reports whether a target is one of the three installation
// targets.
func knownTarget(t string) bool {
	return t == TargetGlobal || t == TargetUser || t == TargetCustom
}

// sortedNames returns the names of a set in a fixed order.
func sortedNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedSoftwareKeys returns the keys of the software preferences in a
// fixed order.
func sortedSoftwareKeys(m map[string]rawDesiredSoftware) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sortedConfigKeys returns the keys of the desired configurations in a
// fixed order.
func sortedConfigKeys(m map[string]rawDesiredConfig) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
