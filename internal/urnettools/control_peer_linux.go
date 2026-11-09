//go:build linux

package urnettools

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

// SO_PEERCRED identifies the process that created the listener, not a later
// inheritor. HotSwap must bind a fresh control listener for the new process.
func socketPeerPID(conn net.Conn) (int, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) { cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) }); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	if cred == nil || cred.Pid <= 0 {
		return 0, fmt.Errorf("peer pid not reported")
	}
	return int(cred.Pid), nil
}
