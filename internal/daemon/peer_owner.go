//go:build !windows

package daemon

import (
	"os"
	"os/user"
	"strconv"
)

func ownerPeerID() int { return os.Getuid() }

// extraPeerIDs returns any additional local owner account UIDs allowed to dial
// the shared prefix socket on two-account hosts (where `admin` runs
// agent-machine and `user` runs agent-daemon). Root and agent accounts are
// never included.
func extraPeerIDs() []int {
	var out []int
	for _, name := range []string{"user", "admin"} {
		if agentAccount(name) {
			continue
		}
		u, err := user.Lookup(name)
		if err != nil {
			continue
		}
		uid, err := strconv.Atoi(u.Uid)
		if err != nil || uid <= 0 || uid == os.Getuid() {
			continue
		}
		out = append(out, uid)
	}
	return out
}
