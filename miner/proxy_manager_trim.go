package miner

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// This file ports the 3.23-fix v33 provider's operator trim model
// (proxy_trim.go): the persisted `proxy trim <N>` hard cap on running proxies
// and the worst-first shed ranking that startup uses to hold proxies back.
//
// The GRADE resolver is the real v33 unifier (proxyGradeFor,
// proxy_manager_grade_report.go, ported in P1(c)): the paid/file store wins
// over the address-keyed URL cache. The tests that need a live earnings store
// are deferred to P1(c2)/P2 (see the report); the trim ranking passes a zero
// earnings function until the earnings store lands.

// trimCapOperator is the source label for a cap the operator set by hand.
const trimCapOperator = "operator"

// proxyTrimPath returns the operator trim-target file path.
func proxyTrimPath() (string, error) {
	return providerStatePath("proxy_trim")
}

// readTrimTarget returns the operator hard cap on running proxies (0 = no cap).
// A missing, empty, "off", or unparseable file means no cap.
func readTrimTarget() (int, error) {
	path, err := proxyTrimPath()
	if err != nil {
		return 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	s := strings.ToLower(strings.TrimSpace(string(b)))
	if s == "" || s == "off" || s == "0" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		// Treat unparseable as no cap, never a false cap, but say so once: a
		// cap the operator believes is set has silently stopped applying.
		if w := trimGarbageWarning(s); w != "" {
			tlog("%s\n", w)
		}
		return 0, nil
	}
	return n, nil
}

// trimGarbageSeen is every unparseable proxy_trim value already warned about,
// so a bad file is reported once instead of on every reload. A SET, keyed by
// the FULL value.
var trimGarbageSeen struct {
	sync.Mutex
	seen map[string]bool
}

// trimGarbageReset clears the warned set. Tests only.
func trimGarbageReset() {
	trimGarbageSeen.Lock()
	defer trimGarbageSeen.Unlock()
	trimGarbageSeen.seen = nil
}

// trimGarbageWarning returns the warning line for unparseable proxy_trim
// content the first time that exact value is seen, and "" for any repeat.
// Truncation happens only when formatting the message, never in the key.
func trimGarbageWarning(content string) string {
	trimGarbageSeen.Lock()
	if trimGarbageSeen.seen == nil {
		trimGarbageSeen.seen = map[string]bool{}
	}
	if trimGarbageSeen.seen[content] {
		trimGarbageSeen.Unlock()
		return ""
	}
	trimGarbageSeen.seen[content] = true
	trimGarbageSeen.Unlock()

	shown := content
	if len(shown) > 40 {
		shown = shown[:40] + "..."
	}
	return fmt.Sprintf("[proxy][trim] warn: proxy_trim holds %q, which is not a proxy count, so no operator cap applies; set one with `urnet-tools proxy trim <count>`", shown)
}

// writeTrimTarget sets the operator cap. n <= 0 clears it.
func writeTrimTarget(n int) error {
	path, err := proxyTrimPath()
	if err != nil {
		return err
	}
	if n <= 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Write to a unique temp file and rename over the target. A plain WriteFile
	// truncates first, so a reader (or a crash) between truncate and write sees
	// an empty file, which reads as "no cap", and the cap silently lapses.
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.WriteString(strconv.Itoa(n)); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}

// trimPreviewText renders what `proxy trim <count>` would do to the given
// running set. It is pure so it can be tested and so the running provider (the
// only process that knows its running set) can produce it for the CLI.
func trimPreviewText(state *ProxyState, urlState *ProxyURLState, running []string, traffic map[string]uint64, count int) string {
	if len(running) <= count {
		return fmt.Sprintf("preview: running=%d <= %d, nothing to shed\n", len(running), count)
	}
	gradeFor := buildTrimGradeResolver(state, urlState)
	shed := selectWorstRunningProxies(state.Proxies, gradeFor, traffic, running, len(running)-count)
	var b strings.Builder
	fmt.Fprintf(&b, "preview: %d running; would shed %d worst-graded to reach %d:\n", len(running), len(shed), count)
	for _, key := range shed {
		fmt.Fprintf(&b, "  %s\n", proxyKeyDisplay(key))
	}
	return b.String()
}

