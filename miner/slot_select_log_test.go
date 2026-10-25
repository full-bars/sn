package miner

import (
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
	log := withSlotSelectLabel(base, slotSelectLabel(17, "65.111.15.115:1081"), nil)

	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "normal success=2 error=0", int64(153))
	log.Infof("🌐 [net][s]select: %s = %s dur=%dms\n", "fragment success=0 error=3", "i/o timeout", int64(15000))
	log.Infof("unrelated %d\n", 7)

	text := base.text()
	for _, want := range []string{
		"🌐 [net][s]select: proxy[17] (65.111.15.115:1081) normal success=2 error=0 dur=153ms",
		"🌐 [net][s]select: proxy[17] (65.111.15.115:1081) fragment success=0 error=3 = i/o timeout dur=15000ms",
		"unrelated 7",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
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

func TestSlotSelectLabelEscapesPercent(t *testing.T) {
	base := &captureLogger{}
	log := withSlotSelectLabel(base, "proxy[1] (a%sb)", nil)
	log.Infof("🌐 [net][s]select: %s dur=%dms\n", "x", int64(1))
	if !strings.Contains(base.text(), "proxy[1] (a%sb) x dur=1ms") {
		t.Fatalf("label was formatted: %s", base.text())
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
	for !strings.Contains(buffer.String(), "provider 1 (proxy:10.0.0.2:1080) authenticated but has not reached the control plane") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	text := buffer.String()
	if !strings.Contains(text, "📡 [control] provider 0 (proxy:10.0.0.1:1080) reached the control plane via fragment in 120ms") {
		t.Fatalf("reached line missing:\n%s", text)
	}
	if strings.Count(text, "provider 0 (proxy:10.0.0.1:1080) reached") != 1 {
		t.Fatalf("reached narrated more than once:\n%s", text)
	}
	if !strings.Contains(text, "provider 1 (proxy:10.0.0.2:1080) authenticated but has not reached the control plane") {
		t.Fatalf("missing warning for the slot that never reached:\n%s", text)
	}
	if strings.Contains(text, "provider 0 (proxy:10.0.0.1:1080) authenticated but has not") {
		t.Fatalf("warned about a slot that did reach:\n%s", text)
	}
	// The next tally carries the count.
	tracker.emit()
	if !strings.Contains(buffer.String(), "control plane reached: 1)") {
		t.Fatalf("tally lacks the reached count:\n%s", buffer.String())
	}
}
