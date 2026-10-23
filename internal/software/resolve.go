// Package software implements discovery over the normalized catalog and the
// pure recipe-selection rules of the software contract (cli/software.md,
// "Provider selection"). Selection is reachable only from installation,
// which lands in S2; in S1 it is exercised by fixtures.
package software

import (
	"fmt"
	"sort"
	"strings"

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/diag"
)

// Request is everything selection may consult. Recipe and Provider are the
// explicit flags; Preference is the desired-configuration preference for
// this software and target (a recipe id or a provider name), or "".
type Request struct {
	Platform   string
	Target     string // global, user or custom
	Recipe     string
	Provider   string
	Preference string
}

// SelectError is a typed selection failure carrying its diagnostic id.
type SelectError struct {
	ID      string
	Message string
}

func (e *SelectError) Error() string { return e.ID + ": " + e.Message }

func fail(id, format string, args ...any) *SelectError {
	return &SelectError{ID: id, Message: fmt.Sprintf(format, args...)}
}

// Select picks exactly one recipe or fails. Precedence, highest first:
// explicit recipe, explicit provider, desired preference, sole compatible
// recipe. There is no catalog-order fallback: every path that leaves more
// than one candidate is an ambiguity error, and an incompatible preference
// is an error even when another recipe would apply.
func Select(sw definition.Software, req Request) (definition.Recipe, error) {
	if req.Recipe != "" && req.Provider != "" {
		return definition.Recipe{}, fail(diag.Usage, "--recipe and --provider are mutually exclusive")
	}
	where := sw.ID + " " + req.Target + " on " + req.Platform
	applies := func(r definition.Recipe) bool {
		return contains(r.Platforms, req.Platform) && contains(r.Targets, req.Target)
	}
	compatible := func(match func(definition.Recipe) bool) []definition.Recipe {
		var out []definition.Recipe
		for _, r := range sw.Recipes {
			if match(r) && applies(r) {
				out = append(out, r)
			}
		}
		return out
	}
	byProvider := func(p string) func(definition.Recipe) bool {
		return func(r definition.Recipe) bool { return r.Provider == p }
	}

	// 1. --recipe selects that recipe directly.
	if req.Recipe != "" {
		for _, r := range sw.Recipes {
			if r.ID == req.Recipe {
				if !applies(r) {
					return definition.Recipe{}, fail(diag.SoftwareUnsupportedCombo, "%s: recipe %s does not support this platform and target", where, r.ID)
				}
				return r, nil
			}
		}
		return definition.Recipe{}, fail(diag.SoftwareUnknownRecipe, "%s has no recipe %q; recipes: %s", sw.ID, req.Recipe, strings.Join(recipeIDs(sw.Recipes), ", "))
	}
	// 2. --provider narrows to that provider's recipes.
	if req.Provider != "" {
		return one(where, "provider "+req.Provider, compatible(byProvider(req.Provider)), diag.SoftwareAmbiguousRecipe)
	}
	// 3. A declared preference is checked before the sole-recipe shortcut:
	// a recipe id selects that recipe, otherwise it names a provider.
	if req.Preference != "" {
		for _, r := range sw.Recipes {
			if r.ID == req.Preference {
				if !applies(r) {
					return definition.Recipe{}, fail(diag.SoftwareUnsupportedCombo, "%s: preferred recipe %s does not support this platform and target", where, r.ID)
				}
				return r, nil
			}
		}
		return one(where, "preferred provider "+req.Preference, compatible(byProvider(req.Preference)), diag.SoftwareAmbiguousRecipe)
	}
	// 4. Otherwise exactly one compatible recipe, or an ambiguity error
	// naming the valid providers.
	all := compatible(func(definition.Recipe) bool { return true })
	switch len(all) {
	case 0:
		return definition.Recipe{}, fail(diag.SoftwareUnsupportedCombo, "%s: no recipe supports this platform and target", where)
	case 1:
		return all[0], nil
	}
	return definition.Recipe{}, fail(diag.SoftwareAmbiguousProvider, "%s: valid providers: %s", where, strings.Join(providers(all), ", "))
}

func one(where, what string, rs []definition.Recipe, ambiguous string) (definition.Recipe, error) {
	switch len(rs) {
	case 0:
		return definition.Recipe{}, fail(diag.SoftwareUnsupportedCombo, "%s: no %s recipe supports this platform and target", where, what)
	case 1:
		return rs[0], nil
	}
	return definition.Recipe{}, fail(ambiguous, "%s: %s has several recipes: %s; use --recipe", where, what, strings.Join(recipeIDs(rs), ", "))
}

func recipeIDs(rs []definition.Recipe) []string {
	out := make([]string, 0, len(rs))
	for _, r := range rs {
		out = append(out, r.ID)
	}
	sort.Strings(out)
	return out
}

func providers(rs []definition.Recipe) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rs {
		if !seen[r.Provider] {
			seen[r.Provider] = true
			out = append(out, r.Provider)
		}
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
