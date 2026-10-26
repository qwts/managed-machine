package legacy

import (
	"fmt"
	"strings"

	"github.com/qwts/managed-machine/internal/diag"
)

// CompatibilityRemovalVersion is the earliest release in which legacy routes
// with a new-core replacement may be removed under ADR-0016 and ADR-0017 (S6
// decision D2, D5).
const CompatibilityRemovalVersion = "v0.11.0"

// LegacyStatusSections are the surviving section names served by the legacy
// Bash "managed-machine status --section <name>" implementation (F01, S6 D5,
// S8 D1).
var LegacyStatusSections = []string{
	"bootstrap",
	"update",
}

type removedStatusSection struct {
	section     string
	replacement string
}

// removedStatusSections settles each removed legacy section from its section
// function in scripts/status (managed_machine_version at 156-190,
// ssh_enrollment_status at 203-244, machine_status at 257-283,
// local_bin_status at 353-384, zsh_functions_status at 386-417,
// agent_accounts_status at 424-450, nvm_status at 483-503, node_status at
// 505-523, status_catalog_rows at 525-600, pins/toolchain expansion at
// 624-631, and print_status at 757-801; S8 D1(b)). Sections with no real
// equivalent in the portable core leave replacement empty ("no replacement" in
// MM_LEGACY_ROUTE_REMOVED).
var removedStatusSections = []removedStatusSection{
	{section: "managed-machine", replacement: "managed-machine version"},
	{section: "machine"},
	{section: "ssh", replacement: "managed-machine ssh status"},
	{section: "agent-accounts", replacement: "managed-machine account doctor"},
	{section: "local-bin"},
	{section: "zsh-functions"},
	{section: "pins"},
	{section: "toolchain"},
	{section: "homebrew"},
	{section: "gh"},
	{section: "nvm"},
	{section: "node"},
	{section: "hostname"},
	{section: "rustup"},
	{section: "rustc"},
	{section: "cargo"},
	{section: "catalog", replacement: "managed-machine setup --list and managed-machine software --available"},
}

// Route is one entry in the D5 static legacy route table.
type Route struct {
	// Name is the canonical legacy route spelling reported in
	// MM_LEGACY_ROUTE_UNSUPPORTED and MM_LEGACY_ROUTE_REMOVED diagnostics
	// (for example "setup", "--bootstrap", "fleet list",
	// "status --section ssh").
	Name string
	// Command is the top-level command or flag token ("setup", "catalog",
	// "adopt", "add-agent", "account", "ssh", "--bootstrap", "--update",
	// "fleet", "status").
	Command string
	// Subcommand, when non-empty, requires the first positional argument
	// after Command to equal Subcommand ("list", "remove").
	Subcommand string
	// Section, when non-empty, requires Command == "status" and the
	// --section value to equal Section.
	Section string
	// Replacement, when non-empty, names the command or disposition that
	// supersedes a compatibility-period legacy route (S6 D2/D5, S8 D1).
	// Empty for surviving legacy routes with no v1 core replacement.
	Replacement string
	// RemovedIn, when non-empty, names the release in which the
	// compatibility-period legacy route was removed ("v0.11.0", S8 D1).
	RemovedIn string
}

// Routes is the static D5/S8 D1 table of legacy Bash routes (surviving and
// removed) that the portable Go core recognizes.
var Routes = buildRoutes()

func buildRoutes() []Route {
	base := []Route{
		{Name: "setup", Command: "setup"},
		{Name: "catalog", Command: "catalog"},
		{Name: "adopt", Command: "adopt"},
		{Name: "add-agent", Command: "add-agent"},
		{Name: "account", Command: "account"},
		{Name: "ssh", Command: "ssh"},
		{Name: "--bootstrap", Command: "--bootstrap"},
		{Name: "--update", Command: "--update"},
		{
			Name:        "fleet list",
			Command:     "fleet",
			Subcommand:  "list",
			Replacement: "managed-machine fleet --keys",
			RemovedIn:   CompatibilityRemovalVersion,
		},
		{
			Name:       "fleet remove",
			Command:    "fleet",
			Subcommand: "remove",
		},
	}
	out := make([]Route, 0, len(base)+len(LegacyStatusSections)+len(removedStatusSections))
	out = append(out, base...)
	for _, sec := range LegacyStatusSections {
		out = append(out, Route{
			Name:    "status --section " + sec,
			Command: "status",
			Section: sec,
		})
	}
	for _, rm := range removedStatusSections {
		out = append(out, Route{
			Name:        "status --section " + rm.section,
			Command:     "status",
			Section:     rm.section,
			Replacement: rm.replacement,
			RemovedIn:   CompatibilityRemovalVersion,
		})
	}
	return out
}

