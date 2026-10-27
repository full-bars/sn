package miner

// identity_events.go keeps one durable record of every identity outcome a
// start produced, in the provider's bbolt store. A host whose state directory
// is not the one holding its identities mints every slot and looks healthy by
// every other measure, and the only account of it was one line per slot in a
// log nobody watches. The events let that start be read back afterwards.
//
// Only the four outcomes that change which identity a slot serves under are
// recorded. A saved credential the platform accepted is not an event, so a
// host with intact state writes nothing at all.

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/urfoundation/sn/clientauth"
)

const (
	// providerStoreBucketIdentityEvents holds the identity outcome records.
	providerStoreBucketIdentityEvents = "identity_events"
)

// identityEventKeep bounds the bucket. A start on a host that mints every slot
// writes one record per slot, so the bucket would otherwise grow for the life
// of the file; the newest records are kept and the oldest are dropped. A
// variable so a test can reach the bound without writing it out.
var identityEventKeep = 2000

// minerIdentityOutcome names one recorded outcome. The values are the words
// the provider docs use, so a reader of the store and a reader of the log
// agree on what happened.
type minerIdentityOutcome string

const (
	identityOutcomeMintedNew         minerIdentityOutcome = "minted_new"
	identityOutcomeMintedReplacement minerIdentityOutcome = "minted_replacement"
	identityOutcomeAdopted           minerIdentityOutcome = "adopted"
	identityOutcomeRevoked           minerIdentityOutcome = "revoked"
)

// providerIdentityEvent is one slot's identity outcome: which slot, what
// happened to its identity, why when the path had a reason, and when.
type providerIdentityEvent struct {
	At      time.Time `json:"at"`
	Slot    string    `json:"slot"`
	Outcome string    `json:"outcome"`
	Reason  string    `json:"reason,omitempty"`
}

// minerIdentityOutcomeOf maps the registration path's outcome onto a recorded
// one. false means the attempt decided nothing worth recording: the saved
// credential was reused, a retained operation completed as the identity the
// server already bound, or no decision was made at all.
func minerIdentityOutcomeOf(outcome clientauth.IdentityOutcome) (minerIdentityOutcome, bool) {
	switch outcome {
	case clientauth.IdentityOutcomeMintedNew:
		return identityOutcomeMintedNew, true
	case clientauth.IdentityOutcomeMintedReplacement:
		return identityOutcomeMintedReplacement, true
	case clientauth.IdentityOutcomeRevoked:
		return identityOutcomeRevoked, true
	}
	return "", false
}

// identityEventStoreOnce reports a store that will not take the records once,
// however many slots follow it into the failure.
var identityEventStoreOnce sync.Once

// recordProviderIdentityEvent appends one event to the store. The store is an
// aid, never a dependency: the slot's identity is already decided, saved and
// serving when this runs, so a failed open or write costs the durable copy and
// nothing else.
func recordProviderIdentityEvent(display string, outcome minerIdentityOutcome, reason string) {
	recordProviderIdentityEvents([]string{display}, outcome, reason)
}

// recordProviderIdentityEvents appends one record per display in a single
// transaction. The automatic legacy import hands over a whole pool before any
// worker runs, and a commit per slot would put a disk sync per identity in
// front of the first proxy; a pool of 500 adopted identities on block storage
// is seconds of startup for records that say one thing.
func recordProviderIdentityEvents(displays []string, outcome minerIdentityOutcome, reason string) {
	if len(displays) == 0 {
		return
	}
	store := providerStoreInstance()
	if store == nil {
		// providerStoreInstance has already reported and counted the failure.
		return
	}
	at := time.Now().UTC()
	events := make([]any, 0, len(displays))
	for _, display := range displays {
		events = append(events, providerIdentityEvent{At: at, Slot: display, Outcome: string(outcome), Reason: reason})
	}
	if err := store.appendBoundedMany(providerStoreBucketIdentityEvents, events, identityEventKeep); err != nil {
		identityEventStoreOnce.Do(func() {
			tlog("⚠️ [identity] outcomes not recorded: %v\n", err)
		})
	}
}

// providerIdentityEvents returns up to limit of the newest recorded outcomes,
// oldest first, for a reader such as an urnet-tools command. A negative limit
// is every record the bucket holds.
func providerIdentityEvents(limit int) ([]providerIdentityEvent, error) {
	store := providerStoreInstance()
	if store == nil {
		return nil, nil
	}
	records, err := store.tail(providerStoreBucketIdentityEvents, limit)
	if err != nil {
		return nil, err
	}
	events := make([]providerIdentityEvent, 0, len(records))
	for _, record := range records {
		var event providerIdentityEvent
		// A record a later build shaped differently is skipped rather than
		// allowed to hide the rest of the history.
		if json.Unmarshal(record, &event) != nil || event.Outcome == "" {
			continue
		}
		events = append(events, event)
	}
	return events, nil
}
