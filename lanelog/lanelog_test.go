package lanelog

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fake clock ----

type fakeTicker struct {
	c      chan time.Time
	period time.Duration
	next   time.Time
}

func (f *fakeTicker) C() <-chan time.Time { return f.c }
func (f *fakeTicker) Stop()               {}

type fakeAfter struct {
	at time.Time
	c  chan time.Time
}

type fakeClock struct {
	mu         sync.Mutex
	now        time.Time
	tickers    []*fakeTicker
	afters     []*fakeAfter
	afterAdded chan struct{}
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_000_000, 0), afterAdded: make(chan struct{}, 16)}
}

func (f *fakeClock) NewTicker(d time.Duration) Ticker {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTicker{c: make(chan time.Time, 1), period: d, next: f.now.Add(d)}
	f.tickers = append(f.tickers, t)
	return t
}

func (f *fakeClock) After(d time.Duration) <-chan time.Time {
	f.mu.Lock()
	a := &fakeAfter{at: f.now.Add(d), c: make(chan time.Time, 1)}
	f.afters = append(f.afters, a)
	f.mu.Unlock()
	f.afterAdded <- struct{}{}
	return a.c
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	for _, t := range f.tickers {
		for !t.next.After(f.now) {
			select {
			case t.c <- t.next:
			default:
			}
			t.next = t.next.Add(t.period)
		}
	}
	for _, a := range f.afters {
		if !a.at.After(f.now) {
			select {
			case a.c <- f.now:
			default:
			}
		}
	}
}

// ---- fake destination ----

type dest struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	gate    chan struct{} // when non-nil every Write waits for it to be closed
	entered chan struct{} // signalled on every Write entry
	wrote   chan struct{} // signalled after every Write
	fail    error
}

func newDest(gated bool) *dest {
	d := &dest{entered: make(chan struct{}, 1<<16), wrote: make(chan struct{}, 1<<16)}
	if gated {
		d.gate = make(chan struct{})
	}
	return d
}

func (d *dest) Write(p []byte) (int, error) {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	if d.gate != nil {
		<-d.gate
	}
	d.mu.Lock()
	if d.fail != nil {
		err := d.fail
		d.fail = nil // fail exactly once
		d.mu.Unlock()
		return 0, err
	}
	d.buf.Write(p)
	d.mu.Unlock()
	select {
	case d.wrote <- struct{}{}:
	default:
	}
	return len(p), nil
}

func (d *dest) release() { close(d.gate) }

func (d *dest) text() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.buf.String()
}

func (d *dest) lines() []string {
	t := strings.TrimSuffix(d.text(), "\n")
	if t == "" {
		return nil
	}
	return strings.Split(t, "\n")
}

// waitFor blocks (no sleeping) until the destination text satisfies ok.
func (d *dest) waitFor(t *testing.T, ok func(string) bool) {
	t.Helper()
	for !ok(d.text()) {
		select {
		case <-d.wrote:
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out; dest has %q", d.text())
		}
	}
}

// wedge writes one important line and returns once the writer is blocked
// inside the destination, so every later write queues deterministically.
func wedge(t *testing.T, s *Sink, d *dest) {
	t.Helper()
	if _, err := s.Write([]byte("E1025 00:00:00.000000 1 w.go:1] wedge\n")); err != nil {
		t.Fatal(err)
	}
	<-d.entered
}

var summaryRe = regexp.MustCompile(`(?m)^\[log\] (\d+) lines dropped$`)

func summed(text string) (total int) {
	for _, m := range summaryRe.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		total += n
	}
	return
}

// ---- tests ----

func TestClassification(t *testing.T) {
	s := New(Config{Dest: newDest(false), Clock: newFakeClock(), Markers: []string{"[t]connect", "STALL"}})
	defer s.Close()
	for _, c := range []struct {
		line string
		want bool
	}{
		{"E1025 12:00:00.000000 1 a.go:1] boom", true},
		{"W1025 12:00:00.000000 1 a.go:1] hmm", true},
		{"F1025 12:00:00.000000 1 a.go:1] dead", true},
		{"I1025 12:00:00.000000 1 a.go:1] fine", false},
		{"Error without digit", false},
		{"E", false},
		{"I1025 x] [t]connect failed", true},
		{"I1025 x] watchdog STALL seen", true},
		{"I1025 x] nothing", false},
	} {
		if got := s.Important([]byte(c.line)); got != c.want {
			t.Errorf("Important(%q) = %v, want %v", c.line, got, c.want)
		}
	}
}

