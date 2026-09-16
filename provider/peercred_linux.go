//go:build linux

package provider

import (
	"fmt"
	"net"
	"os"
	"syscall"
)

// peerAllowed reports whether a peer UID may use the control socket: the
// provider's own UID or root. verifyPeerCredentials delegates here so the
// decision is testable without connecting as another user.
func peerAllowed(peerUID, providerUID uint32) bool {
	return peerUID == providerUID || peerUID == 0
}

// verifyPeerCredentials checks that the connecting process has the same
// UID as the provider, or is root (uid 0). Root can always manage any
// provider (it already has filesystem access to the state dir and the
// ability to signal the process). This prevents unprivileged users on the
// system from sending commands to the control socket while allowing
// root (systemd, cron, fleet scripts) to manage the provider.
func verifyPeerCredentials(conn *net.UnixConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("peer cred: %w", err)
	}

	var ucred *syscall.Ucred
	var credErr error

	err = raw.Control(func(fd uintptr) {
		ucred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return fmt.Errorf("peer cred control: %w", err)
	}
	if credErr != nil {
		return fmt.Errorf("peer cred: %w", credErr)
	}

	providerUID := uint32(os.Getuid())
	if !peerAllowed(ucred.Uid, providerUID) {
		return fmt.Errorf("peer cred: uid %d is not the provider uid %d or root", ucred.Uid, providerUID)
	}
	return nil
}
