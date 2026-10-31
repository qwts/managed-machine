package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/qwts/managed-machine/internal/execute"
	"github.com/qwts/managed-machine/internal/plan"
	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/brew"
	"github.com/qwts/managed-machine/internal/provider/npm"
	"github.com/qwts/managed-machine/internal/provider/uv"
)

// minimalEnv is the environment a provider child may see. PATH is the
// prefix for this OS, not the inherited PATH. Inherited package-manager
// configuration is dropped.
func minimalEnv(home string, _ []string) []string {
	return []string{
		"HOME=" + home,
		"USERPROFILE=" + home,
		"PATH=" + fixedPath(),
	}
}

func fixedPath() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return "/opt/homebrew/bin:/usr/bin:/bin"
	case "darwin/amd64":
		return "/usr/local/bin:/usr/bin:/bin"
	case "linux/arm64", "linux/amd64":
		return "/home/linuxbrew/.linuxbrew/bin:/usr/bin:/bin"
	case "windows/amd64", "windows/arm64":
		return `C:\Windows\System32`
	default:
		return "/usr/bin:/bin"
	}
}

// errFormulaProvider refuses a formula token on a row that is not brew.
var errFormulaProvider = errors.New("a formula token requires the brew provider")

// errBrewFormula refuses a brew row that names neither a formula nor a cask.
var errBrewFormula = errors.New("a brew row requires a formula token or a cask recipe")

// errCaskProvider refuses cask fields on a row that is not brew.
var errCaskProvider = errors.New("a cask recipe requires the brew provider")

// errCaskFields refuses a cask recipe that is missing a required field.
var errCaskFields = errors.New("a cask recipe requires a token, app name, team id, and hosts")

// errPackageProvider refuses a package token on a row that is not npm.
var errPackageProvider = errors.New("a package token requires the npm provider")

// errNpmPackage refuses an npm row that does not name a package token.
var errNpmPackage = errors.New("an npm row requires a package token")

// errToolProvider refuses a tool token on a row that is not uv.
var errToolProvider = errors.New("a tool token requires the uv provider")

// errUvTool refuses a uv row that does not name a tool token.
var errUvTool = errors.New("a uv row requires a tool token")

// errOneToken refuses a row that carries more than one recipe token.
var errOneToken = errors.New("a catalog row carries one recipe token")

// recipeOptions checks the catalog token before a provider is constructed.
// Brew, npm, and uv receive only their catalog token. Any other provider
// receives the declared option as a version selector.
func recipeOptions(item Item, option string) (map[string]string, error) {
	n := 0
	if item.Formula != "" {
		n++
	}
	if item.Package != "" {
		n++
	}
	if item.Tool != "" {
		n++
	}
	if caskRecipe(item) {
		n++
	}
	if n > 1 {
		return nil, errOneToken
	}
	if item.Formula != "" && item.Provider != brew.Name {
		return nil, errFormulaProvider
	}
	if item.Package != "" && item.Provider != npm.Name {
		return nil, errPackageProvider
	}
	if item.Tool != "" && item.Provider != uv.Name {
		return nil, errToolProvider
	}
	if caskRecipe(item) && item.Provider != brew.Name {
		return nil, errCaskProvider
	}
	if caskRecipe(item) && !caskComplete(item) {
		return nil, errCaskFields
	}
	if item.Provider == brew.Name && item.Formula == "" && !caskRecipe(item) {
		return nil, errBrewFormula
	}
	if item.Provider == npm.Name && item.Package == "" {
		return nil, errNpmPackage
	}
	if item.Provider == uv.Name && item.Tool == "" {
		return nil, errUvTool
	}
	switch {
	case item.Formula != "":
		return map[string]string{"formula": item.Formula}, nil
	case caskRecipe(item):
		return caskOptions(item), nil
	case item.Package != "":
		return map[string]string{"package": item.Package}, nil
	case item.Tool != "":
		return map[string]string{"tool": item.Tool}, nil
	default:
		return map[string]string{"version": option}, nil
	}
}

func caskRecipe(item Item) bool {
	return item.Cask != "" || item.AppName != "" || item.TeamID != "" || item.URLHosts != "" || item.HomepageHosts != "" || item.AllowRollingURL
}

