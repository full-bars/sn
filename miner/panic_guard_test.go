package miner

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

// panic_guard_test.go covers the containment contract of panic_guard.go: a
// panicking body must not propagate to the caller or take the process down, and
// the panic must still be reported. The audit spawn test is the live path: the
// control socket's `audit on|off` spawns the same runOnce the supervised ticker
// runs, and the runOnceGate must be free afterwards or every later toggle is
// silently dropped.

// recordingConnectLogger captures what connect.HandleError reports. The barrier
// reports the panic through connect's default logger, so installing one here is
// how a test observes the report.
type recordingConnectLogger struct {
	mu     sync.Mutex
	lines  []string
	signal chan struct{}
}

func newRecordingConnectLogger(t *testing.T) *recordingConnectLogger {
	t.Helper()
	l := &recordingConnectLogger{signal: make(chan struct{}, 1)}
	previous := connect.DefaultLogger()
	connect.SetDefaultLogger(l)
	t.Cleanup(func() { connect.SetDefaultLogger(previous) })
	return l
}

func (l *recordingConnectLogger) record(line string) {
	l.mu.Lock()
	l.lines = append(l.lines, line)
	l.mu.Unlock()
	select {
	case l.signal <- struct{}{}:
	default:
	}
}

func (l *recordingConnectLogger) Info(args ...any)               { l.record(fmt.Sprint(args...)) }
func (l *recordingConnectLogger) Infof(f string, args ...any)    { l.record(fmt.Sprintf(f, args...)) }
func (l *recordingConnectLogger) Warningf(f string, args ...any) { l.record(fmt.Sprintf(f, args...)) }
func (l *recordingConnectLogger) Errorf(f string, args ...any)   { l.record(fmt.Sprintf(f, args...)) }
func (l *recordingConnectLogger) V(level int32) connect.Verbose  { return discardVerbose{} }

type discardVerbose struct{}

func (discardVerbose) Enabled() bool               { return false }
func (discardVerbose) Info(args ...any)            {}
func (discardVerbose) Infof(f string, args ...any) {}

// waitForLine blocks until a recorded line contains want, so the tests never
// depend on a sleep to know the goroutine got there.
func (l *recordingConnectLogger) waitForLine(t *testing.T, want string) {
	t.Helper()
	l.waitForLines(t, want, 1)
}

// waitForLines blocks until at least n recorded lines contain want. The second
// occurrence is what proves a later guarded call actually ran rather than the
// assertion matching the first one again.
func (l *recordingConnectLogger) waitForLines(t *testing.T, want string, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		l.mu.Lock()
		seen := 0
		for _, line := range l.lines {
			if strings.Contains(line, want) {
				seen++
			}
		}
		if seen >= n {
			l.mu.Unlock()
			return
		}
		lines := append([]string(nil), l.lines...)
		l.mu.Unlock()
		select {
		case <-l.signal:
		case <-deadline:
			t.Fatalf("want %d log line(s) containing %q, got %d: %v", n, want, seen, lines)
		}
	}
}

// A panic in the guarded body must not propagate: runSafe returns and the panic
// is reported. Were the panic unhandled, it would take this test binary down
// at the call, which is exactly the process death being fixed.
func TestRunSafe_ContainsAPanicAndReportsIt(t *testing.T) {
	logger := newRecordingConnectLogger(t)

	runSafe("test-guard", func() {
		panic("guard boom")
	})
	// Reaching here at all is the containment assertion.
	logger.waitForLine(t, "guard boom")

	// The guard still works afterwards: the contained panic left the process
	// usable rather than merely alive.
	ranNormally := false
	runSafe("test-guard-after-panic", func() { ranNormally = true })
	if !ranNormally {
		t.Fatal("a guarded body after a contained panic did not run")
	}
}

// The same contract for the spawned form: the goroutine's own defer still runs
// while the panic unwinds, and the process keeps running with the panic
// reported.
func TestGoSafe_ContainsAPanicInTheSpawnedGoroutine(t *testing.T) {
	logger := newRecordingConnectLogger(t)

	returned := make(chan struct{})
	goSafe("test-spawned-guard", func() {
		defer close(returned)
		panic("spawned boom")
	})

	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the guarded goroutine never reached its defer")
	}
	logger.waitForLine(t, "spawned boom")
}

// The live path: the control-socket toggle spawns one immediate audit tick. A
// panic in that tick must be contained, and the gate it holds must be released
// so a later toggle still gets its tick.
func TestSpawnRunOnce_ContainsAPanicAndReleasesTheGate(t *testing.T) {
	logger := newRecordingConnectLogger(t)

	a := newProxyAuditor(proxyAuditConfig{}, proxyAuditEnv{
		now: func() time.Time { panic("audit tick boom") },
		log: tlog,
	})

	spawnRunOnce(a)
	// The report is logged after the body's defers unwind, so observing it
	// also proves the gate defer ran.
	logger.waitForLine(t, "audit tick boom")

	if held := len(runOnceGate); held != 0 {
		t.Fatalf("runOnceGate still holds %d token(s) after a contained panic; later toggles would be dropped", held)
	}

	// And the gate is genuinely reusable: a second toggle spawns and is
	// contained the same way.
	spawnRunOnce(a)
	logger.waitForLines(t, "audit tick boom", 2)
}
