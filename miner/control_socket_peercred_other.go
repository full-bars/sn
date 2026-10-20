//go:build !linux && !freebsd

package miner

import "net"

// verifyPeerCredentials is a no-op on platforms where neither SO_PEERCRED
// nor LOCAL_PEERCRED is available. The socket file permissions (0600) provide
// the equivalent protection on macOS, and Windows uses the platform ACL
// restriction in control_socket_restrict_windows.go.
func verifyPeerCredentials(_ *net.UnixConn) error {
	return nil
}
