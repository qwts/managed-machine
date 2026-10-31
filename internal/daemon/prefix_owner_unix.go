//go:build !windows

package daemon

import (
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// PrefixOwner returns the username that owns prefix, or "" when prefix is
// empty or absent.
func PrefixOwner(prefix string) string {
	if prefix == "" {
		return ""
	}
	fi, err := os.Stat(prefix)
	if err != nil {
		return ""
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil || u.Username == "" {
		return strconv.FormatUint(uint64(st.Uid), 10)
	}
	return u.Username
}
