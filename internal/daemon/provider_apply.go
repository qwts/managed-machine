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

// applyWithProvider plans and applies one catalog row through a provider.
// The request carries the catalog name, the declared option and the declared
// target. A brew row carries its formula token on the catalog item. That
// token is the only brew recipe field. The request option stays a declared
// selector and is not sent to brew. A privilege error from Plan does not
// call Apply.
func applyWithProvider(ctx context.Context, item Item, option, target, stateDir string, impl provider.Provider) error {
	if impl == nil {
		return provider.ErrMissing
	}
	opts := map[string]string{"version": option}
	if item.Formula != "" {
		opts = map[string]string{"formula": item.Formula}
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
	step, err := impl.Step(resolution.Operations[0])
	if err != nil {
		return err
	}
	_, err = step.Apply(ctx)
	return err
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
	req     provider.Request
	applied bool
	planErr error
}

func (r *recordingProvider) Name() string { return "test" }

func (r *recordingProvider) Version(context.Context) (string, error) { return "0", nil }

func (r *recordingProvider) Plan(_ context.Context, req provider.Request) (*provider.Resolution, error) {
	r.req = req
	if r.planErr != nil {
		return nil, r.planErr
	}
	return &provider.Resolution{Operations: []plan.Operation{{ID: "install", Provider: "test"}}}, nil
}

func (r *recordingProvider) Check(context.Context, plan.Precondition) (string, error) {
	return "", nil
}

func (r *recordingProvider) Step(plan.Operation) (execute.Step, error) {
	return recordingStep{r}, nil
}

type recordingStep struct{ provider *recordingProvider }

func (s recordingStep) Apply(context.Context) (execute.Evidence, error) {
	s.provider.applied = true
	return execute.Evidence{}, nil
}

func (s recordingStep) Verify(context.Context) (execute.Evidence, error) {
	return execute.Evidence{}, nil
}
