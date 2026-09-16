package provider

// Stubs for symbols referenced by downstream files that are not yet
// present in the v2026 target workspace.

import (
	"time"
)

// verifyPeerCredentials is a no-op on non-Linux platforms.
// On Linux the real implementation lives in peercred_linux.go.

// atomicBool is a simple atomic boolean used by proxyWarmupDone.
type atomicBool struct{ v int32 }

func (a *atomicBool) Load() bool  { return a.v != 0 }
func (a *atomicBool) Store(b bool) {
	if b {
		a.v = 1
	} else {
		a.v = 0
	}
}

// backoffPacerWithDelay returns true after the delay elapses or ctx is done.
func backoffPacerWithDelay(baseDelay, extraDelay time.Duration, ctx interface{ Done() <-chan struct{} }) bool {
	select {
	case <-time.After(baseDelay + extraDelay):
		return true
	case <-ctx.Done():
		return false
	}
}

// DefaultConnectUrl is the default WebSocket connect endpoint.
