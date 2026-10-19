package miner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

// This file ports the 3.23-fix v33 provider's per-address proxy bookkeeping:
// proxy_failure_history.go (lifetime failure/give-up/backoff history),
// proxy_auth_history.go (the proven-proxy set) and proxy_slow_retry.go
// (persisted slow-retry state). The miner's P1(a) does not yet drive the auth
// pipeline from these (P1(b)/P2 wiring), but the state machines are ported in
// full, with their tests, because the URL give-up/reload paths consume them.

// proxyFailureHistory tracks, for the lifetime of this process, how many
// auth attempts have failed for a given proxy address - across every
// launch, including any requeue after maxAuthFailures gives up and the
// URL-source fetcher brings the address back in.
type proxyFailureHistory struct {
	mu       sync.Mutex
	failures map[string]int
	giveUps  map[string]int

	// backoffUntil records, per URL-sourced address, the earliest time it may
	// be relaunched. It is set on each give-up to now+proxyURLGiveUpRetryDelay.
	// The reload path consults Eligible() and skips re-adding an address whose
	// window has not elapsed, so the escalating backoff is actually enforced.
	backoffUntil map[string]time.Time
}

var globalProxyFailureHistory = &proxyFailureHistory{failures: map[string]int{}}

// RecordFailure records another failed auth attempt for address and returns
// the new lifetime total.
func (h *proxyFailureHistory) RecordFailure(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failures[address]++
	return h.failures[address]
}

// FailureCount reports address's lifetime failure count.
func (h *proxyFailureHistory) FailureCount(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.failures[address]
}

// RecordGiveUp records another give-up cycle for address: the proxy
// exhausted its per-launch auth attempts and is backing off before its next
// requeue. Returns the new lifetime total.
func (h *proxyFailureHistory) RecordGiveUp(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.giveUps == nil {
		h.giveUps = map[string]int{}
	}
	h.giveUps[address]++
	return h.giveUps[address]
}

// GiveUpCount reports address's lifetime give-up count.
func (h *proxyFailureHistory) GiveUpCount(address string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.giveUps[address]
}

// SetBackoffUntil records the earliest time address may be relaunched.
func (h *proxyFailureHistory) SetBackoffUntil(address string, until time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.backoffUntil == nil {
		h.backoffUntil = map[string]time.Time{}
	}
	h.backoffUntil[address] = until
}

// ExtendBackoffUntil is SetBackoffUntil that can only lengthen a window. It
// records until only when address has no backoff, or until is later than the
// current one, and reports whether it did.
func (h *proxyFailureHistory) ExtendBackoffUntil(address string, until time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.backoffUntil == nil {
		h.backoffUntil = map[string]time.Time{}
	}
	if cur, ok := h.backoffUntil[address]; ok && !until.After(cur) {
		return false
	}
	h.backoffUntil[address] = until
	return true
}

// Eligible reports whether address may be (re)launched as of now: true if it
// has no recorded backoff window, or that window has elapsed.
func (h *proxyFailureHistory) Eligible(address string, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.backoffUntil[address]
	if !ok {
		return true
	}
	return !now.Before(until)
}

// Keys lists every key the history holds any record for (failure count,
// give-up count or backoff), sorted.
func (h *proxyFailureHistory) Keys() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	seen := map[string]bool{}
	for k := range h.failures {
		seen[k] = true
	}
	for k := range h.giveUps {
		seen[k] = true
	}
	for k := range h.backoffUntil {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Reset clears address's failure and give-up counts, called when it
// successfully authenticates.
func (h *proxyFailureHistory) Reset(address string) {
	h.mu.Lock()
	delete(h.failures, address)
	delete(h.giveUps, address)
	delete(h.backoffUntil, address)
	h.mu.Unlock()
}

// ReleaseBackoff clears address's backoff only if it is exactly until, the
// window the caller set, and reports whether it did.
func (h *proxyFailureHistory) ReleaseBackoff(address string, until time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	cur, ok := h.backoffUntil[address]
	if !ok || !cur.Equal(until) {
		return false
	}
	delete(h.backoffUntil, address)
	return true
}

// AddressesInBackoff returns addresses still mid-backoff as of now, for
// merging into a pruning keep-set so their history survives the wait.
func (h *proxyFailureHistory) AddressesInBackoff(now time.Time) map[string]bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string]bool, len(h.backoffUntil))
	for addr, until := range h.backoffUntil {
		if now.Before(until) {
			out[addr] = true
		}
	}
	return out
}

