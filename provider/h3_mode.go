package provider

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// H3 identity set: which identities run H3 (QUIC) at all.
//
// The platform transport runs H3 when its transport mode preferences include an
// H3 mode, and those preferences are copied at construction. So which
// identities run H3 is decided per identity, and changing the set means
// reconnecting the identities that join or leave it. The control key `h3`
// carries the mode, and this file resolves it to a per-identity decision.
//
// Values:
//   - off       no identity runs H3
//   - direct    the direct (native) identity only
//   - <int>     the N best proxies, by grade then earnings, best first; the
//               number caps the PROXY set only, and the direct identity is
//               ALWAYS additionally eligible, so it never consumes one of the
//               N and `h3 = 10` yields ten H3 proxies plus the direct identity
//   - all       every identity
//
// `on` is kept as an alias for `direct` so a hand-written config that predates
// this key keeps working. The default is `all`, which is what sn does today:
// sn sets no mode preference that removes H3, so every identity has an H3 path.
//
// snH3Eligible is the ONE place the set is decided. The transport wiring in
// provide.go calls it, and a mode change reuses the proxy cancel-and-reload
// path to reconnect the identities whose membership changed.

type h3ModeKind int

const (
	h3ModeOff h3ModeKind = iota
	h3ModeDirect
	h3ModeCap
	h3ModeAll
)

const (
	h3ModeOffName    = "off"
	h3ModeDirectName = "direct"
	h3ModeAllName    = "all"
)

// h3ResolvedMode is a parsed mode plus, for a cap, the resolved proxy identity
// set. It is stored behind an atomic pointer so readers never lock.
type h3ResolvedMode struct {
	kind h3ModeKind
	cap  int
	raw  string
	// eligibleKeys is the resolved set of proxy identity keys allowed to run
	// H3 for a cap mode. Empty for the other kinds.
	eligibleKeys map[string]bool
}

func (m *h3ResolvedMode) name() string {
	if m == nil {
		return h3ModeAllName
	}
	switch m.kind {
	case h3ModeOff:
		return h3ModeOffName
	case h3ModeDirect:
		return h3ModeDirectName
	case h3ModeAll:
		return h3ModeAllName
	case h3ModeCap:
		return strconv.Itoa(m.cap)
	}
	return h3ModeAllName
}

var h3ModeValue atomic.Pointer[h3ResolvedMode]

func init() {
	h3ModeValue.Store(&h3ResolvedMode{kind: h3ModeAll, raw: h3ModeAllName})
}

// parseH3Mode parses a control value into a mode. `on` is an alias for
// `direct`; a bare positive integer is a cap; zero is `off`.
func parseH3Mode(value string) (kind h3ModeKind, cap int, err error) {
	v := strings.ToLower(strings.TrimSpace(value))
	switch v {
	case "":
		// An unset or cleared key means the default, not a parse error.
		return h3ModeAll, 0, nil
	case h3ModeOffName, "0":
		return h3ModeOff, 0, nil
	case h3ModeDirectName, "on":
		return h3ModeDirect, 0, nil
	case h3ModeAllName, "auto":
		return h3ModeAll, 0, nil
	}
	if n, convErr := strconv.Atoi(v); convErr == nil {
		// Only a positive count is a cap. The literal "0" is handled above as
		// the documented alias for off; every other zero spelling (e.g. "00",
		// "+0", "-0") must not slip through as a cap of zero, which would keep
		// the direct identity eligible and silently mean `direct`.
		if n <= 0 {
			return 0, 0, fmt.Errorf("h3: %q must be off, direct, a positive proxy count, or all", value)
		}
		return h3ModeCap, n, nil
	}
	return 0, 0, fmt.Errorf("h3: must be off, direct, a positive proxy count, or all (got %q)", value)
}

// h3ResolvedModeLabel is a stable label for health and metrics.
func h3ResolvedModeLabel() string {
	return currentH3Mode().name()
}

func currentH3Mode() *h3ResolvedMode {
	if m := h3ModeValue.Load(); m != nil {
		return m
	}
	return &h3ResolvedMode{kind: h3ModeAll, raw: h3ModeAllName}
}

