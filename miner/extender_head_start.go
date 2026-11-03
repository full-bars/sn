package miner

// extender_head_start.go gives the extender role one quiet activation attempt
// before the proxy slots start.
//
// On a 1 vCPU box carrying ~1400 proxy slots the extender's TLS handshake
// measured 2.0-5.1 s, against 0.15 s on an idle host, and on such a box the
// attempt can fail to complete at all. The proxies are the product, so they are
// held only for a bounded head start: the cap is absolute and independent of
// any answer from the network, and a completed attempt, the cap, a shutdown and
// a launch the activation budget withheld all release the proxies at once. The
// hold is skipped entirely when startup was already slow, because a 3.23-fix to
// sn transition start (the legacy import doing real work, a store that took its
// time to open) is the wrong moment to add a delay. Nothing here retries
// anything: the extender's own role behaviour in the sdk is untouched.
//
// The role itself lives in the pinned sdk, and the miner learns of it only
// through the status listener each slot installs. That listener is constructed
// inside the slot worker (run.go), so the startup gate has no handle on it: the
// newest observation and the direct launch's role verdict are published here,
// at package level, for the hold to read.

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// The cap and the poll are variables so a test can make them short. Production
// values are the two below and nothing in the code changes them.
var (
	// extenderHeadStartCap bounds how long the proxy slots are held. It is
	// absolute: no network answer extends it, and only a completed attempt
	// ends the hold earlier.
	extenderHeadStartCap = 20 * time.Second
	// extenderHeadStartPoll is how often the hold re-reads the observation.
	// A completed attempt also wakes the hold directly, so this is the
	// backstop for a status that changes without an attempt.
	extenderHeadStartPoll = 250 * time.Millisecond
)

// extenderHeadStartSlowStore is the provider store open time past which the
// start is already slow and the hold is skipped.
const extenderHeadStartSlowStore = 10 * time.Second

// extenderHeadStartEnd is why the hold ended.
type extenderHeadStartEnd uint8

const (
	// extenderHeadStartAttempted: one attempt completed, successfully or not.
	extenderHeadStartAttempted extenderHeadStartEnd = iota + 1
	// extenderHeadStartCapped: the cap expired with no attempt.
	extenderHeadStartCapped
	// extenderHeadStartSkippedSlowStartup: startup had already done real
	// work (a legacy import that adopted identities, a slow store open), so
	// no hold was added.
	extenderHeadStartSkippedSlowStartup
	// extenderHeadStartDisabled: there was nothing to hold for (no direct
	// slot, direct transport off, no proxy slots), or the extender role is
	// off for this launch.
	extenderHeadStartDisabled
	// extenderHeadStartShutdown: the run ended while holding.
	extenderHeadStartShutdown
)

// extenderHeadStartResult is what the hold did, for the one log line.
type extenderHeadStartResult struct {
	end         extenderHeadStartEnd
	elapsed     time.Duration
	activatedV4 bool
	activatedV6 bool
	// reason is set when the hold was skipped before it began, so the line
	// says what the start had already done instead of a hold duration.
	reason string
}

// extenderHeadStartSources is what startup already knows when the hold would
// begin. Any of it saying the start is slow, or that no attempt can complete,
// skips the hold rather than shortening it.
type extenderHeadStartSources struct {
	// directSlot is whether the run has a direct slot at all: only that slot
	// can hold the extender role, so no other slot has an attempt to wait
	// for.
	directSlot bool
	// directEnabled is the operator's direct-transport state. A disabled
	// direct slot parks without launching, so no attempt would complete.
	directEnabled bool
	// hotSwapCandidate is a handoff this process exists to complete: it
	// must authenticate at once so the parent can yield.
	hotSwapCandidate bool
	// proxies is how many proxy slots the hold would delay. Nothing to
	// release means nothing to hold.
	proxies int
	// legacyAdopted is how many saved identities the legacy import adopted
	// in this start. A transition start did real work and must not wait.
	legacyAdopted int
	// storeOpen is how long opening the provider store took.
	storeOpen time.Duration
}

// The extender role's own observation, published from the per-slot listener.
var latestExtenderObservation atomic.Pointer[providerExtenderObservation]

// extenderHeadStartWake wakes the hold when something it waits on changed, so a
// completed attempt releases the proxies without waiting out a poll interval.
var extenderHeadStartWake = make(chan struct{}, 1)

