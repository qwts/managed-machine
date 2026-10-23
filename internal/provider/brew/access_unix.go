//go:build unix

package brew

import (
	"os"
	"syscall"
)

// writable asks the kernel whether the current user may write the prefix
// and its Cellar, without writing anything.
func writable(prefix string) bool {
	for _, p := range []string{prefix, prefix + "/Cellar"} {
		if _, err := os.Stat(p); err != nil {
			if p == prefix {
				return false
			}
			continue
		}
		if syscall.Access(p, 2) != nil { // W_OK
			return false
		}
	}
	return true
}

func owned(prefix string) bool {
	fi, err := os.Stat(prefix)
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
