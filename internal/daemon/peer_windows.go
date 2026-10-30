//go:build windows

package daemon

import (
	"net"
	"os"
)

// The pipe DACL admits only the creating user. The uid check matches
// os.Getuid, which is -1 on Windows, so the signature remains the
// distinguishing control.
func peerCred(net.Conn) (int, error) {
	return os.Getuid(), nil
}
