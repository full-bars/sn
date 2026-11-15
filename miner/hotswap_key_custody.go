package miner

import (
	"sync"
	"time"
)

// The provider key's registration lock is taken without waiting, so a hot swap
// candidate cannot open the key while the retiring process holds it. The
// parent lets go of the lock when it sends TAKEOVER, next to provider_state.db,
// and takes it back when the handoff aborts and it keeps serving.

type hotSwapKeyCustody interface {
	ReleaseForHotSwap() error
	ReacquireAfterAbortedHotSwap() error
}

var (
	hotSwapKeyCustodyMu       sync.Mutex
	hotSwapKeyCustodyHeld     hotSwapKeyCustody
	hotSwapKeyCustodyReleased bool

	// keyCustodyRetakeWait bounds how long an aborted handoff waits for the
	// killed candidate to exit and free the lock.
	keyCustodyRetakeWait = 5 * time.Second
	keyCustodyRetakePoll = 50 * time.Millisecond
)

// setHotSwapKeyCustody registers the key owner this process holds. A nil owner
// clears it.
func setHotSwapKeyCustody(custody hotSwapKeyCustody) {
	hotSwapKeyCustodyMu.Lock()
	defer hotSwapKeyCustodyMu.Unlock()
	hotSwapKeyCustodyHeld = custody
	hotSwapKeyCustodyReleased = false
}

// releaseKeyCustodyForCandidate gives the key's owner lock to the candidate.
// A process without a registered key has nothing to give.
func releaseKeyCustodyForCandidate() error {
	hotSwapKeyCustodyMu.Lock()
	defer hotSwapKeyCustodyMu.Unlock()
	if hotSwapKeyCustodyHeld == nil || hotSwapKeyCustodyReleased {
		return nil
	}
	if err := hotSwapKeyCustodyHeld.ReleaseForHotSwap(); err != nil {
		return err
	}
	hotSwapKeyCustodyReleased = true
	hotswapLog("⚡ [hotswap] Released the provider key lock for the candidate\n")
	return nil
}

// restoreKeyCustodyAfterAbortedHandoff takes the key's owner lock back. The
// candidate is killed before this runs, so the lock frees as soon as it exits.
func restoreKeyCustodyAfterAbortedHandoff() error {
	hotSwapKeyCustodyMu.Lock()
	defer hotSwapKeyCustodyMu.Unlock()
	if hotSwapKeyCustodyHeld == nil || !hotSwapKeyCustodyReleased {
		return nil
	}
	deadline := time.Now().Add(keyCustodyRetakeWait)
	for {
		err := hotSwapKeyCustodyHeld.ReacquireAfterAbortedHotSwap()
		if err == nil {
			hotSwapKeyCustodyReleased = false
			hotswapLog("⚡ [hotswap] Handoff aborted: took the provider key lock back\n")
			return nil
		}
		if !time.Now().Before(deadline) {
			hotswapLog("❌ [hotswap] Handoff aborted but the provider key lock did not come back (%v): the parent keeps serving without it\n", err)
			return err
		}
		time.Sleep(keyCustodyRetakePoll)
	}
}
