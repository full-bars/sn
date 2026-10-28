package miner

import (
	"sync"
	"testing"
	"time"
)

func joinReturns(j *slotJoin, quiesce func()) <-chan struct{} {
	returned := make(chan struct{})
	go func() {
		j.wait(quiesce)
		close(returned)
	}()
	return returned
}

// The run must outlive its original slots while a reload has started a
// replacement: the original worker returning is not the end of the run.
func TestSlotJoinOutlivesOriginalWorkersWhileReplacementRuns(t *testing.T) {
	j := newSlotJoin()
	j.add() // original slot
	returned := joinReturns(j, nil)

	if !j.add() { // reload starts a replacement before the original returns
		t.Fatal("add refused while the run is live")
	}
	j.done() // original slot returns

	select {
	case <-returned:
		t.Fatal("run ended while a reloaded worker was still running")
	case <-time.After(50 * time.Millisecond):
	}

	j.done()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("run did not end after the last worker returned")
	}
}

// A retry is scheduled by the failing worker before it returns, so a lone
// failing slot keeps the run alive until the retry has run.
func TestSlotJoinWaitsForScheduledRetry(t *testing.T) {
	j := newSlotJoin()
	j.add() // the only slot
	returned := joinReturns(j, nil)

	j.add()  // its failure schedules a retry
	j.done() // the slot returns

	select {
	case <-returned:
		t.Fatal("run ended before the scheduled retry ran")
	case <-time.After(50 * time.Millisecond):
	}
	j.done() // retry finished, nothing started
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("run did not end after the retry finished")
	}
}

// A reload that cancels the last worker and adds its replacement does both
// under the reloader's lock. Zero is not believed until that lock is free.
func TestSlotJoinBelievesZeroOnlyAfterReloadFinishes(t *testing.T) {
	j := newSlotJoin()
	j.add()
	reloadInFlight := make(chan struct{})
	finishReload := make(chan struct{})
	var once sync.Once
	quiesce := func() {
		once.Do(func() {
			close(reloadInFlight)
			<-finishReload
		})
	}
	returned := joinReturns(j, quiesce)

	j.done() // old worker returns first
	<-reloadInFlight
	if !j.add() { // the reload's replacement
		t.Fatal("replacement refused while a reload was in flight")
	}
	close(finishReload)

	select {
	case <-returned:
		t.Fatal("run ended although the reload had started a replacement")
	case <-time.After(50 * time.Millisecond):
	}
	j.done()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("run did not end after the replacement returned")
	}
	if j.add() {
		t.Fatal("add accepted after the run ended")
	}
}
