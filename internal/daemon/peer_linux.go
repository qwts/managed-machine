//go:build linux

package daemon

import (
	"net"
	"syscall"
)

func peerCred(conn net.Conn) (int, error) {
	raw, ok := conn.(syscall.Conn)
	if !ok {
		return 0, syscall.EINVAL
	}
	sc, err := raw.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var out error
	err = sc.Control(func(fd uintptr) {
		cred, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
		if err != nil {
			out = err
			return
		}
		uid = int(cred.Uid)
	})
	if err != nil {
		return 0, err
	}
	if out != nil {
		return 0, out
	}
	return uid, nil
}
