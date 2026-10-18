// The bounded, observable proving phase of the 3.23-fix identity import.
//
// The phase proves the DIRECT identity first (so its result is the first proof
// line an operator sees), then the rest of the candidate slots with bounded
// concurrency. Every proof call runs under its own deadline even when the
// prover ignores the context, a heartbeat reports the tally every ~10s, and a
// total budget (or a run of unreachable API dials) ends the phase loudly:
// proven slots keep their proofs, everything else is left for the next start
// and named in the summary. There is no path where the import sits silent.
package miner

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// default bounds. Each is overridable through legacyImportOptions for tests
// and through the run's flags/env for operators.
const (
	// legacyImportDefaultBudget is the total proving budget: after it the
	// import stops with a loud summary, keeps every slot already proven, and
	// leaves the rest for the next start.
	legacyImportDefaultBudget = 5 * time.Minute
	// legacyImportProverBuildTimeout bounds the wait for the prover client to
	// be constructed: a constructor that hangs must not hide the import.
	legacyImportProverBuildTimeout = 15 * time.Second
	// legacyImportProofTimeout bounds ONE proof call, including its dial.
	legacyImportProofTimeout = 12 * time.Second
	// legacyImportProofConcurrency is how many proof calls may be in flight.
	// 1000 slots must not crawl one by one.
	legacyImportProofConcurrency = 16
	// legacyImportHeartbeatInterval is the cadence of the progress heartbeat.
	legacyImportHeartbeatInterval = 10 * time.Second
	// legacyImportUnreachableShortCircuit is how many consecutive proof reads
	// that never reached the API stop the phase: hammering a down API a
	// thousand times is not proving anything.
	legacyImportUnreachableShortCircuit = 16
)

// The phase's stop reasons, reported in the summary's result= token.
const (
	legacyImportResultComplete    = "complete"
	legacyImportResultBudget      = "budget"
	legacyImportResultUnreachable = "api_unreachable"
	legacyImportResultCanceled    = "canceled"
)

// The defer reasons the phase itself assigns when it stops early.
const (
	legacyImportReasonBudget      = "import_budget"
	legacyImportReasonUnreachable = "api_unreachable"
)

// legacyImportProveOptions are the phase's bounds, normalized by
// withDefaults. Zero values take the package defaults, so existing callers
// that never set them keep working.
type legacyImportProveOptions struct {
	budget       time.Duration
	proofTimeout time.Duration
	concurrency  int
	heartbeat    time.Duration
	shortCircuit int
	attempts     int
}

func (self legacyImportProveOptions) withDefaults() legacyImportProveOptions {
	if self.budget <= 0 {
		self.budget = legacyImportDefaultBudget
	}
	if self.proofTimeout <= 0 {
		self.proofTimeout = legacyImportProofTimeout
	}
	if self.concurrency <= 0 {
		self.concurrency = legacyImportProofConcurrency
	}
	if self.heartbeat <= 0 {
		self.heartbeat = legacyImportHeartbeatInterval
	}
	if self.shortCircuit <= 0 {
		self.shortCircuit = legacyImportUnreachableShortCircuit
	}
	if self.attempts <= 0 {
		self.attempts = legacyImportTransientAttempts
	}
	return self
}

// legacyImportProofOutcome is one slot's proof verdict. The slot is not
// mutated by the proof worker; the collector applies the outcome.
type legacyImportProofOutcome struct {
	slot        *legacyImportSlot
	state       string // "commit", "fallback", or "defer"
	proof       string // "server_key" or "refresh"
	reason      string
	unreachable bool
}

// legacyImportProofTracker carries the heartbeat counters across the direct
// step and the concurrent step.
type legacyImportProofTracker struct {
	mu       sync.Mutex
	total    int
	proven   int
	fallback int
	skipped  int
}

func (self *legacyImportProofTracker) record(state string) {
	self.mu.Lock()
	defer self.mu.Unlock()
	switch state {
	case "commit":
		self.proven++
	case "fallback":
		self.fallback++
	default:
		self.skipped++
	}
}

func (self *legacyImportProofTracker) progress() legacyImportProgress {
	self.mu.Lock()
	defer self.mu.Unlock()
	pending := self.total - (self.proven + self.fallback + self.skipped)
	if pending < 0 {
		pending = 0
	}
	return legacyImportProgress{
		proven:   self.proven,
		fallback: self.fallback,
		skipped:  self.skipped,
		pending:  pending,
	}
}

