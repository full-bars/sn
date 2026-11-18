package urnettools

import (
	"math"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/tui"
	"github.com/urfoundation/sn/internal/tui/tcellui"
)

func TestTopWindowBucketsAndExpiry(t *testing.T) {
	var w topWindow
	now := time.Unix(1800000120, 0)
	w.push(10, now)
	w.push(30, now.Add(time.Second))
	for _, hours := range []int{1, 24} {
		series := w.windowFor(hours, now)
		if series[len(series)-1] != 20 || !math.IsNaN(series[len(series)-2]) {
			t.Fatalf("%dh window: %v", hours, series)
		}
	}
	if got := w.windowFor(1, now.Add(time.Hour)); !math.IsNaN(got[0]) {
		t.Fatalf("expired bucket: %v", got[0])
	}
	w.reset()
	if got := w.windowFor(24, now); !math.IsNaN(got[len(got)-1]) {
		t.Fatalf("reset bucket: %v", got)
	}
}

func TestTopWindowModelSelection(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1800000120, 0)}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.apply(0, topFlowing(t), nil)
	for _, hours := range []int{1, 24, 0} {
		m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 't'})
		if m.windowHours != hours {
			t.Fatalf("window = %d, want %d", m.windowHours, hours)
		}
		if hours != 0 {
			s := m.series()
			if len(s.billable) == 0 || s.anchor == 0 || s.capacity != len(s.billable) {
				t.Fatalf("invalid window series: %+v", s)
			}
			if got := m.throughputTitle("Billable", s); got != "Billable, "+windowTitle(hours) {
				t.Fatalf("title = %q", got)
			}
		}
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 't'})
	m.selectProvider(1)
	if m.windowHours != 1 || !math.IsNaN(m.windowB.windowFor(1, clock.Now())[59]) {
		t.Fatal("provider switch must preserve selection and clear history")
	}
}
