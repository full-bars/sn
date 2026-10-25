// The provide run narrates its own bring-up at default verbosity: one line per
// slot's authentication outcome (reused, new, failed with reason and retry)
// and, while any slot is still pending, a periodic
// "providers: R running, P pending, F failed" heartbeat. An operator watching
// a 1000-slot start sees who is up, who is still waiting, and who fell out,
// instead of inferring it from a silent stretch between the [reuse]/[new]
// stream.
package miner

import (
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// proxyControlEvidence is what this process can say about its proxies from
// its own control-plane dials. It replaces a monotonic "reached" set: a count
// that can only rise answers "did this proxy ever get through", not "is it up",
// and the health line, the [profit] line and the [health][proxies] line all
// need the second answer.
//
// live counts, per proxy identity, the provider slots that are holding
// control-plane evidence right now: a slot adds its identity when its first
// dial through its proxy succeeds and removes it when the slot exits or its
// credential is rejected (controlEvidence.lost). It is a count rather than a
// flag because two slot indexes can serve the same proxy at once (a reload
// hands the proxy a new index while the old slot drains), and the old slot
// exiting must not take the new one's evidence with it.
//
// ever is the monotonic set the previous version reported, kept because it is
// a cheap answer to a different question (has this process ever proven this
// proxy) and printed only as reached_ever, never as up.
//
// Identities are connect.ProxySettings.Key(), the key proxy.state and the live
// health registry use, so a pool line can match evidence to members one by
// one. The direct slot has no proxy identity and is not counted: every surface
// that reads this reports proxies.
//
// Why any of this exists: proxy.state carries each proxy's persisted grade, so
// a process that has not completed a single dial can still render a few
// hundred "up" proxies from a previous run, which reads as a healthy pool at
// uptime=0s. That persisted figure survives only as last_known_up.
var proxyControlEvidence = struct {
	sync.Mutex
	live map[string]int
	ever map[string]struct{}
}{live: map[string]int{}, ever: map[string]struct{}{}}

// controlEvidence is one slot's hold on its proxy's evidence. It is created per
// slot launch, so it is also per slot index and per relaunch of an index.
//
// closed makes the slot's evidence one-way: a dial callback that lands after
// the slot exited (the SDK's goroutines can outlive the slot's return) must not
// re-add an identity nothing will ever remove again.
type controlEvidence struct {
	key    string
	mu     sync.Mutex
	held   bool
	closed bool
}

// newControlEvidence returns the hold for one slot launch. An empty key (the
// direct slot) yields a hold whose reached and lost do nothing.
func newControlEvidence(key string) *controlEvidence {
	return &controlEvidence{key: key}
}

// reached records the slot's first successful control-plane dial. Repeated
// calls, and calls after lost, change nothing.
func (self *controlEvidence) reached() {
	if self == nil || self.key == "" {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.held || self.closed {
		return
	}
	self.held = true
	// Lock order is always the slot's mu, then the process set's.
	proxyControlEvidence.Lock()
	proxyControlEvidence.live[self.key]++
	proxyControlEvidence.ever[self.key] = struct{}{}
	proxyControlEvidence.Unlock()
}

// lost withdraws the slot's evidence: the slot exited or its credential was
// rejected. Idempotent, and it only ever removes what this slot added, so the
// count cannot go negative and cannot exceed the slots that are holding it.
func (self *controlEvidence) lost() {
	if self == nil || self.key == "" {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	self.closed = true
	if !self.held {
		return
	}
	self.held = false
	proxyControlEvidence.Lock()
	if n := proxyControlEvidence.live[self.key] - 1; n > 0 {
		proxyControlEvidence.live[self.key] = n
	} else {
		delete(proxyControlEvidence.live, self.key)
	}
	proxyControlEvidence.Unlock()
}

// proxyControlEvidenceKeys is the raw evidence: every proxy at least one live
// slot holds control-plane evidence for. Callers apply proxyHonestlyUp.
func proxyControlEvidenceKeys() map[string]struct{} {
	proxyControlEvidence.Lock()
	defer proxyControlEvidence.Unlock()
	keys := make(map[string]struct{}, len(proxyControlEvidence.live))
	for key := range proxyControlEvidence.live {
		keys[key] = struct{}{}
	}
	return keys
}

// proxyHonestlyUp is the rule every "up" applies to one proxy: a live slot
// holds control-plane evidence for it (evidenced), AND its in-process health
// bucket (proxyPoolMemberBucket) is not degraded or dead, because
// proxy_transport_health.go can watch a transport drop without the slot
// exiting. A persisted grade can never make a proxy up on its own: without a
// dial in this process there is no evidence.
func proxyHonestlyUp(evidenced bool, bucket string) bool {
	if !evidenced {
		return false
	}
	switch bucket {
	case proxyPoolHealthDegraded, proxyPoolHealthDead:
		return false
	}
	return true
}

// honestProxyUpKeys is the one definition of "up" the health line, the [profit]
// line and the [health][proxies] line report: the proxies this process can
// vouch for right now (proxyHonestlyUp over the live health registry). The pool
// line applies the same rule to its own single registry read
// (honestProxyPoolMembers) so a scrape still reads the registry once.
func honestProxyUpKeys() map[string]struct{} {
	keys := proxyControlEvidenceKeys()
	if len(keys) == 0 {
		return keys
	}
	// Read the registry outside the evidence lock: the two locks are never
	// held together from this side.
	for key, status := range liveProxyHealthByKey() {
		if _, ok := keys[key]; ok && !proxyHonestlyUp(true, proxyPoolMemberBucket(status.Health)) {
			delete(keys, key)
		}
	}
	return keys
}

// honestProxiesUp is len(honestProxyUpKeys()), the count every "up" field
// prints.
func honestProxiesUp() int {
	return len(honestProxyUpKeys())
}

// proxiesReachedEver is the monotonic count: proxies that reached the control
// plane at least once in this process, whether or not they still do. It is
// printed as reached_ever and must never be reported as up.
func proxiesReachedEver() int {
	proxyControlEvidence.Lock()
	defer proxyControlEvidence.Unlock()
	return len(proxyControlEvidence.ever)
}

// resetControlReached exists for tests: the evidence is process-wide on purpose
// (the health, profit and pool lines have no handle on the slots), so a test
// that asserts a fresh value has to start from empty.
func resetControlReached() {
	proxyControlEvidence.Lock()
	defer proxyControlEvidence.Unlock()
	proxyControlEvidence.live = map[string]int{}
	proxyControlEvidence.ever = map[string]struct{}{}
}

// providerBringUpHeartbeatInterval is the heartbeat cadence while slots are
// pending. A variable so tests can shorten it.
var providerBringUpHeartbeatInterval = 10 * time.Second

// providerSlotDisplay names a provider slot for operator lines: the direct
// worker or the proxy address, never credentials.
func providerSlotDisplay(settings *connect.ProxySettings) string {
	if settings == nil {
		return "direct"
	}
	return "proxy:" + settings.Address
}

// providerBringUpLine is the heartbeat's line: the live tally of the run's
// provider slots, and how many of the authenticated ones kept their client id
// across the restart (reused) versus were minted (new), and how many have
// completed a dial to the control plane. A reused id alone proves only that a
// stored login was found; the reached count is the evidence that the slot is
// talking to the platform. The counts trail the
// stable "N running, N pending, N failed" prefix so existing readers of it
// still match.
func providerBringUpLine(running, pending, failed, reused, minted, reached int) string {
	return fmt.Sprintf("providers: %d running, %d pending, %d failed (client ids: %d reused, %d new; control plane reached: %d)", running, pending, failed, reused, minted, reached)
}

// providerBringUpTracker counts the launch list's slots by bring-up state.
// pending slots have not completed authentication; running slots authenticated
// (their device follows immediately); failed slots' workers returned an error
// before they could run. Safe for the concurrent provider workers.
type providerBringUpTracker struct {
	mu      sync.Mutex
	total   int
	running int
	failed  int
	states  map[uint64]string
	// identities is each authenticated slot's latest outcome: true when the
	// slot kept its client id, false when it was minted.
	identities map[uint64]bool
	// reached is the slots whose first dial through their proxy succeeded.
	reached map[uint64]bool
	// watches holds each authenticated slot's pending "never reached" check.
	watches map[uint64]*time.Timer
	// closed is set at shutdown, so a late watch stays silent. stop() alone is
	// also called when the last slot resolves, and the watches must outlive
	// that: they exist to report the slots that authenticated and then never
	// reached the control plane.
	closed   bool
	started  bool
	finished bool
	stopCh   chan struct{}
}

func newProviderBringUpTracker(total int) *providerBringUpTracker {
	return &providerBringUpTracker{
		total:      total,
		states:     map[uint64]string{},
		identities: map[uint64]bool{},
		reached:    map[uint64]bool{},
		watches:    map[uint64]*time.Timer{},
		stopCh:     make(chan struct{}),
	}
}

// identityCounts returns how many authenticated slots reused their client id
// and how many were minted. The caller must hold mu.
func (self *providerBringUpTracker) identityCounts() (reused, minted int) {
	for _, wasReused := range self.identities {
		if wasReused {
			reused++
		} else {
			minted++
		}
	}
	return reused, minted
}

// start emits the opening heartbeat and begins the periodic one. It is a no-op
// when every slot is already resolved, and idempotent.
func (self *providerBringUpTracker) start() {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.started {
		self.mu.Unlock()
		return
	}
	self.started = true
	pending := self.total - self.running - self.failed
	self.mu.Unlock()
	self.emit()
	if pending <= 0 {
		return
	}
	// Read the interval here, not inside the goroutine: the variable is a test
	// seam, and a goroutine that starts late would read it while the test that
	// shortened it is restoring it.
	interval := providerBringUpHeartbeatInterval
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-self.stopCh:
				return
			case <-ticker.C:
				self.mu.Lock()
				pending := self.total - self.running - self.failed
				resolved := pending <= 0
				self.mu.Unlock()
				if resolved {
					// The resolving transition already emitted the final
					// tally; nothing is pending, so nothing left to report.
					return
				}
				self.emit()
			}
		}
	}()
}