// startupTrimSelection splits the desired list into the proxies to launch and
// the proxies to hold back so a fresh provider never launches more than the
// operator's trim cap. Nothing is running yet, so the "worst" are chosen from
// persisted health, decayed earnings (a proven earner is protected) and grade,
// with the same ranking a live trim uses. Launch order is preserved; held
// proxies stay desired and are admitted by the reload budget when the cap
// rises. A cap <= 0 or >= len(all) launches everything.
func startupTrimSelection(all []*connect.ProxySettings, cap int, state map[string]ProxyEntry, gradeFor func(string) (float64, bool), earnings func(string) float64) (launch, held []*connect.ProxySettings) {
	if cap <= 0 || len(all) <= cap {
		return all, nil
	}
	keys := make([]string, 0, len(all))
	traffic := make(map[string]uint64, len(all))
	for _, p := range all {
		k := p.Key()
		keys = append(keys, k)
		if e := earnings(k); e > 0 {
			traffic[k] = uint64(e)
		}
	}
	heldSet := make(map[string]bool, len(all)-cap)
	for _, k := range selectWorstRunningProxies(state, gradeFor, traffic, keys, len(all)-cap) {
		heldSet[k] = true
	}
	launch = make([]*connect.ProxySettings, 0, cap)
	held = make([]*connect.ProxySettings, 0, len(all)-cap)
	for _, p := range all {
		if heldSet[p.Key()] {
			held = append(held, p)
		} else {
			launch = append(launch, p)
		}
	}
	return launch, held
}

// trimCapSeen is the last effective trim cap the reload loop acknowledged
// (0 = none). It lets the reload log a receipt exactly once per change.
// trimCapSourceSeen is the source (trimCapOperator/trimCapOOM) that cap came
// from, so a later change away from it (in particular a clear) can attribute
// the ledger entry correctly instead of guessing.
var (
	trimCapSeen       atomic.Int64
	trimCapSourceSeen atomic.Value // string
)

// primeTrimCapSeen seeds trimCapSeen/trimCapSourceSeen with the cap this
// process's startup already read and acted on (startupTrimSelection), without
// logging anything.
func primeTrimCapSeen(trimCap int, source string) {
	trimCapSeen.Store(int64(trimCap))
	trimCapSourceSeen.Store(source)
}

// noteTrimCap records the cap and source the reload just read and reports the
// previous cap and source and whether the cap differs, so a new or cleared cap
// is acknowledged in the log once instead of on every periodic reload.
func noteTrimCap(cur int, source string) (prev int, prevSource string, changed bool) {
	prev = int(trimCapSeen.Swap(int64(cur)))
	if v, ok := trimCapSourceSeen.Load().(string); ok {
		prevSource = v
	}
	trimCapSourceSeen.Store(source)
	return prev, prevSource, prev != cur
}

func resetTrimCapSeen() {
	trimCapSeen.Store(0)
	trimCapSourceSeen.Store("")
}

// effectiveTrimCap returns the operator trim cap (0 = none). The OOM-derived
// automatic cap (v33 effectiveTrimCapSource / oom_cap.go) is P2, so the port
// reads the operator file only.
func effectiveTrimCap() (int, error) {
	return readTrimTarget()
}

// trimmedConfiguredCount is the number of proxies this provider will actually
// run: the desired count, capped by the operator trim target. The status line
// uses it as its denominator, so a healthy box trimmed to N reads N/N, not
// N/desired (which reads "critical" and teaches operators to ignore it).
func trimmedConfiguredCount(desired int) int {
	if cap, err := effectiveTrimCap(); err == nil && cap > 0 && desired > cap {
		return cap
	}
	return desired
}

// healthRank orders the shed priority by last-known health (lower = shed first),
// mirroring the URL pool controller's ranking.
func healthRank(health string) int {
	switch health {
	case "dead":
		return 0
	case "inactive":
		return 1
	case "long_offline":
		return 2
	case "offline":
		return 3
	case "recently_offline":
		return 4
	default: // "up" and unknown shed last
		return 5
	}
}

// trimRank captures the sort key for a running proxy: shed the A-F-worst first.
type trimRank struct {
	addr    string
	health  int
	grade   float64 // -1 = never graded (shed before any graded proxy)
	traffic uint64
}

