package software

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/qwts/managed-machine/core/internal/cli"
	"github.com/qwts/managed-machine/core/internal/diag"
	"github.com/qwts/managed-machine/core/internal/install"
	"github.com/qwts/managed-machine/core/internal/output"
	"github.com/qwts/managed-machine/core/internal/source"
)

// installRun validates the request against the catalog, selects exactly
// one recipe with the F06 rules and hands it to the installer. Every
// validation failure happens before the installer is called.
func installRun(d Deps, ctx cli.Context, f flags) cli.Result {
	fail := func(id, format string, args ...any) cli.Result {
		dg := diag.New(id, fmt.Sprintf(format, args...))
		return cli.Result{Diagnostic: &dg}
	}
	// discover performs the shared name, provider, category and recipe
	// checks; its records are not needed here.
	if _, dg := discover(d, ctx, flags{name: f.name, provider: f.provider, recipe: f.recipe, category: f.category}); dg != nil {
		return cli.Result{Diagnostic: dg}
	}
	host, _ := ctx.Detect()
	entries, _, _ := d.Catalog()
	var entry source.Entry
	for _, e := range entries {
		if e.Software.ID == f.name {
			entry = e
		}
	}
	if f.category != "" && !contains(entry.Software.Categories, f.category) {
		return fail(diag.SoftwareUnsupportedCombo, "%s is not in category %s", f.name, f.category)
	}
	recipe, err := Select(entry.Software, Request{Platform: string(host.Target), Target: f.target(), Recipe: f.recipe, Provider: f.provider})
	if err != nil {
		var se *SelectError
		if errors.As(err, &se) {
			return fail(se.ID, "%s", se.Message)
		}
		return fail(diag.SoftwareInstallFailed, "%v", err)
	}
	tgt := f.target()
	if f.hasCustom {
		abs, err := filepath.Abs(f.custom)
		if err != nil {
			return fail(diag.Usage, "--custom %q: %v", f.custom, err)
		}
		tgt = "custom:" + abs
	}
	if d.Install == nil {
		return fail(diag.SoftwareInstallUnavailable, "installation is not available in this build")
	}
	res, err := d.Install(context.Background(), install.Request{Entry: entry, Recipe: recipe, Target: tgt, Platform: string(host.Target)})
	if err != nil {
		var rej *install.Rejected
		var fl *install.Failed
		switch {
		case errors.Is(err, install.ErrUnavailable):
			return fail(diag.SoftwareInstallUnavailable, "%v", err)
		case errors.Is(err, install.ErrPrivilege):
			return fail(diag.SoftwarePrivilegeRequired, "%v", err)
		case errors.Is(err, install.ErrMissing):
			return fail(diag.SoftwareProviderMissing, "%v", err)
		case errors.Is(err, install.ErrUnsupported):
			return fail(diag.SoftwareUnsupportedCombo, "%v", err)
		case errors.Is(err, install.ErrBusy):
			return fail(diag.SoftwareBusy, "%v", err)
		case errors.As(err, &rej):
			return fail(diag.SoftwarePlanRejected, "%v", err)
		case errors.As(err, &fl):
			return fail(diag.SoftwareInstallFailed, "%v", err)
		}
		return fail(diag.SoftwareInstallFailed, "%v", err)
	}
	rec := output.New().Set("software", entry.Software.ID).Set("recipe", recipe.ID).Set("provider", recipe.Provider).
		Set("target", tgt).Set("resolved_version", res.ResolvedVersion).Set("outcome", res.Outcome).Set("plan", res.Plan)
	return cli.Result{Records: []*output.Record{rec}}
}