// The direct launch's verdict on the extender role. A withheld verdict means
// the activation budget kept the role off, so no attempt will ever complete and
// the hold must end rather than run out its cap.
var extenderHeadStartRole atomic.Int32

const (
	extenderHeadStartRoleUndecided int32 = iota
	extenderHeadStartRoleAllowed
	extenderHeadStartRoleWithheld
)

// extenderHeadStartHeld publishes the deliberate hold for the STATUS= line: the
// proxies are waiting on the extender, not on proxy resolution.
var extenderHeadStartHeld atomic.Bool

// The last real provider store open, so a start whose store was slow is not
// given an extra hold (state_store.go records it).
var providerStoreLastOpenDuration atomic.Int64

// publishProviderExtenderObservation records the newest observation for the
// startup gate. The listener publishes on every callback: the gate reads the
// latest, so a repeated observation costs nothing.
//
// Only the direct slot can hold the extender role, and the hold runs before any
// proxy slot launches, so during the hold the direct slot is the only listener
// publishing: a global "latest" is this slot's observation for as long as it
// matters.
func publishProviderExtenderObservation(observation providerExtenderObservation) {
	latestExtenderObservation.Store(&observation)
	if observation.attempted() {
		wakeExtenderHeadStart()
	}
}

// latestProviderExtenderObservation is the newest observation, or the zero
// value (unknown, disabled, never attempted) before the first callback.
func latestProviderExtenderObservation() providerExtenderObservation {
	if observation := latestExtenderObservation.Load(); observation != nil {
		return *observation
	}
	return providerExtenderObservation{}
}

// attempted reports that one activation attempt has completed. A success sets
// its family flag; a failure leaves the attempt's time. Both are the attempt,
// so either releases the proxies.
func (observation providerExtenderObservation) attempted() bool {
	return observation.ActivationTime != 0 || observation.ActivatedV4 || observation.ActivatedV6
}

// publishExtenderHeadStartRole records the direct launch's extender verdict.
func publishExtenderHeadStartRole(allowed bool) {
	if allowed {
		extenderHeadStartRole.Store(extenderHeadStartRoleAllowed)
	} else {
		extenderHeadStartRole.Store(extenderHeadStartRoleWithheld)
	}
	wakeExtenderHeadStart()
}

func wakeExtenderHeadStart() {
	select {
	case extenderHeadStartWake <- struct{}{}:
	default:
	}
}

// providerStoreOpenDuration is how long the last real provider store open took.
func providerStoreOpenDuration() time.Duration {
	return time.Duration(providerStoreLastOpenDuration.Load())
}

// extenderHeadStartWait holds the proxy launches for at most the cap, so the
// extender's activation attempt runs before ~1400 proxy slots compete for this
// box's CPU. It returns why it stopped, and logs exactly one line for it.
//
// It must be called after the run has signalled READY=1: a hold before the
// startup barrier would let systemd time the start job out (90 s) and SIGTERM a
// healthy provider.
func extenderHeadStartWait(ctx context.Context, sources extenderHeadStartSources) extenderHeadStartResult {
	switch {
	case !sources.directSlot:
		return extenderHeadStartSkip(extenderHeadStartDisabled, "no direct slot")
	case !sources.directEnabled:
		return extenderHeadStartSkip(extenderHeadStartDisabled, "direct transport off")
	case sources.hotSwapCandidate:
		return extenderHeadStartSkip(extenderHeadStartDisabled, "the hotswap candidate must authenticate at once")
	case sources.proxies == 0:
		return extenderHeadStartSkip(extenderHeadStartDisabled, "no proxy slots to hold")
	case 0 < sources.legacyAdopted:
		return extenderHeadStartSkip(extenderHeadStartSkippedSlowStartup,
			fmt.Sprintf("legacy import adopted %d identities", sources.legacyAdopted))
	case extenderHeadStartSlowStore < sources.storeOpen:
		return extenderHeadStartSkip(extenderHeadStartSkippedSlowStartup,
			fmt.Sprintf("provider store took %s to open", headStartSeconds(sources.storeOpen)))
	}
	if ctx.Err() != nil {
		return extenderHeadStartResult{end: extenderHeadStartShutdown}
	}

	setExtenderHeadStartHeld(true)
	defer setExtenderHeadStartHeld(false)

	start := time.Now()
	deadline := start.Add(extenderHeadStartCap)
	for {
		observation := latestProviderExtenderObservation()
		if observation.attempted() {
			return extenderHeadStartResult{
				end:         extenderHeadStartAttempted,
				elapsed:     time.Since(start),
				activatedV4: observation.ActivatedV4,
				activatedV6: observation.ActivatedV6,
			}.finish()
		}
		// A known disabled extender never activates, and the direct launch
		// publishes the budget's withheld verdict before the role is off for
		// the same reason: neither will complete an attempt.
		if observation.Known && !observation.Enabled {
			return extenderHeadStartResult{end: extenderHeadStartDisabled, elapsed: time.Since(start)}.finish()
		}
		if extenderHeadStartRole.Load() == extenderHeadStartRoleWithheld {
			return extenderHeadStartResult{end: extenderHeadStartDisabled, elapsed: time.Since(start)}.finish()
		}
		if !time.Now().Before(deadline) {
			return extenderHeadStartResult{end: extenderHeadStartCapped, elapsed: time.Since(start)}.finish()
		}
		select {
		case <-ctx.Done():
			return extenderHeadStartResult{end: extenderHeadStartShutdown, elapsed: time.Since(start)}.finish()
		case <-extenderHeadStartWake:
		case <-time.After(extenderHeadStartPoll):
		}
	}
}

