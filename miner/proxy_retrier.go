package miner

import (
	"context"
	"sync"
	"time"
)

// proxyRetrier re-runs a reload after a slot's worker failed to start. Each key
// gets a few attempts, spaced one second further apart each time, and every
// attempt runs on the context of the run that scheduled it: a retry must not
// bring a slot up after its supervisor has given up.
type proxyRetrier struct {
	ctx    context.Context
	join   *slotJoin
	reload func(context.Context) error
	// wait is how long to hold off before attempt number attempt.
	wait func(attempt int) time.Duration

	mu       sync.Mutex
	attempts map[string]int
}

const proxyRetryMaxAttempts = 3

func newProxyRetrier(ctx context.Context, join *slotJoin, reload func(context.Context) error) *proxyRetrier {
	return &proxyRetrier{
		ctx:      ctx,
		join:     join,
		reload:   reload,
		wait:     func(attempt int) time.Duration { return time.Duration(attempt) * time.Second },
		attempts: map[string]int{},
	}
}

func (p *proxyRetrier) retry(key string) {
	p.mu.Lock()
	p.attempts[key]++
	attempt := p.attempts[key]
	p.mu.Unlock()
	if attempt > proxyRetryMaxAttempts {
		tlog("[proxy][retry] stopped retrying %s after three attempts\n", proxyKeyDisplay(key))
		return
	}
	// The retry counts as a running worker, so the run cannot end between the
	// failure and the retry. The failing worker is still registered when this
	// runs, so the count never touches zero in between.
	if !p.join.add() {
		return
	}
	// Recompute desired state and eligibility before restarting anything.
	go func() {
		defer p.join.done()
		timer := time.NewTimer(p.wait(attempt))
		defer timer.Stop()
		select {
		case <-p.ctx.Done():
			return
		case <-timer.C:
		}
		if err := p.reload(p.ctx); err != nil {
			if p.ctx.Err() != nil {
				return
			}
			tlog("[proxy][retry] reload for %s failed: %v\n", proxyKeyDisplay(key), err)
			// A failed reload applied nothing, so nothing else will bring the
			// slot back: try again until the attempts run out.
			p.retry(key)
		}
	}()
}
