package miner

import (
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The v33 provider freezes the proxy reaper while the backend is down: during
// an outage every proxy fails auth, so scoring those failures against proxies
// blacklists healthy ones for a cooldown that outlives the outage. The port
// carried the freeze sites (proxy_manager_url_source.go) but not a detector —
// degradedReaperFrozen was a constant false — because the connect pin does not
// export connect.OutageRecent.
//
// This is the interim detector. It watches the auth stream the miner already
// produces and reports an outage when nothing has authenticated anywhere in the
// pool for a sustained window, with a failure floor so a single bad proxy (or a
// tiny pool) cannot freeze the reaper on its own. A single success anywhere
// clears it immediately: the point is to distinguish "the platform is
// unreachable" from "these particular proxies are bad".
const (
	// Defaults only: both are overridable so a box with a different pool size or
	// a slower platform can tune the freeze without a rebuild.
	proxyOutageFreezeDefaultWindow      = 5 * time.Minute
	proxyOutageFreezeDefaultMinFailures = 5

	// proxyOutageFreezeWindowEnv and proxyOutageFreezeMinFailuresEnv override
	// the defaults (a duration like "90s", and an integer).
	proxyOutageFreezeWindowEnv      = "PROXY_OUTAGE_FREEZE_WINDOW"
	proxyOutageFreezeMinFailuresEnv = "PROXY_OUTAGE_FREEZE_MIN_FAILURES"
)

// proxyOutageFreezeWindow answers the configured window, falling back to the
// default when unset or unparseable.
func proxyOutageFreezeWindow() time.Duration {
	raw := strings.TrimSpace(os.Getenv(proxyOutageFreezeWindowEnv))
	if raw == "" {
		return proxyOutageFreezeDefaultWindow
	}
	window, err := time.ParseDuration(raw)
	if err != nil || window <= 0 {
		return proxyOutageFreezeDefaultWindow
	}
	return window
}

// proxyOutageFreezeMinFailures answers the configured failure floor.
func proxyOutageFreezeMinFailures() int {
	raw := strings.TrimSpace(os.Getenv(proxyOutageFreezeMinFailuresEnv))
	if raw == "" {
		return proxyOutageFreezeDefaultMinFailures
	}
	floor, err := strconv.Atoi(raw)
	if err != nil || floor <= 0 {
		return proxyOutageFreezeDefaultMinFailures
	}
	return floor
}

// proxyOutageFreeze tracks the pool-wide auth outcome.
type proxyOutageFreeze struct {
	mu          sync.Mutex
	window      time.Duration
	minFailures int
	now         func() time.Time

	// lastSuccess is when the pool last authenticated anything. It starts at
	// construction so a process that boots into an outage still needs a full
	// window of failure before it freezes.
	lastSuccess time.Time
	failures    int
}

func newProxyOutageFreeze() *proxyOutageFreeze {
	f := &proxyOutageFreeze{
		window:      proxyOutageFreezeWindow(),
		minFailures: proxyOutageFreezeMinFailures(),
		now:         time.Now,
	}
	f.lastSuccess = f.now()
	return f
}

// success records an authenticated proxy. Any success means the platform is
// reachable, so the freeze clears immediately.
func (f *proxyOutageFreeze) success() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastSuccess = f.now()
	f.failures = 0
}

// failure records an auth failure that did not authenticate.
func (f *proxyOutageFreeze) failure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failures++
}

// frozen reports whether the pool looks platform-unreachable: enough failures
// to rule out a single bad proxy, and no success for a full window.
func (f *proxyOutageFreeze) frozen() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failures < f.minFailures {
		return false
	}
	return f.now().Sub(f.lastSuccess) >= f.window
}

// proxyOutageTracker is the process-wide instance, fed by the per-slot auth
// accounting.
var proxyOutageTracker = newProxyOutageFreeze()

// proxyAuthOutageFrozen reports whether the AUTH stream says the platform is
// unreachable. It deliberately does not feed degradedReaperFrozen: the reaper
// has probe results and decides from those (noProbeReachedTheAPI), while the
// give-up ladder only ever sees auth outcomes. One lever per evidence source.
func proxyAuthOutageFrozen() bool {
	return proxyOutageTracker.frozen()
}
