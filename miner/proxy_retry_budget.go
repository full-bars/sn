package miner

import (
	"sync"
	"time"
)

const (
	// maxProxyWorkerRetries is how many times one worker's unexpected exit is
	// retried before the miner stops trying.
	maxProxyWorkerRetries = 3

	// proxyWorkerStableAfter is how long a worker must have run for its exit to
	// count as a NEW fault rather than the next step of the same crash loop.
	//
	// Without this the attempt count was a lifetime total per key, so a proxy
	// that failed once a week accumulated three failures over three weeks and
	// then stopped being retried for the rest of the process's life: the retry
	// existed precisely to bring back the workers that matter, and it gave up on
	// the ones that mostly worked. Thirty seconds is well past an auth
	// handshake and well inside a healthy run, so a crash loop still exhausts
	// its budget while an isolated failure starts fresh.
	proxyWorkerStableAfter = 30 * time.Second
)

// proxyRetryBudget holds per-key retry attempts for workers that exited
// unexpectedly. It is deliberately not a lifetime counter: an exit after a
// stable run resets the key.
type proxyRetryBudget struct {
	mu       sync.Mutex
	attempts map[string]int
}

func newProxyRetryBudget() *proxyRetryBudget {
	return &proxyRetryBudget{attempts: map[string]int{}}
}

// next records one unexpected exit for key and reports the attempt number and
// whether it should be retried. uptime is how long the worker ran before
// exiting; a run at or past proxyWorkerStableAfter starts a fresh budget.
func (b *proxyRetryBudget) next(key string, uptime time.Duration) (attempt int, retry bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.attempts == nil {
		b.attempts = map[string]int{}
	}
	if uptime >= proxyWorkerStableAfter {
		delete(b.attempts, key)
	}
	b.attempts[key]++
	attempt = b.attempts[key]
	return attempt, attempt <= maxProxyWorkerRetries
}

// forget drops a key's budget, for a worker that was deliberately stopped so it
// cannot inherit an old crash-loop count when it is started again.
func (b *proxyRetryBudget) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.attempts, key)
}