// finish clears the held state and logs the one line, so the log and the
// STATUS= line never disagree about whether proxies are held.
func (result extenderHeadStartResult) finish() extenderHeadStartResult {
	setExtenderHeadStartHeld(false)
	tlog("[startup] %s\n", result.line())
	return result
}

func extenderHeadStartSkip(end extenderHeadStartEnd, reason string) extenderHeadStartResult {
	return extenderHeadStartResult{end: end, reason: reason}.finish()
}

// setExtenderHeadStartHeld publishes the hold to the STATUS= line, and pushes
// the line on every change so `systemctl status` shows the hold immediately
// instead of at the next proxy transition.
func setExtenderHeadStartHeld(held bool) {
	if extenderHeadStartHeld.Swap(held) == held {
		return
	}
	reportProxyStatusToSystemd()
}

// extenderHeadStartStatusLine is the STATUS= line while the proxies are held
// for the extender's head start. It deliberately does not read "resolving
// proxies": nothing is being resolved, and the bounded hold is what an operator
// reading systemctl status needs to see.
func extenderHeadStartStatusLine() string {
	return fmt.Sprintf("starting: giving the extender one activation attempt (up to %ds)", int(extenderHeadStartCap/time.Second))
}

// line is the one log sentence for the end of the hold. Plain sentences, no em
// dashes, so it reads the same in a journal and in a bug report.
func (result extenderHeadStartResult) line() string {
	if result.reason != "" {
		return fmt.Sprintf("extender head start skipped: %s", result.reason)
	}
	switch result.end {
	case extenderHeadStartAttempted:
		family := "attempt failed"
		switch {
		case result.activatedV4 && result.activatedV6:
			family = "activated v4 and v6"
		case result.activatedV4:
			family = "activated v4"
		case result.activatedV6:
			family = "activated v6"
		}
		return fmt.Sprintf("extender head start %s: %s, releasing proxies", headStartSeconds(result.elapsed), family)
	case extenderHeadStartCapped:
		return fmt.Sprintf("extender head start %s: no attempt yet, releasing proxies", headStartSeconds(result.elapsed))
	case extenderHeadStartShutdown:
		return fmt.Sprintf("extender head start %s: shutting down, releasing proxies", headStartSeconds(result.elapsed))
	case extenderHeadStartDisabled:
		return fmt.Sprintf("extender head start %s: extender role off, releasing proxies", headStartSeconds(result.elapsed))
	default:
		return fmt.Sprintf("extender head start %s: releasing proxies", headStartSeconds(result.elapsed))
	}
}

// headStartSeconds renders a hold duration to a tenth of a second.
func headStartSeconds(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

// resetExtenderHeadStart restores the package state the hold and its STATUS=
// line read, for tests.
func resetExtenderHeadStart() {
	latestExtenderObservation.Store(nil)
	extenderHeadStartRole.Store(extenderHeadStartRoleUndecided)
	extenderHeadStartHeld.Store(false)
	providerStoreLastOpenDuration.Store(0)
	for {
		select {
		case <-extenderHeadStartWake:
		default:
			return
		}
	}
}
