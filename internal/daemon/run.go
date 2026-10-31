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
	"github.com/qwts/managed-machine/internal/provider/brew"
	"github.com/qwts/managed-machine/internal/provider/npm"
	"github.com/qwts/managed-machine/internal/provider/uv"
)

// stateDir is the per-user directory that holds the socket and the daemon key.
func stateDir(root string) string {
	return filepath.Join(root, ".config", "agent-machine")
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

// commandOnFixedPath refuses a provider whose executable is not on the
// daemon's fixed PATH. The daemon does not install the tool and does not
// search the inherited PATH.
func commandOnFixedPath(name string) error {
	names := []string{name}
	if runtime.GOOS == "windows" {
		names = []string{name + ".exe", name + ".cmd"}
	}
	for _, dir := range filepath.SplitList(fixedPath()) {
		for _, candidate := range names {
			info, err := os.Stat(filepath.Join(dir, candidate))
			if err == nil && !info.IsDir() {
				return nil
			}
		}
	}
	return fmt.Errorf("%w: %s", provider.ErrMissing, name)
}

// Run listens on the per-user socket until ctx is cancelled. Each client's
// public key is read from peers/<client>.pub before each request. A legacy
// peer.pub is moved to peers/terminal.pub. Revoking one client refuses that
// key and leaves the other client live. A confirmed replacement is accepted
// on the next request. Receipts name this process's account.
func Run(ctx context.Context, root string) error {
	dir := stateDir(root)
	ln, err := Listen(dir)
	if err != nil {
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
			env := minimalEnv(root, os.Environ())
			var impl provider.Provider
			switch item.Provider {
			case brew.Name:
				if err := brewInstalled(brew.DefaultPrefix(runtime.GOOS + "/" + runtime.GOARCH)); err != nil {
					return err
				}
				impl = &brew.Provider{
					Prefix:  brew.DefaultPrefix(runtime.GOOS + "/" + runtime.GOARCH),
					Run:     brew.Exec,
					Environ: func() []string { return env },
				}
			case npm.Name:
				if err := commandOnFixedPath("npm"); err != nil {
					return err
				}
				impl = &npm.Provider{
					GOOS:    runtime.GOOS,
					Run:     npm.Exec,
					Environ: func() []string { return env },
				}
			case uv.Name:
				if err := commandOnFixedPath("uv"); err != nil {
					return err
				}
				impl = &uv.Provider{
					Run:     uv.Exec,
					Environ: func() []string { return env },
				}
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
