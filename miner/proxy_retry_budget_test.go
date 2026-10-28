package miner

import (
	"testing"
	"time"
)

// A crash loop must still exhaust its budget, or a proxy that authenticates and
// immediately dies would be retried forever.
func TestProxyRetryBudgetStopsACrashLoop(t *testing.T) {
	b := newProxyRetryBudget()
	const key = "10.0.0.1:1080:user:pass"
	for want := 1; want <= maxProxyWorkerRetries; want++ {
		attempt, retry := b.next(key, time.Second)
		if attempt != want || !retry {
			t.Fatalf("attempt %d of a rapid loop = (%d, %v), want (%d, true)", want, attempt, retry, want)
		}
	}
	if attempt, retry := b.next(key, time.Second); retry || attempt != maxProxyWorkerRetries+1 {
		t.Fatalf("attempt past the budget = (%d, %v), want (%d, false): a crash loop must stop", attempt, retry, maxProxyWorkerRetries+1)
	}
}

// The defect this replaces: the count was a lifetime total, so a proxy that
// failed once a day stopped being retried from the fourth day on. An exit after
// a stable run is a new fault and must start a fresh budget.
func TestProxyRetryBudgetResetsAfterAStableRun(t *testing.T) {
	b := newProxyRetryBudget()
	const key = "10.0.0.2:1080:user:pass"

	// Three isolated failures, each after a long healthy run, spread over what
	// would be days of uptime. The old lifetime counter refused the fourth.
	for day := 1; day <= 6; day++ {
		if _, retry := b.next(key, 24*time.Hour); !retry {
			t.Fatalf("day %d: an isolated failure after a stable run was not retried; the budget is still a lifetime total", day)
		}
	}
}

// A run just short of the stability threshold must NOT reset, so a worker that
// dies a moment before it would have proven itself keeps climbing the budget.
func TestProxyRetryBudgetJustUnderThresholdDoesNotReset(t *testing.T) {
	b := newProxyRetryBudget()
	const key = "10.0.0.3:1080:user:pass"
	almost := proxyWorkerStableAfter - time.Millisecond
	for want := 1; want <= maxProxyWorkerRetries; want++ {
		if attempt, retry := b.next(key, almost); attempt != want || !retry {
			t.Fatalf("attempt %d after a nearly-stable run = (%d, %v), want (%d, true)", want, attempt, retry, want)
		}
	}
	if _, retry := b.next(key, almost); retry {
		t.Fatal("a run under the stability threshold reset the budget, so a slow crash loop would retry forever")
	}
}

// A deliberately stopped worker must not inherit an old count when restarted.
func TestProxyRetryBudgetForgetClears(t *testing.T) {
	b := newProxyRetryBudget()
	const key = "10.0.0.4:1080:user:pass"
	for i := 0; i < maxProxyWorkerRetries; i++ {
		b.next(key, time.Second)
	}
	b.forget(key)
	if attempt, retry := b.next(key, time.Second); attempt != 1 || !retry {
		t.Fatalf("after forget, attempt = (%d, %v), want (1, true)", attempt, retry)
	}
}
