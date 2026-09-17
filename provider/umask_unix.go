//go:build !windows

package provider

import "syscall"

// setUmask sets the process umask and returns the previous value.
// Used to create the control socket with restrictive permissions
// without a TOCTOU race. Only meaningful on Unix.
func setUmask(mask int) int {
	return syscall.Umask(mask)
}
