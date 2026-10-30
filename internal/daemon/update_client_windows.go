//go:build windows

package daemon

import (
	"net"
	"os"

	"golang.org/x/sys/windows"
)

func dialDaemon(string) (net.Conn, error) {
	path, err := windows.UTF16PtrFromString(pipeName)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		if err == windows.ERROR_FILE_NOT_FOUND {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return &pipeConn{f: os.NewFile(uintptr(h), pipeName)}, nil
}