// startLegacyImportHeartbeat emits the running tally immediately and every
// interval until the returned stop func runs. The opening emission is
// synchronous, before any proof can resolve: the first heartbeat is the
// phase starting (proven=0, pending=all), not a race with a fast prover.
func startLegacyImportHeartbeat(narrator *legacyImportNarrator, tracker *legacyImportProofTracker, interval time.Duration, start time.Time) func() {
	narrator.heartbeat(tracker.progress(), time.Since(start))
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				narrator.heartbeat(tracker.progress(), time.Since(start))
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
	}
}

// legacyImportBoundedProof runs one proof call under its own deadline, even
// when the prover ignores the context: the caller stops waiting at the bound
// and the worker's late answer is dropped.
func legacyImportBoundedProof[T any](ctx context.Context, timeout time.Duration, call func(context.Context) (T, error)) (value T, err error, timedOut bool) {
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type answer struct {
		value T
		err   error
	}
	done := make(chan answer, 1)
	go func() {
		value, err := call(callCtx)
		done <- answer{value, err}
	}()
	select {
	case result := <-done:
		return result.value, result.err, false
	case <-callCtx.Done():
		// The call may have finished between the deadline firing and this
		// select: prefer its own answer when it is already there.
		select {
		case result := <-done:
			return result.value, result.err, false
		default:
		}
		var zero T
		return zero, callCtx.Err(), true
	}
}

// legacyImportProveSlot answers the two proof questions for one slot:
// tier 1 compares the retained public key with the key the server holds for
// the client_id; a null or unreachable read is inconclusive and falls through
// to tier 2, the refresh probe the normal retained-credential path runs. Every
// call is bounded by opts.proofTimeout.
func legacyImportProveSlot(ctx context.Context, narrator *legacyImportNarrator, prover legacyImportProver, public ed25519.PublicKey, slot *legacyImportSlot, opts legacyImportProveOptions) legacyImportProofOutcome {
	outcome := legacyImportProofOutcome{slot: slot}
	serverKey, err, _ := legacyImportBoundedProof(ctx, opts.proofTimeout, func(callCtx context.Context) ([]byte, error) {
		return prover.ServerPublicKey(callCtx, slot.clientID)
	})
	inconclusive := false
	switch {
	case err != nil:
		inconclusive = true
	case len(serverKey) == 0:
		inconclusive = true
	case !bytes.Equal(serverKey, public):
		outcome.state = "fallback"
		outcome.reason = "server_key_differs"
	default:
		outcome.state = "commit"
		outcome.proof = "server_key"
	}
	if !inconclusive {
		return outcome
	}

	// Tier 2. A transient read (the API was never reached) is retried within
	// this start, bounded by the per-proof deadline and the attempt ceiling.
	// If it stays unavailable the slot is deferred: the saved credential
	// stays untouched, nothing is journaled as a decision, no new client is
	// allocated, and the next start proves it again. Only a completed
	// refusal is a fallback.
	outcome.state = "defer"
	outcome.reason = "refresh_unavailable"
	var lastErr error
	for attempt := 1; ; attempt++ {
		_, refreshErr, _ := legacyImportBoundedProof(ctx, opts.proofTimeout, func(callCtx context.Context) (struct{}, error) {
			return struct{}{}, prover.RefreshAccepted(callCtx, slot.entry.ByClientJWT)
		})
		if refreshErr == nil {
			outcome.state = "commit"
			outcome.proof = "refresh"
			outcome.reason = ""
			return outcome
		}
		lastErr = refreshErr
		if errors.Is(refreshErr, context.Canceled) {
			// The phase was stopped while this call was in flight (budget,
			// short circuit, or the run's own context). A canceled call is no
			// verdict about the identity: defer it, never fall back, so a
			// stopped phase cannot mint over an unproven identity.
			outcome.unreachable = true
			return outcome
		}
		if !legacyImportProofRetryable(refreshErr) {
			outcome.state = "fallback"
			outcome.reason = "refresh_rejected"
			return outcome
		}
		if ctx.Err() != nil || attempt >= opts.attempts {
			break
		}
		// The backoff before another transient attempt is a wait: name it like
		// every other wait, so the phase is never silent while it blocks.
		narrator.wait("proof retry", fmt.Sprintf("client_id=%s, next attempt %d of %d",
			slot.clientID8, attempt+1, opts.attempts))
		select {
		case <-ctx.Done():
		case <-time.After(legacyImportTransientBackoff):
		}
		if ctx.Err() != nil {
			break
		}
	}
	outcome.unreachable = legacyImportProofUnreachable(lastErr)
	return outcome
}

