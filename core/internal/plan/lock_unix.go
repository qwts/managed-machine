//go:build unix

package plan

import (
	"errors"
	"os"
	"syscall"
)

// lockFile is an advisory exclusive lock that the OS releases when the
// holding process ends, however it ends.
type lockFile struct{ f *os.File }

func acquire(path string) (*lockFile, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &lockFile{f}, nil
}

func (l *lockFile) release() error {
	syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
