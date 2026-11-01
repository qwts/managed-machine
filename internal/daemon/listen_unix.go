//go:build unix

package daemon

import (
	"net"
	"os"
	"path/filepath"
)

// Listen opens the per-user socket under dir. The directory is mode 0700
// (or 0750 when inside a shared prefix var/agent-machine directory) and the
// socket is mode 0600 (or 0660 for shared prefix var/agent-machine). A
// previous socket file is removed first.
func Listen(dir string) (net.Listener, error) {
	dirMode := os.FileMode(0o700)
	sockMode := os.FileMode(0o600)
	if isSharedPrefixDir(dir) {
		dirMode = 0o750
		sockMode = 0o660
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return nil, err
	}
	if dirMode == 0o750 {
		ensureStaffGroup(dir)
	}
	sock := filepath.Join(dir, "daemon.sock")
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(sock, sockMode); err != nil {
		ln.Close()
		return nil, err
	}
	if sockMode == 0o660 {
		ensureStaffGroup(sock)
	}
	return ln, nil
}
