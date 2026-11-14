//go:build !linux && !freebsd && !openbsd && !darwin

package miner

func protectLogPipes() {}
