// The automatic 3.23-fix identity import narrates itself at default verbosity:
// detection, mapping, the bounded proving phase with its waits and heartbeats,
// and a final summary. The decision records keep going through the provider
// diagnostics exporter; these lines are the human-readable story beside the
// [proxy]/[pool]/[reuse] pipeline lines, so an operator can see the import is
// alive during the seconds-to-minutes it runs instead of watching a silent
// process.
package miner

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// legacyImportLogWriter receives the import's narration lines. stderr by
// default (the same stream tlog uses), so the lines land in the operator log
// without any verbosity flag; a variable so tests capture it.
var legacyImportLogWriter io.Writer = os.Stderr

const legacyImportLinePrefix = "[import] "

// legacyImportWaitInterval is the per-kind emission floor for wait lines: a
// retry storm inside one wait names itself once per window, with the
// suppressed count attached to the next emission (the same pattern as the
// control-dial evidence lines).
const legacyImportWaitInterval = 5 * time.Second

// legacyImportProgress is the running proof tally a heartbeat reports.
// proven/fallback/skipped are resolved slots; pending is the rest.
type legacyImportProgress struct {
	proven   int
	fallback int
	skipped  int
	pending  int
}

func (self legacyImportProgress) resolved() int {
	return self.proven + self.fallback + self.skipped
}

// legacyImportNarrator writes the import's lines. Safe for the concurrent
// proof workers; the only state is the wait throttle.
type legacyImportNarrator struct {
	mu         sync.Mutex
	w          io.Writer
	now        func() time.Time
	lastWait   map[string]time.Time
	suppressed map[string]int
}

func newLegacyImportNarrator(w io.Writer) *legacyImportNarrator {
	if w == nil {
		w = io.Discard
	}
	return &legacyImportNarrator{
		w:          w,
		now:        time.Now,
		lastWait:   map[string]time.Time{},
		suppressed: map[string]int{},
	}
}

// line writes one complete narration line. nil is silent, so an options
// struct that predates the narrator cannot panic.
func (self *legacyImportNarrator) line(format string, args ...any) {
	if self == nil {
		return
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	fmt.Fprintf(self.w, legacyImportLinePrefix+format+"\n", args...)
}

// wait names one wait the import is blocked on. Repeats of the same kind
// inside the throttle window are counted and reported on the next emission.
func (self *legacyImportNarrator) wait(kind, detail string) {
	if self == nil {
		return
	}
	self.mu.Lock()
	now := self.now()
	last, seen := self.lastWait[kind]
	if seen && now.Sub(last) < legacyImportWaitInterval {
		self.suppressed[kind]++
		self.mu.Unlock()
		return
	}
	suppressed := self.suppressed[kind]
	self.suppressed[kind] = 0
	self.lastWait[kind] = now
	self.mu.Unlock()
	if 0 < suppressed {
		self.line("%s (%d more suppressed)", legacyImportWaitLine(kind, detail), suppressed)
		return
	}
	self.line("%s", legacyImportWaitLine(kind, detail))
}

func (self *legacyImportNarrator) detection(storeName string, entries int, keyShape, marker, network string) {
	self.line("%s", legacyImportDetectionLine(storeName, entries, keyShape, marker, network))
}

func (self *legacyImportNarrator) mapping(configured, mapped, custodied, absent int) {
	self.line("%s", legacyImportMappingLine(configured, mapped, custodied, absent))
}

func (self *legacyImportNarrator) slotOutcome(display, outcome, detail string) {
	self.line("%s", legacyImportSlotOutcomeLine(display, outcome, detail))
}

func (self *legacyImportNarrator) heartbeat(p legacyImportProgress, elapsed time.Duration) {
	self.line("%s", legacyImportHeartbeatLine(p, elapsed))
}

func (self *legacyImportNarrator) summary(imported, fallback, skipped, left int, elapsed time.Duration, result string) {
	self.line("%s", legacyImportSummaryLine(imported, fallback, skipped, left, elapsed, result))
}

// --- pure line helpers ------------------------------------------------------
//
// Each returns the message without the "[import] " prefix and without the
// trailing newline, so a test can assert the exact shape.

// legacyImportDetectionLine names the state the import found before it acts.
// keyShape is "zero" or "normal" for the retained provider seed; marker is the
// .provider.key.identity marker's presence; network is whether the retained
// sign-in carries a network id.
func legacyImportDetectionLine(storeName string, entries int, keyShape, marker, network string) string {
	return fmt.Sprintf("detection: store=%s entries=%d key=%s marker=%s network=%s",
		storeName, entries, keyShape, marker, network)
}

// legacyImportMappingLine names how the run's configured slots mapped onto the
// saved store: mapped slots have a saved entry and no sn credential yet,
// already-custodied slots already own an sn credential file, and
// no-store-entry slots are configured but absent from the store.
func legacyImportMappingLine(configured, mapped, custodied, absent int) string {
	return fmt.Sprintf("mapping: slots=%d configured, %d mapped, %d already-custodied, %d no-store-entry",
		configured, mapped, custodied, absent)
}

// legacyImportWaitLine names one wait: "waiting for <kind> (<detail>)".
func legacyImportWaitLine(kind, detail string) string {
	if detail == "" {
		return "waiting for " + kind
	}
	return fmt.Sprintf("waiting for %s (%s)", kind, detail)
}

// legacyImportHeartbeatLine is the periodic proof tally with its elapsed time.
func legacyImportHeartbeatLine(p legacyImportProgress, elapsed time.Duration) string {
	return fmt.Sprintf("heartbeat: proven=%d fallback=%d skipped=%d pending=%d elapsed=%s",
		p.proven, p.fallback, p.skipped, p.pending, legacyImportElapsed(elapsed))
}

// legacyImportSummaryLine is the final account: what was imported, what
// decided to fall back, what was left alone, how long the import ran, and the
// bound that ended it.
func legacyImportSummaryLine(imported, fallback, skipped, left int, elapsed time.Duration, result string) string {
	return fmt.Sprintf("summary: imported=%d fallback=%d skipped=%d left=%d elapsed=%s result=%s",
		imported, fallback, skipped, left, legacyImportElapsed(elapsed), result)
}

// legacyImportSlotOutcomeLine names one slot's result. display is "direct" or
// "slot proxy:<address>" (never credentials).
func legacyImportSlotOutcomeLine(display, outcome, detail string) string {
	if detail == "" {
		return fmt.Sprintf("%s: %s", display, outcome)
	}
	return fmt.Sprintf("%s: %s %s", display, outcome, detail)
}

// legacyImportElapsed renders a duration to a tenth of a second: enough to
// read a cadence, never a false precision.
func legacyImportElapsed(d time.Duration) string {
	return d.Round(100 * time.Millisecond).String()
}

// legacyImportBound renders a bound exactly: an operator reading a timeout or
// a budget must see the number the code enforces, not a rounded
// approximation (150ms rounds to 200ms under legacyImportElapsed).
func legacyImportBound(d time.Duration) string {
	return d.String()
}

// legacyImportID8 is the display prefix of an identity string: enough to
// correlate a detection or a result with the store, never a full credential.
func legacyImportID8(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}
