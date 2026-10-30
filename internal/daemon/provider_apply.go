package daemon

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

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
// target. A privilege error from Plan does not call Apply.
func applyWithProvider(ctx context.Context, item Item, option, target, stateDir string, impl provider.Provider) error {
	if impl == nil {
		return provider.ErrMissing
	}
	resolution, err := impl.Plan(ctx, provider.Request{
		Software: item.Name,
		Target:   target,
		Options:  map[string]string{"version": option},
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

// loadPinnedCatalog reads catalog.json only when catalog.sha256 matches its
// bytes and catalog.sig verifies under catalog.pub. The first accepted
// catalog.pub is stored as catalog.pub.pin. A later different key is refused.
// Missing catalog.json is an empty catalog. A mismatch or an unknown field
// is an error.
func loadPinnedCatalog(dir string) (Catalog, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "catalog.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Catalog{}, nil
	}
	if err != nil {
		return Catalog{}, err
	}
	pin, err := os.ReadFile(filepath.Join(dir, "catalog.sha256"))
	if err != nil {
		return Catalog{}, err
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != strings.TrimSpace(string(pin)) {
		return Catalog{}, errors.New("catalog pin does not match")
	}
	pub, err := loadPublic(filepath.Join(dir, "catalog.pub"))
	if err != nil {
		return Catalog{}, errors.New("catalog signing key is missing")
	}
	sig, err := os.ReadFile(filepath.Join(dir, "catalog.sig"))
	if err != nil || !ed25519.Verify(pub, raw, sig) {
		return Catalog{}, errors.New("catalog signature does not match")
	}
	pinnedPath := filepath.Join(dir, "catalog.pub.pin")
	pinned, err := os.ReadFile(pinnedPath)
	if err == nil {
		if !bytes.Equal(pinned, pub) {
			return Catalog{}, errors.New("catalog signing key changed")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Catalog{}, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var catalog Catalog
	if err := dec.Decode(&catalog); err != nil {
		return Catalog{}, err
	}
	if len(pinned) == 0 {
		if err := os.WriteFile(pinnedPath, pub, 0o600); err != nil {
			return Catalog{}, err
		}
	}
	return catalog, nil
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
