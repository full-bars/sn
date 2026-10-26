package miner

// extender_budget.go keeps this provider's own activation posts under the
// operator's per-client budget.
//
// The operator allows six activation posts per client per hour and records every
// attempt (`controller.ExtenderActivateRateLimit` in urnetwork/server), because
// each activation costs it several dials back to the caller. One activation pass
// posts once per family it supports, so a start on a dual-stack host spends a
// third of the budget. The activation loop's own backoff lives in memory, which
// means a restart forgets it; the record here is what makes the budget survive a
// restart or a reboot.
//
// The record is a section of provider_state.json (provider_state_sections.go),
// not a file of its own.

import "time"

const (
	// extenderActivationBudget is the operator's six per hour minus one. The
	// spare attempt covers what one start can spend beyond its family posts:
	// a hello check or a resumed pass.
	extenderActivationBudget = 5
	// extenderActivationWindow is the operator's window.
	extenderActivationWindow = time.Hour

	// The refusal backoff: the first refusal holds the next attempt for
	// extenderRefusalBackoff and doubles with each further refusal, capped at
	// the operator's own window. A cap longer than the window would park a
	// healthy host for longer than the server itself ever would.
	extenderRefusalBackoff    = 10 * time.Minute
	extenderRefusalBackoffMax = extenderActivationWindow
	// extenderRefusalDecay drops a refusal streak that has gone quiet, so
	// refusals spread over weeks cannot pin a host at the maximum wait.
	extenderRefusalDecay = 24 * time.Hour
)

type extenderActivationState struct {
	// Attempts are the unix times of the posts counted in the window.
	Attempts []int64 `json:"attempts,omitempty"`
	// BackoffUntilUnix holds the next allowed attempt after a refusal, so a
	// restart cannot spend the budget the refusal was meant to protect.
	BackoffUntilUnix int64 `json:"backoff_until_unix,omitempty"`
	// Refusals is the current refusal streak, which doubles the wait.
	Refusals int `json:"refusals,omitempty"`
	// LastRefusalUnix bounds the streak in time, so a refusal from weeks ago
	// does not escalate today's.
	LastRefusalUnix int64 `json:"last_refusal_unix,omitempty"`
}

func readExtenderActivationState() extenderActivationState {
	var state extenderActivationState
	if store := providerStoreInstance(); store != nil {
		store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &state)
	}
	return state
}

func writeExtenderActivationState(state extenderActivationState) {
	if store := providerStoreInstance(); store != nil {
		_ = store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, state)
	}
}

// pruneExtenderAttempts drops the attempts that have aged out of the window and
// returns the number still counted.
func pruneExtenderAttempts(state *extenderActivationState, now time.Time, window time.Duration) int {
	cutoff := now.Add(-window).Unix()
	kept := state.Attempts[:0]
	for _, at := range state.Attempts {
		if cutoff < at {
			kept = append(kept, at)
		}
	}
	state.Attempts = kept
	return len(kept)
}

// extenderActivationAllow reports whether this start may run the extender role,
// how long to wait when it may not, and how many posts the window already
// holds. It records this start's posts when it allows the role, so the caller
// calls it once, before the slots are built.
func extenderActivationAllow(now time.Time, posts int) (allow bool, wait time.Duration, used int) {
	state := readExtenderActivationState()
	used = pruneExtenderAttempts(&state, now, extenderActivationWindow)
	if now.Unix() < state.BackoffUntilUnix {
		wait = time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second
		return false, wait, used
	}
	if extenderActivationBudget < used+posts {
		// Wait for the oldest counted post to age out of the window.
		oldest := state.Attempts[0]
		for _, at := range state.Attempts[1:] {
			if at < oldest {
				oldest = at
			}
		}
		if wait = time.Unix(oldest, 0).Add(extenderActivationWindow).Sub(now); wait < 0 {
			wait = 0
		}
		return false, wait, used
	}
	for i := 0; i < posts; i++ {
		state.Attempts = append(state.Attempts, now.Unix())
	}
	writeExtenderActivationState(state)
	return true, 0, used
}

// recordExtenderActivationRefused persists the wait a refusal implies, so the
// next start honours it instead of posting again into the same refusal.
func recordExtenderActivationRefused(now time.Time) time.Duration {
	state := readExtenderActivationState()
	// A streak that has gone quiet starts over: refusals spread across weeks
	// are not one escalating incident.
	if state.LastRefusalUnix > 0 && now.Sub(time.Unix(state.LastRefusalUnix, 0)) > extenderRefusalDecay {
		state.Refusals = 0
	}
	state.Refusals++
	state.LastRefusalUnix = now.Unix()
	wait := extenderRefusalBackoff
	for i := 1; i < state.Refusals; i++ {
		wait *= 2
		if extenderRefusalBackoffMax <= wait {
			wait = extenderRefusalBackoffMax
			break
		}
	}
	state.BackoffUntilUnix = now.Add(wait).Unix()
	writeExtenderActivationState(state)
	return wait
}

// recordExtenderActivationSucceeded clears what a refusal left behind: the
// activation the operator signed resets its own budget, so the next start may
// activate again.
func recordExtenderActivationSucceeded() {
	writeExtenderActivationState(extenderActivationState{})
}
