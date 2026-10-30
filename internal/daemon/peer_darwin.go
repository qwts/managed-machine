//go:build darwin

package daemon

import (
	"net"
	"syscall"
	"unsafe"
)

const (
	solLocal      = 0
	localPeerCred = 1
)

// xucred matches the Darwin xucred layout: version, uid, ngroups, pad, 16 groups.
type xucred struct {
	version uint32
	uid     uint32
	ngroups int16
	_       int16
	groups  [16]uint32
}

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
		cred := xucred{}
		size := uint32(unsafe.Sizeof(cred))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			uintptr(solLocal),
			uintptr(localPeerCred),
			uintptr(unsafe.Pointer(&cred)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			out = errno
			return
		}
		uid = int(cred.uid)
	})
	if err != nil {
		return 0, err
	}
	if out != nil {
		return 0, out
	}
	return uid, nil
}
