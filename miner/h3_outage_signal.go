package miner

import (
	"sync"
	"time"
)

// h3_outage_signal.go is the evidence the H3 shed (h3_outage_shed.go) acts on.
//
// The shed used to read the reaper's detector (proxy_outage_freeze.go), and that
// detector is built for a different decision. It guards blacklisting, which is
// destructive and lasts a day, so it waits five minutes and clears on one success
// anywhere. Shedding H3 is the opposite: it is cheap and reversible, and each
// minute of an outage on an `h3 all` miner costs a full QUIC retransmit ladder per
// proxy. So the shed gets its own, quicker signal, and it asks more of a recovery
// than a single success.
//
// The signal keeps one-second buckets of authentication outcomes. It reports an
// outage when the window holds enough failures and no success at all, and a
// recovery only when the window holds several successes and most attempts
// succeed. A platform that answers one sign-in in a hundred is neither.

const (
	// h3OutageShedWindow is how far back the outage reading looks, and also how
	// long the signal must have been watching before it may report an outage, so a
	// start-up stampede of slow first sign-ins is not read as one.
	h3OutageShedWindow = 90 * time.Second
	// h3OutageShedMinFailures is the failures, with no success in the window, that
	// make an outage. A handful of dead proxies on a pool that cannot authenticate
	// at all is the pool's problem; this many with nothing succeeding is the
	// platform's.
	h3OutageShedMinFailures = 12

	// h3OutageRestoreWindow is how far back the recovery reading looks.
	h3OutageRestoreWindow = 2 * time.Minute
	// h3OutageRestoreMinSuccesses is the successes a recovery needs in that
	// window, so one lucky sign-in is not a recovery.
	h3OutageRestoreMinSuccesses = 5
	// h3OutageRestoreMinSharePercent is the share of attempts that must have
	// succeeded in that window. A pool with a long dead tail never reaches the
	// share of a healthy one, which is why it is a majority and not a high bar.
	h3OutageRestoreMinSharePercent = 50

	// h3OutageSignalSeconds is the length of the bucket ring: the longest window.
	h3OutageSignalSeconds = 120
)

type h3OutageBucket struct {
	second int64
	ok     int
	failed int
}

// h3OutageSignal counts authentication outcomes per second over the last two
// minutes. Its cost does not depend on how many proxies the miner runs.
type h3OutageSignal struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time
	buckets [h3OutageSignalSeconds]h3OutageBucket
}

func newH3OutageSignal(now func() time.Time) *h3OutageSignal {
	return &h3OutageSignal{now: now, started: now()}
}

// h3OutageEvidence is the process-wide instance, fed beside the reaper's
// detector by the per-slot auth accounting.
var h3OutageEvidence = newH3OutageSignal(time.Now)

func (s *h3OutageSignal) record(ok bool) {
	second := s.now().Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	b := &s.buckets[int(second%h3OutageSignalSeconds)]
	if b.second != second {
		*b = h3OutageBucket{second: second}
	}
	if ok {
		b.ok++
	} else {
		b.failed++
	}
}

func (s *h3OutageSignal) success() { s.record(true) }
func (s *h3OutageSignal) failure() { s.record(false) }

// counts is the outcomes in the last window ending at now.
func (s *h3OutageSignal) countsLocked(now time.Time, window time.Duration) (ok, failed int) {
	end := now.Unix()
	start := end - int64(window/time.Second)
	for i := range s.buckets {
		b := s.buckets[i]
		if b.second > start && b.second <= end {
			ok += b.ok
			failed += b.failed
		}
	}
	return ok, failed
}

// outage reports whether the platform looks unreachable: enough failures in the
// window and not one success, from a signal that has been watching long enough to
// have seen a success if there were one.
func (s *h3OutageSignal) outage(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.Sub(s.started) < h3OutageShedWindow {
		return false
	}
	ok, failed := s.countsLocked(now, h3OutageShedWindow)
	return ok == 0 && h3OutageShedMinFailures <= failed
}

// recovered reports whether the platform is answering again: several successes in
// the window and most attempts among them succeeding.
func (s *h3OutageSignal) recovered(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	ok, failed := s.countsLocked(now, h3OutageRestoreWindow)
	return h3OutageRestoreMinSuccesses <= ok && h3OutageRestoreMinSharePercent*(ok+failed) <= 100*ok
}
