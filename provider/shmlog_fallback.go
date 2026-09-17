//go:build !linux

package provider

import (
	"fmt"
	"os"
)

const (
	shmLogPath    = "/dev/shm/urnetwork.log"
	shmLogMaxSize = 1024 * 1024 // 1MB
)

func initSHMLogger() {
	// No-op for non-Linux platforms
}

// shmLogFatal writes a FATAL message to stderr and calls os.Exit. This
// intentionally bypasses deferred cleanup because the error is unrecoverable.
func shmLogFatal(code int, format string, args ...any) {
	msg := fmt.Sprintf("FATAL [exit %d]: %s\n", code, fmt.Sprintf(format, args...))
	os.Stderr.Write([]byte(msg))
	os.Exit(code)
}
