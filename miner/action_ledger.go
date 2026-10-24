package miner

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// action_ledger.go ports the v33 provider's capacity-decision ledger
// (provider/action_ledger.go): an append-only, size-bounded JSONL record of
// every AUTOMATIC or operator-issued capacity decision, in one timeline at
// <state dir>/autopilot.jsonl. The RAM log rotates and is noisy; an autonomous
// system is only trustworthy if each thing it did can be audited later.
//
// The control socket serves this file (`ledger`, and `urnet-tools autopilot
// log` above it), so a reader must find the miner's own decisions here, not an
// empty list.
//
// What the miner records - the actions it actually takes:
//
//   - oomcap: the OOM-aware start cap's decisions (reduce/relax/clear/frozen)
//     and the kill switch forgetting a standing cap.
//   - thrash: the swap-thrash watchdog's supervised restart and its refusals
//     to restart (alert).
//   - trim: a binding trim cap's application to the pool - the launch-time hold
//     (startupTrimSelection), and the same hold re-applied to a RUNNING pool
//     when an operator `proxy trim` or an automatic cap arrives as a live
//     control update. Mode carries the binding source (operator/oomcap/
//     thrashcap); the live case is suffixed -live, so the timeline says both
//     who bound the pool and when in the process's life it happened.
//
// What the provider records that the miner does not: the trim "cleared"
// receipt. The miner's reloader applies a desired-set diff and never learns
// which cap caused a shrink or a regrow, so there is no moment that can
// attribute one. An absent entry beats one that guesses the actor.
//
// The wire shape is the provider's, byte for byte, because the two builds
// share `urnet-tools autopilot log` (internal/urnettools/autopilot.go), which
// unmarshals {t, actor, action, from, to, mode, reason}.

const (
	ledgerFileName = "autopilot.jsonl"
	ledgerMaxBytes = 256 << 10

	// trimLiveModeSuffix marks a trim entry the reload path wrote while the
	// provider was already running. The binding source is the prefix, so the
	// entry still says who bound the pool; the suffix says the cap arrived as a
	// live control update rather than in the launch selection.
	trimLiveModeSuffix = "-live"
)

type ledgerEntry struct {
	Time   string `json:"t"`
	Actor  string `json:"actor"`  // oomcap | thrash | trim
	Action string `json:"action"` // reduce | relax | clear | frozen | restart | alert | applied
	From   int    `json:"from"`
	To     int    `json:"to"`
	Mode   string `json:"mode,omitempty"` // shadow | on | operator | oomcap | thrashcap
	Reason string `json:"reason,omitempty"`
}

// ledgerPath is <state dir>/autopilot.jsonl, beside oom_cap.json and the rest
// of the miner's provider files.
func ledgerPath() (string, error) {
	dir, err := oomCapDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ledgerFileName), nil
}

// ledgerAppend adds one entry through the shared append+trim primitive
// (bounded_jsonl.go), which takes the inter-process lock, creates the parent
// directory and rotates to the newest whole lines when the file passes
// maxBytes. The entry's timestamp is the caller's clock, so a decision is
// recorded at the instant it was decided rather than at the instant the write
// landed.
func ledgerAppend(path string, e ledgerEntry, now time.Time, maxBytes int64) error {
	e.Time = now.UTC().Format(time.RFC3339)
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	return appendBoundedLine(path, line, maxBytes)
}

// ledgerTail returns the newest n entries in chronological order. A missing
// file is empty; a line that does not parse is skipped, so one torn write
// cannot hide the rest of the timeline.
func ledgerTail(path string, n int) ([]ledgerEntry, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var all []ledgerEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e ledgerEntry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Actor != "" {
			all = append(all, e)
		}
	}
	if n >= 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return all, sc.Err()
}

// ledgerRecord appends to the default ledger at the caller's clock, so the
// timeline carries the instant the decision was taken rather than the instant
// the write landed. A failed append never blocks or fails the action it
// records: the decision is still taken and still reaches importantLogf/
// events.log, so the only thing lost is the durable copy - which is worth one
// diagnostic line instead of silence.
func ledgerRecord(e ledgerEntry, now time.Time) {
	path, err := ledgerPath()
	if err != nil {
		tlog("[ledger] warn: no ledger path, %s/%s not recorded: %v\n", e.Actor, e.Action, err)
		return
	}
	if err := ledgerAppend(path, e, now, ledgerMaxBytes); err != nil {
		tlog("[ledger] warn: could not record %s/%s: %v\n", e.Actor, e.Action, err)
	}
}

// lastLiveTrim is the (cap, source) application the reload path last wrote.
// Reloads are triggered AND periodic, so without it a standing cap would append
// an identical entry on every pass and bury the decisions the ledger exists
// for. Startup seeds it through ledgerTrimApplied: the launch selection already
// wrote that cap, and the first reload must not write it twice.
var lastLiveTrim struct {
	mu     sync.Mutex
	cap    int
	source string
	set    bool
}

// noteLiveTrimApplied reports whether this (cap, source) pair has not been
// recorded by this process yet, and marks it recorded.
func noteLiveTrimApplied(cap int, source string) bool {
	lastLiveTrim.mu.Lock()
	defer lastLiveTrim.mu.Unlock()
	if lastLiveTrim.set && lastLiveTrim.cap == cap && lastLiveTrim.source == source {
		return false
	}
	lastLiveTrim.cap, lastLiveTrim.source, lastLiveTrim.set = cap, source, true
	return true
}

// resetLiveTrimLedger forgets the recorded live application. Tests only: one
// test's cap must not suppress the next test's entry.
func resetLiveTrimLedger() {
	lastLiveTrim.mu.Lock()
	lastLiveTrim.cap, lastLiveTrim.source, lastLiveTrim.set = 0, "", false
	lastLiveTrim.mu.Unlock()
}

// ledgerTrimApplied records the launch-time trim decision: a binding cap held
// back `held` of `desired` configured proxies. It is the miner's counterpart of
// the provider's trim "applied" receipt, recorded where the cap actually binds
// (the launch selection) instead of in a reload loop the miner does not have.
// Only the start path calls it - a command reader that computed the same
// selection must not append an entry for an action it did not take.
func ledgerTrimApplied(desired, cap int, source string, held int, now time.Time) {
	ledgerRecord(ledgerEntry{
		Actor:  "trim",
		Action: "applied",
		From:   desired,
		To:     cap,
		Mode:   source,
		Reason: fmt.Sprintf("held %d worst-graded at launch until the cap is raised", held),
	}, now)
	noteLiveTrimApplied(cap, source)
}

// ledgerTrimLiveApplied records a binding trim cap the reload path applied to a
// RUNNING pool: an operator `proxy trim`, or an automatic cap, arriving as a
// control update minutes or days into the run. The same startupTrimSelection
// shrinks the pool there, so without this entry an operator who trims a live
// box sees capacity drop out of the timeline with no decision behind it - and
// an audit ledger that omits operator actions is worse than none.
//
// The mode is the binding source plus trimLiveModeSuffix, which is what
// separates this from the launch-time application the same process may already
// have recorded. One entry per (cap, source) change: a periodic reload that
// re-applies the standing cap is not a new decision.
func ledgerTrimLiveApplied(desired, cap int, source string, held int, now time.Time) {
	if !noteLiveTrimApplied(cap, source) {
		return
	}
	ledgerRecord(ledgerEntry{
		Actor:  "trim",
		Action: "applied",
		From:   desired,
		To:     cap,
		Mode:   source + trimLiveModeSuffix,
		Reason: fmt.Sprintf("held %d worst-graded by a live capacity update until the cap is raised", held),
	}, now)
}
