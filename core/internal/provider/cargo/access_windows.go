//go:build windows

package cargo

import (
	"os"
	"syscall"
)

// writable asks the kernel whether the current user may write the path,
// without writing anything: opening a directory with write access and
// closing it again changes nothing on disk.
func writable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil, syscall.OPEN_EXISTING, syscall.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return false
	}
	syscall.CloseHandle(h)
	return true
}
