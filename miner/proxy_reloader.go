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
	// onApplied, when set, is called after a reload has applied a new desired
	// set, with the settings it applied (nil entries filtered out). It runs
	// without mu held, so a callback that reads the reloader back cannot
	// deadlock it. The provide run uses it as the APPLY half of the h3
	// candidate set: the desired read is side-effect free, so the ranking and
	// any identity relaunch are driven from here (see applyH3CandidateSet).
	onApplied func(applied []*connect.ProxySettings)

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

	// lastSeq is the reload trigger sequence this process has already applied,
	// and seeded records that it has been read once. The baseline belongs to
	// the reloader, not to one watch() call: the supervisor restarts watch()
	// after a panic, and a fresh baseline on every restart would adopt the
	// sequence of the reload that panicked - or of a trigger written while the
	// watcher waited out a backoff of up to loopBackoffMax - as already
	// applied. The operator's `proxy add` would then be dropped with no log.
	// Only the first watch seeds it, and it advances only after a reload
	// really succeeded, so a restart retries the pending sequence.
	lastSeq int64
	seeded  bool
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

// reloadSeq returns the last applied trigger sequence and whether a baseline
// has been taken yet.
func (r *minerProxyReloader) reloadSeq() (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSeq, r.seeded
}

// seedReloadSeq records the first baseline this process reads. It is a no-op
// once seeded: only the first watch() may adopt a sequence, so a restart can
// never swallow a change the previous watcher did not apply.
func (r *minerProxyReloader) seedReloadSeq(seq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seeded {
		return
	}
	r.lastSeq = seq
	r.seeded = true
}

