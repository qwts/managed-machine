//go:build !windows

package daemon

import (
	"errors"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// PrefixOwner returns the username that owns prefix, or ("", nil) when prefix
// is empty or does not exist. Any other stat failure is returned as an error
// so a non-owner denied traversal cannot bypass the prefix-owner check.
func PrefixOwner(prefix string) (string, error) {
	if prefix == "" {
		return "", nil
	}
	fi, err := os.Stat(prefix)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("prefix owner could not be determined")
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil || u.Username == "" {
		return strconv.FormatUint(uint64(st.Uid), 10), nil
	}
	return u.Username, nil
}
