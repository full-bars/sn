package miner

// proxy_leg.go: evidence that a proxy cannot carry traffic to the platform.
//
// A proxy can pass its SOCKS login and still be unable to reach the platform:
// the dial through it times out inside the proxy's own connect step. In the
// relay this surfaced only as the flattened "auth error ... http request
// attempts exhausted", which is filed under timeouts, so nothing acted on it
// and status still counted the proxy as authenticated.
//
// The connect library reports the typed result of every control dial to a
// logger that implements connect.ControlDialObserver. The miner has no
// per-proxy logger yet, so this file owns the evidence and the decision; the
// observer is attached where each proxy's client strategy is built.
//
// What acts on it is a SHADOW decision: after a streak of leg failures with no
// dial succeeding through the proxy, while some other path reached the platform
// recently, the proxy is flagged and counted. Nothing is shed. The second
// condition is the outage guard: when the platform itself is down every proxy
// shows the same failure and none of them is to blame.

import (
	"sync"
	"time"
)

const (
	// proxyLegShedStreak is how many leg failures in a row, with no dial
	// succeeding through the proxy, make it a shed candidate.
	proxyLegShedStreak = 5
	// proxyLegShedMinAge is how long the streak must have lasted.
	proxyLegShedMinAge = 10 * time.Minute
	// proxyLegPlatformRecent is how recently another path must have reached
	// the platform for the outage guard to allow a flag.
	proxyLegPlatformRecent = 5 * time.Minute
)

type proxyLegEntry struct {
	streak      int
	streakStart time.Time
	lastSuccess time.Time
	flagged     bool
}

// proxyLegTracker holds the per-proxy leg evidence. The zero value is ready.
type proxyLegTracker struct {
	mu      sync.Mutex
	entries map[string]*proxyLegEntry
}

func (self *proxyLegTracker) entry(id string) *proxyLegEntry {
	if self.entries == nil {
		self.entries = map[string]*proxyLegEntry{}
	}
	entry, ok := self.entries[id]
	if !ok {
		entry = &proxyLegEntry{}
		self.entries[id] = entry
	}
	return entry
}

// RecordFailure counts one control dial that failed inside the proxy's own
// connect step. It never touches auth or timeout counters.
func (self *proxyLegTracker) RecordFailure(id string, now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entry(id)
	if entry.streak == 0 {
		entry.streakStart = now
	}
	entry.streak++
}

// RecordSuccess clears the streak: a platform dial did go through this proxy,
// so it can carry traffic and the evidence is void.
func (self *proxyLegTracker) RecordSuccess(id string, now time.Time) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entry(id)
	entry.streak = 0
	entry.streakStart = time.Time{}
	entry.lastSuccess = now
	entry.flagged = false
}

// Flagged reports whether the shadow decision flags this proxy now, and records
// the flag so a caller can log it once. platformReachedAt is when any path last
// reached the platform; the zero value means nothing has, and the outage guard
// then blocks every flag.
func (self *proxyLegTracker) Flagged(id string, now time.Time, platformReachedAt time.Time) bool {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry := self.entry(id)
	if entry.flagged {
		return false
	}
	if entry.streak < proxyLegShedStreak {
		return false
	}
	if now.Sub(entry.streakStart) < proxyLegShedMinAge {
		return false
	}
	// outage guard: if no path reached the platform recently, the platform is
	// the problem, not this proxy
	if platformReachedAt.IsZero() || proxyLegPlatformRecent < now.Sub(platformReachedAt) {
		return false
	}
	entry.flagged = true
	return true
}

// Totals returns the summed streak and the number of currently flagged proxies,
// which is what the scrape reports. A flagged entry stays counted until a dial
// succeeds through it.
func (self *proxyLegTracker) Totals() (failures int64, flagged int64) {
	self.mu.Lock()
	defer self.mu.Unlock()
	for _, entry := range self.entries {
		failures += int64(entry.streak)
		if entry.flagged {
			flagged++
		}
	}
	return failures, flagged
}

// Ids snapshots the proxies the tracker holds, which is what the shadow sweep
// walks.
func (self *proxyLegTracker) Ids() []string {
	self.mu.Lock()
	defer self.mu.Unlock()
	ids := make([]string, 0, len(self.entries))
	for id := range self.entries {
		ids = append(ids, id)
	}
	return ids
}

// Forget drops a proxy that left the pool, so its streak cannot outlive it.
func (self *proxyLegTracker) Forget(id string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	delete(self.entries, id)
}