// h3TopProxyKeys returns the N best proxy identity keys. The ranking is by
// graded-then-score, then by decayed earnings, then by key for stability. The
// set is recomputed when the mode is set and on each re-apply, so it tracks the
// grades the box has collected.
//
// The direct identity is not part of this set and never consumes a slot: a cap
// of N yields N proxies here, and h3EligibleForKey admits the direct identity
// on top of them.
func h3TopProxyKeys(n int, settings []*connect.ProxySettings, state *ProxyState) map[string]bool {
	out := map[string]bool{}
	if n <= 0 {
		return out
	}
	type cand struct {
		key     string
		graded  bool
		score   float64
		earning float64
	}
	cands := make([]cand, 0, len(settings))
	now := time.Now()
	for _, s := range settings {
		key := s.Key()
		c := cand{key: key, earning: proxyEarningsScore(key, now)}
		if state != nil {
			if e, ok := state.Proxies[key]; ok {
				c.score = e.Score
				c.graded = e.Graded
			}
		}
		cands = append(cands, c)
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].graded != cands[j].graded {
			return cands[i].graded
		}
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		if cands[i].earning != cands[j].earning {
			return cands[i].earning > cands[j].earning
		}
		return cands[i].key < cands[j].key
	})
	for i := 0; i < len(cands) && i < n; i++ {
		out[cands[i].key] = true
	}
	return out
}

// h3ProxyCandidates is the launcher's full desired proxy set once it is known:
// the internal or file source, plus URL-sourced proxies. readProxySettings()
// reads only the internal config, so a cap resolved from it alone would rank
// zero file- or URL-sourced proxies. The launcher publishes the same set it
// launches, and a cap ranks over every proxy the box runs. The CLI and test
// paths fall back to readProxySettings() until something publishes.
var (
	h3ProxyCandidatesMu    sync.RWMutex
	h3ProxyCandidatesSet   []*connect.ProxySettings
	h3ProxyCandidatesKnown bool
)

// publishH3ProxyCandidates records the launcher's current desired proxy set so
// a cap resolves over the proxies the box actually runs. The launcher calls
// this at startup and on every reload.
func publishH3ProxyCandidates(settings []*connect.ProxySettings) {
	h3ProxyCandidatesMu.Lock()
	h3ProxyCandidatesSet = settings
	h3ProxyCandidatesKnown = true
	h3ProxyCandidatesMu.Unlock()
}

func h3ProxyCandidateSettings() []*connect.ProxySettings {
	h3ProxyCandidatesMu.RLock()
	defer h3ProxyCandidatesMu.RUnlock()
	if !h3ProxyCandidatesKnown {
		// Unknown: the launcher has not published yet (the CLI and unit
		// tests), so the caller falls back to readProxySettings().
		return nil
	}
	// Known, so return the published set even when it is EMPTY. A nil return
	// means "no launcher has published", and the empty-but-known case must not
	// be confused with it: the caller's nil fallback would otherwise resolve a
	// cap over the internal config instead of the (empty) set the box actually
	// runs, capping over phantom keys that match no running identity. make([]T,
	// 0) is non-nil, so the clone below is a non-nil empty slice for an empty
	// (or nil) published set.
	//
	// Clone the slice: the caller must not receive the internal slice across
	// the mutex boundary, where a later publish (a slice-header swap) or any
	// future element write would race the caller's reads.
	clone := make([]*connect.ProxySettings, len(h3ProxyCandidatesSet))
	copy(clone, h3ProxyCandidatesSet)
	return clone
}

// buildH3ResolvedMode parses a value and resolves any cap set.
func buildH3ResolvedMode(value string) (*h3ResolvedMode, error) {
	kind, cap, err := parseH3Mode(value)
	if err != nil {
		return nil, err
	}
	m := &h3ResolvedMode{kind: kind, cap: cap, raw: value}
	if kind == h3ModeCap {
		settings := h3ProxyCandidateSettings()
		if settings == nil {
			// No launcher has published a set yet (the CLI and unit tests): the
			// internal config is the best available.
			settings = readProxySettings()
		}
		state, _ := readProxyState()
		m.eligibleKeys = h3TopProxyKeys(cap, settings, state)
	}
	return m, nil
}

