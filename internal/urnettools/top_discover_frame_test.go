package urnettools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urfoundation/sn/internal/tui"
	"github.com/urfoundation/sn/internal/tui/tcellui"
)

// frameScreen is a tcellui.Screen that records every frame drawn on it. Input
// is queued by the test and read by the command, as on a real terminal.
type frameScreen struct {
	mu     sync.Mutex
	frames []string
	drawn  chan struct{}
	events chan tcellui.Event
	closes int
}

func newFrameScreen() *frameScreen {
	return &frameScreen{drawn: make(chan struct{}, 1), events: make(chan tcellui.Event, 8)}
}

func (s *frameScreen) Size() (int, int) { return 60, 10 }

func (s *frameScreen) Draw(b *tui.Buffer) {
	s.mu.Lock()
	s.frames = append(s.frames, b.String())
	s.mu.Unlock()
	select {
	case s.drawn <- struct{}{}:
	default:
	}
}

func (s *frameScreen) Events() <-chan tcellui.Event { return s.events }

func (s *frameScreen) Close() {
	s.mu.Lock()
	s.closes++
	s.mu.Unlock()
}

func (s *frameScreen) closeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closes
}

// waitForFrame blocks until a drawn frame contains want, and returns every
// frame seen so far. The failsafe only fires when the screen never shows it.
func (s *frameScreen) waitForFrame(t *testing.T, want string) []string {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		s.mu.Lock()
		for _, f := range s.frames {
			if strings.Contains(f, want) {
				frames := append([]string(nil), s.frames...)
				s.mu.Unlock()
				return frames
			}
		}
		s.mu.Unlock()
		select {
		case <-s.drawn:
		case <-deadline:
			t.Fatalf("no frame containing %q was drawn", want)
		}
	}
}

// stubTopDiscovery runs cmdTop against scr with discovery replaced by fn. The
// source fails fast, so no socket is touched, and the seams are restored when
// the test ends.
func stubTopDiscovery(t *testing.T, scr *frameScreen, fn func() []Provider) {
	t.Helper()
	stubTopCmd(t, true, nil, nil)
	origOpen, origSource, origDisc := topOpenScreen, topSourceFn, discoverStatusFn
	t.Cleanup(func() { topOpenScreen, topSourceFn, discoverStatusFn = origOpen, origSource, origDisc })
	topOpenScreen = func() (tcellui.Screen, error) { return scr, nil }
	topSourceFn = func() topSource { return failingTopSource{} }
	discoverStatusFn = fn
}

// failingTopSource answers every snapshot fetch with an error, so the model
// has nothing to draw and the test never reaches a live provider.
type failingTopSource struct{}

func (failingTopSource) Fetch(Provider) (*NodeSnapshot, error) {
	return nil, errors.New("no provider in this test")
}

// The first frame must be on the screen while discovery is still running. The
// old order opened the screen only after discovery, so the terminal stayed
// empty for the whole pass. Discovery here blocks until the test releases it.
func TestCmdTopDrawsItsFirstFrameBeforeDiscoveryReturns(t *testing.T) {
	scr := newFrameScreen()
	release := make(chan struct{})
	discoveryReturned := make(chan struct{})
	stubTopDiscovery(t, scr, func() []Provider {
		<-release
		close(discoveryReturned)
		return []Provider{{Unit: "a", Running: true}}
	})

	result := make(chan error, 1)
	go func() { result <- cmdTop(nil) }()

	frames := scr.waitForFrame(t, "looking for providers")
	select {
	case <-discoveryReturned:
		t.Fatal("discovery finished before the first frame was checked")
	default:
	}
	if !strings.Contains(frames[0], "looking for providers") {
		t.Fatalf("the first frame must say what is happening, got:\n%s", frames[0])
	}

	close(release)
	scr.events <- tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("top should quit cleanly on ctrl-c, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("top did not return after ctrl-c")
	}
	if scr.closeCount() == 0 {
		t.Fatal("the terminal must be given back on the way out")
	}
}

// When discovery finds nothing, the frame says so and waits for a key. The
// command still fails with the same error, so the exit code is unchanged.
func TestCmdTopSaysSoWhenDiscoveryFindsNothing(t *testing.T) {
	scr := newFrameScreen()
	stubTopDiscovery(t, scr, func() []Provider { return nil })

	result := make(chan error, 1)
	go func() { result <- cmdTop(nil) }()

	scr.waitForFrame(t, "no providers found on this box")
	scr.events <- tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEnter}
	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "no providers found on this box") {
			t.Fatalf("top should still fail with the discovery error, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("top did not return after a key press")
	}
	if scr.closeCount() == 0 {
		t.Fatal("the terminal must be given back before the error is printed")
	}
}

// Ctrl-C must still work while discovery runs. The screen is now open during
// that wait, and the keys are read there, so a stuck probe cannot trap the
// operator.
func TestCmdTopQuitsOnCtrlCWhileDiscoveryIsStillRunning(t *testing.T) {
	scr := newFrameScreen()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	stubTopDiscovery(t, scr, func() []Provider {
		<-release
		return nil
	})

	result := make(chan error, 1)
	go func() { result <- cmdTop(nil) }()

	scr.waitForFrame(t, "looking for providers")
	scr.events <- tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ctrl-c during discovery should end top cleanly, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ctrl-c did not end top while discovery was still running")
	}
}

// Keeps the cancellation path honest: a signal context that is already done
// ends the wait without waiting for discovery.
func TestDiscoverWhileDrawingHonoursContextCancel(t *testing.T) {
	scr := newFrameScreen()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	orig := discoverStatusFn
	t.Cleanup(func() { discoverStatusFn = orig })
	discoverStatusFn = func() []Provider { <-release; return nil }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := discoverWhileDrawing(ctx, scr); ok {
		t.Fatal("a cancelled context must end the discovery wait")
	}
}
