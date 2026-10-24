package miner

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// This file adds the live half of the proxy reload contract to the miner.
//
// The provider lineage (urnetwork-3.23-fix provider/proxy_reload.go) applies a
// reload trigger by diffing the desired set against the pool it is running and
// spawning/cancelling per-proxy goroutines. The miner port kept only the lock
// and trigger halves, so `urnet-tools proxy add` wrote the trigger and nothing
// read it: the CLI promised a hot reload and the running provider ignored the
// change until a restart (see miner/proxy_manager_reload.go's own comment).
//
// The reloader here is the miner-native equivalent, deliberately narrower than
// the provider's: it owns the diff and the lifecycle of the slots it started,
// and the caller supplies the two seams that only the provide run can know —
// how to compute the desired set (proxyManager.desired) and how to start one
// slot (the provideWithProxy closure). Trim, pressure, the OOM cap and the
// auth/probe registries stay with the code that already owns them: a slot
// cancelled here runs the same teardown it runs on any other cancellation.

// minerReloadSlot is one running proxy slot owned by the reloader.
type minerReloadSlot struct {
	settings *connect.ProxySettings
	cancel   context.CancelFunc
}

// minerReloadSummary reports what one reload changed, for the operator log.
type minerReloadSummary struct {
	Added   int
	Removed int
	// Lost counts identities that were being rotated and whose replacement
	// failed to start: the old slot was already cancelled, so the identity is
	// not being served. It must be visible, not folded into "rotated".
	Lost      int
	Rotated   int
	Unchanged int
}

// String renders the summary as the single line operators see after a reload.
func (s minerReloadSummary) String() string {
	return fmt.Sprintf("added %d, removed %d, rotated %d, unchanged %d, lost %d", s.Added, s.Removed, s.Rotated, s.Unchanged, s.Lost)
}

// minerProxyReloader applies desired-set changes to a running miner.
//
// Reloads are serialised by mu. The desired set is recomputed on every reload
// rather than cached, so a reload always reflects the files and URL cache as
// they are now.
type minerProxyReloader struct {
	mu sync.Mutex

	// desired recomputes the proxy set the run should be serving. It must not
	// mutate the running pool; a non-nil error aborts the reload with the pool
	// untouched, which matters in Workflow A: a transient read error must not
	// be read as "no proxies" and cancel the whole fleet.
	desired func() ([]*connect.ProxySettings, error)

	// spawn starts one slot and returns the function that stops it. The
	// cancel must be safe to call more than once: removal, stop and the run's
	// own shutdown can all race to stop the same slot.
	spawn func(*connect.ProxySettings) (context.CancelFunc, error)

	// running is keyed by proxy identity (ProxySettings.Key), the same key the
	// bandwidth registry and the earnings store use, so two accounts behind
	// one gateway stay distinct.
	running map[string]*minerReloadSlot

	// onChange, when set, is called with the identities still served - sorted -
	// after every successful reload and after stop. The provide run uses it to
	// keep the OOM cap and the control surface's running set truthful: once a
	// reload has added or cancelled a slot, the reloader is the only thing that
	// knows the live set.
	onChange func(keys []string)

	// onReload, when set, runs at the start of every reload, before the desired
	// proxy set is read. The provide run uses it for the native direct slot:
	// that slot is not a proxy identity and never enters `running`, but its
	// operator toggle travels on the same reload trigger as `proxy add`, so this
	// is where the control path reaches it (direct_slot.go). Running it before
	// the read means `provider direct off` still takes effect when proxy_url.json
	// is unreadable and the proxy diff below aborts. Held under mu like the rest
	// of the reload, so a direct stop cannot interleave with a proxy diff.
	onReload func()

	// addressOwners records which identity owns each host:port for this run.
	// providerSlotIdentities resolves a configured address to a slot hash once,
	// at start; a second identity appearing at a live address later would make
	// two slots share one credential file and one progress slot, so a runtime
	// add at an owned address is refused rather than silently re-identifying
	// the running slot.
	addressOwners sync.Map // address -> identity key

	// stopped makes stop() terminal: a reload that was already computing its
	// desired set when the yield landed must not re-spawn the pool.
	stopped bool
	// watchCancel ends the watcher, so stop() does not leave it polling a
	// trigger for a process that has yielded.
	watchCancel context.CancelFunc
}

func newMinerProxyReloader(
	desired func() ([]*connect.ProxySettings, error),
	spawn func(*connect.ProxySettings) (context.CancelFunc, error),
) *minerProxyReloader {
	return &minerProxyReloader{
		desired: desired,
		spawn:   spawn,
		running: map[string]*minerReloadSlot{},
	}
}

