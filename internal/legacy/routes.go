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

// LegacyStatusSections are the 19 section names served by the legacy Bash
// "managed-machine status --section <name>" implementation (F01, S6 D5).
var LegacyStatusSections = []string{
	"managed-machine",
	"machine",
	"ssh",
	"bootstrap",
	"update",
	"agent-accounts",
	"local-bin",
	"zsh-functions",
	"pins",
	"toolchain",
	"homebrew",
	"gh",
	"nvm",
	"node",
	"hostname",
	"rustup",
	"rustc",
	"cargo",
	"catalog",
}

// Route is one entry in the D5 static legacy route table.
type Route struct {
	// Name is the canonical legacy route spelling reported in
	// MM_LEGACY_ROUTE_UNSUPPORTED diagnostics (for example "setup",
	// "--bootstrap", "fleet list", "skill path", "status --section ssh").
	Name string
	// Command is the top-level command or flag token ("setup", "catalog",
	// "adopt", "add-agent", "account", "ssh", "--bootstrap", "--update",
	// "fleet", "skill", "status").
	Command string
	// Subcommand, when non-empty, requires the first positional argument
	// after Command to equal Subcommand ("list", "remove", "path").
	Subcommand string
	// Section, when non-empty, requires Command == "status" and the
	// --section value to equal Section.
	Section string
	// Replacement, when non-empty, names the new-core command that
	// supersedes a compatibility-period legacy route (kept until at least
	// v0.11.0 per D2 and D5). Empty for legacy routes with no v1 core
	// replacement.
	Replacement string
}

// Routes is the static D5 table of legacy Bash routes that the portable Go
// core recognizes and refuses with MM_LEGACY_ROUTE_UNSUPPORTED when invoked
// directly on any target.
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
		},
		{
			Name:       "fleet remove",
			Command:    "fleet",
			Subcommand: "remove",
		},
		{
			Name:        "skill path",
			Command:     "skill",
			Subcommand:  "path",
			Replacement: "managed-machine skill",
		},
	}
	out := make([]Route, 0, len(base)+len(LegacyStatusSections))
	out = append(out, base...)
	for _, sec := range LegacyStatusSections {
		out = append(out, Route{
			Name:        "status --section " + sec,
			Command:     "status",
			Section:     sec,
			Replacement: "managed-machine status --section software|config|flavor|fleet",
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
	case "setup", "catalog", "adopt", "add-agent", "account", "ssh":
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
		sub := firstPositional(rest, map[string]bool{
			"--kind":    true,
			"--bundle":  true,
			"--search":  true,
			"--version": true,
			"--next":    true,
		})
		if sub == "path" {
			return lookupSubcommand("skill", "path")
		}
	case "status":
		sec, ok := singleStatusSection(rest)
		if ok {
			return lookupSection(sec)
		}
	}
	return Route{}, false
}

// Diagnostic builds the MM_LEGACY_ROUTE_UNSUPPORTED diagnostic for r.
func Diagnostic(r Route) diag.Diagnostic {
	msg := fmt.Sprintf(
		"legacy route %q is not supported by the portable core; during the compatibility period it is served only by the Homebrew-installed managed-machine on macOS",
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
