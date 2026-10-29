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
// The budget is five against the operator's six, and a pass posts once per
// family, so a dual-stack host spends two a pass and strands the fifth: a
// third pass cannot afford the two it would post. A family whose activation
// failed can spend that post on one retry instead, which is what
// extenderActivationSpendSpare does.
//
// The record is a section of provider_state.db (state_store.go), not a file of
// its own. When it cannot be used after bounded retries, a fallback ledger of
// the same shape counts launches, and the first read of the real record merges
// it in (extender_fallback.go).

import (
	"errors"
	"os"
	"sync"
	"time"

	"github.com/urfoundation/sn/clientauth"
)

const (
	// extenderActivationBudget is the operator's six per hour minus one, a
	// margin for what a start can spend beyond its family posts: a hello
	// check or a resumed pass. A pass that failed can spend the post left
	// over on one retry of the family that failed.
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
	// SpareFamily is the family the window's leftover post funded a retry
	// for, and SpareAtUnix when, so one window funds one retry rather than a
	// loop of them.
	SpareFamily string `json:"spare_family,omitempty"`
	SpareAtUnix int64  `json:"spare_at_unix,omitempty"`
}

// extenderActivationFamily names one address family an activation pass posts on.
type extenderActivationFamily string

const (
	// extenderActivationFamilyV4 is the path that still carries traffic when
	// v6 does not work, so a retry prefers it.
	extenderActivationFamilyV4 extenderActivationFamily = "ipv4"
	extenderActivationFamilyV6 extenderActivationFamily = "ipv6"
)

// extenderActivationFamilyResult is what one pass got for one family.
type extenderActivationFamilyResult struct {
	// Tried records that the pass posted for this family.
	Tried bool
	// Activated records that the operator answered with an activation.
	Activated bool
}

// extenderActivationPass is what one activation pass did, one result per
// family. A pass posts once for each family it tries and never twice.
type extenderActivationPass struct {
	V4 extenderActivationFamilyResult
	V6 extenderActivationFamilyResult
}

// failedFamily names the family a retry would target, or false when none
// failed. A family that activated is never a target: the operator records
// every post, and a second one for a family it already activated buys nothing.
func (pass extenderActivationPass) failedFamily() (extenderActivationFamily, bool) {
	candidates := []struct {
		family extenderActivationFamily
		result extenderActivationFamilyResult
	}{
		{extenderActivationFamilyV4, pass.V4},
		{extenderActivationFamilyV6, pass.V6},
	}
	for _, candidate := range candidates {
		if candidate.result.Tried && !candidate.result.Activated {
			return candidate.family, true
		}
	}
	return "", false
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
	// FallbackStamp is the newest fallback ledger already merged into Accounts
	// (extender_fallback.go), so a merge is applied once.
	FallbackStamp int64 `json:"fallback_stamp,omitempty"`
	extenderActivationLegacy
}

// present reports whether the flat record holds anything worth adopting.
func (legacy extenderActivationLegacy) present() bool {
	return 0 < len(legacy.Attempts) || legacy.BackoffUntilUnix != 0 || legacy.Refusals != 0 || legacy.LastRefusalUnix != 0
}

// extenderActivationRecordMu serialises the record's read-modify-write. The
// store makes a load and a save each atomic, but the record is one value: two
// slots' status listeners can report a refusal and an activation at once, and
// without this lock each loads the same map and the later save drops the
// other's change. One lock for the whole record is enough; the section is a
// small json decode, a map update and one bbolt transaction.
var extenderActivationRecordMu sync.Mutex

// warnExtenderBudgetUnavailable reports why the record could not be used.
func warnExtenderBudgetUnavailable(account string, err error) {
	tlog("⚠️ [extender] activation budget unavailable for account %s: %v\n", account, err)
}

// readExtenderActivationRecord loads the record. An empty record and no record
// read the same to the budget, but a store that is unavailable or a value this
// build cannot decode is an error: a caller that treated it as empty would
// save its own few accounts over every account the record still holds.
func readExtenderActivationRecord(account string) (extenderActivationRecord, error) {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnavailable(account, errProviderStoreClosed)
		return extenderActivationRecord{}, errProviderStoreClosed
	}
	var record extenderActivationRecord
	found, err := store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &record)
	if err != nil {
		warnExtenderBudgetUnavailable(account, err)
		return extenderActivationRecord{}, err
	}
	if !found {
		return extenderActivationRecord{}, nil
	}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	return record, nil
}

