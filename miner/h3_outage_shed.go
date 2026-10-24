package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// h3_outage_shed.go keeps proxy-relayed H3 off while the platform is dark.
//
// A proxied identity under `h3 all` or `h3 <N>` runs the H3, H3-over-dns and dns
// pump carriers beside its H1. Against an edge that answers nothing, every one
// of those dials runs the whole QUIC PTO ladder (a dozen or more padded Initial
// packets, tens of KB) and every carrier of every proxy redials on its own timer
// for as long as the outage lasts. Measured on a 115-proxy node set to `h3 all`
// in an outage that left the edge silent: a flat 16 Mbit/s of egress with 30
// times more tx than rx, while default nodes, where only the direct identity
// dials H3, spent next to nothing in the same outage.
//
// The pinned full-bars/connect has no reconnect budget to hold those dials, so
// the miner sheds them: while the outage signal says the
// platform is unreachable, no proxied identity is eligible for the fallback
// transports. The relaunch pass that already follows eligibility then restarts
// the affected identities a few at a time and never one with a client, and H1,
// the health signal, keeps probing the platform. The direct identity keeps its
// H3: it is one identity, and not what the shed is about.
//
// What it reads is h3_outage_signal.go: its own quick outage reading and a
// recovery reading that wants several successes and a majority, not the reaper's
// detector, which clears on a single success anywhere. A backend that is coming
// back flaps, and restoring on every flap would relaunch the whole pool each time
// and hand the first silent minute straight back to the carriers, so the shed
// only lets go once the signal has read as recovered for a hold, and then gives
// H3 back slowly.

const (
	// h3OutageShedPoll is how often the signal is read.
	h3OutageShedPoll = 15 * time.Second
	// h3OutageShedClearHold is how long the signal must keep reading as recovered
	// before proxied identities get the fallback transports back. The reading
	// already asks for several successes and a majority over two minutes, so this
	// is on top of that: a backend that is coming back flaps.
	h3OutageShedClearHold = 2 * time.Minute
	// h3OutageRestoreRelaunchInterval is the pause between relaunches when H3 is
	// given back. Dropping H3 is the safe direction and keeps the fast pace;
	// restoring it relaunches every identity just as the platform returns, and
	// this miner has no reconnect budget to meter that, so it is metered here: one
	// identity every three seconds, not three a second.
	h3OutageRestoreRelaunchInterval = 3 * time.Second
)

// h3OutageShedding is whether proxied identities are currently shed. It is read
// by every eligibility check, so it is a lone atomic.
var h3OutageShedding atomic.Bool

// h3OutageShedder turns the signal's readings into the shed state.
type h3OutageShedder struct {
	mu        sync.Mutex
	calmSince time.Time
}

// observe feeds one pair of readings and reports the shed state after it and
// whether that state changed. A reading that is neither an outage nor a recovery
// leaves a shed in place and restarts the hold.
func (s *h3OutageShedder) observe(now time.Time, outage bool, recovered bool) (shed bool, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := h3OutageShedding.Load()
	switch {
	case outage:
		s.calmSince = time.Time{}
		h3OutageShedding.Store(true)
	case !was:
		return false, false
	case !recovered:
		s.calmSince = time.Time{}
	case s.calmSince.IsZero():
		s.calmSince = now
	case h3OutageShedClearHold <= now.Sub(s.calmSince):
		s.calmSince = time.Time{}
		h3OutageShedding.Store(false)
	}
	shed = h3OutageShedding.Load()
	return shed, shed != was
}

// resetH3OutageShed drops the shed state, for the end of a provide run.
func resetH3OutageShed(s *h3OutageShedder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calmSince = time.Time{}
	h3OutageShedding.Store(false)
}

// runH3OutageShed reads the signal until ctx ends, shedding proxied H3 while the
// platform is unreachable and relaunching the identities whose eligibility changed
// when the state flips: at the normal pace when H3 is dropped, and at the slow
// restore pace when it is given back.
func runH3OutageShed(ctx context.Context, shedder *h3OutageShedder, signal *h3OutageSignal) {
	ticker := time.NewTicker(h3OutageShedPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			shed, changed := shedder.observe(now, signal.outage(now), signal.recovered(now))
			if !changed {
				continue
			}
			setting := resolveH3Mode(globalControlState, currentProxyResolution())
			if shed {
				importantLogf("[h3] platform unreachable: proxied identities drop H3 until it has answered again (H1 keeps probing)\n")
				relaunchH3ProxiesNow(setting)
			} else {
				importantLogf("[h3] platform answering again: proxied identities get H3 back, one every %s\n", h3OutageRestoreRelaunchInterval)
				relaunchH3ProxiesNowPaced(setting, h3OutageRestoreRelaunchInterval)
			}
		}
	}
}
