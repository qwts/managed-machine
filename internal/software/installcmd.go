package software

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/qwts/managed-machine/internal/cli"
	"github.com/qwts/managed-machine/internal/diag"
	"github.com/qwts/managed-machine/internal/install"
	"github.com/qwts/managed-machine/internal/legacy"
	"github.com/qwts/managed-machine/internal/output"
	"github.com/qwts/managed-machine/internal/source"
)

// privilegeHint names the way forward when a provider refuses an unwritable
// target. On macOS the Homebrew prefix belongs to an admin-group owner by
// design and is never chowned to the invoking user, so the only sanctioned
// path for cutover catalog software is the legacy setup route, which asks for
// administrator authorization through the OS dialog. Elsewhere the message
// stands on its own.
func privilegeHint(platform, software string) string {
	if !strings.HasPrefix(platform, "darwin/") {
		return ""
	}
	r, ok := legacy.SetupRouteForSoftware(software)
	if !ok {
		return ""
	}
	return "; run `managed-machine setup " + r.Name + "`, which installs it after the macOS administrator authorization dialog"
}

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
			return fail(diag.SoftwarePrivilegeRequired, "%v%s", err, privilegeHint(string(host.Target), entry.Software.ID))
		case errors.Is(err, install.ErrMissing):
			return fail(diag.SoftwareProviderMissing, "%v", err)
		case errors.Is(err, install.ErrCaskRefused):
			return fail(diag.SoftwareCaskRefused, "%v", err)
		case errors.Is(err, install.ErrArtifactMismatch):
			return fail(diag.SoftwareArtifactMismatch, "%v", err)
		case errors.Is(err, install.ErrSignatureRejected):
			return fail(diag.SoftwareSignatureRejected, "%v", err)
		case errors.Is(err, install.ErrHostUnlisted):
			return fail(diag.SoftwareHostUnlisted, "%v", err)
		case errors.Is(err, install.ErrOccupied):
			return fail(diag.SoftwareDestinationOccupied, "%v", err)
		case errors.Is(err, install.ErrBusy):
			return fail(diag.SoftwareBusy, "%v", err)
		case errors.As(err, &rej):
			return fail(diag.SoftwarePlanRejected, "%v", err)
		case errors.As(err, &fl):
			return fail(diag.SoftwareInstallFailed, "%v", err)
		case errors.Is(err, install.ErrUnsupported):
			return fail(diag.SoftwareUnsupportedCombo, "%v", err)
		}
		return fail(diag.SoftwareInstallFailed, "%v", err)
	}
	rec := output.New().Set("software", entry.Software.ID).Set("recipe", recipe.ID).Set("provider", recipe.Provider).
		Set("target", tgt).Set("resolved_version", res.ResolvedVersion).Set("outcome", res.Outcome).Set("plan", res.Plan)
	return cli.Result{Records: []*output.Record{rec}}
}
