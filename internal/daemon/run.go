package daemon

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	"fmt"

	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/appcopy"
	"github.com/qwts/managed-machine/internal/provider/archive"
	"github.com/qwts/managed-machine/internal/provider/brew"
	"github.com/qwts/managed-machine/internal/provider/cargo"
	"github.com/qwts/managed-machine/internal/provider/golang"
	"github.com/qwts/managed-machine/internal/provider/npm"
	"github.com/qwts/managed-machine/internal/provider/uv"
	"github.com/qwts/managed-machine/internal/provider/winget"
)

// StateDir resolves the daemon state directory under root (<root>/.config/agent-machine).
// It lives inside the prefix owner's private home directory (0700) and is never
// placed in a shared location accessible to `user` or agent accounts.
func StateDir(root string) string {
	return stateDir(root)
}

func stateDir(root string) string {
	return filepath.Join(root, ".config", "agent-machine")
}

func defaultLegacySharedDir(root string) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || filepath.Clean(root) != filepath.Clean(home) {
		return ""
	}
	prefix := brew.DefaultPrefix(runtime.GOOS + "/" + runtime.GOARCH)
	if prefix == "" {
		return ""
	}
	return filepath.Join(prefix, "var", "agent-machine")
}

// brewInstalled refuses a brew row when the canonical prefix has no brew
// binary. An empty prefix is a platform with no Homebrew, including Windows.
// A missing binary is Linux or macOS without Homebrew. The daemon does not
// install Homebrew and does not create the prefix.
func brewInstalled(prefix string) error {
	if prefix == "" {
		return fmt.Errorf("%w: no canonical Homebrew prefix", provider.ErrMissing)
	}
	bin := filepath.Join(prefix, "bin", "brew")
	if _, err := os.Stat(bin); err != nil {
		return fmt.Errorf("%w: %s", provider.ErrMissing, bin)
	}
	return nil
}

func runProvider(root string, item Item) (provider.Provider, error) {
	env := minimalEnv(root, os.Environ())
	switch item.Provider {
	case brew.Name:
		if err := brewInstalled(brew.DefaultPrefix(runtime.GOOS + "/" + runtime.GOARCH)); err != nil {
			return nil, err
		}
		return &brew.Provider{
			Prefix:  brew.DefaultPrefix(runtime.GOOS + "/" + runtime.GOARCH),
			Run:     brew.Exec,
			Environ: func() []string { return env },
		}, nil
	case npm.Name:
		return &npm.Provider{
			GOOS:    runtime.GOOS,
			Run:     npm.Exec,
			Environ: func() []string { return env },
		}, nil
	case uv.Name:
		return &uv.Provider{
			Run:     uv.Exec,
			Environ: func() []string { return env },
		}, nil
	case appcopy.Name:
		return &appcopy.Provider{
			Run:      appcopy.Exec,
			Environ:  func() []string { return env },
			StateDir: stateDir(root),
		}, nil
	case winget.Name:
		return &winget.Provider{
			Run:      winget.Exec,
			Environ:  func() []string { return env },
			Elevated: func() bool { return false },
		}, nil
	case golang.Name:
		return &golang.Provider{
			GOOS:     runtime.GOOS,
			Run:      golang.Exec,
			Environ:  func() []string { return env },
			LookPath: golang.LookPath,
			Client:   golang.DefaultClient(),
		}, nil
	case cargo.Name:
		return &cargo.Provider{
			GOOS:    runtime.GOOS,
			Run:     cargo.Exec,
			Environ: func() []string { return env },
			Client:  cargo.DefaultClient(),
		}, nil
	case archive.Name:
		return &archive.Provider{
			GOOS:     runtime.GOOS,
			Environ:  func() []string { return env },
			Client:   archive.DefaultClient(),
			StateDir: stateDir(root),
		}, nil
	default:
		return nil, nil
	}
}

// Run listens on the per-user socket until ctx is cancelled. Each client's
// public key is read from peers/<client>.pub before each request. A legacy
// peer.pub is moved to peers/terminal.pub. Revoking one client refuses that
// key and leaves the other client live. A confirmed replacement is accepted
// on the next request. Receipts name this process's account.
func Run(ctx context.Context, root string) error {
	dir := stateDir(root)
	if err := purgeLegacySharedState(dir, defaultLegacySharedDir(root), defaultKeyStore(filepath.Join(dir, "daemon.key"))); err != nil {
		return err
	}
	ln, err := Listen(dir)
	if err != nil {
		return err
	}
	if _, err := ensureStateDirAndPeers(dir); err != nil {
		ln.Close()
		return err
	}
	priv, err := loadOrCreateKey(filepath.Join(dir, "daemon.key"))
	if err != nil {
		ln.Close()
		return err
	}
	pairing, err := loadPairing(dir)
	if err != nil {
		ln.Close()
		return err
	}
	gate := &Gate{Pairing: pairing, Dir: dir, PeerUID: ownerPeerID()}
	catalog, err := loadPinnedCatalog(dir)
	if err != nil {
		ln.Close()
		return err
	}
	go func() {
		ticker := time.NewTicker(digestEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, body, err := statusBody(dir); err == nil {
					_ = writeDigest(filepath.Join(dir, "status-digest.json"), body, priv)
				}
			}
		}
	}()
	account := ""
	if current, err := user.Current(); err == nil {
		account = current.Username
	}
	session := &catalogSession{cat: catalog}
	planner := Planner{
		current:  session.current,
		StateDir: dir,
		Account:  account,
		Exec: func(item Item, option, target string) error {
			impl, err := runProvider(root, item)
			if err != nil {
				return err
			}
			return applyWithProvider(ctx, item, option, target, dir, impl)
		},
	}
	return Serve(ctx, ln, gate, priv, peerCred, Hooks{
		Status:   func() (string, []byte, error) { return statusBody(dir) },
		Receipts: func() ([]byte, error) { return receiptsBody(dir) },
		Plan:     planner.Plan,
		Apply:    planner.Apply,
		Update:   planner.Update,
		Catalog:  func(req Request) (string, string, string) { return session.accept(dir, req) },
		OnReceipt: func() {
			if _, body, err := statusBody(dir); err == nil {
				_ = writeDigest(filepath.Join(dir, "status-digest.json"), body, priv)
			}
		},
	})
}
