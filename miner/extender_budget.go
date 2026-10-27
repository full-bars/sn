package miner

// extender_budget.go keeps this provider's own activation posts under the
// operator's activation budget.
//
// The operator allows six activation posts an hour and records every attempt
// (`controller.ExtenderActivateRateLimit` in urnetwork/server), because each
// activation costs it several dials back to the caller. One activation pass
// posts once per family it supports, so a start on a dual-stack host spends a
// third of the budget. The activation loop's own backoff lives in memory, which
// means a restart forgets it; the record here is what makes the budget survive a
// restart or a reboot.
//
// The record is keyed by the account the direct slot signs in as: the user_id
// claim of the sign-in token at <state>/jwt, which is the key the operator
// counts. ExtenderActivateRateLimit is enforced per user
// (model.CheckAndRecordAccountActionRateLimit is passed
// clientSession.ByJwt.UserId), and every client of one sign-in shares that
// user, so keying by client let a heal start fresh while the operator's window
// still held the account's posts. Keying by the account is the operator's own
// key: a healed or re-minted client spends what the account has left.
//
// A start that cannot read the token yet charges the unidentified key. The
// first start that resolves the account adopts those posts and any refusal
// hold still standing, so the account inherits what the host already spent
// rather than opening a fresh window.
//
// The record is a section of provider_state.db (state_store.go), not a file of
// its own. A store that will not open is logged once and read as empty rather
// than as a refusal: the operator's own rate limit still answers the posts, and
// a host must keep providing when its own state file is unreadable.

import (
	"sync"
	"time"

	"github.com/urfoundation/sn/clientauth"
)

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

	// extenderActivationUnidentifiedAccount keys the record while there is no
	// readable sign-in token: the start is about to sign in, so there is no
	// account to charge yet. The posts still count against something, so a
	// host that restarts in a loop without ever signing in is not handed a
	// fresh budget by every start. The first start that resolves the token
	// adopts the entry (adoptUnidentified), so the account inherits what was
	// spent before it could be named.
	extenderActivationUnidentifiedAccount = "unidentified"
)

// extenderActivationState is one account's spending.
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

// extenderActivationLegacy is the record before it was keyed by any identity:
// one flat state for the whole host. It is embedded in the record, so a store
// written by an older build still decodes, and it is read only to charge those
// attempts to the account signed in now (adoptLegacy) rather than discard them.
type extenderActivationLegacy struct {
	Attempts         []int64 `json:"attempts,omitempty"`
	BackoffUntilUnix int64   `json:"backoff_until_unix,omitempty"`
	Refusals         int     `json:"refusals,omitempty"`
	LastRefusalUnix  int64   `json:"last_refusal_unix,omitempty"`
}

// extenderActivationRecord is the stored shape: one state per account. The map
// is not bounded because an entry needs a distinct sign-in account on this
// host, which the operator controls and a heal does not add to.
type extenderActivationRecord struct {
	Accounts map[string]extenderActivationState `json:"accounts,omitempty"`
	extenderActivationLegacy
}

// present reports whether the flat record holds anything worth adopting.
func (legacy extenderActivationLegacy) present() bool {
	return 0 < len(legacy.Attempts) || legacy.BackoffUntilUnix != 0 || legacy.Refusals != 0 || legacy.LastRefusalUnix != 0
}

// extenderBudgetWarnOnce keeps the unenforced-budget line to one per process:
// a store that cannot be opened fails every later call, and the same warning
// on each start attempt buries the first one. A pointer so a test can swap in
// a fresh one without copying a lock.
var extenderBudgetWarnOnce = new(sync.Once)

// extenderActivationRecordMu serialises the record's read-modify-write. The
// store makes a load and a save each atomic, but the record is one value: two
// slots' status listeners can report a refusal and an activation at once, and
// without this lock each loads the same map and the later save drops the
// other's change. One lock for the whole record is enough; the section is a
// small json decode, a map update and one bbolt transaction.
var extenderActivationRecordMu sync.Mutex

// warnExtenderBudgetUnenforced says the record could not be read or written.
// The budget is best effort by design (a host must never stop providing
// because its own state file is read-only, and the operator's own rate limit
// still answers the posts with a 429), so this line is the only evidence the
// operator gets that the budget went unenforced.
func warnExtenderBudgetUnenforced(account string) {
	extenderBudgetWarnOnce.Do(func() {
		tlog("⚠️ [extender] activation budget unenforced for account %s: provider state store unavailable this run\n", account)
	})
}

