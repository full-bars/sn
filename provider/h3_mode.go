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
		if n < 0 {
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

// buildH3ResolvedMode parses a value and resolves any cap set.
func buildH3ResolvedMode(value string) (*h3ResolvedMode, error) {
	kind, cap, err := parseH3Mode(value)
	if err != nil {
		return nil, err
	}
	m := &h3ResolvedMode{kind: kind, cap: cap, raw: value}
	if kind == h3ModeCap {
		state, _ := readProxyState()
		m.eligibleKeys = h3TopProxyKeys(cap, readProxySettings(), state)
	}
	return m, nil
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
)

func registerH3Running(key string, eligible bool) {
	if key == "" {
		return
	}
	h3RunningMu.Lock()
	h3RunningEligible[key] = eligible
	h3RunningMu.Unlock()
}

func unregisterH3Running(key string) {
	if key == "" {
		return
	}
	h3RunningMu.Lock()
	delete(h3RunningEligible, key)
	h3RunningMu.Unlock()
}

// h3SetSize is the GRAND TOTAL of running identities that currently run H3:
// the eligible proxies plus the direct identity when it is eligible. Under a
// cap it is the cap plus at most one, never the cap alone.
func h3SetSize() int {
	h3RunningMu.Lock()
	defer h3RunningMu.Unlock()
	n := 0
	for _, eligible := range h3RunningEligible {
		if eligible {
			n++
		}
	}
	return n
}

// h3ProxySetSize is how many of those are PROXIES. The direct identity sits on
// top of the cap, so h3SetSize is this plus the direct identity when it runs.
func h3ProxySetSize() int {
	h3RunningMu.Lock()
	defer h3RunningMu.Unlock()
	n := 0
	for key, eligible := range h3RunningEligible {
		if eligible && key != directProxyKey {
			n++
		}
	}
	return n
}

// h3ReapplyLive is set by provideLauncherLoop once the identities and the proxy
// cancel map are live. It reconnects the identities whose membership changed.
var h3ReapplyLive func()

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
	if h3ReapplyLive != nil {
		h3ReapplyLive()
	}
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
		if tracked && had == want {
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
