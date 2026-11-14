//go:build linux || freebsd || openbsd || darwin

package miner

import (
	"os/signal"
	"syscall"
)

func protectLogPipes() {
	signal.Ignore(syscall.SIGPIPE)
}