// trimGradeFor resolves the grade for a proxy identity key through the real
// v33 unifier (proxyGradeFor, proxy_manager_grade_report.go): the paid/file
// store (proxy.state, ProxyEntry) wins over the URL cache (proxy_url.json,
// ProxyURLEntry, looked up by the key's address part); the second return is
// false when neither store has a graded entry. P1(c) replaced the P1(a)
// stand-in that re-read the persisted fields here, so the trim ranking can no
// longer drift from the report/summary resolution.
func trimGradeFor(key string, state *ProxyState, urlState *ProxyURLState) (float64, bool) {
	if info, ok := proxyGradeFor(key, state, urlState); ok {
		return info.Score, true
	}
	return 0, false
}

// buildTrimGradeResolver returns a per-identity-key A-F grade resolver backed by
// the proxyGradeFor unifier: the paid/file store wins, else the URL cache. A nil
// urlState degrades to the paid store only.
func buildTrimGradeResolver(state *ProxyState, urlState *ProxyURLState) func(key string) (float64, bool) {
	return func(key string) (float64, bool) {
		return trimGradeFor(key, state, urlState)
	}
}

// effectiveTrimScore maps the probe grade score to an effective reputation for
// trim eviction. Confirmed F-tier proxies (score < 0.6) are proven broken/failing
// and shed before unverified/probationary (ungraded) proxies. Ungraded proxies
// are assigned an effective score of 0.595 (sitting between F < 0.6 and D >= 0.6),
// so proven failures shed first, while proven healthy D/C/B/A are retained.
func effectiveTrimScore(grade float64) float64 {
	if grade < 0 {
		return 0.595 // ungraded / probationary
	}
	return grade
}

// selectWorstRunningProxies ranks the given running addresses worst-first using
// health, active billable traffic protection, and reachability grade score, and
// returns the worst `n` to shed.
//
// Eviction Hierarchy:
//  1. Unhealthy/Dead First: Dead, inactive, and offline proxies always shed first.
//  2. Earning Protection: Proxies with active billable traffic (traffic > 0) are
//     protected against idle proxies (traffic == 0).
//  3. Idle Proxies (traffic == 0): F-tier (proven failing, score < 0.6) sheds first
//     then Ungraded / Probationary (0.595), then D-tier (0.6), C, B, A.
//  4. Active Earners (traffic > 0): Smaller traffic earners shed before larger
//     earners (preserving high revenue streams), with grade as secondary tiebreak.
func selectWorstRunningProxies(state map[string]ProxyEntry, gradeFor func(addr string) (float64, bool), traffic map[string]uint64, running []string, n int) []string {
	var cands []trimRank
	for _, addr := range running {
		e := state[addr]
		rank := trimRank{addr: addr, health: healthRank(e.Health), grade: -1, traffic: traffic[addr]}
		if gradeFor != nil {
			if score, graded := gradeFor(addr); graded {
				rank.grade = score
			}
		} else if e.Graded {
			rank.grade = e.Score
		}
		cands = append(cands, rank)
	}
	slices.SortFunc(cands, func(a, b trimRank) int {
		if a.health != b.health {
			return a.health - b.health
		}

		hasTrafficA := a.traffic > 0
		hasTrafficB := b.traffic > 0
		if hasTrafficA != hasTrafficB {
			if !hasTrafficA {
				return -1 // A is idle, B is earning -> shed A first
			}
			return 1 // A is earning, B is idle -> shed B first
		}

		if hasTrafficA && hasTrafficB {
			// Both are active earners: preserve larger traffic streams.
			if a.traffic != b.traffic {
				if a.traffic < b.traffic {
					return -1
				}
				return 1
			}
		}

		// Among same traffic level (e.g. both idle, or equal traffic):
		// Higher reputation score wins (F < Ungraded < D < C < B < A).
		effA := effectiveTrimScore(a.grade)
		effB := effectiveTrimScore(b.grade)
		if effA != effB {
			if effA < effB {
				return -1
			}
			return 1
		}

		return strings.Compare(a.addr, b.addr)
	})
	if n > len(cands) {
		n = len(cands)
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = cands[i].addr
	}
	return out
}

// triggerProxyReload pokes the running provider's reload watcher so it applies
// a state change (trim target) immediately.
func triggerProxyReload() {
	if reloadPath, err := proxyReloadPath(); err == nil {
		if err := writeReloadTrigger(reloadPath); err != nil {
			tlog("[proxy][trim] warn: reload trigger write failed: %v\n", err)
		}
	}
}