func readExtenderActivationRecord(account string) (extenderActivationRecord, bool) {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnenforced(account)
		return extenderActivationRecord{}, false
	}
	var record extenderActivationRecord
	if !store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &record) {
		return extenderActivationRecord{}, false
	}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	return record, true
}

func writeExtenderActivationRecord(record extenderActivationRecord, account string) {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnenforced(account)
		return
	}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		warnExtenderBudgetUnenforced(account)
	}
}

// extenderActivationRecordForAccount loads the record and adopts a pre-account
// flat record onto the account, so every write path persists the new shape and
// the first start after an upgrade charges the history it inherited.
func extenderActivationRecordForAccount(account string) extenderActivationRecord {
	record, _ := readExtenderActivationRecord(account)
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	record.adoptLegacy(account)
	return record
}

// adoptLegacy moves a pre-account record onto the account. The old record
// described this host's own spending, and the identity an upgrade keeps is the
// account signed in now, so its attempts and its refusal streak belong to it.
func (record *extenderActivationRecord) adoptLegacy(account string) {
	legacy := record.extenderActivationLegacy
	if 0 < len(record.Accounts) || !legacy.present() {
		return
	}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	record.Accounts[account] = extenderActivationState{
		Attempts:         legacy.Attempts,
		BackoffUntilUnix: legacy.BackoffUntilUnix,
		Refusals:         legacy.Refusals,
		LastRefusalUnix:  legacy.LastRefusalUnix,
	}
	record.extenderActivationLegacy = extenderActivationLegacy{}
}

// extenderRefusalStreakLive reports whether a refusal streak still counts at
// now. The count and the time it was last charged are one value: a count whose
// own last refusal is outside the decay has nothing left to escalate, so
// carrying its number onto a fresher timestamp would invent a longer streak
// than the refusals justify.
func extenderRefusalStreakLive(state extenderActivationState, now time.Time) bool {
	return 0 < state.Refusals && 0 < state.LastRefusalUnix &&
		now.Sub(time.Unix(state.LastRefusalUnix, 0)) <= extenderRefusalDecay
}

// adoptUnidentified moves what a start spent before the sign-in token was
// readable onto the account the token now names. The operator counted those
// posts when they were made, so the account inherits them instead of opening a
// fresh window; the entry is cleared so no start charges them twice. An
// attempt that has left the window is already outside the operator's own
// limit, and a hold that has expired no longer holds anything. The refusal
// streak travels with the hold: a refusal taken before the account could be
// named is the same incident, so the next one keeps doubling the wait instead
// of restarting at the first. The longer live streak is taken whole and a
// streak whose own last refusal is outside the decay is dropped, so no count
// is carried on a timestamp it was never charged at.
func (record *extenderActivationRecord) adoptUnidentified(account string, now time.Time) {
	if account == extenderActivationUnidentifiedAccount {
		return
	}
	pending, ok := record.Accounts[extenderActivationUnidentifiedAccount]
	if !ok {
		return
	}
	delete(record.Accounts, extenderActivationUnidentifiedAccount)
	state := record.Accounts[account]
	cutoff := now.Add(-extenderActivationWindow).Unix()
	for _, at := range pending.Attempts {
		if cutoff < at {
			state.Attempts = append(state.Attempts, at)
		}
	}
	if now.Unix() < pending.BackoffUntilUnix && state.BackoffUntilUnix < pending.BackoffUntilUnix {
		state.BackoffUntilUnix = pending.BackoffUntilUnix
	}
	stateLive := extenderRefusalStreakLive(state, now)
	pendingLive := extenderRefusalStreakLive(pending, now)
	switch {
	case !stateLive && !pendingLive:
		// Neither streak is still running, so the account starts over.
		state.Refusals, state.LastRefusalUnix = 0, 0
	case pendingLive && (!stateLive || state.Refusals < pending.Refusals):
		// Merge rather than overwrite: an account that already carries a
		// longer live streak keeps its own count and its own timestamp.
		state.Refusals, state.LastRefusalUnix = pending.Refusals, pending.LastRefusalUnix
	}
	record.Accounts[account] = state
}

// extenderActivationAccountKey is the record key for one account. A start that
// cannot read the sign-in token yet is charged to the unidentified key rather
// than to a guessed account.
func extenderActivationAccountKey(userId string) string {
	if userId == "" {
		return extenderActivationUnidentifiedAccount
	}
	return userId
}

