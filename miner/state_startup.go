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
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
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

func writeStateStoreMetrics(b *strings.Builder) {
	degraded := 0
	if providerStoreRunningDegraded() {
		degraded = 1
	}
	fmt.Fprintf(b, "# HELP urnet_state_store_degraded 1 when this run started without a state store it could open.\n")
	fmt.Fprintf(b, "# TYPE urnet_state_store_degraded gauge\n")
	fmt.Fprintf(b, "urnet_state_store_degraded %d\n", degraded)
}
