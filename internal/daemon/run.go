package daemon

import (
	"context"
	"crypto/ed25519"
	"os"
	"path/filepath"
	"runtime"

	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/brew"
)

// stateDir is the per-user directory that holds the socket and the daemon key.
func stateDir(root string) string {
	return filepath.Join(root, ".config", "agent-machine")
}

// Run listens on the per-user socket until ctx is cancelled. The paired
// public key is read from peer.pub. When that file is absent every request
// is refused.
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
	var live ed25519.PublicKey
	if pub, err := loadPublic(filepath.Join(dir, "peer.pub")); err == nil {
		live = pub
	}
	gate := &Gate{Pairing: Pairing{Live: live}, PeerUID: os.Getuid()}
	catalog, err := loadPinnedCatalog(dir)
	if err != nil {
		ln.Close()
		return err
	}
	planner := Planner{
		Catalog:  catalog,
		StateDir: dir,
		Account:  "admin",
		Exec: func(item Item, option, target string) error {
			var impl provider.Provider
			if item.Provider == brew.Name {
				env := minimalEnv(root, os.Environ())
				impl = &brew.Provider{
					Prefix:  brew.DefaultPrefix(runtime.GOOS + "/" + runtime.GOARCH),
					Run:     brew.Exec,
					Environ: func() []string { return env },
				}
			}
			return applyWithProvider(ctx, item, option, target, dir, impl)
		},
	}
	return Serve(ctx, ln, gate, priv, peerCred, Hooks{
		Status:   statusBody,
		Receipts: func() ([]byte, error) { return receiptsBody(dir) },
		Plan:     planner.Plan,
		Apply:    planner.Apply,
		Update:   planner.Update,
	})
}
