package miner

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// captureLogger records Infof output. The embedded nil Logger is never used:
// the wrapper only forwards Infof here.
type captureLogger struct {
	connect.Logger
	mu    sync.Mutex
	lines []string
}

func (self *captureLogger) Infof(format string, args ...any) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.lines = append(self.lines, fmt.Sprintf(format, args...))
}

func (self *captureLogger) text() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	return strings.Join(self.lines, "")
}

func TestSlotSelectLabelNamesTheProxyOnSuccessAndFailure(t *testing.T) {
	base := &captureLogger{}
	var buf bytes.Buffer
	prev := proxyManagerLogWriter
	proxyManagerLogWriter = &buf
	defer func() { proxyManagerLogWriter = prev }()
	log := withSlotSelectLabel(base, slotSelectLabel(17, "65.111.15.115:1081"), nil)

	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "normal success=2 error=0", int64(153))
	log.Infof("🌐 [net][s]select: %s = %s dur=%dms\n", "fragment success=0 error=3", "i/o timeout", int64(15000))
	log.Infof("unrelated %d\n", 7)

	// Select lines carry the slot label and no wrapper location; anything else
	// on the logger still passes through untouched.
	text := buf.String()
	for _, want := range []string{
		"🌐 [net][s]select: proxy[17] (65.111.15.115:1081) normal success=2 error=0 dur=153ms",
		"🌐 [net][s]select: proxy[17] (65.111.15.115:1081) fragment success=0 error=3 = i/o timeout dur=15000ms",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "slot_select_log.go") {
		t.Fatalf("select line kept a wrapper location:\n%s", text)
	}
	// Every info line through this wrapper is emitted in the tab format, so the
	// slot's own strategy lines stop claiming the wrapper's file:line too.
	if !strings.Contains(text, "unrelated 7") {
		t.Fatalf("non-select line did not reach the tab stream: %s", text)
	}
}

func TestSlotSelectLabelDirectSlot(t *testing.T) {
	if got := slotSelectLabel(0, ""); got != "[direct] provider=0" {
		t.Fatalf("direct label = %q", got)
	}
}

func TestSlotSelectReportsOnlyTheFirstOwnSuccess(t *testing.T) {
	base := &captureLogger{}
	var calls int
	var gotVia string
	var gotDur time.Duration
	log := withSlotSelectLabel(base, "proxy[3] (10.0.0.3:1080)", func(dur time.Duration, via string) {
		calls++
		gotVia, gotDur = via, dur
	})

	// A failure and an extender carrier are not the slot's own success.
	log.Infof("🌐 [net][s]select: %s = %s dur=%dms\n", "normal success=0 error=1", "timeout", int64(9))
	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "extender (x) success=1 error=0", int64(5))
	if calls != 0 {
		t.Fatalf("reported %d times before an own success", calls)
	}
	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "fragment success=1 error=0", int64(120))
	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "fragment success=2 error=0", int64(80))
	if calls != 1 || gotVia != "fragment success=1 error=0" || gotDur != 120*time.Millisecond {
		t.Fatalf("calls=%d via=%q dur=%v", calls, gotVia, gotDur)
	}
}

// A label with a percent in it is a proxy address or account, not a format:
// the label joins the rendered line, so it must come out exactly as given.
func TestSlotSelectLabelKeepsPercentLiteral(t *testing.T) {
	base := &captureLogger{}
	var buf bytes.Buffer
	prev := proxyManagerLogWriter
	proxyManagerLogWriter = &buf
	defer func() { proxyManagerLogWriter = prev }()
	log := withSlotSelectLabel(base, "proxy[1] (a%sb)", nil)
	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "x", int64(1))
	line := buf.String()
	if !strings.Contains(line, "proxy[1] (a%sb) x dur=1ms") {
		t.Fatalf("label was formatted: %s", line)
	}
	// The wrapper is not the call site: stamping this file would be long and
	// untrue, and the label already names the slot.
	if strings.Contains(line, "slot_select_log.go") {
		t.Fatalf("select line kept a wrapper location: %s", line)
	}
}

func TestProviderBringUpCountsControlReachedAndWarnsWhenMissing(t *testing.T) {
	oldGrace := providerControlGrace
	providerControlGrace = 30 * time.Millisecond
	defer func() { providerControlGrace = oldGrace }()

	buffer := captureProviderBringUpLog(t)
	tracker := newProviderBringUpTracker(2)
	tracker.start()
	defer tracker.shutdown()

	tracker.authReused(0, connect.NewId(), "proxy:10.0.0.1:1080")
	tracker.authReused(1, connect.NewId(), "proxy:10.0.0.2:1080")
	tracker.controlReached(0, "proxy:10.0.0.1:1080", 120*time.Millisecond, "fragment")
	tracker.controlReached(0, "proxy:10.0.0.1:1080", 5*time.Millisecond, "normal") // second is ignored

	deadline := time.Now().Add(2 * time.Second)
	// Wait for the overdue tick to fire and emit the summary.
	for strings.Count(buffer.String(), "identities authenticated but not control-connected") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	text := buffer.String()
	// The dial is already in the log as the library's select line, so the
	// tracker must not narrate it a second time.
	if strings.Contains(text, "reached the control plane via") {
		t.Fatalf("reached narrated a second time:\n%s", text)
	}
	// No individual per-slot warning for the unreached slot.
	if strings.Contains(text, "provider 1 (proxy:10.0.0.2:1080) authenticated but has not reached the control plane") {
		t.Fatalf("unexpected individual warning for unreached slot:\n%s", text)
	}
	if strings.Contains(text, "provider 0 (proxy:10.0.0.1:1080) authenticated but has not") {
		t.Fatalf("warned about a slot that did reach:\n%s", text)
	}
	// Summary carries total and sample.
	if !strings.Contains(text, "⚠️ [control] 1 of 2 identities authenticated but not control-connected after") {
		t.Fatalf("missing the not-connected summary with oldest age:\n%s", text)
	}
	if !strings.Contains(text, "oldest") {
		t.Fatalf("summary missing oldest age:\n%s", text)
	}
	if !strings.Contains(text, "sample:") {
		t.Fatalf("summary missing sample:\n%s", text)
	}
	// The next tally carries the count.
	tracker.emit()
	if !strings.Contains(buffer.String(), "control plane reached: 1)") {
		t.Fatalf("tally lacks the reached count:\n%s", buffer.String())
	}
}

