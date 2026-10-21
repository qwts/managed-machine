//go:build windows

package main

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// accessWritable asks whether the current user may write the path. Homebrew
// has no Windows prefix, so the gate never reaches a writable one there.
func accessWritable(string) bool { return false }

// trustFixtureCertificate trusts the certificate of the fixture server for
// the time of the refresh. Go reads the Windows certificate store, not
// SSL_CERT_FILE, so the certificate is added to the machine root store and
// removed again afterwards. The user root store would raise a confirmation
// dialog that hangs a headless run, and changing the machine store is only
// acceptable on a disposable hosted runner.
func trustFixtureCertificate(ca string) (func(), error) {
	if !hostedRunner() {
		return nil, errors.New("trusting the fixture certificate changes the machine root store; it runs only on GitHub-hosted runners")
	}
	der := filepath.Join(filepath.Dir(ca), "ca.cer")
	raw, err := os.ReadFile(der)
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum(raw)
	thumbprint := strings.ToUpper(hex.EncodeToString(sum[:]))
	out, err := exec.Command("certutil", "-addstore", "Root", der).CombinedOutput()
	if err != nil {
		return nil, errors.New("certutil -addstore: " + strings.TrimSpace(string(out)))
	}
	return func() {
		exec.Command("certutil", "-delstore", "Root", thumbprint).Run()
	}, nil
}

// isolateProcessGroup is a no-op on Windows: the gate runs no install there.
func isolateProcessGroup(*exec.Cmd) {}

// killProcessGroup kills the process itself. Windows has no process group
// to kill, and the gate runs no install on this platform.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errNoProcess
	}
	return cmd.Process.Kill()
}