// reResolveActiveH3Cap re-resolves an active cap over the current published
// candidate set and applies the result to the running identities. It is the
// shared startup/reload path (provide.go and ProxyReloader.reload both call it
// after publishing the candidate set).
//
// The resolved set is a function of the candidates, the persisted proxy state,
// and the cap, so if none of those moved the mode is left untouched.
//
// The swap is a compare-and-swap against the mode that was just read:
// buildH3ResolvedMode reads proxy state from disk, so a control-socket update
// that lands during that window replaces the stored pointer and the CAS fails,
// leaving the operator's newer mode in place instead of clobbering it with this
// stale re-resolve.
//
// When the cap set did change, the installed live re-apply hook (see
// runH3ReapplyLive) reconnects the running identities whose membership changed
// — the same path SetH3Mode uses. Without it, a proxy promoted into (or
// displaced from) the top N keeps its old H3 state until the next control
// update, so a cap of N can end up with N+1 proxies running H3. The hook is
// unset before the launcher installs it (early startup, CLI, tests), when there
// is nothing running to reconnect.
func reResolveActiveH3Cap() {
	m := currentH3Mode()
	if m.kind != h3ModeCap {
		return
	}
	resolved, err := buildH3ResolvedMode(m.raw)
	if err != nil {
		return
	}
	if sameH3EligibleKeys(m.eligibleKeys, resolved.eligibleKeys) {
		return
	}
	if !h3ModeValue.CompareAndSwap(m, resolved) {
		// A concurrent control update replaced the mode while the cap was being
		// re-resolved; leave the operator's newer mode in place.
		return
	}
	runH3ReapplyLive()
}

// sameH3EligibleKeys reports whether two resolved cap sets select the same
// identities. A nil map and an empty map are the same (no eligible proxies).
func sameH3EligibleKeys(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for key, v := range a {
		if b[key] != v {
			return false
		}
	}
	return true
}

// h3EligibleForKey is the core decision for one identity key.
func h3EligibleForKey(key string, isNative bool) bool {
	m := currentH3Mode()
	switch m.kind {
	case h3ModeOff:
		return false
	case h3ModeDirect:
		return isNative
	case h3ModeAll:
		return true
	case h3ModeCap:
		// A cap limits only the PROXY set. The direct identity is the baseline
		// path and costs exactly one carrier, so it always keeps H3.
		if isNative {
			return true
		}
		return m.eligibleKeys[key]
	}
	return true
}

// snH3Eligible reports whether an identity may run H3, and therefore whether it
// may offer QUIC DATAGRAM on that H3 connection. This predicate is the ONE place
// the H3 rollout is bounded: the transport wiring in provide.go calls only this.
//
// The arguments are the hook for the policy: proxyIndex identifies the proxy
// identity, proxySettings is nil for a direct identity, and isNative marks the
// host's own identity. Today the decision comes from the `h3` control key (see
// h3EligibleForKey): all, direct, a best-N cap, or off.
func snH3Eligible(proxyIndex int, proxySettings *connect.ProxySettings, isNative bool) bool {
	key := ""
	if proxySettings != nil {
		key = proxySettings.Key()
	}
	return h3EligibleForKey(key, isNative)
}

// h3RunningEligible tracks, per running identity key, whether that identity
// currently runs H3. It is the set size for health and metrics, and the diff
// the live re-apply uses to find identities that join or leave the set.
var (
	h3RunningMu       sync.Mutex
	h3RunningEligible = map[string]bool{}
	// h3RunningLaunch is the launch id that currently owns each key's
	// h3RunningEligible entry. registerH3Running stamps a fresh id and returns
	// it; a goroutine releases the entry only while its id is still the owner.
	// Without that check a cancelled launch would delete the entry its
	// replacement had just registered, because a live mode change reconnects an
	// identity by cancelling the old launch and respawning a new one that
	// overlaps in time. proxyLaunches records the same generation for the
	// cancel map.
	h3RunningLaunch = map[string]uint64{}
	h3RunningNext   uint64
)

// registerH3Running records whether a running identity currently runs H3 and
// returns the launch id that owns the entry. Callers that will later release
// the entry hold the id so they release only their own launch (see
// unregisterH3RunningIfCurrent).
func registerH3Running(key string, eligible bool) uint64 {
	if key == "" {
		return 0
	}
	h3RunningMu.Lock()
	h3RunningNext++
	launch := h3RunningNext
	h3RunningEligible[key] = eligible
	h3RunningLaunch[key] = launch
	h3RunningMu.Unlock()
	return launch
}

