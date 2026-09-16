//go:build linux && !arm64

package provider

import "syscall"

func dup2(oldfd, newfd int) error {
	return syscall.Dup2(oldfd, newfd)
}
