//go:build windows

package daemon

import (
	"fmt"
	"net"

	"golang.org/x/sys/windows"
)

// ownerPeerID is the peer value for a client whose token user matches this
// process. os.Getuid is -1 for every Windows caller, so it cannot tell them
// apart.
func ownerPeerID() int { return 0 }

func extraPeerIDs() []int { return nil }

// peerCred reports 0 when the named-pipe client's process token user is the
// daemon's user, and -1 otherwise. The pipe DACL is the first restriction.
// The signature check still runs after this.
func peerCred(conn net.Conn) (int, error) {
	same, err := pipeClientIsOwner(conn)
	if err != nil {
		return -1, err
	}
	if !same {
		return -1, nil
	}
	return 0, nil
}

func pipeClientIsOwner(conn net.Conn) (bool, error) {
	pc, ok := conn.(*pipeConn)
	if !ok || pc == nil || pc.f == nil {
		return false, fmt.Errorf("windows peer is not a named pipe")
	}
	var pid uint32
	if err := windows.GetNamedPipeClientProcessId(windows.Handle(pc.f.Fd()), &pid); err != nil {
		return false, err
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return false, err
	}
	defer token.Close()
	client, err := tokenUserSID(token)
	if err != nil {
		return false, err
	}
	self, err := tokenUserSID(windows.GetCurrentProcessToken())
	if err != nil {
		return false, err
	}
	return client != "" && client == self, nil
}

func tokenUserSID(token windows.Token) (string, error) {
	user, err := token.GetTokenUser()
	if err != nil {
		return "", err
	}
	if user.User.Sid == nil {
		return "", fmt.Errorf("token has no user SID")
	}
	return user.User.Sid.String(), nil
}
