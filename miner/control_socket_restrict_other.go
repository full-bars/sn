//go:build !windows

package miner

// restrictSocketACL is a no-op on non-Windows platforms. On Unix, the 0600
// chmod on the socket file combined with SO_PEERCRED verification (Linux and
// FreeBSD) already restricts access to the owning user.
func restrictSocketACL(_ string) error {
	return nil
}