// Prune removes entries for addresses not in keepAddrs, called periodically
// to prevent unbounded growth from proxies that cycled out of the fleet.
func (h *proxyFailureHistory) Prune(keepAddrs map[string]bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for addr := range h.failures {
		if !keepAddrs[addr] {
			delete(h.failures, addr)
		}
	}
	for addr := range h.giveUps {
		if !keepAddrs[addr] {
			delete(h.giveUps, addr)
		}
	}
	for addr := range h.backoffUntil {
		if !keepAddrs[addr] {
			delete(h.backoffUntil, addr)
		}
	}
}

// ---- proven-proxy set (v33 proxy_auth_history.go) ----

// provenProxySet tracks, for the lifetime of this process, which proxy
// addresses have completed at least one successful auth. A proxy that has
// never succeeded and then times out is at least as likely to be a broken
// local hop as a sign the API itself is overloaded; gating which timeouts are
// allowed to feed the shared auth rate limiter on this history keeps a list
// full of "port open, proxy broken" entries from throttling every other proxy.
type provenProxySet struct {
	mu     sync.Mutex
	proven map[string]bool
}

var globalProvenProxies = &provenProxySet{proven: map[string]bool{}}

// MarkSucceeded records that address has completed at least one successful
// auth this run.
func (s *provenProxySet) MarkSucceeded(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.proven[address] = true
}

// HasSucceeded reports whether address has ever completed a successful auth.
func (s *provenProxySet) HasSucceeded(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.proven[address]
}

// Prune removes entries for addresses not in keepAddrs.
func (s *provenProxySet) Prune(keepAddrs map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for addr := range s.proven {
		if !keepAddrs[addr] {
			delete(s.proven, addr)
		}
	}
}

// ---- slow-retry state (v33 proxy_slow_retry.go) ----

// proxySlowRetryStateVersion mirrors proxyStateVersion: 2 = Proxies is keyed by
// proxy identity (ProxySettings.Key()), not bare address.
const proxySlowRetryStateVersion = 2

// proxySlowRetryState tracks operator-curated proxies that have entered
// the slow-retry phase (post-maxAuthFailures). Persists to disk so
// reboots don't reset the 14-day drop clock.
type proxySlowRetryState struct {
	mu      sync.Mutex
	Version int                             `json:"version,omitempty"`
	Proxies map[string]*proxySlowRetryEntry `json:"proxies"`
}

type proxySlowRetryEntry struct {
	// StartedAt records when this proxy first entered slow retry.
	StartedAt time.Time `json:"started_at"`
	// LastAttemptAt records the last time we tried this proxy during
	// slow retry.
	LastAttemptAt time.Time `json:"last_attempt_at"`
	// DroppedAt is set when the proxy exceeds slowRetryMaxDuration.
	DroppedAt *time.Time `json:"dropped_at,omitempty"`
}

const (
	// slowRetryDailyInterval is the pause between retry attempts once
	// a proxy enters slow retry.
	slowRetryDailyInterval = 24 * time.Hour

	// slowRetryMaxDuration is how long a proxy may continuously fail
	// in slow retry before being dropped from the active pool.
	slowRetryMaxDuration = 14 * 24 * time.Hour

	// proxySlowRetryCleanupAge is how long dropped entries remain in
	// the state file before being pruned on load.
	proxySlowRetryCleanupAge = 30 * 24 * time.Hour

	// slowRetryMaxConcurrent is the maximum number of slow-retry auth
	// attempts allowed to run concurrently.
	slowRetryMaxConcurrent = 3

	// slowRetryRampAttempts is the number of initial slow-retry
	// attempts that use the short ramp before switching to 24h daily.
	slowRetryRampAttempts = 3
)

