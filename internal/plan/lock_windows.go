//go:build windows

package plan

import (
	"errors"
	"syscall"
)

// lockFile holds the lock file open with no sharing, which Windows releases
// when the holding process ends, however it ends.
type lockFile struct{ h syscall.Handle }

const errSharingViolation syscall.Errno = 32

func acquire(path string) (*lockFile, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE, 0, nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return &lockFile{h}, nil
}

func (l *lockFile) release() error { return syscall.CloseHandle(l.h) }
