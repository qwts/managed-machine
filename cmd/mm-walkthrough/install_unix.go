//go:build unix

package main

import (
	"os/exec"
	"syscall"
)

// accessWritable asks the kernel whether the current user may write the
// path, without writing anything.
func accessWritable(path string) bool {
	return syscall.Access(path, 2) == nil // W_OK
}

// trustFixtureCertificate trusts the certificate of the fixture server for
// the time of the refresh. Go reads SSL_CERT_FILE on this platform, so the
// environment is enough and nothing on the machine changes.
func trustFixtureCertificate(string) (func(), error) {
	return func() {}, nil
}

// isolateProcessGroup starts the command in a process group of its own, so
// the whole group can be killed at once and no child of the install
// survives it.
func isolateProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills every process in the command's group.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errNoProcess
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