var (
	globalProxySlowRetryState = newProxySlowRetryState()
	// slowRetrySemaphore limits concurrent slow-retry auth attempts.
	slowRetrySemaphore = make(chan struct{}, slowRetryMaxConcurrent)
)

func newProxySlowRetryState() *proxySlowRetryState {
	return &proxySlowRetryState{Proxies: make(map[string]*proxySlowRetryEntry)}
}

// RecordSlowRetryStart records when this proxy first entered slow retry.
// Subsequent calls for the same address are no-ops (preserves the original
// start time). Returns the entry's StartedAt for use in expiry checks.
func (s *proxySlowRetryState) RecordSlowRetryStart(address string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.Proxies[address]; ok {
		if entry.DroppedAt == nil {
			return entry.StartedAt
		}
		// Previously dropped - preserve original StartedAt so the 14-day
		// ceiling is continuous across restarts. Clear DroppedAt so the
		// entry re-enters active slow retry.
		entry.DroppedAt = nil
		entry.LastAttemptAt = time.Time{} // reset daily gate
		persistProxySlowRetryState(s)
		return entry.StartedAt
	}
	now := time.Now()
	s.Proxies[address] = &proxySlowRetryEntry{StartedAt: now}
	persistProxySlowRetryState(s)
	return now
}

// RecordSlowRetryAttempt records a retry attempt. Returns true if the proxy
// should be attempted now (enforces the 24h daily interval after the first
// three slow retries, which use the ramp).
func (s *proxySlowRetryState) RecordSlowRetryAttempt(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	if !ok || entry.DroppedAt != nil {
		return false
	}
	now := time.Now()
	if entry.LastAttemptAt.IsZero() || now.Sub(entry.LastAttemptAt) >= slowRetryDailyInterval {
		entry.LastAttemptAt = now
		persistProxySlowRetryState(s)
		return true
	}
	return false
}

// TimeUntilNextAttempt returns how long until the next daily retry is
// allowed. Returns zero if ready now.
func (s *proxySlowRetryState) TimeUntilNextAttempt(address string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	if !ok || entry.DroppedAt != nil || entry.LastAttemptAt.IsZero() {
		return slowRetryDailyInterval
	}
	remaining := slowRetryDailyInterval - time.Since(entry.LastAttemptAt)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// MarkDropped marks a proxy as dropped from the active pool. Returns the
// drop timestamp for use in log messages.
func (s *proxySlowRetryState) MarkDropped(address string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if entry, ok := s.Proxies[address]; ok {
		entry.DroppedAt = &now
	} else {
		s.Proxies[address] = &proxySlowRetryEntry{DroppedAt: &now}
	}
	persistProxySlowRetryState(s)
	return now
}

// ShouldDrop checks if a proxy has been in slow retry longer than
// slowRetryMaxDuration. Uses the persisted StartedAt timestamp so
// reboots don't reset the clock.
func (s *proxySlowRetryState) ShouldDrop(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	if !ok || entry.DroppedAt != nil {
		return false
	}
	return time.Since(entry.StartedAt) >= slowRetryMaxDuration
}

// WasDropped returns true if this address was previously dropped.
func (s *proxySlowRetryState) WasDropped(address string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.Proxies[address]
	return ok && entry.DroppedAt != nil
}

// DropTime returns when a previously-dropped proxy was dropped, or zero time.
func (s *proxySlowRetryState) DropTime(address string) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.Proxies[address]; ok && entry.DroppedAt != nil {
		return *entry.DroppedAt
	}
	return time.Time{}
}

// ClearDropped removes a drop record for an address. No-ops (and skips
// disk write) if the address has no entry.
func (s *proxySlowRetryState) ClearDropped(address string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.Proxies[address]; !ok {
		return
	}
	delete(s.Proxies, address)
	persistProxySlowRetryState(s)
}

