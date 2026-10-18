package miner

import (
	"os"
	"runtime/debug"
	"sync"
)

// The provide lifecycle records itself in events.log the way the v33 provider
// does (provider/provide.go provide()): STARTUP when the identity is staged
// and the run is committed, SIGNAL when the run context drains, and PROVIDER
// EXIT on a clean return. run() calls into here so the lines and the
// first-canceller reporting stay pinned by tests without needing a full
// providing run.

// providerStartupCritLog writes the STARTUP record: version, pid and host.
func providerStartupCritLog() {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	critLog("STARTUP: version=%s pid=%d host=%s", RequireVersion(), os.Getpid(), host)
}

// runCancelSource reports what cancelled a providing run. The first canceller
// leaves its stack, the v33 record; a run whose parent context went down first
// (the startup signal handler cancels the parent, not this run's wrapper) has
// no stack and reports the v33 parent-cancellation text instead.
type runCancelSource struct {
	mu    sync.Mutex
	stack string
}

// record keeps the first canceller's stack; later cancellers do not replace
// it, exactly like v33's cancelSourceOnce.
func (self *runCancelSource) record() {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.stack == "" {
		self.stack = string(debug.Stack())
	}
}

func (self *runCancelSource) text() string {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.stack != "" {
		return self.stack
	}
	return "context cancelled by parent (signal or parent cancel)"
}