// stop ends the heartbeat goroutine. Safe to call more than once.
func (self *providerBringUpTracker) stop() {
	if self == nil {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	select {
	case <-self.stopCh:
	default:
		close(self.stopCh)
	}
}

// emit writes the current tally. The caller must not hold mu.
func (self *providerBringUpTracker) emit() {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.closed {
		self.mu.Unlock()
		return
	}
	pending := self.total - self.running - self.failed
	reused, minted := self.identityCounts()
	line := providerBringUpLine(self.running, pending, self.failed, reused, minted, len(self.reached))
	self.mu.Unlock()
	tlog("%s\n", line)
}

// authReused narrates one slot's reused authentication and moves it to
// running. The leading text matches the line the v3.23 provider and
// docs/Hot-Restart.md document, so a grep for "(reused)" finds every slot that
// kept its identity; the slot follows it.
func (self *providerBringUpTracker) authReused(index uint64, clientID connect.Id, display string) {
	self.apply(index, "running", func() {
		self.identities[index] = true
		self.watchControl(index, display)
		tlog("♻️ client_id: %s (reused) provider=%d slot=%s\n", clientID, index, display)
	})
}

// authNew narrates one slot's fresh authentication and moves it to running.
func (self *providerBringUpTracker) authNew(index uint64, clientID connect.Id, display string) {
	self.apply(index, "running", func() {
		self.identities[index] = false
		self.watchControl(index, display)
		tlog("✨ client_id: %s (new) provider=%d slot=%s\n", clientID, index, display)
	})
}

// providerControlGrace is how long an authenticated slot may go without a
// successful dial before it is called out. A variable so tests can shorten it.
var providerControlGrace = 60 * time.Second

// shutdown ends the tracker's heartbeat and cancels every pending control
// check. Called when the run ends.
func (self *providerBringUpTracker) shutdown() {
	if self == nil {
		return
	}
	self.mu.Lock()
	self.closed = true
	for index, watch := range self.watches {
		watch.Stop()
		delete(self.watches, index)
	}
	self.mu.Unlock()
	self.stop()
}

// watchControl starts the slot's "never reached the control plane" check. The
// caller holds mu.
func (self *providerBringUpTracker) watchControl(index uint64, display string) {
	if self.reached[index] {
		return
	}
	if previous := self.watches[index]; previous != nil {
		previous.Stop()
	}
	self.watches[index] = time.AfterFunc(providerControlGrace, func() {
		self.mu.Lock()
		reached, closed := self.reached[index], self.closed
		self.mu.Unlock()
		if !reached && !closed {
			tlog("⚠️ [control] provider %d (%s) authenticated but has not reached the control plane after %s\n", index, display, formatDuration(providerControlGrace))
		}
	})
}

// controlReached records a slot's first successful dial through its proxy and
// narrates it once. When the final tally was already emitted, it emits an
// updated tally with the reached slot included.
func (self *providerBringUpTracker) controlReached(index uint64, display string, dur time.Duration, via string) {
	if self == nil {
		return
	}
	self.mu.Lock()
	if self.reached[index] {
		self.mu.Unlock()
		return
	}
	// The health surfaces do not count from here: reached is per index and
	// only ever set, which is right for the bring-up narration and wrong for
	// "up". Their evidence is the slot's controlEvidence, which run.go
	// withdraws when the slot exits or loses its credential.
	self.reached[index] = true
	if watch := self.watches[index]; watch != nil {
		watch.Stop()
		delete(self.watches, index)
	}
	shouldEmit := self.finished
	self.mu.Unlock()
	tlog("📡 [control] provider %d (%s) reached the control plane via %s in %dms\n", index, display, via, dur.Milliseconds())
	if shouldEmit {
		self.emit()
	}
}

// authFailed narrates one slot's failed authentication, with the reason and
// what happens next (retry), and moves it to failed.
func (self *providerBringUpTracker) authFailed(index uint64, display string, reason error, retry string) {
	self.apply(index, "failed", func() {
		tlog("[auth] provider %d failed: %v (slot %s, retry %s)\n", index, reason, display, retry)
	})
}

// workerFailed records a worker that ended with an error after authentication
// (or before any auth line); the tally moves without a duplicate line.
func (self *providerBringUpTracker) workerFailed(index uint64) {
	self.apply(index, "failed", nil)
}

// apply updates one slot under the lock, prints its line (when any), and emits
// the final tally exactly once when the last pending slot resolves.
func (self *providerBringUpTracker) apply(index uint64, state string, print func()) {
	if self == nil {
		return
	}
	self.mu.Lock()
	current, ok := self.states[index]
	if !ok {
		current = "pending"
	}
	if current != state {
		switch current {
		case "running":
			self.running--
		case "failed":
			self.failed--
		}
		switch state {
		case "running":
			self.running++
		case "failed":
			self.failed++
		}
		self.states[index] = state
	}
	if print != nil {
		print()
	}
	pending := self.total - self.running - self.failed
	final := pending <= 0 && !self.finished
	if final {
		self.finished = true
	}
	reused, minted := self.identityCounts()
	line := providerBringUpLine(self.running, pending, self.failed, reused, minted, len(self.reached))
	self.mu.Unlock()
	if final {
		tlog("%s\n", line)
		self.stop()
	}
}

// providerAuthGiveUpInfo is what a failed authentication recorded for the
// per-slot line: whether the identity gave up, whether it was evicted, and
// when it is eligible again.
type providerAuthGiveUpInfo struct {
	giveUp  int
	retryIn time.Duration
	evicted bool
}

// providerAuthRetryToken renders the retry half of a failed-auth line.
func providerAuthRetryToken(info providerAuthGiveUpInfo) string {
	switch {
	case info.evicted:
		return "never (evicted from the URL cache)"
	case 0 < info.retryIn:
		return "in " + formatDuration(info.retryIn)
	default:
		return "on the next start"
	}
}
