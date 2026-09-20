package provider

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logSink) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "")
}

// The reproduction of the live finding: the parent holds the port, the
// candidate's first bind fails, and it must succeed once the parent lets go.
func TestEnableProfilingWithRetryBindsOnceParentReleases(t *testing.T) {
	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := parent.Addr().String()

	go func() {
		time.Sleep(250 * time.Millisecond)
		parent.Close() // parent drain finished, port is free
	}()

	var sink logSink
	done := make(chan struct{})
	go func() {
		enableProfilingWithRetry(context.Background(), addr, EnableProfiling, 5*time.Second, 25*time.Millisecond, sink.logf)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(6 * time.Second):
		t.Fatal("retry did not finish")
	}
	if !strings.Contains(sink.joined(), "diagnostics enabled") {
		t.Fatalf("expected success after the parent released the port, log: %q", sink.joined())
	}
	// It really serves.
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("diagnostics not listening after retry: %v", err)
	}
	conn.Close()
}

func TestEnableProfilingWithRetryGivesUpWhilePortStaysBusy(t *testing.T) {
	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	var sink logSink
	start := time.Now()
	enableProfilingWithRetry(context.Background(), parent.Addr().String(), EnableProfiling, 150*time.Millisecond, 20*time.Millisecond, sink.logf)
	if !strings.Contains(sink.joined(), "gave up") {
		t.Fatalf("expected a give-up message, got %q", sink.joined())
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("did not honor the wait bound: %s", time.Since(start))
	}
}

// An address that can never bind (malformed) will never start working, so it
// is reported once, not retried.
func TestEnableProfilingWithRetryDoesNotRetryPermanentErrors(t *testing.T) {
	calls := 0
	var sink logSink
	enableProfilingWithRetry(context.Background(), "not-a-listen-address", func(a string) error {
		calls++
		return EnableProfiling(a)
	}, 5*time.Second, 10*time.Millisecond, sink.logf)
	if calls != 1 {
		t.Fatalf("permanent error retried %d times", calls)
	}
	if !strings.Contains(sink.joined(), "failed to enable diagnostics") {
		t.Fatalf("log: %q", sink.joined())
	}
}

func TestEnableProfilingWithRetryStopsOnContextCancel(t *testing.T) {
	parent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		enableProfilingWithRetry(ctx, parent.Addr().String(), EnableProfiling, time.Minute, 20*time.Millisecond, func(string, ...any) {})
		close(done)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("did not stop on context cancel")
	}
}
