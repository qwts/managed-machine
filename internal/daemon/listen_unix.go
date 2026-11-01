//go:build unix

package daemon

import (
	"net"
	"os"
	"path/filepath"
)

// Listen opens the per-user socket under dir. The directory is mode 0700
// (with sticky bit when inside a shared prefix var/agent-machine directory)
// and the socket is mode 0600. On macOS, explicit user ACLs grant access to
// the local `user` and `admin` accounts without opening group `staff` (which
// includes agent accounts). A previous socket file is removed first.
func Listen(dir string) (net.Listener, error) {
	dirMode := os.FileMode(0o700)
	if isSharedPrefixDir(dir) {
		dirMode = 0o700 | os.ModeSticky
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, err
	}
	if isSharedPrefixDir(dir) {
		applyDarwinSharedDirACL(dir, "")
	}
	sock := filepath.Join(dir, "daemon.sock")
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if isSharedPrefixDir(dir) {
		grantDarwinUserSocketACL(sock)
	}
	return ln, nil
}