func caskComplete(item Item) bool {
	return item.Cask != "" && item.AppName != "" && item.TeamID != "" && item.URLHosts != "" && item.HomepageHosts != ""
}

func caskOptions(item Item) map[string]string {
	opts := map[string]string{
		"cask":           item.Cask,
		"app_name":       item.AppName,
		"team_id":        item.TeamID,
		"url_hosts":      item.URLHosts,
		"homepage_hosts": item.HomepageHosts,
	}
	if item.AllowRollingURL {
		opts["allow_rolling_url"] = "true"
	}
	return opts
}

// applyWithProvider plans and applies one catalog row through a provider.
// The request carries the catalog name, the declared option and the declared
// target. A brew row carries its formula token, an npm row its package
// token, and a uv row its tool token. A brew cask row carries the cask
// token, app name, team id, and hosts. That recipe is the only field set
// sent to the provider. The request option stays a declared selector and is
// not sent to brew, npm, or uv. A privilege error from Plan does not call Apply.
func applyWithProvider(ctx context.Context, item Item, option, target, stateDir string, impl provider.Provider) error {
	opts, err := recipeOptions(item, option)
	if err != nil {
		return err
	}
	if impl == nil {
		return provider.ErrMissing
	}
	resolution, err := impl.Plan(ctx, provider.Request{
		Software: item.Name,
		Target:   target,
		Options:  opts,
		StateDir: stateDir,
	})
	if err != nil {
		return err
	}
	if resolution == nil || len(resolution.Operations) == 0 {
		return nil
	}
	for _, op := range resolution.Operations {
		step, err := impl.Step(op)
		if err != nil {
			return err
		}
		if _, err := step.Apply(ctx); err != nil {
			return err
		}
		if _, err := step.Verify(ctx); err != nil {
			return err
		}
	}
	return nil
}

// loadPinnedCatalog refuses a catalog on disk. catalog.pub, catalog.pub.pin,
// and catalog.version are writable by the same account that writes the
// catalog, so none of them is a trust anchor. The signing key has to arrive
// from the paired channel. Until it does, a present catalog.json is an error
// and Run does not listen. Missing catalog.json is an empty catalog. A
// document whose version is missing or below 1 is refused before that, and
// this function never writes a pin or a version record.
func loadPinnedCatalog(dir string) (Catalog, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Catalog{}, nil
	}
	if err != nil {
		return Catalog{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var catalog Catalog
	if err := dec.Decode(&catalog); err != nil {
		return Catalog{}, err
	}
	if catalog.Version < 1 {
		return Catalog{}, errors.New("catalog version must be at least 1")
	}
	return Catalog{}, errors.New("catalog signing key is not anchored")
}

// recordingProvider is the test double for applyWithProvider.
type recordingProvider struct {
	req       provider.Request
	applied   bool
	planErr   error
	verifyErr error
	ops       []plan.Operation
	steps     []string
	verified  []string
}

func (r *recordingProvider) Name() string { return "test" }

func (r *recordingProvider) Version(context.Context) (string, error) { return "0", nil }

func (r *recordingProvider) Plan(_ context.Context, req provider.Request) (*provider.Resolution, error) {
	r.req = req
	if r.planErr != nil {
		return nil, r.planErr
	}
	if len(r.ops) > 0 {
		return &provider.Resolution{Operations: r.ops}, nil
	}
	return &provider.Resolution{Operations: []plan.Operation{{ID: "install", Provider: "test"}}}, nil
}

func (r *recordingProvider) Check(context.Context, plan.Precondition) (string, error) {
	return "", nil
}

func (r *recordingProvider) Step(op plan.Operation) (execute.Step, error) {
	return recordingStep{provider: r, id: op.ID}, nil
}

type recordingStep struct {
	provider *recordingProvider
	id       string
}

func (s recordingStep) Apply(context.Context) (execute.Evidence, error) {
	s.provider.applied = true
	s.provider.steps = append(s.provider.steps, s.id)
	return execute.Evidence{}, nil
}

func (s recordingStep) Verify(context.Context) (execute.Evidence, error) {
	if s.provider.verifyErr != nil {
		return execute.Evidence{}, s.provider.verifyErr
	}
	s.provider.verified = append(s.provider.verified, s.id)
	return execute.Evidence{}, nil
}
