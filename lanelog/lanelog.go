// Package lanelog is a two-lane, non-blocking log sink.
//
// Producers (Write) never wait on I/O: they take a mutex that guards only
// in-memory queue operations. One writer goroutine owns the destination.
//
//   - Important lane: glog severity W, E or F, or a line containing a
//     caller-supplied marker. Own queue, never displaced by cold lines. If it
//     is full (the writer is wedged) the OLDEST entry is dropped and counted.
//   - Cold lane: everything else. Bounded by lines and bytes; when full the
//     NEWEST line is dropped and counted.
//
// Dropped lines are reported as "[log] N lines dropped" once per summary
// interval, so loss is never silent.
package lanelog

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrClosed is returned by Write after Close.
	ErrClosed = errors.New("lanelog: sink closed")
	// ErrCloseDeadline is returned by Close when the drain did not finish in
	// time (typically a wedged destination). The writer goroutine exits as
	// soon as the blocked destination call returns.
	ErrCloseDeadline = errors.New("lanelog: close deadline exceeded before drain finished")
)

const (
	DefaultImportantCap    = 4096
	DefaultColdCap         = 16 << 10
	DefaultColdMaxBytes    = 8 << 20
	DefaultBufferSize      = 64 << 10
	DefaultFlushInterval   = 250 * time.Millisecond
	DefaultSummaryInterval = 5 * time.Second
	DefaultCloseDeadline   = 2 * time.Second
	coldChunk              = 256 // cold lines taken per lock acquisition
)

// Config configures a Sink. Zero values take the defaults above.
type Config struct {
	// Dest receives the log stream. Required. Only the writer goroutine
	// calls it, so it need not be goroutine safe. It is never closed.
	Dest io.Writer
	// Clock defaults to SystemClock.
	Clock Clock
	// Markers: a line containing any of these is important.
	Markers []string

	ImportantCap    int // lines
	ColdCap         int // lines
	ColdMaxBytes    int // bytes queued in the cold lane
	BufferSize      int // bufio size
	FlushInterval   time.Duration
	SummaryInterval time.Duration
	CloseDeadline   time.Duration
}

// Stats is a snapshot of the counters.
type Stats struct {
	ColdDropped      uint64 // cold lines dropped because the cold lane was full
	ImportantDropped uint64 // oldest important lines displaced (writer wedged)
	WriteErrors      uint64 // failed destination writes or flushes
	WriteErrorLost   uint64 // lines buffered at the time of a failed write (upper bound)
	RejectedClosed   uint64 // Write calls after Close
}

// Sink implements io.Writer. Build with New.
type Sink struct {
	cfg     Config
	markers [][]byte

	mu        sync.Mutex
	closed    bool
	imp       queue
	cold      queue
	coldBytes int

	wake    chan struct{}
	closing chan struct{}
	done    chan struct{}
	abandon atomic.Bool

	coldDropped atomic.Uint64
	impDropped  atomic.Uint64
	ioErrors    atomic.Uint64
	ioLost      atomic.Uint64
	rejected    atomic.Uint64

	// writer-goroutine state
	bw        *bufio.Writer
	flushT    Ticker
	sumT      Ticker
	pending   uint64 // lines in bw not yet flushed
	repCold   uint64
	repImp    uint64
	repIO     uint64
	scratch   [][]byte
	closeOnce sync.Once
	closeErr  error
}

// New starts the sink and its single writer goroutine.
func New(cfg Config) *Sink {
	if cfg.Dest == nil {
		panic("lanelog: Config.Dest is required")
	}
	if cfg.Clock == nil {
		cfg.Clock = SystemClock{}
	}
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defd := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&cfg.ImportantCap, DefaultImportantCap)
	def(&cfg.ColdCap, DefaultColdCap)
	def(&cfg.ColdMaxBytes, DefaultColdMaxBytes)
	def(&cfg.BufferSize, DefaultBufferSize)
	defd(&cfg.FlushInterval, DefaultFlushInterval)
	defd(&cfg.SummaryInterval, DefaultSummaryInterval)
	defd(&cfg.CloseDeadline, DefaultCloseDeadline)

	s := &Sink{
		cfg:     cfg,
		wake:    make(chan struct{}, 1),
		closing: make(chan struct{}),
		done:    make(chan struct{}),
		bw:      bufio.NewWriterSize(cfg.Dest, cfg.BufferSize),
	}
	for _, m := range cfg.Markers {
		if m != "" {
			s.markers = append(s.markers, []byte(m))
		}
	}
	// tickers are created here, before the goroutine starts, so a fake clock
	// sees them registered when New returns
	s.flushT = cfg.Clock.NewTicker(cfg.FlushInterval)
	s.sumT = cfg.Clock.NewTicker(cfg.SummaryInterval)
	go s.run()
	return s
}

// Important reports how a line would be classified. Exported for tests and
// for callers that want to pre-filter.
func (s *Sink) Important(p []byte) bool {
	// glog header: severity letter then the mmdd digits, e.g. "E1025 12:00:00".
	if len(p) > 1 && (p[0] == 'W' || p[0] == 'E' || p[0] == 'F') && p[1] >= '0' && p[1] <= '9' {
		return true
	}
	for _, m := range s.markers {
		if bytes.Contains(p, m) {
			return true
		}
	}
	return false
}

