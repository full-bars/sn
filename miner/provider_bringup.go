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
// provider slots.
func providerBringUpLine(running, pending, failed int) string {
	return fmt.Sprintf("providers: %d running, %d pending, %d failed", running, pending, failed)
}

// providerBringUpTracker counts the launch list's slots by bring-up state.
// pending slots have not completed authentication; running slots authenticated
// (their device follows immediately); failed slots' workers returned an error
// before they could run. Safe for the concurrent provider workers.
type providerBringUpTracker struct {
	mu       sync.Mutex
	total    int
	running  int
	failed   int
	states   map[uint64]string
	started  bool
	finished bool
	stopCh   chan struct{}
}

func newProviderBringUpTracker(total int) *providerBringUpTracker {
	return &providerBringUpTracker{
		total:  total,
		states: map[uint64]string{},
		stopCh: make(chan struct{}),
	}
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
	go func() {
		ticker := time.NewTicker(providerBringUpHeartbeatInterval)
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
	pending := self.total - self.running - self.failed
	line := providerBringUpLine(self.running, pending, self.failed)
	self.mu.Unlock()
	tlog("%s\n", line)
}

// authReused narrates one slot's reused authentication and moves it to
// running.
func (self *providerBringUpTracker) authReused(index uint64, clientID connect.Id, display string) {
	self.apply(index, "running", func() {
		tlog("[reuse] client_id: %s (provider %d, slot %s)\n", clientID, index, display)
	})
}

// authNew narrates one slot's fresh authentication and moves it to running.
func (self *providerBringUpTracker) authNew(index uint64, clientID connect.Id, display string) {
	self.apply(index, "running", func() {
		tlog("[new] client_id: %s (provider %d, slot %s)\n", clientID, index, display)
	})
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
	line := providerBringUpLine(self.running, pending, self.failed)
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
