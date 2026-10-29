package miner

import (
	"testing"
	"time"
)

// The shadow sweep logs the tracker id, which for a proxied slot is the raw
// identity key: address, separator, user. Drive the real sweep and read the
// output it writes.
func TestProxyLegShadowSweepDoesNotPrintTheRawIdentityKey(t *testing.T) {
	minerProxyLeg = proxyLegTracker{}
	t.Cleanup(func() { minerProxyLeg = proxyLegTracker{} })
	previous := lastPlatformDialOK.Load()
	t.Cleanup(func() { lastPlatformDialOK.Store(previous) })

	key := sinkTestSettings().Key()
	now := time.Now()
	// the streak must also span the window, so the last failure sits at its end
	start := now.Add(-proxyLegShedMinAge - time.Minute)
	for i := 0; i < proxyLegShedStreak-1; i++ {
		minerProxyLeg.RecordFailure(key, start.Add(time.Duration(i)*time.Second))
	}
	minerProxyLeg.RecordFailure(key, start.Add(proxyLegShedMinAge+time.Second))
	lastPlatformDialOK.Store(now.Add(-time.Minute).UnixNano())

	out := captureTlog(t, func() { proxyLegShadowSweep(now) })
	if out == "" {
		t.Fatal("the sweep logged nothing, so the sink was not exercised")
	}
	assertNoKeyLeak(t, "proxyLegShadowSweep", out)
}