// legacyImportProveAll proves the candidates: the direct slot first, then the
// rest with bounded concurrency, a heartbeat and the total budget. Outcomes
// are applied to the slots by the collector. It returns the phase's stop
// reason and the number of slots left without an attempt.
func legacyImportProveAll(
	ctx context.Context,
	narrator *legacyImportNarrator,
	prover legacyImportProver,
	public ed25519.PublicKey,
	prove []*legacyImportSlot,
	opts legacyImportProveOptions,
) (string, int) {
	opts = opts.withDefaults()
	start := time.Now()
	tracker := &legacyImportProofTracker{total: len(prove)}
	stopHeartbeat := startLegacyImportHeartbeat(narrator, tracker, opts.heartbeat, start)
	defer stopHeartbeat()

	proofCtx, cancelProof := context.WithCancel(ctx)
	defer cancelProof()
	// The budget is a timer, not a context deadline, so the stop can say
	// whether the budget or the unreachable short circuit ended the phase.
	budget := make(chan struct{})
	budgetTimer := time.AfterFunc(opts.budget, func() {
		close(budget)
		cancelProof()
	})
	defer budgetTimer.Stop()

	resolved := map[*legacyImportSlot]bool{}
	collector := &legacyImportOutcomeNarrator{narrator: narrator}
	apply := func(outcome legacyImportProofOutcome) {
		if resolved[outcome.slot] {
			return
		}
		collector.apply(outcome)
		tracker.record(outcome.state)
		resolved[outcome.slot] = true
	}

	// The direct identity first, and alone: its proof result is the line the
	// operator greps to know the migration is running.
	var direct *legacyImportSlot
	rest := make([]*legacyImportSlot, 0, len(prove))
	for _, slot := range prove {
		if slot.storeKey == legacyImportDirectKey && direct == nil {
			direct = slot
			continue
		}
		rest = append(rest, slot)
	}
	if direct != nil {
		narrator.wait("proof slot", fmt.Sprintf("direct (1 of %d, timeout=%s)", len(prove), legacyImportBound(opts.proofTimeout)))
		apply(legacyImportProveSlot(proofCtx, narrator, prover, public, direct, opts))
		if proofCtx.Err() != nil {
			return legacyImportStop(ctx, proofCtx, budget, narrator, prove, resolved, start)
		}
	}

	// The rest, with bounded concurrency. Workers only read their slot and
	// send an outcome; the collector owns every slot mutation.
	sem := make(chan struct{}, opts.concurrency)
	outcomes := make(chan legacyImportProofOutcome, len(rest))
	for i := range rest {
		go func(slot *legacyImportSlot) {
			select {
			case sem <- struct{}{}:
			case <-proofCtx.Done():
				outcomes <- legacyImportProofOutcome{slot: slot, state: "defer", reason: legacyImportReasonUnreachable, unreachable: true}
				return
			}
			defer func() { <-sem }()
			outcomes <- legacyImportProveSlot(proofCtx, narrator, prover, public, slot, opts)
		}(rest[i])
	}

	// Collector: applies outcomes, keeps the wait lines named, and decides
	// when a run of API-unreachable reads has to stop the hammering.
	drain := func() {
		for {
			select {
			case outcome := <-outcomes:
				apply(outcome)
			default:
				return
			}
		}
	}
	unreachableStreak := 0
	tick := time.NewTicker(opts.heartbeat)
	defer tick.Stop()
	for len(resolved) < len(prove) {
		select {
		case outcome := <-outcomes:
			apply(outcome)
			if outcome.state == "defer" && outcome.unreachable {
				unreachableStreak++
			} else {
				unreachableStreak = 0
			}
			if unreachableStreak >= opts.shortCircuit && len(resolved) < len(prove) {
				narrator.line("api unreachable: %d consecutive proof reads could not reach the API; stopping the proof phase", unreachableStreak)
				cancelProof()
				drain()
				return legacyImportStop(ctx, proofCtx, budget, narrator, prove, resolved, start)
			}
		case <-proofCtx.Done():
			drain()
			return legacyImportStop(ctx, proofCtx, budget, narrator, prove, resolved, start)
		case <-tick.C:
			pending := tracker.progress()
			narrator.wait("proof slot", fmt.Sprintf("resolved=%d of %d, pending=%d", pending.resolved(), len(prove), pending.pending))
		}
	}
	return legacyImportResultComplete, 0
}