// Match reports whether args invoke a legacy Bash route in Routes and returns
// the matched Route. It inspects only the argument vector and performs no I/O
// or host probe.
func Match(args []string) (Route, bool) {
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--json" || a == "--human" {
			continue
		}
		filtered = append(filtered, a)
	}
	if len(filtered) == 0 {
		return Route{}, false
	}

	first := filtered[0]
	if first == "--bootstrap" || first == "--update" {
		return lookupCommand(first)
	}
	if strings.HasPrefix(first, "-") {
		return Route{}, false
	}

	cmd := first
	rest := filtered[1:]

	switch cmd {
	case "setup":
		for _, a := range rest {
			if a == "-h" || a == "--help" {
				return Route{}, false
			}
		}
		return lookupCommand(cmd)
	case "catalog", "adopt", "add-agent", "account", "ssh":
		return lookupCommand(cmd)
	case "fleet":
		sub := firstPositional(rest, map[string]bool{
			"--unpublish": true,
			"--owner":     true,
			"--key":       true,
			"--label":     true,
			"--from-plan": true,
			"--custom":    true,
		})
		if sub == "list" || sub == "remove" {
			return lookupSubcommand("fleet", sub)
		}
	case "skill":
		if len(args) >= 2 && args[0] == "skill" && args[1] == "path" {
			return Route{}, false
		}
		sub := firstPositional(rest, map[string]bool{
			"--kind":    true,
			"--bundle":  true,
			"--search":  true,
			"--version": true,
			"--next":    true,
		})
		if sub == "path" {
			return Route{
				Name:        "skill path",
				Command:     "skill",
				Subcommand:  "path",
				Replacement: "managed-machine skill path",
			}, true
		}
	case "status":
		sec, ok := singleStatusSection(rest)
		if ok {
			return lookupSection(sec)
		}
	}
	return Route{}, false
}

// Diagnostic builds the MM_LEGACY_ROUTE_REMOVED or
// MM_LEGACY_ROUTE_UNSUPPORTED diagnostic for r.
func Diagnostic(r Route) diag.Diagnostic {
	if r.RemovedIn != "" {
		replacement := r.Replacement
		if replacement == "" {
			replacement = "no replacement"
		}
		msg := fmt.Sprintf(
			"legacy route %q was removed in %s; replacement: %s (see docs/plans/f01-caller-characterization.md)",
			r.Name, r.RemovedIn, replacement,
		)
		return diag.New(diag.LegacyRouteRemoved, msg)
	}
	if r.Command == "skill" && r.Subcommand == "path" {
		return diag.New(
			diag.LegacyRouteUnsupported,
			`legacy route "skill path" with flags before "path" is not supported by the portable core; use "managed-machine skill path [--json|--human]"`,
		)
	}
	msg := fmt.Sprintf(
		"legacy route %q is a macOS-only legacy route served only by the Homebrew-installed managed-machine on macOS",
		r.Name,
	)
	if r.Replacement != "" {
		msg += fmt.Sprintf(" (use %q)", r.Replacement)
	}
	return diag.New(diag.LegacyRouteUnsupported, msg)
}

func lookupCommand(cmd string) (Route, bool) {
	for _, r := range Routes {
		if r.Command == cmd && r.Subcommand == "" && r.Section == "" {
			return r, true
		}
	}
	return Route{}, false
}

func lookupSubcommand(cmd, sub string) (Route, bool) {
	for _, r := range Routes {
		if r.Command == cmd && r.Subcommand == sub {
			return r, true
		}
	}
	return Route{}, false
}

func lookupSection(sec string) (Route, bool) {
	for _, r := range Routes {
		if r.Command == "status" && r.Section == sec {
			return r, true
		}
	}
	return Route{}, false
}

func firstPositional(args []string, valueFlags map[string]bool) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if valueFlags[a] {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

func singleStatusSection(args []string) (string, bool) {
	var section string
	count := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--section":
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "-") {
				return "", false
			}
			i++
			section = args[i]
			count++
		case strings.HasPrefix(a, "--section="):
			section = strings.TrimPrefix(a, "--section=")
			if section == "" {
				return "", false
			}
			count++
		case a == "--custom":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
			}
		}
	}
	if count != 1 {
		return "", false
	}
	return section, true
}