func writeExtenderActivationRecord(record extenderActivationRecord, account string) {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnavailable(account, errProviderStoreClosed)
		return
	}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		warnExtenderBudgetUnavailable(account, err)
	}
}

// extenderActivationRecordForAccount loads the record and adopts a pre-account
// flat record onto the account, so every write path persists the new shape and
// the first start after an upgrade charges the history it inherited. A record
// that could not be read is reported, never replaced with an empty one.
func extenderActivationRecordForAccount(account string) (extenderActivationRecord, error) {
	record, err := readExtenderActivationRecord(account)
	if err != nil {
		return extenderActivationRecord{}, err
	}
	if record.Accounts == nil {
		record.Accounts = map[string]extenderActivationState{}
	}
	record.adoptLegacy(account)
	return record, nil
}

// mutateExtenderActivationRecord is the budget's write path: it holds the
// package lock and the cross-process ledger lock for the whole decision and
// mutates inside one store transaction. A fallback ledger found under the lock
// is merged into the record in the same transaction and removed once it
// commits. A store that cannot be used is reported to the caller.
var mutateExtenderActivationRecord = func(account string, mutate func(*extenderActivationRecord) error) error {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnavailable(account, errProviderStoreClosed)
		return errProviderStoreClosed
	}
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	unlock, err := lockExtenderLedger()
	if err != nil {
		warnExtenderBudgetUnavailable(account, err)
		return err
	}
	defer unlock()
	// The real ledger decides even when the fallback file cannot be read.
	var fallback extenderFallbackRecord
	var fallbackPath string
	var fallbackFound bool
	if path, pathErr := extenderFallbackPath(); pathErr == nil {
		if loaded, found, loadErr := loadExtenderFallback(path); loadErr != nil {
			tlog("⚠️ [extender] activation fallback record not merged: %v\n", loadErr)
		} else {
			fallback, fallbackFound, fallbackPath = loaded, found, path
		}
	}
	var record extenderActivationRecord
	err = store.updateJSON(providerStoreBucketState, providerStoreKeyExtenderActivation, &record, func(found bool) error {
		if !found {
			record = extenderActivationRecord{}
		}
		if record.Accounts == nil {
			record.Accounts = map[string]extenderActivationState{}
		}
		record.adoptLegacy(account)
		if fallbackFound {
			record.mergeFallback(fallback, time.Now())
		}
		return mutate(&record)
	})
	if err != nil {
		warnExtenderBudgetUnavailable(account, err)
		return err
	}
	if fallbackFound {
		if rmErr := os.Remove(fallbackPath); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			tlog("⚠️ [extender] merged activation fallback record not removed: %v\n", rmErr)
		}
	}
	return nil
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
	record.Accounts[account] = mergeExtenderActivationState(record.Accounts[account], pending, now)
}