// registerH3RunningEligible reads the identity's current H3 eligibility and
// records it as a running entry in ONE critical section, returning the running
// key, the eligibility actually registered, and the owning launch id. It is the
// launcher's registration path; callers later release through
// unregisterH3RunningIfCurrent.
//
// The read and the registration MUST be atomic. reapplyH3ModeLive (a concurrent
// `h3` control update, or a reload re-resolve) skips an untracked key on the
// assumption that the key reads the mode when it builds its transport. With the
// read outside h3RunningMu the two can interleave so the re-apply observes the
// key untracked and skips it, and only then does the launch register the stale
// value and apply stale ModePreferences — leaving the identity on the wrong
// side of the cap until the next control update. Holding the lock across both
// makes the two orderings safe: the re-apply either observes this registration
// (and reconnects the identity if the mode moved) or runs before it, in which
// case the read here sees the newer mode.
func registerH3RunningEligible(proxySettings *connect.ProxySettings, isNative bool) (key string, eligible bool, launch uint64) {
	key = directProxyKey
	if proxySettings != nil {
		key = proxySettings.Key()
	}
	h3RunningMu.Lock()
	defer h3RunningMu.Unlock()
	eligible = snH3Eligible(0, proxySettings, isNative)
	if key == "" {
		return key, eligible, 0
	}
	h3RunningNext++
	launch = h3RunningNext
	h3RunningEligible[key] = eligible
	h3RunningLaunch[key] = launch
	return key, eligible, launch
}

// unregisterH3Running drops the identity's entry unconditionally. Callers that
// are not a tracked launch (tests, one-off probes) use this.
func unregisterH3Running(key string) {
	if key == "" {
		return
	}
	h3RunningMu.Lock()
	delete(h3RunningEligible, key)
	delete(h3RunningLaunch, key)
	h3RunningMu.Unlock()
}

// unregisterH3RunningIfCurrent drops the identity's entry only while launch is
// still the identity's current launch. The cancelled launch on the way out
// after a live mode change or a credential rotation must not erase the entry
// the replacement launch registered, which would drop a live identity from
// health and metrics.
func unregisterH3RunningIfCurrent(key string, launch uint64) {
	if key == "" || launch == 0 {
		return
	}
	h3RunningMu.Lock()
	if h3RunningLaunch[key] == launch {
		delete(h3RunningEligible, key)
		delete(h3RunningLaunch, key)
	}
	h3RunningMu.Unlock()
}

// h3SetSizes returns the running H3 counts under one lock: the GRAND TOTAL
// (eligible proxies plus the direct identity when it runs) and the PROXY-only
// count. Reading the two via separate calls can interleave with a
// register/unregister and report an impossible pair, for example a total
// smaller than the proxy count.
func h3SetSizes() (setSize int, proxySetSize int) {
	h3RunningMu.Lock()
	defer h3RunningMu.Unlock()
	for key, eligible := range h3RunningEligible {
		if !eligible {
			continue
		}
		setSize++
		if key != directProxyKey {
			proxySetSize++
		}
	}
	return
}

// h3SetSize is the GRAND TOTAL of running identities that currently run H3:
// the eligible proxies plus the direct identity when it is eligible. Under a
// cap it is the cap plus at most one, never the cap alone.
func h3SetSize() int {
	setSize, _ := h3SetSizes()
	return setSize
}

// h3ProxySetSize is how many of those are PROXIES. The direct identity sits on
// top of the cap, so h3SetSize is this plus the direct identity when it runs.
func h3ProxySetSize() int {
	_, proxySetSize := h3SetSizes()
	return proxySetSize
}

// h3ReapplyLive is set by provideLauncherLoop once the identities and the proxy
// cancel map are live. It reconnects the identities whose membership changed.
// Reads come from the control socket (SetH3Mode) and the reload goroutine
// (reResolveActiveH3Cap) while the launcher installs it at startup, so both the
// store and the load are guarded by h3ReapplyLiveMu: an unsynchronized store
// races those readers under the Go memory model, and the reload read is the one
// this feature adds.
var (
	h3ReapplyLiveMu sync.Mutex
	h3ReapplyLive   func()
)