// extenderActivationUserId is the account the direct slot's posts are charged
// to: the user_id claim of the sign-in token at <state>/jwt. That is the key
// the operator's ExtenderActivateRateLimit counts, because
// model.CheckAndRecordAccountActionRateLimit is passed
// clientSession.ByJwt.UserId, so two clients of one sign-in are one budget and
// a heal spends what the account has left. Empty before the host has signed
// in, when there is no account to charge yet.
func extenderActivationUserId() string {
	path, err := providerStatePath("jwt")
	if err != nil {
		return ""
	}
	token, err := clientauth.ReadToken(path)
	if err != nil {
		return ""
	}
	userId, err := clientauth.JwtUserId(token)
	if err != nil {
		return ""
	}
	return userId
}

// storeExtenderActivation records one account's state at now and persists it.
func storeExtenderActivation(record extenderActivationRecord, account string, state extenderActivationState, now time.Time) {
	record.Accounts[account] = state
	writeExtenderActivationRecord(record, account)
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
// how long to wait when it may not, and how many posts the account's window
// already holds. It records this start's posts when it allows the role, so the
// caller calls it once, before the slots are built. userId is the account the
// direct slot signs in as (extenderActivationUserId).
func extenderActivationAllow(now time.Time, posts int, userId string) (allow bool, wait time.Duration, used int) {
	account := extenderActivationAccountKey(userId)
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	record := extenderActivationRecordForAccount(account)
	// The token resolves on this path, so this is where posts charged while it
	// was unreadable first learn the account that spent them.
	record.adoptUnidentified(account, now)
	state := record.Accounts[account]
	used = pruneExtenderAttempts(&state, now, extenderActivationWindow)
	if now.Unix() < state.BackoffUntilUnix {
		wait = time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second
		storeExtenderActivation(record, account, state, now)
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
		storeExtenderActivation(record, account, state, now)
		return false, wait, used
	}
	for i := 0; i < posts; i++ {
		state.Attempts = append(state.Attempts, now.Unix())
	}
	storeExtenderActivation(record, account, state, now)
	return true, 0, used
}

// recordExtenderActivationRefused persists the wait a refusal implies for one
// account, so the next start honours it instead of posting again into the same
// refusal.
func recordExtenderActivationRefused(now time.Time, userId string) time.Duration {
	account := extenderActivationAccountKey(userId)
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	record := extenderActivationRecordForAccount(account)
	state := record.Accounts[account]
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
	storeExtenderActivation(record, account, state, now)
	return wait
}

// recordExtenderActivationSucceeded clears what a refusal left behind for one
// account: the activation the operator signed resets that account's budget, so
// the next start may activate again. Other accounts in the record are
// untouched.
func recordExtenderActivationSucceeded(userId string) {
	account := extenderActivationAccountKey(userId)
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	record := extenderActivationRecordForAccount(account)
	delete(record.Accounts, account)
	writeExtenderActivationRecord(record, account)
}

// extenderActivationGate decides the direct slot's extender role once per
// launch. The startup decision is charged before the slots are built, so the
// first launch spends that verdict instead of charging twice; every relaunch
// asks the record again, because a relaunch posts again and the hold the
// record persisted may have expired, or been recorded, since the process
// started. One verdict frozen at startup got both directions wrong: a slot
// relaunched after its hold expired stayed out of the role until the next
// process, and one relaunched inside a hold went back in.
type extenderActivationGate struct {
	mu sync.Mutex
	// pending is the startup verdict no launch has spent yet.
	pending bool
	allow   bool
}

// newExtenderActivationGate returns a gate with no decision made. The run
// makes one when its direct slot is enabled at startup; a direct slot the
// operator turns on later makes its first decision at its first launch.
func newExtenderActivationGate() *extenderActivationGate {
	return &extenderActivationGate{}
}

// decideAtStart charges this start's posts and keeps the verdict for the first
// launch, so a hold is logged once, before the slots come up, and the launch
// that follows does not charge the same posts again.
func (gate *extenderActivationGate) decideAtStart() bool {
	allow := gate.allowLaunch()
	gate.mu.Lock()
	gate.pending, gate.allow = true, allow
	gate.mu.Unlock()
	return allow
}

// allowLaunch reports whether this launch may run the extender role. It spends
// the startup verdict once and asks the record on every later launch, resolving
// the account afresh because the sign-in token can be rewritten between
// launches.
func (gate *extenderActivationGate) allowLaunch() bool {
	gate.mu.Lock()
	pending, allow := gate.pending, gate.allow
	gate.pending = false
	gate.mu.Unlock()
	if pending {
		return allow
	}
	allow, wait, used := extenderActivationAllow(time.Now(), hostActivationPosts(), extenderActivationUserId())
	if !allow {
		tlog("[extender] activation held: %d of %d posts in the last hour, next in %s\n",
			used, extenderActivationBudget, formatDuration(wait))
	}
	return allow
}
