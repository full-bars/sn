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
// the miner sheds them: while the pool-wide auth outage detector says the
// platform is unreachable, no proxied identity is eligible for the fallback
// transports. The relaunch pass that already follows eligibility then restarts
// the affected identities a few at a time and never one with a client, and H1,
// the health signal, keeps probing the platform. The direct identity keeps its
// H3: it is one identity, and not what the shed is about.
//
// The detector clears the moment a single auth succeeds anywhere, and a backend
// that is coming back flaps. Restoring on every flap would relaunch the whole
// pool each time and hand the first silent minute straight back to the carriers,
// so the shed only lets go once the detector has stayed clear for a hold.

const (
	// h3OutageShedPoll is how often the detector is read.
	h3OutageShedPoll = 15 * time.Second
	// h3OutageShedClearHold is how long the detector must stay clear before
	// proxied identities get the fallback transports back.
	h3OutageShedClearHold = 5 * time.Minute
)

// h3OutageShedding is whether proxied identities are currently shed. It is read
// by every eligibility check, so it is a lone atomic.
var h3OutageShedding atomic.Bool

// h3OutageShedder turns the detector's readings into the shed state.
type h3OutageShedder struct {
	mu        sync.Mutex
	calmSince time.Time
}

// observe feeds one reading of the detector and reports the shed state after it
// and whether that state changed.
func (s *h3OutageShedder) observe(now time.Time, frozen bool) (shed bool, changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	was := h3OutageShedding.Load()
	switch {
	case frozen:
		s.calmSince = time.Time{}
		h3OutageShedding.Store(true)
	case !was:
		return false, false
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

// runH3OutageShed polls the pool-wide auth outage detector until ctx ends,
// shedding proxied H3 while the platform is unreachable and relaunching the
// identities whose eligibility changed when the state flips.
func runH3OutageShed(ctx context.Context, shedder *h3OutageShedder) {
	ticker := time.NewTicker(h3OutageShedPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			shed, changed := shedder.observe(now, proxyAuthOutageFrozen())
			if !changed {
				continue
			}
			if shed {
				importantLogf("[h3] platform unreachable: proxied identities drop H3 until it has answered for %s (H1 keeps probing)\n", h3OutageShedClearHold)
			} else {
				importantLogf("[h3] platform answering again: proxied identities get H3 back\n")
			}
			relaunchH3ProxiesNow(resolveH3Mode(globalControlState, currentProxyResolution()))
		}
	}
}