// legacyImportStop classifies the stop and marks the slots that never got an
// attempt, so the caller journals and reports them consistently.
func legacyImportStop(
	ctx context.Context,
	proofCtx context.Context,
	budget <-chan struct{},
	narrator *legacyImportNarrator,
	prove []*legacyImportSlot,
	resolved map[*legacyImportSlot]bool,
	start time.Time,
) (string, int) {
	if ctx.Err() != nil {
		// A canceled startup makes no permanent identity decision.
		return legacyImportResultCanceled, len(prove) - len(resolved)
	}
	budgetStop := false
	select {
	case <-budget:
		budgetStop = true
	default:
	}
	reason := legacyImportReasonUnreachable
	result := legacyImportResultUnreachable
	if budgetStop {
		reason = legacyImportReasonBudget
		result = legacyImportResultBudget
	}
	left := 0
	for _, slot := range prove {
		if resolved[slot] {
			continue
		}
		slot.state = "defer"
		slot.reason = reason
		slot.unreachable = true
		slot.left = true
		left++
	}
	if budgetStop {
		narrator.line("budget: reached after %s; proven slots keep their proofs, leaving %d slots for the next start", legacyImportElapsed(time.Since(start)), left)
	}
	if left > 0 {
		narrator.line("slots: %d left for the next start (%s)", left, reason)
	}
	return result, left
}

// legacyImportBuildProver constructs the proof client under a bounded wait: a
// constructor that hangs must not hide the import, and each family dial it
// makes names itself in the narration.
func legacyImportBuildProver(ctx context.Context, opts legacyImportOptions) (legacyImportProver, func(), error) {
	timeout := opts.proverBuildTimeout
	if timeout <= 0 {
		timeout = legacyImportProverBuildTimeout
	}
	opts.narrator.wait("prover construction", fmt.Sprintf("timeout=%s", legacyImportBound(timeout)))
	type built struct {
		prover legacyImportProver
		close  func()
		err    error
	}
	done := make(chan built, 1)
	wait := func(family string, attempt int, dialTimeout time.Duration) {
		opts.narrator.wait("api dial", fmt.Sprintf("family=%s, attempt=%d, timeout=%s",
			family, attempt, legacyImportBound(dialTimeout)))
	}
	go func() {
		prover, closeProver, err := opts.newProver(wait)
		done <- built{prover, closeProver, err}
	}()
	select {
	case b := <-done:
		return b.prover, b.close, b.err
	case <-time.After(timeout):
		opts.narrator.line("prover construction timed out after %s; the proof cannot run this start", legacyImportBound(timeout))
		return nil, nil, fmt.Errorf("prover construction timed out after %s", legacyImportBound(timeout))
	}
}

// legacyImportLeftReason is the journal/skip reason for slots a stopped phase
// never attempted.
func legacyImportLeftReason(result string) string {
	switch result {
	case legacyImportResultBudget:
		return legacyImportReasonBudget
	case legacyImportResultUnreachable:
		return legacyImportReasonUnreachable
	default:
		return "not_attempted"
	}
}

// legacyImportOutcomeNarrator applies one outcome to its slot and narrates it.
type legacyImportOutcomeNarrator struct {
	narrator *legacyImportNarrator
}

func (self *legacyImportOutcomeNarrator) apply(outcome legacyImportProofOutcome) {
	slot := outcome.slot
	slot.state = outcome.state
	slot.proof = outcome.proof
	slot.reason = outcome.reason
	slot.unreachable = outcome.unreachable
	slot.attempted = true
	display := legacyImportNarrateDisplay(slot.settings)
	switch outcome.state {
	case "commit":
		self.narrator.slotOutcome(display, "proven", fmt.Sprintf("client_id=%s proof=%s", slot.clientID8, outcome.proof))
	case "fallback":
		self.narrator.slotOutcome(display, "fallback", fmt.Sprintf("reason=%s (a new client identity will be registered)", outcome.reason))
	default:
		self.narrator.slotOutcome(display, "deferred", fmt.Sprintf("reason=%s (left for the next start)", outcome.reason))
	}
}

// legacyImportNarrateDisplay names a slot for the narration with an
// obfuscated user, so two identities behind one address stay distinguishable
// without ever printing a credential.
func legacyImportNarrateDisplay(settings *connect.ProxySettings) string {
	if settings == nil {
		return "direct"
	}
	if settings.Auth != nil && settings.Auth.User != "" {
		return "proxy:" + settings.Address + " (user " + obfuscateUser(settings.Auth.User) + ")"
	}
	return "proxy:" + settings.Address
}
