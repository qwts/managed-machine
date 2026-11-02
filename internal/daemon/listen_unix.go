//go:build unix

package daemon

import (
	"net"
	"os"
	"path/filepath"
)

// Listen opens the per-user socket under dir. The directory is mode 0700
// and the socket is mode 0600, owned by the prefix owner with no ACLs or
// group access. A previous socket file is removed first.
func Listen(dir string) (net.Listener, error) {
	if filepath.Base(filepath.Dir(dir)) == ".config" {
		if err := verifyPrivateDir(filepath.Dir(dir)); err != nil {
			return nil, err
		}
	}
	if err := verifyPrivateDir(dir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := verifyPrivateDir(dir); err != nil {
		return nil, err
	}
	if err := stripDarwinACL(dir); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "daemon.sock")
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := stripDarwinACL(sock); err != nil {
		ln.Close()
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
