package provider

// DESIGN ADAPTATION: backoffPacerWithDelay was in fork's main.go:58.
// Real implementation ported from fork. atomicBool kept for proxyWarmupDone.

import (
	"context"
	"math/rand"
	"time"
)

// atomicBool is a simple atomic boolean used by proxyWarmupDone.
type atomicBool struct{ v int32 }

func (a *atomicBool) Load() bool { return a.v != 0 }
func (a *atomicBool) Store(b bool) {
	if b {
		a.v = 1
	} else {
		a.v = 0
	}
}

// backoffPacerWithDelay sleeps for baseDelay ± jitter and returns true,
// or returns false if the context is cancelled. Real implementation
// ported from fork main.go:58.
func backoffPacerWithDelay(baseDelay time.Duration, slotDuration time.Duration, ctx context.Context) bool {
	if baseDelay <= 0 && slotDuration <= 0 {
		return true
	}

	var jitter time.Duration
	if slotDuration > 0 {
		halfSlotMs := int(slotDuration.Milliseconds()) / 2
		if halfSlotMs > 0 {
			jMs := rand.Intn(halfSlotMs + 1)
			if rand.Intn(2) == 0 {
				jMs = -jMs
			}
			jitter = time.Duration(jMs) * time.Millisecond
		}
	}

	wait := baseDelay + jitter
	if wait < 0 {
		wait = 0
	}

	select {
	case <-ctx.Done():
		return false
	case <-time.After(wait):
	}
	return true
}
