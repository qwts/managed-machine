package software

import (
	"errors"
	"testing"

	"github.com/qwts/managed-machine/internal/definition"
	"github.com/qwts/managed-machine/internal/diag"
)

func rcp(id, provider string, targets ...string) definition.Recipe {
	return definition.Recipe{ID: id, Provider: provider, Package: "p", Platforms: []string{"darwin/arm64", "linux/amd64"}, Targets: targets}
}

// tool has two uv recipes (only --recipe can tell them apart), a brew
// recipe for global/user, and a linux-only pipx recipe.
var tool = definition.Software{ID: "tool", Recipes: []definition.Recipe{
	rcp("homebrew", "brew", "global", "user"),
	rcp("uv-tool", "uv", "user", "custom"),
	rcp("uv-pinned", "uv", "user"),
	{ID: "pipx", Provider: "pipx", Package: "p", Platforms: []string{"linux/amd64"}, Targets: []string{"user"}},
}}

func TestSelectPrecedence(t *testing.T) {
	cases := []struct {
		name string
		req  Request
		want string // recipe id, or a diagnostic id
	}{
		// 1. --recipe wins over everything, including a preference.
		{"recipe", Request{Target: "user", Recipe: "uv-pinned", Preference: "brew"}, "uv-pinned"},
		{"recipe unknown", Request{Target: "user", Recipe: "nope"}, diag.SoftwareUnknownRecipe},
		{"recipe incompatible target", Request{Target: "global", Recipe: "uv-tool"}, diag.SoftwareUnsupportedCombo},
		{"recipe incompatible platform", Request{Target: "user", Recipe: "pipx"}, diag.SoftwareUnsupportedCombo},
		{"recipe and provider", Request{Target: "user", Recipe: "uv-tool", Provider: "uv"}, diag.Usage},
		// 2. --provider wins over a preference.
		{"provider", Request{Target: "user", Provider: "brew", Preference: "uv-tool"}, "homebrew"},
		{"provider ambiguous", Request{Target: "user", Provider: "uv"}, diag.SoftwareAmbiguousRecipe},
		{"provider narrowed by target", Request{Target: "custom", Provider: "uv"}, "uv-tool"},
		{"provider incompatible", Request{Target: "global", Provider: "uv"}, diag.SoftwareUnsupportedCombo},
		// 3. A preference wins over the sole-recipe shortcut.
		{"preference recipe id", Request{Target: "user", Preference: "uv-tool"}, "uv-tool"},
		{"preference provider", Request{Target: "user", Preference: "brew"}, "homebrew"},
		{"preference provider ambiguous", Request{Target: "user", Preference: "uv"}, diag.SoftwareAmbiguousRecipe},
		// Exactly one recipe (homebrew) supports global, but the preference
		// names uv, which does not: fail, never substitute.
		{"incompatible preference with a sole alternative", Request{Target: "global", Preference: "uv"}, diag.SoftwareUnsupportedCombo},
		{"incompatible preferred recipe", Request{Target: "global", Preference: "uv-tool"}, diag.SoftwareUnsupportedCombo},
		// 4. No preference: sole compatible recipe, or ambiguity.
		{"sole compatible", Request{Target: "global"}, "homebrew"},
		{"ambiguous providers", Request{Target: "user"}, diag.SoftwareAmbiguousProvider},
		{"none compatible", Request{Target: "custom", Platform: "windows/amd64"}, diag.SoftwareUnsupportedCombo},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.req.Platform == "" {
				c.req.Platform = "darwin/arm64"
			}
			r, err := Select(tool, c.req)
			var se *SelectError
			switch {
			case err == nil && r.ID != c.want:
				t.Errorf("selected %s, want %s", r.ID, c.want)
			case err != nil && (!errors.As(err, &se) || se.ID != c.want):
				t.Errorf("error %v, want %s", err, c.want)
			}
		})
	}
}

// Reordering recipes never changes an outcome: there is no catalog-order
// fallback for any request.
func TestSelectIgnoresCatalogOrder(t *testing.T) {
	reversed := tool
	reversed.Recipes = nil
	for i := len(tool.Recipes) - 1; i >= 0; i-- {
		reversed.Recipes = append(reversed.Recipes, tool.Recipes[i])
	}
	for _, target := range []string{"global", "user", "custom"} {
		for _, pref := range []string{"", "uv", "brew", "uv-tool", "pipx"} {
			for _, prov := range []string{"", "uv", "brew"} {
				req := Request{Platform: "darwin/arm64", Target: target, Preference: pref, Provider: prov}
				a, errA := Select(tool, req)
				b, errB := Select(reversed, req)
				if a.ID != b.ID || (errA == nil) != (errB == nil) || errA != nil && errA.Error() != errB.Error() {
					t.Errorf("%+v: %s/%v vs %s/%v", req, a.ID, errA, b.ID, errB)
				}
			}
		}
	}
}
