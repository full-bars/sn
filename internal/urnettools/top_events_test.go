package urnettools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/tui"
)

func TestTopEventTimeAndIcons(t *testing.T) {
	now := time.Date(2026, 9, 19, 16, 4, 31, 0, time.UTC)
	for _, tc := range []struct {
		age  time.Duration
		zoom bool
		want string
	}{
		{30 * time.Second, false, "just now"}, {12 * time.Minute, false, "12m ago"},
		{3 * time.Hour, false, "3h ago"}, {48 * time.Hour, false, "2d ago"},
		{12 * time.Minute, true, "15:52:31"}, {72 * time.Hour, true, "09-16 16:04"},
	} {
		if got := eventTime(now.Add(-tc.age), now, tc.zoom); got != tc.want {
			t.Errorf("time=%q, want %q", got, tc.want)
		}
	}
	for _, tc := range []struct {
		level topLevel
		th    tui.Theme
		want  rune
	}{
		{topInfo, tui.DefaultTheme(), '•'}, {topGood, tui.DefaultTheme(), '✓'},
		{topWarn, tui.DefaultTheme(), '!'}, {topBad, tui.DefaultTheme(), '✗'},
		{topInfo, tui.MonoTheme(), 'i'}, {topGood, tui.MonoTheme(), '+'}, {topBad, tui.MonoTheme(), 'x'},
	} {
		if got := eventIcon(tc.level, tc.th); got != tc.want {
			t.Errorf("icon=%q, want %q", got, tc.want)
		}
	}
}

func TestTopEventFilterCollapseAndDraw(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.addEvent(clock.Now(), "slow", topWarn)
	m.addEvent(clock.Now(), "slow", topWarn)
	m.addEvent(clock.Now(), "healthy", topGood)
	m.eventFilter = eventFilter{MinLevel: topWarn, Text: "SLOW"}
	views := collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter))
	if len(views) != 1 || views[0].Count != 2 {
		t.Fatalf("views=%+v", views)
	}
	b := tui.New(40, 2)
	m.drawEvents(b)
	if got := b.String(); !strings.Contains(got, "! slow  x2") {
		t.Fatalf("render=%q", got)
	}
	m.eventFilter.Text = "absent"
	b = tui.New(40, 2)
	m.drawEvents(b)
	if got := b.String(); !strings.Contains(got, "no events match filter") {
		t.Fatalf("render=%q", got)
	}
	m.eventFilter = eventFilter{}
	for i := 0; i < 8; i++ {
		m.addEvent(clock.Now(), fmt.Sprint(i), topWarn)
	}
	m.eventHeight = 2
	m.scrollEvents(100)
	if m.eventScroll != len(collapseEvents(m.sortedEvents()))-2 {
		t.Fatalf("scroll=%d want %d", m.eventScroll, len(collapseEvents(m.sortedEvents()))-2)
	}
	m.selectProvider(1)
	if m.eventScroll != 0 {
		t.Fatal("provider switch retained scroll")
	}
}
