//go:build windows

package daemon

import (
	"net"
	"os"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const pipeName = `\\.\pipe\agent-machine`

// Listen opens a named pipe that only the creating user can connect to.
// Remote clients are rejected. The directory still holds the key files.
func Listen(dir string) (net.Listener, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &pipeListener{name: pipeName}, nil
}

type pipeListener struct {
	name   string
	mu     sync.Mutex
	closed bool
}

func (l *pipeListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	closed := l.closed
	l.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	path, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return nil, err
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;OW)")
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	h, err := windows.CreateNamedPipe(path,
		windows.PIPE_ACCESS_DUPLEX,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES,
		64<<10, 64<<10, 0, sa)
	if err != nil {
		return nil, err
	}
	err = windows.ConnectNamedPipe(h, nil)
	if err != nil && err != windows.ERROR_PIPE_CONNECTED {
		windows.CloseHandle(h)
		return nil, err
	}
	return &pipeConn{f: os.NewFile(uintptr(h), l.name)}, nil
}

func (l *pipeListener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	path, err := windows.UTF16PtrFromString(l.name)
	if err != nil {
		return nil
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ|windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, 0, 0)
	if err == nil {
		windows.CloseHandle(h)
	}
	return nil
}

func (l *pipeListener) Addr() net.Addr { return pipeAddr(l.name) }

type pipeAddr string

func (a pipeAddr) Network() string { return "pipe" }
func (a pipeAddr) String() string  { return string(a) }

type pipeConn struct{ f *os.File }

func (c *pipeConn) Read(b []byte) (int, error)  { return c.f.Read(b) }
func (c *pipeConn) Write(b []byte) (int, error) { return c.f.Write(b) }
func (c *pipeConn) Close() error                { return c.f.Close() }
func (c *pipeConn) LocalAddr() net.Addr         { return pipeAddr(c.f.Name()) }
func (c *pipeConn) RemoteAddr() net.Addr        { return pipeAddr(c.f.Name()) }
func (c *pipeConn) SetDeadline(t time.Time) error {
	return c.f.SetDeadline(t)
}
func (c *pipeConn) SetReadDeadline(t time.Time) error {
	return c.f.SetReadDeadline(t)
}
func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	return c.f.SetWriteDeadline(t)
}
