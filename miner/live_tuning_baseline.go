package miner

import "sync"

// liveTuningBaseline remembers the Go runtime limits that were in force before
// the first control-socket write to them, so a clear can put them back. The
// control keys gomemlimit and gogc document a clear as "back to the startup
// default"; the startup default is whatever the memory plan, a profile tier or
// the environment installed, which the key itself does not record. Only the
// first remembered value counts: a later write sees the operator's own value
// as the previous one.
var liveTuningBaseline liveTuningBaselineState

type liveTuningBaselineState struct {
	mu     sync.Mutex
	memSet bool
	mem    int64
	gcSet  bool
	gc     int
}

func (b *liveTuningBaselineState) rememberMemLimit(prev int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.memSet {
		b.mem, b.memSet = prev, true
	}
}

func (b *liveTuningBaselineState) memLimit() (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mem, b.memSet
}

func (b *liveTuningBaselineState) rememberGCPercent(prev int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.gcSet {
		b.gc, b.gcSet = prev, true
	}
}

func (b *liveTuningBaselineState) gcPercent() (int, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.gc, b.gcSet
}

// gcPercentOr is the remembered percentage, or def when none was recorded.
func (b *liveTuningBaselineState) gcPercentOr(def int) int {
	if p, ok := b.gcPercent(); ok {
		return p
	}
	return def
}
