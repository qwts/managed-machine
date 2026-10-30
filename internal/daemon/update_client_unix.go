//go:build !windows

package daemon

import (
	"net"
	"os"
)

func dialDaemon(endpoint string) (net.Conn, error) {
	if _, err := os.Stat(endpoint); err != nil {
		return nil, err
	}
	return net.Dial("unix", endpoint)
}
