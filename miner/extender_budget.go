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
// The record is keyed by the client the direct slot provides under: the
// identity the status listener records against, and the one a heal replaces. A
// host that heals must not be throttled by the identity it replaced. A start
// that cannot name its client yet charges the unidentified key, which the
// first client it names inherits (adoptUnidentified), so registering does not
// hand that client a budget the start already spent.
//
// That is not quite the operator's key.
// ExtenderActivateRateLimit is enforced per user
// (model.CheckAndRecordAccountActionRateLimit is passed
// clientSession.ByJwt.UserId), and every client of one sign-in shares its
// user, so a healed client starts fresh here while the operator's window still
// holds the account's posts. Keying this record by the account is a separate
// change; until then a heal can under-count against the operator.
//
// The record is a section of provider_state.db (state_store.go), not a file of
// its own.

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

	// extenderActivationClientBound bounds how many client identities the
	// record keeps. A host that heals its identity spends under a new client,
	// and a host that mints fresh on every start leaves one behind each time,
	// so the map needs a ceiling: the least recently used client goes first.
	// A record matters for an hour at most, and 32 is far more clients than
	// one host spends from in an hour.
	extenderActivationClientBound = 32

	// extenderActivationUnidentifiedClient keys the record while the direct
	// slot has no readable client credential: the start is about to mint a
	// client, so there is no identity to charge yet. The posts still count,
	// so a host that restarts in a loop without ever registering is not handed
	// a fresh budget by every start. The first client this host names inherits
	// the entry (adoptUnidentified), so those posts are charged to the identity
	// that spent them.
	extenderActivationUnidentifiedClient = "unidentified"
)

// extenderActivationState is one client's spending.
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
	// LastUsedUnix orders the clients in the record for eviction.
	LastUsedUnix int64 `json:"last_used_unix,omitempty"`
}

// extenderActivationLegacy is the record before it was keyed by client: one
// flat state for the whole host. It is embedded in the record, so a store
// written by an older build still decodes, and it is read only to charge those
// attempts to the client providing now (adoptLegacy) rather than discard them.
type extenderActivationLegacy struct {
	Attempts         []int64 `json:"attempts,omitempty"`
	BackoffUntilUnix int64   `json:"backoff_until_unix,omitempty"`
	Refusals         int     `json:"refusals,omitempty"`
	LastRefusalUnix  int64   `json:"last_refusal_unix,omitempty"`
}

// extenderActivationRecord is the stored shape: one state per client identity.
// The map is bounded (extenderActivationClientBound).
type extenderActivationRecord struct {
	Clients map[string]extenderActivationState `json:"clients,omitempty"`
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
// because its own state file is read-only), so this line is the only evidence
// the operator gets that the budget went unenforced.
func warnExtenderBudgetUnenforced(client string) {
	extenderBudgetWarnOnce.Do(func() {
		tlog("⚠️ [extender] activation budget unenforced for client %s: provider state store unavailable this run\n", client)
	})
}

func readExtenderActivationRecord(client string) (extenderActivationRecord, bool) {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnenforced(client)
		return extenderActivationRecord{}, false
	}
	var record extenderActivationRecord
	if !store.load(providerStoreBucketState, providerStoreKeyExtenderActivation, &record) {
		return extenderActivationRecord{}, false
	}
	if record.Clients == nil {
		record.Clients = map[string]extenderActivationState{}
	}
	return record, true
}

func writeExtenderActivationRecord(record extenderActivationRecord, client string) {
	store := providerStoreInstance()
	if store == nil {
		warnExtenderBudgetUnenforced(client)
		return
	}
	if err := store.save(providerStoreBucketState, providerStoreKeyExtenderActivation, record); err != nil {
		warnExtenderBudgetUnenforced(client)
	}
}

// extenderActivationRecordForClient loads the record and adopts a pre-client
// flat record onto client, so every write path persists the new shape and the
// first start after an upgrade charges the history it inherited. It also moves
// what a start spent before it could name its client onto that client.
func extenderActivationRecordForClient(client string) extenderActivationRecord {
	record, _ := readExtenderActivationRecord(client)
	if record.Clients == nil {
		record.Clients = map[string]extenderActivationState{}
	}
	record.adoptLegacy(client)
	record.adoptUnidentified(client)
	return record
}

// adoptLegacy moves a pre-client record onto client. The old record described
// this host's own spending, and the identity an upgrade keeps is the client
// providing now, so its attempts and its refusal streak belong to that client.
func (record *extenderActivationRecord) adoptLegacy(client string) {
	legacy := record.extenderActivationLegacy
	if 0 < len(record.Clients) || !legacy.present() {
		return
	}
	if record.Clients == nil {
		record.Clients = map[string]extenderActivationState{}
	}
	record.Clients[client] = extenderActivationState{
		Attempts:         legacy.Attempts,
		BackoffUntilUnix: legacy.BackoffUntilUnix,
		Refusals:         legacy.Refusals,
		LastRefusalUnix:  legacy.LastRefusalUnix,
	}
	record.extenderActivationLegacy = extenderActivationLegacy{}
}