// installH3ReapplyLive sets the live re-apply hook. The launcher installs it
// before it starts the reload watcher or runs the first reload, so a cap
// re-resolve triggered from either observes the hook rather than silently
// skipping the reconnect.
func installH3ReapplyLive(f func()) {
	h3ReapplyLiveMu.Lock()
	h3ReapplyLive = f
	h3ReapplyLiveMu.Unlock()
}

// runH3ReapplyLive invokes the installed hook, if any. The hook runs outside
// the lock: it reconnects proxies and triggers a reload, which must not run
// while holding h3ReapplyLiveMu.
func runH3ReapplyLive() {
	h3ReapplyLiveMu.Lock()
	f := h3ReapplyLive
	h3ReapplyLiveMu.Unlock()
	if f != nil {
		f()
	}
}

// SetH3Mode applies a control value at runtime and returns the previous mode
// name. The identities that join or leave the set are closed and reconnected
// through the same cancel-and-reload path a credential rotation uses.
func SetH3Mode(value string) (previous string, err error) {
	resolved, err := buildH3ResolvedMode(value)
	if err != nil {
		return "", err
	}
	previous = currentH3Mode().name()
	h3ModeValue.Store(resolved)
	runH3ReapplyLive()
	return previous, nil
}

// H3ModeName returns the resolved mode name for health and metrics.
func H3ModeName() string {
	return h3ResolvedModeLabel()
}

// H3SetSize returns the grand total size of the running H3 set: the eligible
// proxies plus the direct identity when it is eligible. Under a cap the direct
// identity is counted on top of the N proxies.
func H3SetSize() int {
	return h3SetSize()
}

// H3ProxySetSize returns how many of the running H3 identities are proxies. It
// excludes the direct identity, so H3SetSize is this plus the direct identity
// when it runs.
func H3ProxySetSize() int {
	return h3ProxySetSize()
}

// H3SetSizes returns both running H3 counts from one consistent snapshot, so a
// reader that reports them together (health line, /metrics) can never show a
// total smaller than the proxy count when an identity registers or unregisters
// between two separate reads.
func H3SetSizes() (setSize int, proxySetSize int) {
	return h3SetSizes()
}

// applyH3ModeToSettings writes the mode onto one identity's platform settings:
// an eligible identity keeps the default mode set (H1 plus H3), and one that is
// not eligible is pinned to H1 only so the engine never dials a QUIC mode.
func applyH3ModeToSettings(settings *connect.PlatformTransportSettings, eligible bool) {
	if settings == nil {
		return
	}
	if eligible {
		settings.ModePreferences = connect.DefaultTransportModePreferences()
		return
	}
	settings.ModePreferences = map[connect.TransportMode]int{connect.TransportModeH1: 1}
}

// reapplyH3ModeLive reconnects the running identities whose H3 membership
// changed under the new mode. Identities that stay eligible (or stay excluded)
// are left alone; the ones that changed are cancelled and removed from the
// cancel map, then a proxy reload respawns them, and provideWithProxy applies
// the new mode when it builds their platform transport.
func reapplyH3ModeLive(st *provideState) {
	if st == nil {
		return
	}
	st.proxyCancelMu.Lock()
	var changed []string
	for key, cancel := range st.proxyCancelMap {
		isNative := key == directProxyKey
		want := h3EligibleForKey(key, isNative)
		had, tracked := h3RunningEligibleOf(key)
		// A key that has launched but not yet registered its transport
		// reads the new mode when it builds one, so it is left alone. Only
		// a tracked identity whose eligibility actually changed reconnects.
		if !tracked || had == want {
			continue
		}
		changed = append(changed, key)
		registerH3Running(key, want)
		cancel()
		delete(st.proxyCancelMap, key)
	}
	st.proxyCancelMu.Unlock()
	if len(changed) == 0 {
		return
	}
	tlog("[h3] mode %s: %d identities join or leave the H3 set; reconnecting\n", H3ModeName(), len(changed))
	triggerProxyReload()
}

func h3RunningEligibleOf(key string) (eligible bool, tracked bool) {
	h3RunningMu.Lock()
	defer h3RunningMu.Unlock()
	eligible, tracked = h3RunningEligible[key]
	return
}
