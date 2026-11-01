//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

func ownerPeerID() int { return os.Getuid() }

func verifyDirOwner(path string, info os.FileInfo) error {
	uid := os.Getuid()
	if uid < 0 {
		return nil
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return nil
	}
	if int(stat.Uid) != uid {
		return fmt.Errorf("refusing foreign-owned daemon directory %s (owner uid %d != %d)", path, stat.Uid, uid)
	}
	return nil
}