// adoptUnidentified charges what a start spent before it could name its client
// to the first client it can name. Those posts were this host's, made moments
// before it registered under that client, and the operator counted them at the
// time, so leaving them on the shared key would hand the new client a fresh
// budget for posts it has already sent. A refusal recorded before the identity
// was known carries too: it is the operator holding this host, not the name it
// had not written yet.
func (record *extenderActivationRecord) adoptUnidentified(client string) {
	if client == extenderActivationUnidentifiedClient {
		return
	}
	pending, ok := record.Clients[extenderActivationUnidentifiedClient]
	if !ok {
		return
	}
	delete(record.Clients, extenderActivationUnidentifiedClient)
	state := record.Clients[client]
	state.Attempts = append(state.Attempts, pending.Attempts...)
	if state.BackoffUntilUnix < pending.BackoffUntilUnix {
		state.BackoffUntilUnix = pending.BackoffUntilUnix
	}
	if state.Refusals < pending.Refusals {
		state.Refusals = pending.Refusals
	}
	if state.LastRefusalUnix < pending.LastRefusalUnix {
		state.LastRefusalUnix = pending.LastRefusalUnix
	}
	if state.LastUsedUnix < pending.LastUsedUnix {
		state.LastUsedUnix = pending.LastUsedUnix
	}
	record.Clients[client] = state
}

// extenderActivationClientKey is the record key for one client id. A start
// that cannot read a client credential yet is charged to the unidentified key
// rather than to a guessed identity.
func extenderActivationClientKey(clientId string) string {
	if clientId == "" {
		return extenderActivationUnidentifiedClient
	}
	return clientId
}

// extenderActivationClientId is the client the direct slot provides under: the
// client_id of its durable client credential, which is the credential
// provideClientJWT reuses for that slot and rewrites whenever the identity is
// renewed or replaced. Empty before the host has registered, when the start is
// about to mint a fresh client and there is no identity to charge yet.
func extenderActivationClientId() string {
	// The direct slot's credential path does not depend on the proxy set; only
	// a proxied slot is hashed by it.
	path, err := providerClientJwtPath(providerSlotIdentities{}, nil)
	if err != nil {
		return ""
	}
	token, err := clientauth.ReadToken(path)
	if err != nil {
		return ""
	}
	clientId, err := clientauth.ClientIdFromJwt(token)
	if err != nil {
		return ""
	}
	return clientId.String()
}

// trimExtenderActivationClients bounds the record by dropping the least
// recently used client. keep is the client this call just decided for, so it is
// never the one dropped.
func trimExtenderActivationClients(record *extenderActivationRecord, keep string) {
	for extenderActivationClientBound < len(record.Clients) {
		oldest := ""
		for client, state := range record.Clients {
			if client == keep {
				continue
			}
			if oldest == "" || state.LastUsedUnix < record.Clients[oldest].LastUsedUnix ||
				(state.LastUsedUnix == record.Clients[oldest].LastUsedUnix && client < oldest) {
				oldest = client
			}
		}
		if oldest == "" {
			return
		}
		delete(record.Clients, oldest)
	}
}

// storeExtenderActivation records one client's state at now and persists it.
func storeExtenderActivation(record extenderActivationRecord, client string, state extenderActivationState, now time.Time) {
	state.LastUsedUnix = now.Unix()
	record.Clients[client] = state
	trimExtenderActivationClients(&record, client)
	writeExtenderActivationRecord(record, client)
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
// how long to wait when it may not, and how many posts the client's window
// already holds. It records this start's posts when it allows the role, so the
// caller calls it once, before the slots are built. clientId is the identity
// the direct slot provides under (extenderActivationClientId).
func extenderActivationAllow(now time.Time, posts int, clientId string) (allow bool, wait time.Duration, used int) {
	client := extenderActivationClientKey(clientId)
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	record := extenderActivationRecordForClient(client)
	state := record.Clients[client]
	used = pruneExtenderAttempts(&state, now, extenderActivationWindow)
	if now.Unix() < state.BackoffUntilUnix {
		wait = time.Duration(state.BackoffUntilUnix-now.Unix()) * time.Second
		storeExtenderActivation(record, client, state, now)
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
		storeExtenderActivation(record, client, state, now)
		return false, wait, used
	}
	for i := 0; i < posts; i++ {
		state.Attempts = append(state.Attempts, now.Unix())
	}
	storeExtenderActivation(record, client, state, now)
	return true, 0, used
}

// recordExtenderActivationRefused persists the wait a refusal implies for one
// client, so the next start honours it instead of posting again into the same
// refusal.
func recordExtenderActivationRefused(now time.Time, clientId string) time.Duration {
	client := extenderActivationClientKey(clientId)
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	record := extenderActivationRecordForClient(client)
	state := record.Clients[client]
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
	storeExtenderActivation(record, client, state, now)
	return wait
}

// recordExtenderActivationSucceeded clears what a refusal left behind for one
// client: the activation the operator signed resets that client's budget, so
// the next start may activate again. Other clients in the record are untouched.
func recordExtenderActivationSucceeded(clientId string) {
	client := extenderActivationClientKey(clientId)
	extenderActivationRecordMu.Lock()
	defer extenderActivationRecordMu.Unlock()
	record := extenderActivationRecordForClient(client)
	delete(record.Clients, client)
	writeExtenderActivationRecord(record, client)
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
// the client afresh because the slot may have written its credential since the
// process started.
func (gate *extenderActivationGate) allowLaunch() bool {
	gate.mu.Lock()
	pending, allow := gate.pending, gate.allow
	gate.pending = false
	gate.mu.Unlock()
	if pending {
		return allow
	}
	allow, wait, used := extenderActivationAllow(time.Now(), hostActivationPosts(), extenderActivationClientId())
	if !allow {
		tlog("[extender] activation held: %d of %d posts in the last hour, next in %s\n",
			used, extenderActivationBudget, formatDuration(wait))
	}
	return allow
}
