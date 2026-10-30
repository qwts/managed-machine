//go:build !windows

package daemon

import "os"

func ownerPeerID() int { return os.Getuid() }