// mergeExtenderActivationState folds one account's pending spending into
// another state of the same account.
func mergeExtenderActivationState(state, pending extenderActivationState, now time.Time) extenderActivationState {
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
	return state
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
func extenderActivationAllow(now time.Time, posts int, userId string) (bool, time.Duration, int) {
	allow, wait, used, _ := extenderActivationDecision(now, posts, userId)
	return allow, wait, used
}

// decideExtenderActivationOn is the one decision rule, applied to whichever
// ledger the caller holds. It records the launch's posts when it allows it.
func decideExtenderActivationOn(record *extenderActivationRecord, account string, now time.Time, posts int) (allow bool, wait time.Duration, used int) {
	// The token resolves on this path, so this is where posts charged while
	// it was unreadable first learn the account that spent them.
	record.adoptUnidentified(account, now)
	state := record.Accounts[account]
	used = pruneExtenderAttempts(&state, now, extenderActivationWindow)
	switch {
	case now.Unix() < state.BackoffUntilUnix:
		wait = time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second
	case extenderActivationBudget < used+posts:
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
	default:
		for i := 0; i < posts; i++ {
			state.Attempts = append(state.Attempts, now.Unix())
		}
		allow = true
	}
	record.Accounts[account] = state
	return allow, wait, used
}

// extenderActivationDecision distinguishes a withheld decision from a refusal.
func extenderActivationDecision(now time.Time, posts int, userId string) (allow bool, wait time.Duration, used int, err error) {
	account := extenderActivationAccountKey(userId)
	decided := false
	err = mutateExtenderActivationRecord(account, func(record *extenderActivationRecord) error {
		allow, wait, used = decideExtenderActivationOn(record, account, now, posts)
		decided = true
		return nil
	})
	if err != nil {
		// A readable cap or backoff remains authoritative even if its
		// unchanged record could not be written back.
		if decided && !allow {
			return false, wait, used, nil
		}
		return false, providerStoreRetryInterval, 0, err
	}
	return allow, wait, used, nil
}

// extenderActivationSpendSpare funds one retry of the family a pass failed on
// out of the post the window has left over, and reports which family it went
// to. A pass spends one post per family, so a dual-stack host can afford two
// passes and strands the fifth; a family that failed can use it once per
// window, and only when no refusal wait is still running. Nothing is spent when
// every family activated, because the operator records every post.
func extenderActivationSpendSpare(now time.Time, userId string, pass extenderActivationPass) (extenderActivationFamily, bool) {
	family, failed := pass.failedFamily()
	if !failed {
		return "", false
	}
	account := extenderActivationAccountKey(userId)
	var spent bool
	err := mutateExtenderActivationRecord(account, func(record *extenderActivationRecord) error {
		record.adoptUnidentified(account, now)
		state := record.Accounts[account]
		used := pruneExtenderAttempts(&state, now, extenderActivationWindow)
		switch {
		case 0 < state.SpareAtUnix && now.Sub(time.Unix(state.SpareAtUnix, 0)) < extenderActivationWindow:
		case now.Unix() < state.BackoffUntilUnix:
		case extenderActivationBudget <= used:
		default:
			state.Attempts = append(state.Attempts, now.Unix())
			state.SpareFamily = string(family)
			state.SpareAtUnix = now.Unix()
			spent = true
		}
		record.Accounts[account] = state
		return nil
	})
	if err != nil || !spent {
		return "", false
	}
	return family, true
}

// recordExtenderActivationRefused persists the wait a refusal implies for one
// account, so the next start honours it instead of posting again into the same
// refusal.
func recordExtenderActivationRefused(now time.Time, userId string) time.Duration {
	wait, _ := recordExtenderActivationRefusedPersisted(now, userId)
	return wait
}

// recordExtenderActivationRefusedPersisted is recordExtenderActivationRefused
// that also says whether the refusal reached the store. A caller that holds the
// refusal in memory only can then try again when the store comes back.
func recordExtenderActivationRefusedPersisted(now time.Time, userId string) (time.Duration, bool) {
	account := extenderActivationAccountKey(userId)
	wait := extenderRefusalBackoff
	err := mutateExtenderActivationRecord(account, func(record *extenderActivationRecord) error {
		state := record.Accounts[account]
		// A streak that has gone quiet starts over: refusals spread across weeks
		// are not one escalating incident.
		if state.LastRefusalUnix > 0 && now.Sub(time.Unix(state.LastRefusalUnix, 0)) > extenderRefusalDecay {
			state.Refusals = 0
		}
		state.Refusals++
		state.LastRefusalUnix = now.Unix()
		wait = extenderRefusalBackoff
		for i := 1; i < state.Refusals; i++ {
			wait *= 2
			if extenderRefusalBackoffMax <= wait {
				wait = extenderRefusalBackoffMax
				break
			}
		}
		state.BackoffUntilUnix = now.Add(wait).Unix()
		record.Accounts[account] = state
		return nil
	})
	if err != nil {
		// The streak cannot be recorded, and a caller told to retry now
		// would post again into the refusal. Hold for the first-level wait
		// even though the store did not keep it, and leave the hold where the
		// launches that follow, and the store once it returns, will find it.
		holdExtenderFallback(now, userId, extenderRefusalBackoff)
		return extenderRefusalBackoff, false
	}
	return wait, true
}

// recordExtenderActivationSucceeded clears refusal and spare state, adopts
// unidentified attempts and preserves every counted post.
func recordExtenderActivationSucceeded(userId string) {
	account := extenderActivationAccountKey(userId)
	_ = mutateExtenderActivationRecord(account, func(record *extenderActivationRecord) error {
		// A success clears the refusal and the spare, not the window. The
		// operator records every allowed attempt and only ages them out, so
		// wiping Attempts here would tell the next gate it has a full budget
		// while the operator still counts this hour's posts.
		//
		// Adopting comes first: it can restore a live hold from the
		// unidentified entry, and the clear below has to be the last word on
		// the refusal and backoff fields.
		record.adoptUnidentified(account, time.Now())
		if state, ok := record.Accounts[account]; ok {
			state.Refusals = 0
			state.LastRefusalUnix = 0
			state.BackoffUntilUnix = 0
			state.SpareFamily = ""
			state.SpareAtUnix = 0
			record.Accounts[account] = state
		}
		return nil
	})
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

type extenderLaunchDecision uint8

const (
	extenderLaunchWithheld extenderLaunchDecision = iota
	extenderLaunchRefused
	extenderLaunchAllowed
)

// decideAtStart caches only a real verdict; a withheld decision must be
// retried by the first launch, while an allowed verdict must not charge twice.
func (gate *extenderActivationGate) decideAtStart() bool {
	decision := gate.allowLaunchFor(hostActivationPostsProbe(), extenderActivationUserId())
	gate.mu.Lock()
	gate.pending, gate.allow = decision != extenderLaunchWithheld, decision == extenderLaunchAllowed
	gate.mu.Unlock()
	return decision == extenderLaunchAllowed
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
	posts := hostActivationPostsProbe()
	userId := extenderActivationUserId()
	decision := gate.allowLaunchForWith(posts, userId, launchProbeAttempts, launchProbeDelay)
	if decision == extenderLaunchWithheld {
		return allowExtenderFallback(time.Now(), posts, userId, errProviderStoreClosed)
	}
	return decision == extenderLaunchAllowed
}

// hostActivationPostsProbe is hostActivationPosts with the store opened once
// at the probe timeout. A store that will not open is the unmeasured host, which
// is charged the conservative two, rather than a second open at the ordinary
// two-second timeout.
func hostActivationPostsProbe() int {
	if _, err := openProviderStoreForProcessRetry(true, true, probeOpenTimeout); err != nil {
		return 2
	}
	return hostActivationPosts()
}

// The startup decision may spend a few hundred milliseconds waiting for a
// store that another process is still releasing, because it runs once and
// nothing else can start until it answers. The first launch runs moments
// later and gets a single probe: the startup window has already waited, and
// spending a second one would double the delay without new information.
const (
	startupProbeAttempts = 3
	startupProbeDelay    = 1000 * time.Millisecond
	launchProbeAttempts  = 1
	launchProbeDelay     = 0
	probeOpenTimeout     = 200 * time.Millisecond
)

// Retry the process-wide open after a withheld decision, then retry the
// transaction. A real budget or refusal verdict is final.
func (gate *extenderActivationGate) allowLaunchFor(posts int, userId string) extenderLaunchDecision {
	return gate.allowLaunchForWith(posts, userId, startupProbeAttempts, startupProbeDelay)
}

// allowLaunchForWith retries the process-wide open and the transaction, with
// the attempt budget the caller earned: the startup decision may wait, a later
// launch may not, because it runs after the startup window already waited.
func (gate *extenderActivationGate) allowLaunchForWith(posts int, userId string, attempts int, retryDelay time.Duration) extenderLaunchDecision {
	const probeTimeout = probeOpenTimeout
	for attempt := 0; attempt < attempts; attempt++ {
		_, _ = openProviderStoreForProcessRetry(true, true, probeTimeout)
		allow, wait, used, err := extenderActivationDecision(time.Now(), posts, userId)
		if err != nil {
			if attempt+1 < attempts {
				time.Sleep(retryDelay)
				continue
			}
			tlog("[extender] activation budget record could not be read or written; decision deferred: %v\n", err)
			return extenderLaunchWithheld
		}
		if !allow {
			tlog("[extender] activation held: %d of %d posts in the last hour, next in %s\n",
				used, extenderActivationBudget, formatDuration(wait))
			return extenderLaunchRefused
		}
		return extenderLaunchAllowed
	}
	return extenderLaunchWithheld
}
