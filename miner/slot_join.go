package miner

import "sync"

// slotJoin counts every worker the provide run is waiting on: the slots it
// launched at start, the slots a reload started later, and the retries it has
// scheduled for a worker that failed. The run ends when the count reaches zero
// and no reload is in flight.
//
// A plain sync.WaitGroup over the launch loop ends the run as soon as the
// original slots return, which discards a replacement a reload has just
// started and a retry that has not fired yet.
type slotJoin struct {
	mu     sync.Mutex
	n      int
	sealed bool
	wake   chan struct{}
}

func newSlotJoin() *slotJoin {
	return &slotJoin{wake: make(chan struct{}, 1)}
}

// add registers one worker. It reports false once the join has been released,
// when the run is already ending and nothing new may start.
func (j *slotJoin) add() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.sealed {
		return false
	}
	j.n++
	return true
}

// done releases one worker registered with add.
func (j *slotJoin) done() {
	j.mu.Lock()
	j.n--
	zero := j.n == 0
	j.mu.Unlock()
	if zero {
		select {
		case j.wake <- struct{}{}:
		default:
		}
	}
}

// wait blocks until every registered worker has finished. quiesce runs each
// time the count reaches zero and must return only when any reload that was
// already running has finished: that reload may be about to add the
// replacement for the worker that just returned, so zero is only believed
// after it. wait then seals the join, so a late add is refused.
func (j *slotJoin) wait(quiesce func()) {
	for {
		j.mu.Lock()
		n := j.n
		j.mu.Unlock()
		if n > 0 {
			<-j.wake
			continue
		}
		if quiesce != nil {
			quiesce()
		}
		j.mu.Lock()
		if j.n == 0 {
			j.sealed = true
			j.mu.Unlock()
			return
		}
		j.mu.Unlock()
	}
}
