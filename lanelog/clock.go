package lanelog

import "time"

// Ticker is the subset of time.Ticker the sink needs.
type Ticker interface {
	C() <-chan time.Time
	Stop()
}

// Clock is injected so tests drive the flush interval, the summary interval
// and the close deadline without sleeping.
type Clock interface {
	NewTicker(d time.Duration) Ticker
	After(d time.Duration) <-chan time.Time
}

// SystemClock is the real-time Clock.
type SystemClock struct{}

type sysTicker struct{ t *time.Ticker }

func (s sysTicker) C() <-chan time.Time { return s.t.C }
func (s sysTicker) Stop()               { s.t.Stop() }

func (SystemClock) NewTicker(d time.Duration) Ticker { return sysTicker{time.NewTicker(d)} }
func (SystemClock) After(d time.Duration) <-chan time.Time {
	return time.After(d)
}
