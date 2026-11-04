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

const (
	hotSwapStampNone        = "none"
	hotSwapStampUnavailable = "unavailable"
	hotSwapStampPrefix      = "stamp:"
)

var (
	providerStoreDegraded     atomic.Bool
	providerStoreDegradedOnce sync.Once
)

// providerStoreRunningDegraded reports that this run started without a state
// store it could open.
func providerStoreRunningDegraded() bool { return providerStoreDegraded.Load() }

// noteProviderStoreDegraded raises the flag and logs the loud line, once per
// process.
func noteProviderStoreDegraded(cause error) {
	providerStoreDegraded.Store(true)
	providerStoreDegradedOnce.Do(func() {
		critLog("PROVIDER DEGRADED: state store unavailable, continuing without it (%v); its schema was not checked", cause)
	})
}

func resetProviderStoreDegradedForTest() {
	providerStoreDegraded.Store(false)
	providerStoreDegradedOnce = sync.Once{}
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
// it holds open.
func hotSwapParentStoreStamp() string {
	store, err := openProviderStoreForProcess()
	if err != nil || store == nil {
		return hotSwapStampUnavailable
	}
	var raw []byte
	store.mu.Lock()
	if store.db == nil {
		store.mu.Unlock()
		return hotSwapStampUnavailable
	}
	_ = store.db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket([]byte(providerStoreBucketMeta)); b != nil {
			raw = append([]byte(nil), b.Get([]byte(providerStoreKeySchema))...)
		}
		return nil
	})
	store.mu.Unlock()
	if len(raw) == 0 {
		return hotSwapStampNone
	}
	return hotSwapStampPrefix + string(raw)
}

// hotSwapCandidateStoreVerdict runs in the candidate before READY. A newer
// stamp is the same refusal a normal start gives. A parent that could not read
// its store, or an older parent that sent nothing, leaves the schema unchecked:
// the candidate goes on degraded rather than turning a handoff into downtime.
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
	fmt.Fprintf(b, "# HELP urnet_state_store_degraded 1 when this run started without a state store it could open.\n")
	fmt.Fprintf(b, "# TYPE urnet_state_store_degraded gauge\n")
	fmt.Fprintf(b, "urnet_state_store_degraded %d\n", degraded)
}