// Write enqueues one log line (one call is one entry; glog issues one call
// per line). It copies p, never blocks on I/O, and returns len(p), nil unless
// the sink is closed. A dropped cold line still reports success so the
// logging library does not react; the drop is counted and reported.
func (s *Sink) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	imp := s.Important(p)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		s.rejected.Add(1)
		return 0, ErrClosed
	}
	if imp {
		if s.imp.len() >= s.cfg.ImportantCap {
			s.imp.dropOldest()
			s.impDropped.Add(1)
		}
		s.imp.push(append([]byte(nil), p...))
	} else {
		if s.cold.len() >= s.cfg.ColdCap || s.coldBytes+len(p) > s.cfg.ColdMaxBytes {
			s.mu.Unlock()
			s.coldDropped.Add(1)
			return len(p), nil
		}
		s.cold.push(append([]byte(nil), p...))
		s.coldBytes += len(p)
	}
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
	return len(p), nil
}

// Stats returns a snapshot of the counters.
func (s *Sink) Stats() Stats {
	return Stats{
		ColdDropped:      s.coldDropped.Load(),
		ImportantDropped: s.impDropped.Load(),
		WriteErrors:      s.ioErrors.Load(),
		WriteErrorLost:   s.ioLost.Load(),
		RejectedClosed:   s.rejected.Load(),
	}
}

// Close stops accepting lines, drains both lanes (important first), emits a
// final drop summary, flushes, and returns. If the drain has not finished
// within CloseDeadline it returns ErrCloseDeadline and tells the writer to
// stop at the next opportunity. Safe to call more than once.
func (s *Sink) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		close(s.closing)
		select {
		case <-s.done:
		case <-s.cfg.Clock.After(s.cfg.CloseDeadline):
			s.abandon.Store(true)
			s.closeErr = ErrCloseDeadline
		}
	})
	return s.closeErr
}

func (s *Sink) run() {
	defer close(s.done)
	defer s.flushT.Stop()
	defer s.sumT.Stop()
	for {
		s.drain()
		select {
		case <-s.wake:
		case <-s.flushT.C():
			s.flush()
		case <-s.sumT.C():
			s.summary()
		case <-s.closing:
			s.drain()
			s.summary()
			s.flush()
			return
		}
	}
}

// drain writes until both lanes are empty, important first. The important
// lane is re-checked before every cold chunk, and the tickers are polled
// between chunks so a cold flood cannot starve the 250 ms flush.
func (s *Sink) drain() {
	for !s.abandon.Load() {
		lines, imp := s.take()
		if len(lines) == 0 {
			return
		}
		for _, l := range lines {
			s.put(l)
		}
		if imp {
			s.flush()
		}
		s.pollTickers()
	}
}

// take pops the whole important lane, or else up to coldChunk cold lines.
func (s *Sink) take() (lines [][]byte, important bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := s.imp.len(); n > 0 {
		s.scratch = s.imp.popInto(s.scratch[:0], n)
		return s.scratch, true
	}
	n := min(s.cold.len(), coldChunk)
	if n == 0 {
		return nil, false
	}
	s.scratch = s.cold.popInto(s.scratch[:0], n)
	for _, l := range s.scratch {
		s.coldBytes -= len(l)
	}
	return s.scratch, false
}

func (s *Sink) pollTickers() {
	select {
	case <-s.flushT.C():
		s.flush()
	default:
	}
	select {
	case <-s.sumT.C():
		s.summary()
	default:
	}
}

func (s *Sink) put(l []byte) {
	_, err := s.bw.Write(l)
	if err == nil && l[len(l)-1] != '\n' {
		err = s.bw.WriteByte('\n')
	}
	s.pending++
	if err != nil {
		s.writeFailed()
	}
}

func (s *Sink) flush() {
	if s.bw.Buffered() == 0 {
		s.pending = 0
		return
	}
	if err := s.bw.Flush(); err != nil {
		s.writeFailed()
		return
	}
	s.pending = 0
}

// writeFailed accounts for a failed destination write. bufio's error is
// sticky, so the writer is reset; the lines buffered at that moment are
// counted as lost (an upper bound, part of them may have reached Dest).
func (s *Sink) writeFailed() {
	s.ioErrors.Add(1)
	s.ioLost.Add(s.pending)
	s.pending = 0
	s.bw.Reset(s.cfg.Dest)
}

// summary emits "[log] N lines dropped" for the drops since the last summary.
// N covers cold drops, displaced important lines and lines lost to write
// errors. When any of the latter two is nonzero a second detail line says so.
func (s *Sink) summary() {
	c, i, e := s.coldDropped.Load(), s.impDropped.Load(), s.ioLost.Load()
	dc, di, de := c-s.repCold, i-s.repImp, e-s.repIO
	if dc+di+de == 0 {
		return
	}
	s.repCold, s.repImp, s.repIO = c, i, e
	fmt.Fprintf(s.bw, "[log] %d lines dropped\n", dc+di+de)
	if di+de > 0 {
		fmt.Fprintf(s.bw, "[log] of which %d important displaced, %d lost to write errors\n", di, de)
	}
	s.pending += 2
	s.flush()
}

// queue is a FIFO of lines on a slice that compacts instead of growing.
type queue struct {
	buf  [][]byte
	head int
}

func (q *queue) len() int { return len(q.buf) - q.head }

func (q *queue) push(b []byte) {
	if q.head > 0 && len(q.buf) == cap(q.buf) {
		n := copy(q.buf, q.buf[q.head:])
		clear(q.buf[n:])
		q.buf = q.buf[:n]
		q.head = 0
	}
	q.buf = append(q.buf, b)
}

func (q *queue) dropOldest() {
	q.buf[q.head] = nil
	q.head++
	q.reset()
}

func (q *queue) popInto(dst [][]byte, n int) [][]byte {
	dst = append(dst, q.buf[q.head:q.head+n]...)
	clear(q.buf[q.head : q.head+n])
	q.head += n
	q.reset()
	return dst
}

func (q *queue) reset() {
	if q.head == len(q.buf) {
		q.buf = q.buf[:0]
		q.head = 0
	}
}