// TestProviderBringUpCapsPerSlotControlWarnings pins the flood boundary: a
// start where no identity reaches the control plane names at most
// providerControlNameLimit slots per tick and reports the total with a sample
// in the summary, so 465 identities do not produce 465 warnings.
func TestProviderBringUpCapsPerSlotControlWarnings(t *testing.T) {
	oldGrace := providerControlGrace
	oldInterval := providerBringUpHeartbeatInterval
	providerControlGrace = 20 * time.Millisecond
	providerBringUpHeartbeatInterval = 10 * time.Millisecond
	defer func() {
		providerControlGrace = oldGrace
		providerBringUpHeartbeatInterval = oldInterval
	}()

	buffer := captureProviderBringUpLog(t)
	total := providerControlNameLimit * 2
	tracker := newProviderBringUpTracker(total)
	tracker.start()
	defer tracker.shutdown()

	for index := 0; index < total; index++ {
		tracker.authReused(uint64(index), connect.NewId(), fmt.Sprintf("proxy:10.0.0.%d:1080", index+1))
	}

	deadline := time.Now().Add(2 * time.Second)
	// Wait for the overdue tick to fire and emit the summary.
	for strings.Count(buffer.String(), "identities authenticated but not control-connected") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	text := buffer.String()
	// No individual per-slot warnings should be emitted.
	named := strings.Count(text, "authenticated but has not reached the control plane")
	if named != 0 {
		t.Fatalf("expected zero individual warnings, got %d:\n%s", named, text)
	}
	// One wave of expiries is one tally, not one per slot. The size limit is 5
	// slots per tick in the old design; with the shared summary it must be a
	// single line for the whole wave.
	if summaries := strings.Count(text, "identities authenticated but not control-connected"); summaries != 1 {
		t.Fatalf("expected one summary line for %d expiring slots, got %d:\n%s", total, summaries, text)
	}
	// Summary should carry the total and a sample.
	if !strings.Contains(text, fmt.Sprintf("%d of %d identities authenticated but not control-connected", total, total)) {
		t.Fatalf("summary does not carry the total not-connected count:\\n%s", text)
	}
	if !strings.Contains(text, "sample:") {
		t.Fatalf("summary missing sample:\\n%s", text)
	}
	if strings.Contains(text, "oldest") == false {
		t.Fatalf("summary missing oldest age:\\n%s", text)
	}
}

// A family decision repeats on every dial for the life of the slot, so an
// unchanged decision prints once. A failure prints every time: that is the
// case where the decision is the thing to look at.
func TestSlotSelectPrintsFamilyDialOnlyOnChange(t *testing.T) {
	base := &captureLogger{}
	var buf bytes.Buffer
	prev := proxyManagerLogWriter
	proxyManagerLogWriter = &buf
	defer func() { proxyManagerLogWriter = prev }()
	log := withSlotSelectLabel(base, slotSelectLabel(1, "10.0.0.1:1080"), nil)

	// connect hands the family decision to the logger already formatted
	// (connect/egress_dial.go: Infof("%s\n", controlDialFamilyLine(...))), so
	// the test calls it the way the dialer does: a "%s" format with the whole
	// line as its argument. Calling Infof(line) here would pass whatever the
	// wrapper checks against the format and hide a wrapper that only works on
	// that shape.
	const auto4 = "[family]dial tag=api net=tcp family=4 policy=auto demoted=none"
	log.Infof("%s\n", auto4)
	log.Infof("%s\n", auto4)
	log.Infof("%s\n", auto4)
	if n := strings.Count(buf.String(), "tag=api"); n != 1 {
		t.Fatalf("unchanged decision printed %d times, want 1:\n%s", n, buf.String())
	}

	log.Infof("%s\n", "[family]dial tag=api net=tcp family=6 policy=auto demoted=none")
	if n := strings.Count(buf.String(), "tag=api"); n != 2 {
		t.Fatalf("changed decision printed %d times total, want 2:\n%s", n, buf.String())
	}

	const failed = "[family]dial tag=api net=tcp family=? policy=auto demoted=none err=socks connect tcp: connection refused"
	log.Infof("%s\n", failed)
	log.Infof("%s\n", failed)
	if n := strings.Count(buf.String(), "connection refused"); n != 2 {
		t.Fatalf("repeated failure printed %d times, want 2:\n%s", n, buf.String())
	}
}