// advanceReloadSeq records seq as applied, after a reload really succeeded. The
// assignment is unconditional, like the local baseline it replaces: the trigger
// file is written as current+1, so if it is ever recreated from scratch the
// sequence goes backwards, and refusing to move the baseline with it would
// re-apply every tick until it climbed past the old value.
func (r *minerProxyReloader) advanceReloadSeq(seq int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastSeq = seq
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
//
// A panic anywhere in the body — the desired read, the direct toggle, a slot's
// cancel — releases mu on the way out. The watcher is supervised and restarted
// after a panic, and the restart has to be able to reach the reloader: a lock
// left held would deadlock the retry of the very sequence the panic
// interrupted, which looks exactly like the trigger being lost.
func (r *minerProxyReloader) reload() (minerReloadSummary, error) {
	var summary minerReloadSummary

	r.mu.Lock()
	locked := true
	defer func() {
		if locked {
			r.mu.Unlock()
		}
	}()
	if r.stopped {
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
	locked = false

	// Publish the applied set: the OOM cap and the control surface's running
	// set both describe what this run is really serving.
	r.notify(keys)

	// The apply half: the desired read this reload just did is side-effect
	// free, so the ranking and any relaunch it implies are driven from here,
	// with the pool already reconciled and no lock held.
	if r.onApplied != nil {
		applied := make([]*connect.ProxySettings, 0, len(desired))
		for _, settings := range desired {
			if settings != nil {
				applied = append(applied, settings)
			}
		}
		r.onApplied(applied)
	}

	return summary, errors.Join(errs...)
}

// installProvideH3Relauncher publishes the provide run's per-identity relaunch
// facility for the life of the run, and returns the function that clears it.
//
// It is the run's wiring for F-04: the h3 relaunch pass only ever starts when a
// relauncher is published, and before this existed `set h3 ...` reported a live
// change and did nothing. A nil reloader (a profile run, which owns a fixed,
// validated slot set) publishes nothing: there is no slot bookkeeping to detach
// through, so a relaunch could not remove the running identity and would only
// claim an effect it cannot have.
func installProvideH3Relauncher(ctx context.Context, reloader *minerProxyReloader) (clear func()) {
	if reloader == nil {
		return func() {}
	}
	setH3IdentityRelauncher(&minerH3Relauncher{ctx: ctx, reloader: reloader})
	return func() { setH3IdentityRelauncher(nil) }
}

// relaunchIdentity stops one running identity so its next launch builds under
// the current setting, and reports false when this run is not serving it.
//
// The slot is cancelled and dropped from the bookkeeping in ONE critical
// section, then the pool is reconciled by a reload. That ordering is the whole
// point: a detach that left the bookkeeping alone would make the next diff
// report the identity "unchanged" and never spawn it again - the identity would
// be silently gone until a restart. Detaching through here is what lets an h3
// mode change reconnect an identity without a restart (see
// minerH3Relauncher).
//
// The reload respawns every identity the desired set claims and this run is not
// serving, so the replacement is accounted for exactly like any other slot
// (address claim, progress census admission, onChange publication). A failure
// is logged and the identity stays down, which the reloader's own summary and
// next reload report like any other lost slot.
func (r *minerProxyReloader) relaunchIdentity(key string) bool {
	r.mu.Lock()
	slot, ok := r.running[key]
	if !ok {
		r.mu.Unlock()
		return false
	}
	slot.cancel()
	delete(r.running, key)
	r.releaseAddress(slot.settings)
	r.mu.Unlock()

	if _, err := r.reload(); err != nil {
		tlog("[h3] reload after detaching %s failed: %v\n", key, err)
	}
	return true
}

// minerH3Relauncher is the provide run's h3IdentityRelauncher: the per-identity
// relaunch facility the h3 relaunch pass drives (h3_proxy_set.go). It is backed
// by the live reloader, which owns the slots and their cancels, so a relaunch is
// a detach the reloader itself accounts for rather than one imposed from
// outside it.
type minerH3Relauncher struct {
	ctx      context.Context
	reloader *minerProxyReloader
}

func (r *minerH3Relauncher) detachForRelaunch(key string) bool {
	return r.reloader.relaunchIdentity(key)
}

// hasClients reports whether the identity is serving clients right now, so the
// relaunch pass can leave a live session alone and retry it later. v33 reads
// connect.ProxyBandwidthByKey(addr).Clients; the pinned connect does not export
// that registry, so the miner's own identity-keyed counters - the same objects
// the earnings recorder folds - are the equivalent.
func (r *minerH3Relauncher) hasClients(key string) bool {
	return h3IdentityHasClients(key)
}

func (r *minerH3Relauncher) runContext() context.Context { return r.ctx }

// h3IdentityHasClients is "is this identity serving clients now", read from the
// live per-identity bandwidth counters the provide loop publishes when a slot
// installs its counter and withdraws when it exits. An identity with no
// published counter is not running here, which is not busy.
func h3IdentityHasClients(key string) bool {
	bw := proxyEarnBandwidthFor(key)
	return bw != nil && bw.Clients.Load() > 0
}

// errProxyReloaderStopped reports a reload refused because the reloader has
// already been stopped (shutdown or a hotswap yield).
var errProxyReloaderStopped = errors.New("proxy reloader stopped")

// reap cancels the given identities, for the quality reaper and the pool
// controller's pressure cut.
//
// It is the miner's proxyCancelMap half of the provider's reapProxies: cancel
// each running slot under mu, release the address it claimed, forget it, and
// publish the resulting running set once. The identities that were actually
// cancelled are returned — a key this run is not running is skipped, because a
// slot the reloader does not own is not the caller's to cancel, and the caller
// needs the exact set to record its hold against (and, for a pressure cut, to
// persist a target it can prove).
//
// A stopped reloader cancels nothing: stop() is terminal, and a reaper that
// landed after a yield must not pretend it reaped anything.
func (r *minerProxyReloader) reap(keys []string) []string {
	r.mu.Lock()
	if r.stopped {
		r.mu.Unlock()
		return nil
	}
	var cancelled []string
	for _, key := range keys {
		slot, ok := r.running[key]
		if !ok {
			continue
		}
		slot.cancel()
		delete(r.running, key)
		r.releaseAddress(slot.settings)
		cancelled = append(cancelled, key)
	}
	if len(cancelled) == 0 {
		r.mu.Unlock()
		return nil
	}
	sort.Strings(cancelled)
	running := r.sortedRunningKeysLocked()
	r.mu.Unlock()

	// Publish outside the lock, like reload and stop: the OOM cap and the
	// control surface's running set must describe what is really served.
	r.notify(running)
	return cancelled
}

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
// the next tick instead of being silently dropped. The record is on the
// reloader, so a watcher the supervisor restarts after a panic resumes from the
// last sequence that really applied and retries the one that did not.
func (r *minerProxyReloader) watch(ctx context.Context, path string, interval time.Duration) {
	// Publish that this process now applies reloads only once the baseline has
	// been read: "active" must mean "polling from a known sequence". Set any
	// earlier and a reader can conclude the watcher will apply a change that
	// its first tick will silently miss.
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
	// Seed the baseline once for the reloader's whole life, not once per
	// watch(). The supervisor restarts this watcher after a panic, and the
	// sequence that was being applied when it died must stay pending: a fresh
	// read here would adopt it as the new baseline and the change would never
	// be retried (see lastSeq on minerProxyReloader).
	if _, seeded := r.reloadSeq(); !seeded {
		seq, _ := readReloadSeq(path)
		r.seedReloadSeq(int64(seq))
	}
	// Publish only now: a reader that sees "active" must be able to rely on
	// the watcher having a baseline, or it will write a change the first tick
	// treats as already seen.
	proxyReloadWatcherActive.Store(true)
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
			// The baseline is the reloader's, so a watcher restarted after a
			// panic resumes from the last sequence that really applied.
			lastSeq, _ := r.reloadSeq()
			if int64(seq) == lastSeq {
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
			// Advanced only on success: a panic before here, or a failed
			// reload, leaves the sequence pending for the restart or the next
			// tick to retry.
			r.advanceReloadSeq(int64(seq))
			tlog("✅ [proxy] reload complete: %s\n", summary)
		}
	}
}

// minerProxyReloadWatchInterval is the poll cadence for the reload trigger. The
// operator-facing promise is a couple of seconds, and the trigger write already
// debounces at 30s, so this loop is cheap.
const minerProxyReloadWatchInterval = 2 * time.Second