// sameProxyCredentials reports whether two settings carry identical
// credentials. Address/network are the diff key and are ignored here; only the
// credentials decide whether a running slot must be rotated. A nil-vs-set
// mismatch counts as different: keeping stale credentials is worse than one
// extra relaunch (the LA7 incident: 100 proxies re-pasted with new credentials,
// the tool reported "added 100", and the daemon kept dialing the old user).
func sameProxyCredentials(a, b *connect.ProxySettings) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	case a.Auth == nil && b.Auth == nil:
		return true
	case a.Auth == nil || b.Auth == nil:
		return false
	default:
		return a.Auth.User == b.Auth.User && a.Auth.Password == b.Auth.Password
	}
}

// runningKeys returns the identities currently served, sorted, for tests and
// logs.
func (r *minerProxyReloader) runningKeys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sortedRunningKeysLocked()
}

// sortedRunningKeysLocked returns the running identities sorted by key. The
// caller must hold mu.
func (r *minerProxyReloader) sortedRunningKeysLocked() []string {
	keys := make([]string, 0, len(r.running))
	for key := range r.running {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// notify publishes the running set to onChange. It must be called without mu
// held, so a callback that reads the reloader back cannot deadlock it.
func (r *minerProxyReloader) notify(keys []string) {
	if r.onChange != nil {
		r.onChange(keys)
	}
}

// seed records an identity this run already launched itself, with the cancel
// that stops it. Without it the first diff would see the launched slot as
// missing from its own bookkeeping and spawn a second one for the same
// identity - and re-identifying a live slot is exactly what the platform
// session must not do.
func (r *minerProxyReloader) seed(settings *connect.ProxySettings, cancel context.CancelFunc) {
	if settings == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running[settings.Key()] = &minerReloadSlot{settings: settings, cancel: cancel}
	if settings.Address != "" {
		// Claim, but never refuse: the configured set may legitimately put two
		// identities behind one address (providerSlotIdentities keys those
		// distinctly at start). The first claim wins and the guard below only
		// governs identities added while this run is live.
		r.addressOwners.LoadOrStore(settings.Address, settings.Key())
	}
}

// reserveAddress claims a host:port for one identity, refusing an address that
// already belongs to a DIFFERENT identity. The refused key is not stored, so
// the running claim stays the one that is actually serving. A runtime add at a
// live address would otherwise silently re-identify the running slot: the slot
// hash depends on whether its address is shared, and that classification is
// fixed at start (provider_slot_identity.go).
func (r *minerProxyReloader) reserveAddress(settings *connect.ProxySettings) error {
	if settings == nil || settings.Address == "" {
		return nil
	}
	key := settings.Key()
	if existing, loaded := r.addressOwners.LoadOrStore(settings.Address, key); loaded {
		if owner, _ := existing.(string); owner != key {
			return fmt.Errorf("address %s is already served by identity %s in this run; a second identity at one address would share its slot identity (restart to re-key)", settings.Address, owner)
		}
	}
	return nil
}

// releaseAddress drops the claim when the identity holding it stops, so a
// later reload can put a different identity on the same address without a
// restart.
func (r *minerProxyReloader) releaseAddress(settings *connect.ProxySettings) {
	if settings == nil || settings.Address == "" {
		return
	}
	r.addressOwners.CompareAndDelete(settings.Address, settings.Key())
}

// reload recomputes the desired set and applies it to the running pool.
//
// The whole reload — read and apply — holds mu, so two reloads cannot compute
// concurrently and apply in reverse order with a stale set winning.
//
// A rotation cancels the old slot before spawning the replacement: one identity
// must never hold two platform sessions at once. A rotation therefore has a
// brief window where the identity is not served; if the replacement fails to
// start it is not served at all, and the summary says so (Lost) rather than
// implying a clean swap.
func (r *minerProxyReloader) reload() (minerReloadSummary, error) {
	var summary minerReloadSummary

	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return summary, errProxyReloaderStopped
	}

	// The operator's native direct toggle first, before the desired proxy set
	// is read: the direct slot is not part of that diff, and a failed proxy
	// read must not leave a `direct off` unapplied (direct_slot.go, onReload).
	if r.onReload != nil {
		r.onReload()
	}

	desired, err := r.desired()
	if err != nil {
		// Leave the pool exactly as it is. A failed read is not permission to
		// stop serving.
		r.mu.Unlock()
		return summary, fmt.Errorf("reload aborted, running set left as-is: %w", err)
	}

	wanted := make(map[string]*connect.ProxySettings, len(desired))
	for _, settings := range desired {
		if settings == nil {
			continue
		}
		wanted[settings.Key()] = settings
	}

	var errs []error
	for key, settings := range wanted {
		slot, ok := r.running[key]
		if ok && sameProxyCredentials(slot.settings, settings) {
			summary.Unchanged++
			continue
		}
		if ok {
			// Rotate: cancel first so the identity cannot hold two sessions.
			slot.cancel()
			delete(r.running, key)
			r.releaseAddress(slot.settings)
		}
		cancel, spawnErr := r.spawn(settings)
		if spawnErr != nil {
			// A cancel returned alongside an error would otherwise leak.
			if cancel != nil {
				cancel()
			}
			errs = append(errs, fmt.Errorf("start %s: %w", key, spawnErr))
			if ok {
				// The old slot is gone and the replacement is not running.
				summary.Lost++
				errs = append(errs, fmt.Errorf("identity %s is no longer served after a failed rotation", key))
			}
			// Keep reconciling: one bad identity must not freeze every other
			// operator change, and the removals below still run so a dead proxy
			// stops even when an addition failed.
			continue
		}
		r.running[key] = &minerReloadSlot{settings: settings, cancel: cancel}
		if ok {
			summary.Rotated++
		} else {
			summary.Added++
		}
	}

	// Stop what is gone. This runs regardless of spawn failures above.
	for key, slot := range r.running {
		if _, ok := wanted[key]; ok {
			continue
		}
		slot.cancel()
		delete(r.running, key)
		r.releaseAddress(slot.settings)
		summary.Removed++
	}

	keys := r.sortedRunningKeysLocked()
	r.mu.Unlock()

	// Publish the applied set: the OOM cap and the control surface's running
	// set both describe what this run is really serving.
	r.notify(keys)

	return summary, errors.Join(errs...)
}

// errProxyReloaderStopped reports a reload refused because the reloader has
// already been stopped (shutdown or a hotswap yield).
var errProxyReloaderStopped = errors.New("proxy reloader stopped")

// stop cancels every slot the reloader started, for shutdown and for a hotswap
// yield (the successor takes the pool over and this process must not keep
// holding sessions).
func (r *minerProxyReloader) stop() {
	r.mu.Lock()
	// Terminal: a reload already past the stopped check must not re-spawn the
	// pool after a yield, and the watcher must stop polling a trigger for a
	// process that no longer serves.
	r.stopped = true
	if r.watchCancel != nil {
		r.watchCancel()
		r.watchCancel = nil
	}
	for key, slot := range r.running {
		slot.cancel()
		r.releaseAddress(slot.settings)
		delete(r.running, key)
	}
	keys := r.sortedRunningKeysLocked()
	r.mu.Unlock()
	r.notify(keys)
}

// proxyReloadWatcherActive reports whether this process applies reload
// triggers, so a control-socket reader can tell clients that `proxy add`
// really does take effect without a restart rather than following a
// build-time flag. It is set by the watcher below; no reader is wired yet.
var proxyReloadWatcherActive atomic.Bool

// watch polls the reload trigger until ctx ends, applying a reload whenever the
// sequence number changes. The interval matches the provider's two-second
// contract, which is what the CLI's "no restart" promise is measured against.
//
// A failed reload leaves the sequence unrecorded, so the change is retried on
// the next tick instead of being silently dropped.
func (r *minerProxyReloader) watch(ctx context.Context, path string, interval time.Duration) {
	// Publish that this process now applies reloads, so the control socket can
	// tell clients the truth and the operator messages follow the mechanism
	// rather than a build-time flag.
	proxyReloadWatcherActive.Store(true)
	defer proxyReloadWatcherActive.Store(false)

	// stop() ends this watcher, not just the run ctx: a yielded process must
	// not keep polling a trigger it will refuse to apply.
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		cancel()
		return
	}
	r.watchCancel = cancel
	r.mu.Unlock()
	defer cancel()

	if interval <= 0 {
		interval = minerProxyReloadWatchInterval
	}
	lastSeq, _ := readReloadSeq(path)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			seq, err := readReloadSeq(path)
			if err != nil {
				tlog("[proxy] reload trigger read failed: %v\n", err)
				continue
			}
			if seq == lastSeq {
				continue
			}
			tlog("🔄 [proxy] reload requested (trigger #%d, was #%d)\n", seq, lastSeq)
			summary, err := r.reload()
			if err != nil {
				if errors.Is(err, errProxyReloaderStopped) {
					return
				}
				tlog("[proxy] reload failed: %v (will retry)\n", err)
				continue
			}
			lastSeq = seq
			tlog("✅ [proxy] reload complete: %s\n", summary)
		}
	}
}

// minerProxyReloadWatchInterval is the poll cadence for the reload trigger. The
// operator-facing promise is a couple of seconds, and the trigger write already
// debounces at 30s, so this loop is cheap.
const minerProxyReloadWatchInterval = 2 * time.Second
