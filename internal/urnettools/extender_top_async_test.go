package urnettools

import (
	"errors"
	"github.com/gdamore/tcell/v2"
	"strings"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/tui"
)

// The extender reading is a socket round trip that can take seconds. Building
// the model and selecting a provider must not make it; it is requested by the
// poll scheduler and applied when it returns.
func TestTopExtenderPollIsAsyncAndGenerationGuarded(t *testing.T) {
	calls := 0
	old := socketExtenderFn
	socketExtenderFn = func(Provider) (*ExtenderReading, error) { calls++; return nil, errors.New("slow") }
	defer func() { socketExtenderFn = old }()

	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), []Provider{{StateDir: t.TempDir(), Running: true}, {StateDir: t.TempDir(), Running: true}}, clock)
	if calls != 0 {
		t.Fatalf("building the model read the socket %d times", calls)
	}
	gen, _, ok := m.wantExtender(clock.Now())
	if !ok || m.extenderBusy == false {
		t.Fatal("first poll not requested")
	}
	if _, _, again := m.wantExtender(clock.Now()); again {
		t.Fatal("second poll requested while one is in flight")
	}
	m.selectProvider(1)
	if calls != 0 {
		t.Fatalf("selecting a provider read the socket %d times", calls)
	}
	if m.showExtender || m.extenderBusy {
		t.Fatal("selection kept the previous provider's extender state")
	}
	m.applyExtender(gen, ExtenderReading{Source: SourceControlSocket, RoleState: "active"})
	if m.showExtender || m.extender.Source != "" {
		t.Fatal("a reading fetched for the previous selection was applied")
	}
	gen2, _, ok := m.wantExtender(clock.Now())
	if !ok {
		t.Fatal("new selection did not request a poll")
	}
	m.applyExtender(gen2, ExtenderReading{Source: SourceControlSocket, RoleState: "active"})
	if !m.showExtender || m.extender.RoleState != "active" {
		t.Fatalf("current reading not applied: %+v", m.extender)
	}
	clock.Advance(100 * time.Millisecond)
	if _, _, ok := m.wantExtender(clock.Now()); ok {
		t.Fatal("polled again inside the interval")
	}
}

// The Extender box is sized from the lines it draws: both addresses are shown
// at exactly the height top reserves for it.
func TestTopExtenderBoxHeightShowsAddresses(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), []Provider{{StateDir: t.TempDir(), Running: true}}, &fakeClock{t: topBase})
	m.applyExtender(m.gen, ExtenderReading{Source: SourceControlSocket, RoleState: "active", Bound: true, ActivatedV4: true, ActivatedV6: true,
		Ipv4: "198.51.100.7", Ipv6: "2001:db8::7", DnsPorts: "4053", LastRefusal: "boom", ConnectionCount: 3})
	rows := m.extenderRows()
	if rows != len(m.extenderLines())+2 || rows < 9 {
		t.Fatalf("rows=%d lines=%d", rows, len(m.extenderLines()))
	}
	b := tui.New(40, rows-2)
	m.drawExtender(b)
	out := b.String()
	// Every row fits the 28-column side box without cutting the address.
	b = tui.New(26, rows-2)
	m.drawExtender(b)
	out = b.String()
	for _, want := range []string{"up 198.51.100.7", "up 2001:db8::7", "4053", "boom", "conns"} {
		if !strings.Contains(out, want) {
			t.Fatalf("box of %d content rows hides %q:\n%s", rows-2, want, out)
		}
	}
}

type blockingExtenderSource struct {
	topSource
	release chan struct{}
	started chan struct{}
}

func (s blockingExtenderSource) FetchExtender(Provider) ExtenderReading {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	return ExtenderReading{Source: SourceControlSocket, RoleState: "active", Ipv4: "198.51.100.7", ActivatedV4: true}
}

// A socket that never answers must not stop top drawing or taking keys: the
// reading arrives later and then appears in the Extender box.
func TestTopLoopExtenderSlowSocketDoesNotFreezeTheUI(t *testing.T) {
	src, _ := fakeSource(t)
	slow := blockingExtenderSource{topSource: src, release: make(chan struct{}), started: make(chan struct{}, 1)}
	rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase}), slow, 120, 50)
	rig.waitFor(t, "live data while the extender socket is stuck", func(s string) bool {
		return strings.Contains(s, "FLOWING") && !strings.Contains(s, "Extender")
	})
	select {
	case <-slow.started:
	case <-time.After(3 * time.Second):
		t.Fatal("extender fetch never started")
	}
	rig.sim.InjectKey(tcell.KeyRune, '?', 0) // the UI still takes keys
	rig.waitFor(t, "help drawn while the fetch is stuck", func(s string) bool {
		return strings.Contains(strings.ToLower(s), "help") && !strings.Contains(s, "Extender")
	})
	rig.sim.InjectKey(tcell.KeyRune, '?', 0)
	close(slow.release)
	rig.waitFor(t, "the Extender box", func(s string) bool { return strings.Contains(s, "Extender") && strings.Contains(s, "198.51.100.7") })
	rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
}
