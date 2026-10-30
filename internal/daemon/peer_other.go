//go:build !darwin && !linux

package daemon

import (
	"fmt"
	"net"
	"runtime"
)

func peerCred(net.Conn) (int, error) {
	return 0, fmt.Errorf("peer credentials are not available on %s", runtime.GOOS)
}
