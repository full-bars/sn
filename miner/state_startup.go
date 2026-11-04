package miner

// state_startup.go decides what a start does about the state store before any
// slot runs, and keeps the two outcomes apart:
//
//   - a store that opens and carries a stamp newer than this build reads is a
//     correctness failure. The start is refused, so no slot runs and no identity
//     is minted.
//   - a store that cannot be opened (lock held, I/O error, permission) is an
//     availability problem. The start goes on without it, says so once, and
//     keeps a flag the scrape shows. Nothing is known about its schema then, and
//     nothing here claims otherwise.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"go.etcd.io/bbolt"
)

// EnvHotSwapStoreStamp carries the parent's view of the store stamp to a
// hotswap candidate. The parent holds the store's write lock until takeover, so
// the candidate cannot open the file; it judges the stamp it is handed with its
// own reader level, before it announces READY.
const EnvHotSwapStoreStamp = "URNETWORK_HOTSWAP_STORE_STAMP"

// The parent's report is one of these. "none" means only that the store was
// read and carries no stamp. A stamp the parent could not read is never "none":
// it is "refused:" (the open refused a newer store, carrying the guard's own
// message) or "unreadable:" (the read failed, carrying why).
const (
	hotSwapStampNone             = "none"
	hotSwapStampUnavailable      = "unavailable"
	hotSwapStampPrefix           = "stamp:"
	hotSwapStampRefusedPrefix    = "refused:"
	hotSwapStampUnreadablePrefix = "unreadable:"
)

var (
	providerStoreDegraded        atomic.Bool
	providerStoreDegradedMu      sync.Mutex
	providerStoreDegradedReasons []string
)

// providerStoreRunningDegraded reports that this run started without a state
// store it could open, or on one it could not fully trust.
func providerStoreRunningDegraded() bool { return providerStoreDegraded.Load() }

// providerStoreDegradedReasonList lists why the run is degraded, in the order the
// reasons were first noted.
func providerStoreDegradedReasonList() []string {
	providerStoreDegradedMu.Lock()
	defer providerStoreDegradedMu.Unlock()
	return append([]string(nil), providerStoreDegradedReasons...)
}

// noteProviderStoreDegradedFor raises the flag and logs the loud line the first
// time each distinct reason is noted, so a second cause is not hidden behind
// the first.
func noteProviderStoreDegradedFor(reason string) {
	providerStoreDegraded.Store(true)
	providerStoreDegradedMu.Lock()
	for _, seen := range providerStoreDegradedReasons {
		if seen == reason {
			providerStoreDegradedMu.Unlock()
			return
		}
	}
	providerStoreDegradedReasons = append(providerStoreDegradedReasons, reason)
	providerStoreDegradedMu.Unlock()
	critLog("PROVIDER DEGRADED: %s", reason)
}

// noteProviderStoreDegraded is the store-could-not-be-opened case.
func noteProviderStoreDegraded(cause error) {
	noteProviderStoreDegradedFor(fmt.Sprintf("state store unavailable, continuing without it (%v); its schema was not checked", cause))
}

func resetProviderStoreDegradedForTest() {
	providerStoreDegraded.Store(false)
	providerStoreDegradedMu.Lock()
	providerStoreDegradedReasons = nil
	providerStoreDegradedMu.Unlock()
}

// providerStartupStoreVerdict is the normal start's check. A nil return lets
// the start continue, with the degraded flag raised when the store could not be
// inspected.
func providerStartupStoreVerdict(ctx context.Context) error {
	_, err := openProviderStoreForProcess()
	switch {
	case err == nil:
		return nil
	case errors.Is(err, errProviderStoreNewer):
		critLog("PROVIDER REFUSED: %v", err)
		return startupRefusal(ctx, err)
	default:
		noteProviderStoreDegraded(err)
		return nil
	}
}

// hotSwapParentStoreStamp is what a parent tells its candidate about the store
// it holds open. Only a store that was read and has no stamp reports "none".
func hotSwapParentStoreStamp() string {
	store, err := openProviderStoreForProcess()
	switch {
	case errors.Is(err, errProviderStoreNewer):
		// A refusal is not an availability failure: the candidate must
		// refuse too, with the guard's own message.
		return hotSwapStampRefusedPrefix + err.Error()
	case err != nil || store == nil:
		return hotSwapStampUnavailable
	}
	var raw []byte
	var found bool
	store.mu.Lock()
	if store.db == nil {
		store.mu.Unlock()
		return hotSwapStampUnavailable
	}
	viewErr := store.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte(providerStoreBucketMeta)); b != nil {
			if v := b.Get([]byte(providerStoreKeySchema)); v != nil {
				found = true
				raw = append([]byte(nil), v...)
			}
		}
		return nil
	})
	store.mu.Unlock()
	if viewErr != nil {
		return hotSwapStampUnreadablePrefix + viewErr.Error()
	}
	if !found {
		return hotSwapStampNone
	}
	return hotSwapStampPrefix + string(raw)
}

// hotSwapCandidateStoreVerdict runs in the candidate before READY. A newer
// stamp, or a refusal the parent itself met, is the same refusal a normal start
// gives. A parent that could not open or read its store, or an older parent that
// sent nothing, leaves the schema unchecked: the candidate goes on degraded,
// with the reason recorded, rather than turning a handoff into downtime.
func hotSwapCandidateStoreVerdict() error {
	value, set := os.LookupEnv(EnvHotSwapStoreStamp)
	switch {
	case !set:
		noteProviderStoreDegraded(errors.New("the parent reported no store stamp"))
		return nil
	case value == hotSwapStampNone:
		return nil
	case value == hotSwapStampUnavailable:
		noteProviderStoreDegraded(errors.New("the parent could not open the store"))
		return nil
	case strings.HasPrefix(value, hotSwapStampRefusedPrefix):
		err := &providerStoreNewerError{relayed: strings.TrimPrefix(value, hotSwapStampRefusedPrefix)}
		IncrPersistentError(providerStoreSchemaNewerCategory)
		critLog("PROVIDER REFUSED: %v", err)
		return err
	case strings.HasPrefix(value, hotSwapStampUnreadablePrefix):
		noteProviderStoreDegradedFor(fmt.Sprintf("the parent could not read the store's schema stamp (%s); its schema was not checked",
			strings.TrimPrefix(value, hotSwapStampUnreadablePrefix)))
		return nil
	case !strings.HasPrefix(value, hotSwapStampPrefix):
		noteProviderStoreDegraded(fmt.Errorf("unrecognised store stamp report %q", value))
		return nil
	}
	path, err := providerStatePath(providerStoreFileName)
	if err != nil {
		path = providerStoreFileName
	}
	raw := []byte(strings.TrimPrefix(value, hotSwapStampPrefix))
	if !json.Valid(raw) {
		raw = []byte(`"`) // not decodable: classified as an unreadable stamp
	}
	if err := providerStoreStampVerdict(raw, path); err != nil {
		if errors.Is(err, errProviderStoreNewer) {
			IncrPersistentError(providerStoreSchemaNewerCategory)
			critLog("PROVIDER REFUSED: %v", err)
		}
		return err
	}
	return nil
}

func writeStateStoreMetrics(b *strings.Builder) {
	degraded := 0
	if providerStoreRunningDegraded() {
		degraded = 1
	}
	fmt.Fprintf(b, "# HELP urnet_state_store_degraded 1 when this run started without a state store it could open, or on one it could not fully trust.\n")
	fmt.Fprintf(b, "# TYPE urnet_state_store_degraded gauge\n")
	fmt.Fprintf(b, "urnet_state_store_degraded %d\n", degraded)
}
