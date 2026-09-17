//go:build windows

package provider

// setUmask is a no-op on Windows. Windows does not have a process-wide
// umask; file permissions are controlled via DACLs (see
// restrict_socket_windows.go).
func setUmask(_ int) int {
	return 0
}
