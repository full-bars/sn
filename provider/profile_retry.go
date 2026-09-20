package provider

import (
	"context"
	"errors"
	"syscall"
	"time"
)

// enableProfilingWithRetry keeps trying to bind the diagnostics address until
// it is free. A HotSwap candidate starts while its parent still holds the
// loopback pprof port, and the parent keeps it for the whole stream drain
// (up to 30s) until it exits. Trying once, as the initial startup path does,
// left the promoted process without pprof and /metrics/pool for the rest of
// its life, and only on every other swap: a process that inherited pprof from
// a parent that had it lost it, one whose parent had none kept it.
//
// Only "address in use" is retried; any other failure (bad or non-loopback
// address) will not fix itself and is reported once.
func enableProfilingWithRetry(
	ctx context.Context,
	addr string,
	enable func(string) error,
	wait, poll time.Duration,
	logf func(format string, args ...any),
) {
	deadline := time.Now().Add(wait)
	for {
		err := enable(addr)
		if err == nil {
			logf("[profile] diagnostics enabled on %s (hotswap parent released it)\n", addr)
			return
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			logf("[profile] failed to enable diagnostics: %v\n", err)
			return
		}
		if !time.Now().Before(deadline) {
			logf("[profile] gave up enabling diagnostics on %s after %s: %v\n", addr, wait, err)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
	}
}