func TestBlockedDestination(t *testing.T) {
	const (
		coldCap    = 100
		writers    = 64
		coldEach   = 50
		impWriters = 8
		impEach    = 100
	)
	d := newDest(true)
	s := New(Config{Dest: d, Clock: newFakeClock(), ColdCap: coldCap, ImportantCap: 1000})
	wedge(t, s, d)

	var wg sync.WaitGroup
	for g := range writers {
		wg.Go(func() {
			for i := range coldEach {
				line := fmt.Sprintf("I1025 00:00:00.000000 1 c.go:1] cold g=%d seq=%d\n", g, i)
				if n, err := s.Write([]byte(line)); err != nil || n != len(line) {
					t.Errorf("cold write: n=%d err=%v", n, err)
				}
				if g < impWriters {
					line := fmt.Sprintf("E1025 00:00:00.000000 1 i.go:1] imp g=%d seq=%d\n", g, i)
					if _, err := s.Write([]byte(line)); err != nil {
						t.Errorf("imp write: %v", err)
					}
				}
			}
			if g < impWriters {
				for i := coldEach; i < impEach; i++ {
					line := fmt.Sprintf("E1025 00:00:00.000000 1 i.go:1] imp g=%d seq=%d\n", g, i)
					s.Write([]byte(line))
				}
			}
		})
	}
	allDone := make(chan struct{})
	go func() { wg.Wait(); close(allDone) }()
	select {
	case <-allDone: // every Write returned although the destination is wedged
	case <-time.After(10 * time.Second):
		t.Fatal("producers blocked on a wedged destination")
	}

	total := writers * coldEach
	if got, want := s.Stats().ColdDropped, uint64(total-coldCap); got != want {
		t.Fatalf("ColdDropped = %d, want %d", got, want)
	}
	if s.Stats().ImportantDropped != 0 {
		t.Fatalf("important lines dropped: %d", s.Stats().ImportantDropped)
	}

	d.release()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	out := d.text()

	// no important line lost, none reordered within its producer
	next := make([]int, impWriters)
	impRe := regexp.MustCompile(`imp g=(\d+) seq=(\d+)$`)
	cold := 0
	for _, l := range d.lines() {
		if m := impRe.FindStringSubmatch(l); m != nil {
			g, _ := strconv.Atoi(m[1])
			seq, _ := strconv.Atoi(m[2])
			if seq != next[g] {
				t.Fatalf("g=%d: got seq %d, want %d (lost or reordered)", g, seq, next[g])
			}
			next[g]++
		} else if strings.Contains(l, " cold g=") {
			cold++
		}
	}
	for g, n := range next {
		if n != impEach {
			t.Errorf("g=%d: %d important lines out, want %d", g, n, impEach)
		}
	}
	if cold != coldCap {
		t.Errorf("cold lines out = %d, want %d", cold, coldCap)
	}
	if got := summed(out); got != total-coldCap {
		t.Errorf("summary lines add up to %d, want %d\n%s", got, total-coldCap, out)
	}
}

func TestColdByteBound(t *testing.T) {
	d := newDest(true)
	s := New(Config{Dest: d, Clock: newFakeClock(), ColdMaxBytes: 25})
	wedge(t, s, d)
	for range 5 {
		s.Write([]byte("0123456789\n")) // 11 bytes: two fit, three drop
	}
	if got := s.Stats().ColdDropped; got != 3 {
		t.Fatalf("ColdDropped = %d, want 3", got)
	}
	d.release()
	s.Close()
}

func TestImportantOverflowDropsOldestAndCounts(t *testing.T) {
	d := newDest(true)
	s := New(Config{Dest: d, Clock: newFakeClock(), ImportantCap: 4})
	wedge(t, s, d)
	for i := range 10 {
		s.Write([]byte(fmt.Sprintf("E1025 x] imp %d\n", i)))
	}
	if got := s.Stats().ImportantDropped; got != 6 {
		t.Fatalf("ImportantDropped = %d, want 6", got)
	}
	d.release()
	s.Close()
	out := d.text()
	for i := range 10 {
		has := strings.Contains(out, fmt.Sprintf("imp %d\n", i))
		if want := i >= 6; has != want {
			t.Errorf("imp %d present=%v want %v", i, has, want)
		}
	}
	if !strings.Contains(out, "[log] 6 lines dropped\n") || !strings.Contains(out, "6 important displaced") {
		t.Errorf("summary missing:\n%s", out)
	}
}