// adoptLegacy migrates a legacy (bare-address-keyed) slow-retry entry to
// its new identity key (ProxySettings.Key()) for every address the given
// desired settings actually claim. Mirrors adoptLegacyProxyState's rules
// exactly.
func (s *proxySlowRetryState) adoptLegacy(desired []*connect.ProxySettings) (adopted, split int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	byAddress := make(map[string][]*connect.ProxySettings, len(desired))
	for _, settings := range desired {
		if settings == nil || settings.Address == "" {
			continue
		}
		byAddress[settings.Address] = append(byAddress[settings.Address], settings)
	}

	for address, settingsAtAddress := range byAddress {
		legacy, hasLegacy := s.Proxies[address]
		if !hasLegacy {
			continue
		}

		seen := map[string]bool{}
		var keys []string
		for _, settings := range settingsAtAddress {
			k := settings.Key()
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		winner := keys[0]

		if winner == address {
			continue
		}

		delete(s.Proxies, address)
		s.Proxies[winner] = legacy
		adopted++

		if len(keys) > 1 {
			split++
			tlog("[proxy][identity] slow-retry state for %s split into %d identities on adoption; %s kept its clock, the rest start fresh\n",
				address, len(keys), proxyKeyDisplay(winner))
		}
	}
	return adopted, split
}

// LoadProxySlowRetryState loads persisted slow-retry state from disk,
// prunes stale entries (>30d), and returns the live state. If the file
// is missing or corrupt, returns a fresh empty state - this is purely
// observability data and must never block provider startup.
func LoadProxySlowRetryState() *proxySlowRetryState {
	s := newProxySlowRetryState()
	path, err := providerStatePath("proxy-slow-retry.json")
	if err != nil {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	if err := json.Unmarshal(data, s); err != nil {
		tlog("[proxy][slow-retry] ERROR: corrupt state file, starting fresh: %v\n", err)
		return newProxySlowRetryState()
	}
	if s.Proxies == nil {
		s.Proxies = make(map[string]*proxySlowRetryEntry)
		return s
	}
	// Defensive cleanup: remove nil entries and entries with zero
	// StartedAt so ShouldDrop cannot treat them as expired. Then
	// prune stale entries (>30 days).
	staleThreshold := time.Now().Add(-proxySlowRetryCleanupAge)
	for addr, entry := range s.Proxies {
		if entry == nil || entry.StartedAt.IsZero() {
			delete(s.Proxies, addr)
			continue
		}
		if entry.DroppedAt != nil && entry.DroppedAt.Before(staleThreshold) {
			delete(s.Proxies, addr)
			continue
		}
		if entry.DroppedAt == nil && entry.StartedAt.Before(staleThreshold) {
			delete(s.Proxies, addr)
		}
	}
	persistProxySlowRetryState(s)
	return s
}

// persistProxySlowRetryState writes the current state to disk using
// atomic write (temp + rename) so a crash mid-write never corrupts the file.
func persistProxySlowRetryState(s *proxySlowRetryState) {
	s.Version = proxySlowRetryStateVersion
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	path, err := providerStatePath("proxy-slow-retry.json")
	if err != nil {
		return
	}
	if err := atomicWriteFile(path, data, 0600); err != nil {
		tlog("[proxy][slow-retry] ERROR: could not persist state: %v\n", err)
	}
}

// atomicWriteFile writes data to a temp file beside path and renames it into
// place, so a crash mid-write never leaves a partial file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// usesSlowRetrySemaphore reports whether a failing paid/file proxy's next
// attempt must take the slow-retry semaphore. A genuine give-up pins
// authFailures one BELOW the ceiling and zeroes slowRetryCycles, so the
// condition must also look at the genuine retry cycle counter or every ramp
// and daily attempt after the first give-up skips the semaphore.
func usesSlowRetrySemaphore(hasProxy, isURLSourced bool, authFailures, maxAuthFailures, slowRetryCycles, genuineRetryCycles int) bool {
	if !hasProxy || isURLSourced {
		return false
	}
	return authFailures >= maxAuthFailures || 0 < slowRetryCycles || 0 < genuineRetryCycles
}
