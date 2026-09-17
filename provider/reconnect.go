package provider

import (
	"context"
	"math/rand"
	"time"
)

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

// backoffPacer is the legacy v3.23 wrapper kept for test compatibility.
// It converts integer n and staggerMs into the baseDelay/slotDuration
// shape that backoffPacerWithDelay expects.
func backoffPacer(n int, staggerMs int, now time.Time, proxyCtx context.Context) bool {
	if staggerMs <= 0 {
		return true
	}
	baseDelay := time.Duration(n) * time.Duration(staggerMs) * time.Millisecond
	return backoffPacerWithDelay(baseDelay, time.Duration(staggerMs)*time.Millisecond, proxyCtx)
}
