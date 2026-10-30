package daemon

import (
	"context"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"time"

	"github.com/qwts/managed-machine/internal/provider"
	"github.com/qwts/managed-machine/internal/provider/brew"
)

// stateDir is the per-user directory that holds the socket and the daemon key.
func stateDir(root string) string {
	return filepath.Join(root, ".config", "agent-machine")
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
				if _, body, err := statusBody(); err == nil {
					_ = writeDigest(filepath.Join(dir, "status-digest.json"), body, priv)
				}
			}
		}
	}()
	account := ""
	if current, err := user.Current(); err == nil {
		account = current.Username
	}
	planner := Planner{
		Catalog:  catalog,
		StateDir: dir,
		Account:  account,
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
		OnReceipt: func() {
			if _, body, err := statusBody(); err == nil {
				_ = writeDigest(filepath.Join(dir, "status-digest.json"), body, priv)
			}
		},
	})
}
