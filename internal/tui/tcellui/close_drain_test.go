package tcellui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// drainCloseScreen models tcell's shutdown: Fini closes the quit channel and
// then waits for its input parser, and the parser waits until PollEvent has
// taken every event it still holds. PollEvent returns nil only once the parser
// is done, so a pump that stops polling early leaves Fini waiting forever.
type drainCloseScreen struct {
	tcell.Screen
	quit     chan struct{}
	pending  chan tcell.Event
	finished chan struct{}
}

func (s *drainCloseScreen) PollEvent() tcell.Event {
	select {
	case <-s.quit:
	case <-s.finished:
		return nil
	}
	select {
	case ev := <-s.pending:
		return ev
	case <-s.finished:
		return nil
	}
}

func (s *drainCloseScreen) Fini() {
	close(s.quit)
	for len(s.pending) != 0 {
		time.Sleep(time.Millisecond)
	}
	s.Screen.Fini()
	close(s.finished)
}

func TestCloseKeepsPollingUntilTheParserFinishes(t *testing.T) {
	f := &drainCloseScreen{
		Screen:   tcell.NewSimulationScreen("UTF-8"),
		quit:     make(chan struct{}),
		pending:  make(chan tcell.Event, 64),
		finished: make(chan struct{}),
	}
	for i := 0; i < cap(f.pending); i++ {
		f.pending <- tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone)
	}
	scr, err := Wrap(f)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { scr.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung: the event pump stopped polling while tcell's parser still held events")
	}
}