func TestSummaryPerInterval(t *testing.T) {
	clk := newFakeClock()
	d := newDest(true)
	s := New(Config{Dest: d, Clock: clk, ColdCap: 2, SummaryInterval: 5 * time.Second})
	wedge(t, s, d)
	for range 5 {
		s.Write([]byte("I1025 x] cold\n"))
	}
	d.release()
	clk.Advance(5 * time.Second)
	d.waitFor(t, func(s string) bool { return strings.Contains(s, "[log] 3 lines dropped\n") })

	// a quiet interval emits nothing further
	clk.Advance(5 * time.Second)
	s.Close()
	if n := len(summaryRe.FindAllString(d.text(), -1)); n != 1 {
		t.Fatalf("summary lines = %d, want 1\n%s", n, d.text())
	}
}

func TestImportantFlushesWithoutTick(t *testing.T) {
	d := newDest(false)
	s := New(Config{Dest: d, Clock: newFakeClock()})
	defer s.Close()
	s.Write([]byte("W1025 x] warn\n"))
	d.waitFor(t, func(s string) bool { return s == "W1025 x] warn\n" })
}

func TestColdFlushesOnTick(t *testing.T) {
	clk := newFakeClock()
	d := newDest(false)
	s := New(Config{Dest: d, Clock: clk})
	defer s.Close()
	s.Write([]byte("I1025 x] cold\n"))
	clk.Advance(250 * time.Millisecond)
	d.waitFor(t, func(s string) bool { return s == "I1025 x] cold\n" })
}

func TestCloseDrains(t *testing.T) {
	d := newDest(false)
	s := New(Config{Dest: d, Clock: newFakeClock()})
	const n = 5000 // well under the default caps
	for i := range n {
		s.Write([]byte(fmt.Sprintf("I1025 x] cold %d\n", i)))
		if i%100 == 0 {
			s.Write([]byte(fmt.Sprintf("E1025 x] imp %d\n", i)))
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	cold, imp := 0, 0
	for _, l := range d.lines() {
		switch {
		case strings.Contains(l, "] cold "):
			if want := fmt.Sprintf("I1025 x] cold %d", cold); l != want {
				t.Fatalf("cold out of order: %q want %q", l, want)
			}
			cold++
		case strings.Contains(l, "] imp "):
			imp++
		}
	}
	if cold != n || imp != n/100 {
		t.Fatalf("drained cold=%d imp=%d, want %d/%d", cold, imp, n, n/100)
	}
	if _, err := s.Write([]byte("I1025 x] late\n")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close err = %v", err)
	}
}

func TestCloseDeadline(t *testing.T) {
	clk := newFakeClock()
	d := newDest(true)
	s := New(Config{Dest: d, Clock: clk, CloseDeadline: 2 * time.Second})
	wedge(t, s, d)
	res := make(chan error, 1)
	go func() { res <- s.Close() }()
	<-clk.afterAdded
	clk.Advance(2 * time.Second)
	if err := <-res; !errors.Is(err, ErrCloseDeadline) {
		t.Fatalf("Close = %v, want ErrCloseDeadline", err)
	}
	d.release() // let the writer goroutine exit
	<-s.done
}

func TestWriteErrorRecovers(t *testing.T) {
	d := newDest(false)
	d.fail = errors.New("disk gone")
	s := New(Config{Dest: d, Clock: newFakeClock()})
	s.Write([]byte("E1025 x] lost\n"))
	<-d.entered // the one failing write has happened
	s.Write([]byte("E1025 x] kept\n"))
	d.waitFor(t, func(s string) bool { return strings.Contains(s, "kept\n") })
	s.Close()
	if !strings.Contains(d.text(), "lines dropped") {
		t.Errorf("write-error loss not reported:\n%s", d.text())
	}
}
