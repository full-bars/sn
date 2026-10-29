package miner

import (
	"sync"
	"time"
)

// A refusal the listener has decided but the store has not kept is held here,
// for the process rather than for the listener that decided it. Every budget
// transaction charges what is held before it decides anything
// (mutateExtenderActivationRecord), so admission cannot pass a refusal that is
// only in memory: either the transaction keeps the refusal and sees its wait,
// or it fails and holds the role. A worker retries while anything is held, so
// a store that comes back with nothing else asking still keeps the refusal.

// extenderPendingRefusal is one refusal waiting to be kept.
type extenderPendingRefusal struct {
	account string
	attempt string
}

// extenderRefusalRetryInterval paces the worker. A variable so a test can
// shorten it.
var extenderRefusalRetryInterval = providerStoreRetryInterval

var extenderRefusalsHeld struct {
	mu     sync.Mutex
	items  map[string]extenderPendingRefusal
	worker bool
	// generation ends a worker started before a reset.
	generation uint64
}

// holdExtenderRefusal keeps a decided refusal until a transaction charges it.
func holdExtenderRefusal(account, attempt string) {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	if extenderRefusalsHeld.items == nil {
		extenderRefusalsHeld.items = make(map[string]extenderPendingRefusal)
	}
	extenderRefusalsHeld.items[attempt] = extenderPendingRefusal{account: account, attempt: attempt}
}

// snapshotExtenderRefusals is what the next transaction charges.
func snapshotExtenderRefusals() []extenderPendingRefusal {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	pending := make([]extenderPendingRefusal, 0, len(extenderRefusalsHeld.items))
	for _, refusal := range extenderRefusalsHeld.items {
		pending = append(pending, refusal)
	}
	return pending
}

// settleExtenderRefusals drops refusals a committed transaction has kept.
func settleExtenderRefusals(kept []extenderPendingRefusal) {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	for _, refusal := range kept {
		delete(extenderRefusalsHeld.items, refusal.attempt)
	}
}

func extenderRefusalsHeldCount() int {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	return len(extenderRefusalsHeld.items)
}

// flushExtenderRefusals tries to keep what is held, and starts the worker when
// the store will not take it.
func flushExtenderRefusals() {
	if keepExtenderRefusals() == nil {
		return
	}
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	if !extenderRefusalsHeld.worker {
		extenderRefusalsHeld.worker = true
		go retryExtenderRefusals(extenderRefusalsHeld.generation)
	}
}

// keepExtenderRefusals runs an empty transaction, which charges what is held.
func keepExtenderRefusals() error {
	pending := snapshotExtenderRefusals()
	if len(pending) == 0 {
		return nil
	}
	return mutateExtenderActivationRecord(extenderActivationAccountKey(pending[0].account), func(*extenderActivationRecord) error { return nil })
}

func retryExtenderRefusals(generation uint64) {
	for {
		time.Sleep(extenderRefusalRetryInterval)
		extenderRefusalsHeld.mu.Lock()
		if generation != extenderRefusalsHeld.generation {
			extenderRefusalsHeld.mu.Unlock()
			return
		}
		extenderRefusalsHeld.mu.Unlock()
		_ = keepExtenderRefusals()
		extenderRefusalsHeld.mu.Lock()
		if generation != extenderRefusalsHeld.generation || len(extenderRefusalsHeld.items) == 0 {
			if generation == extenderRefusalsHeld.generation {
				extenderRefusalsHeld.worker = false
			}
			extenderRefusalsHeld.mu.Unlock()
			return
		}
		extenderRefusalsHeld.mu.Unlock()
	}
}

// resetExtenderRefusals forgets what is held and ends the worker. Tests use it
// between state directories.
func resetExtenderRefusals() {
	extenderRefusalsHeld.mu.Lock()
	defer extenderRefusalsHeld.mu.Unlock()
	extenderRefusalsHeld.items = nil
	extenderRefusalsHeld.worker = false
	extenderRefusalsHeld.generation++
}
