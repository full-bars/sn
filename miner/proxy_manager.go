package miner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/connect"
)

// proxy_manager.go is the P1(a) manager that owns the desired proxy set and
// produces the []*connect.ProxySettings the miner's provide loop launches.
//
// Design (see the one-program design notes, section 2):
// the manager keeps the v33 two-store model - proxy.state (identity-keyed
// desired/health bookkeeping) and proxy_url.json (URL-source cache) - while the
// miner's legacy <state>/proxy internal config stays the operator's store for
// curated proxies. On first start it imports the legacy entries into
// proxy.state and mirrors the legacy file back so every existing reader keeps
// working. The provide loop reads the manager's desired set once at startup;
// the fetcher/reaper/cleanup loops keep the state current and write the reload
// trigger for the P2 hot-reload watcher, so a newly fetched proxy joins the
// pool on the next start (P2 makes that live).

// proxyWarmupDone is set when the operator-curated proxies have finished their
// initial ramp. v33 gates the first URL fetch on it so URL-sourced proxies never
// compete for auth-rate-limiter slots with file proxies during startup. The
// miner has no warmup signal yet, so P1(a) sets it as the manager starts and P2
// replaces that with the real signal.
var proxyWarmupDone atomic.Bool

// proxyManagerConfig is the runtime configuration of one manager instance.
type proxyManagerConfig struct {
	Urls                []string
	Refresh             time.Duration
	Max                 int
	DeadCleanupScope    string
	DeadCleanupInterval time.Duration
	SelfHeal            bool
	ApiHost             string
	ApiPort             uint16
	// ProxyFile is the operator-curated proxy source file (v33 Workflow A:
	// --proxy_file / --file / PROXY_FILE). Empty means the legacy internal
	// <state>/proxy config is the curated source (Workflow B). The resolved
	// path is written to proxy.state.Source, the only switch every reader
	// (summary, refresh, remove-dead, the reload engine) consults.
	ProxyFile string
}

// defaultProxyManagerConfig mirrors v33's defaults: refresh 1h, cap 500,
// cleanup scope url every 6h, self-heal off, probe target api.bringyour.com:443.
func defaultProxyManagerConfig() proxyManagerConfig {
	return proxyManagerConfig{
		Refresh:             1 * time.Hour,
		Max:                 500,
		DeadCleanupScope:    "url",
		DeadCleanupInterval: 6 * time.Hour,
		SelfHeal:            os.Getenv("URNETWORK_SELF_HEAL") == "1",
		ApiHost:             defaultAPIHost,
		ApiPort:             defaultAPIPort,
	}
}

// proxyManagerEnvOrFlag resolves a string option: the command line wins, then
// the environment variable, then the default.
func proxyManagerEnvOrFlag(opts docopt.Opts, flag, env, def string) string {
	if v, err := opts.String(flag); err == nil && strings.TrimSpace(v) != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v
	}
	return def
}

// proxyManagerEnvOrFlagInt is proxyManagerEnvOrFlag for integers, and is the
// v33 resolveInt counterpart (provider/main.go): an unparseable value from the
// chosen source is named and the default used, instead of falling back
// silently. A negative value still falls back quietly: v33's resolveInt
// accepts it, so warning about it would invent a message v33 never writes.
func proxyManagerEnvOrFlagInt(opts docopt.Opts, flag, env string, def int) int {
	if v, err := opts.String(flag); err == nil && strings.TrimSpace(v) != "" {
		raw := strings.TrimSpace(v)
		n, err := strconv.Atoi(raw)
		if err == nil && n >= 0 {
			return n
		}
		if err != nil {
			tlog("[proxy][url] warning: invalid integer %q for %s; using default %d\n", raw, flag, def)
		}
		return def
	}
	if raw := strings.TrimSpace(os.Getenv(env)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err == nil && n >= 0 {
			return n
		}
		if err != nil {
			tlog("[proxy][url] warning: invalid integer %q for %s; using default %d\n", raw, env, def)
		}
	}
	return def
}

// proxyManagerEnvOrFlagDuration is proxyManagerEnvOrFlag for durations, and is
// the v33 resolveDuration counterpart (provider/main.go): an unparseable value
// from the chosen source is named and the default used, instead of falling
// back silently. A non-positive value still falls back quietly: v33's
// resolveDuration accepts it, so warning about it would invent a message v33
// never writes.
func proxyManagerEnvOrFlagDuration(opts docopt.Opts, flag, env string, def time.Duration) time.Duration {
	if v, err := opts.String(flag); err == nil && strings.TrimSpace(v) != "" {
		raw := strings.TrimSpace(v)
		d, err := time.ParseDuration(raw)
		if err == nil && d > 0 {
			return d
		}
		if err != nil {
			tlog("[proxy][url] warning: invalid duration %q for %s; using default %s\n", raw, flag, def)
		}
		return def
	}
	if raw := strings.TrimSpace(os.Getenv(env)); raw != "" {
		d, err := time.ParseDuration(raw)
		if err == nil && d > 0 {
			return d
		}
		if err != nil {
			tlog("[proxy][url] warning: invalid duration %q for %s; using default %s\n", raw, env, def)
		}
	}
	return def
}

// resolveProxyURLs collects every URL source: the three flags (each
// repeatable), the PROXY_URL env var, and the sources already persisted in
// proxy_url.json. Duplicates are collapsed in order.
func resolveProxyURLs(opts docopt.Opts) []string {
	var urls []string

	for _, flag := range []string{"--proxy_url", "--url", "--URL"} {
		if v, ok := opts[flag]; ok && v != nil {
			switch vv := v.(type) {
			case []string:
				urls = append(urls, vv...)
			case string:
				if vv != "" {
					urls = append(urls, vv)
				}
			}
		}
	}

	if envURLs := os.Getenv("PROXY_URL"); envURLs != "" {
		for _, u := range strings.Split(envURLs, ",") {
			if u = strings.TrimSpace(u); u != "" {
				urls = append(urls, u)
			}
		}
	}

	if urlState, err := readProxyURLState(); err != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", err)
	} else {
		urls = append(urls, urlState.Sources...)
	}

	seen := map[string]bool{}
	out := urls[:0]
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// apiProbeHostPort extracts the host and port the reachability probe should
// target from the resolved API URL. Defaults to api.bringyour.com:443.
func apiProbeHostPort(apiUrl string) (string, uint16) {
	u, err := url.Parse(apiUrl)
	if err != nil || u.Hostname() == "" {
		return defaultAPIHost, defaultAPIPort
	}
	port := uint16(defaultAPIPort)
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil && 0 < n && n <= 65535 {
			port = uint16(n)
		}
	}
	return u.Hostname(), port
}

// resolveProxyFile selects the file-backed curated source exactly as v33
// (provider/main.go 4090-4098): --proxy_file wins, then --file, then the
// PROXY_FILE environment variable; the resolved path has ~ expanded. Empty
// means Workflow B (the internal <state>/proxy config).
func resolveProxyFile(opts docopt.Opts) string {
	proxyFile := proxyManagerEnvOrFlag(opts, "--proxy_file", "", "")
	if proxyFile == "" {
		proxyFile = proxyManagerEnvOrFlag(opts, "--file", "", "")
	}
	if proxyFile == "" {
		proxyFile = strings.TrimSpace(os.Getenv("PROXY_FILE"))
	}
	if proxyFile == "" {
		return ""
	}
	return expandPath(proxyFile)
}

// proxyManagerConfigFromOpts resolves the manager config from the provide
// command's options and the environment.
func proxyManagerConfigFromOpts(opts docopt.Opts, apiUrl string) proxyManagerConfig {
	cfg := defaultProxyManagerConfig()
	cfg.Urls = resolveProxyURLs(opts)
	cfg.ProxyFile = resolveProxyFile(opts)
	cfg.Refresh = proxyManagerEnvOrFlagDuration(opts, "--proxy_url_refresh", "PROXY_URL_REFRESH", cfg.Refresh)
	cfg.Max = proxyManagerEnvOrFlagInt(opts, "--proxy_url_max", "PROXY_URL_MAX", cfg.Max)
	cfg.DeadCleanupScope = proxyManagerEnvOrFlag(opts, "--proxy_dead_cleanup_scope", "PROXY_DEAD_CLEANUP_SCOPE", cfg.DeadCleanupScope)
	cfg.DeadCleanupInterval = proxyManagerEnvOrFlagDuration(opts, "--proxy_dead_cleanup_interval", "PROXY_DEAD_CLEANUP_INTERVAL", cfg.DeadCleanupInterval)
	cfg.ApiHost, cfg.ApiPort = apiProbeHostPort(apiUrl)
	return cfg
}

// proxyManager owns the desired set, the persisted state and the background
// loops. One instance per provide run.
type proxyManager struct {
	cfg proxyManagerConfig

	mu      sync.Mutex
	state   *ProxyState
	started bool
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// sourceOf records each desired identity's provenance ("file", "internal",
	// "url"), for the reload attribution line.
	sourceOf map[string]string

	// gate is the startup grading gate (proxy_manager_gate.go) the active
	// probe loops wait on. It is armed by start and never closed.
	gate *gradingGate
}

func newProxyManager(cfg proxyManagerConfig) *proxyManager {
	return &proxyManager{cfg: cfg, sourceOf: map[string]string{}}
}

// proxySourceOf returns a copy of the provenance map recorded when the
// desired set was last built (see desired): each identity's source tag
// ("file", "internal", "url"). The provide run hands it to the warmth launch
// ordering, which must not re-derive the tags itself: a URL-sourced identity
// mislabeled as curated would launch ahead of the operator's list. A key
// absent from the map (e.g. a run that never started the manager) launches
// as trusted provenance.
func (m *proxyManager) proxySourceOf() map[string]string {
	out := make(map[string]string, len(m.sourceOf))
	for key, source := range m.sourceOf {
		out[key] = source
	}
	return out
}

// legacyProxySettings reads the miner's internal `<state>/proxy` config. This
// is the fallback the manager uses when proxy.state has no external source
// file configured.
func legacyProxySettings() []*connect.ProxySettings {
	return readProxySettingsLegacy()
}

// primarySource returns the source label and settings of the operator-curated
// half of the desired set: the external file named by proxy.state.Source when
// one is configured (v33 workflow A), else the internal config (workflow B).
func (m *proxyManager) primarySource(state *ProxyState) (tag string, settings []*connect.ProxySettings, err error) {
	if state.Source != "" {
		settings, err = readProxySettingsFromFile(state.Source)
		if err != nil {
			return "", nil, err
		}
		return "file", settings, nil
	}
	return "internal", legacyProxySettings(), nil
}

// desired builds the desired set: primary source first (operator-curated
// entries win at an address), then the URL cache, then the operator trim cap.
// It is the read the provide loop and readProxySettings() consume. When
// importLegacy is true (manager start) it also brings proxy.state up to date
// (IDs, source tags, legacy-key adoption, StartedAt) and reports whether the
// state needs writing.
func (m *proxyManager) desired(importLegacy bool) (launch []*connect.ProxySettings, held []*connect.ProxySettings, writeState bool, err error) {
	state, err := readProxyState()
	if err != nil {
		return nil, nil, false, err
	}
	if state.Proxies == nil {
		state.Proxies = map[string]ProxyEntry{}
	}
	// v33 main.go 4090-4121: the selected source file is recorded in
	// proxy.state.Source, and Workflow B (no file configured) clears it. This
	// is the writer every other reader of the switch needs - primarySource
	// below, proxy summary/refresh/remove-dead and the reload engine. It runs
	// on the start/import path only: a command reader must honor whatever the
	// running provider selected, not re-decide the source from its own flags.
	if importLegacy && state.Source != m.cfg.ProxyFile {
		state.Source = m.cfg.ProxyFile
		writeState = true
	}
	// Live health (v33 connect health parity): the persisted health seeds the
	// registry on the first read after a restart, and any health this process
	// has observed since is overlaid onto the entries the trim ranking reads.
	seedLiveProxyHealthFromState(state)
	if overlaid := overlayLiveProxyHealth(state.Proxies); overlaid > 0 {
		tlog("[proxy][health] overlaid live health onto %d prox%s before the desired-set build\n", overlaid, plural(overlaid, "y", "ies"))
	}

	tag, primary, err := m.primarySource(state)
	if err != nil {
		return nil, nil, false, err
	}
	// v33 refuses a file-backed start whose source file read as empty or
	// yielded no valid entries (shmLogFatal 20/21): silently falling back to
	// the URL cache would provide with a curated pool of zero. Only the start
	// path refuses; command readers surface the empty list as a normal diff.
	if importLegacy && m.cfg.ProxyFile != "" && len(primary) == 0 {
		return nil, nil, false, fmt.Errorf("proxy file %s contained no valid proxies (expected one host:port:user:pass per line)", m.cfg.ProxyFile)
	}

	desiredSet := make(map[string]*connect.ProxySettings, len(primary))
	sourceOf := make(map[string]string, len(primary))
	for _, s := range primary {
		desiredSet[s.Key()] = s
		sourceOf[s.Key()] = tag
	}

	urlState, urlErr := readProxyURLState()
	if urlErr != nil {
		tlog("[proxy][url] warning: could not read proxy_url.json: %v\n", urlErr)
	} else {
		mergeProxyURLCache(desiredSet, sourceOf, urlState)
	}

	// Order the desired set deterministically (identity key) so launch order
	// and trace output are stable across restarts.
	all := make([]*connect.ProxySettings, 0, len(desiredSet))
	for _, s := range desiredSet {
		all = append(all, s)
	}
	sortProxySettingsByKey(all)

	// Record the trim decision the snapshot reports, from the full desired set:
	// every configured proxy counts, including one a launch-time hold keeps
	// back this cycle. Only the start path records it (importLegacy): that is
	// the running provider's own decision, and a command reader must not
	// fabricate one.
	//
	// Only a COMPLETE desired set counts. A failed proxy_url.json read leaves
	// every URL-sourced proxy out of the set, so recording that length would
	// understate the pool and clamp the cap below the count the operator
	// configured; the last complete decision stays in place instead. On a first
	// start there is none, and no report is better than a wrong one: the reader
	// then falls back to the operator file.
	if importLegacy && urlErr == nil {
		noteTrimSelection(len(all))
	}

	// Fork hook: v33's launch-time give-up backoff gate. A URL-sourced identity
	// still inside its backoff window is held back instead of relaunched
	// (proxy_manager_auth.go).
	if heldCount := dropBackoffHeldURLProxies(desiredSet, sourceOf, time.Now()); heldCount > 0 {
		all = all[:0]
		for _, s := range desiredSet {
			all = append(all, s)
		}
		sortProxySettingsByKey(all)
		tlog("[proxy][auth] %d URL-sourced proxies are held back inside their give-up backoff window\n", heldCount)
	}

	if importLegacy {
		// Highest ID first so new IDs never collide with persisted ones.
		highestID := -1
		for _, e := range state.Proxies {
			if e.ID > highestID {
				highestID = e.ID
			}
		}
		initProxyIDCounter(highestID)

		startedAt := time.Now()
		if state.StartedAt.IsZero() {
			state.StartedAt = startedAt
		}
		before := len(state.Proxies)
		for key, src := range sourceOf {
			resolveProxyID(state, key)
			tagProxySourceIfUnset(state, key, src)
		}
		adopted, split := adoptLegacyProxyState(state, all)
		if adopted > 0 {
			tlog("[proxy][identity] adopted %d legacy proxy.state entries (%d shared-address splits)\n", adopted, split)
		}
		// Earnings history (v33 main startup): load the persisted store and
		// migrate any legacy bare-address entries onto the identity keys the
		// desired set claims, so the launch ranking, the scout and the audit
		// see real earnings across a restart.
		if loadErr := globalProxyEarningsStore.Load(); loadErr != nil {
			tlog("[proxy][earn] warn: could not load proxy earnings history: %v\n", loadErr)
		}
		if adoptedEarn, splitEarn := globalProxyEarningsStore.adoptLegacy(all); adoptedEarn > 0 {
			tlog("[proxy][identity] adopted %d legacy earnings entries (%d shared-address splits)\n", adoptedEarn, splitEarn)
		}
		state.NextID = currentProxyIDCounter()
		if len(state.Proxies) != before || adopted > 0 {
			writeState = true
		}
		if m.state == nil || m.state.StartedAt.IsZero() {
			writeState = true // first manager start records StartedAt
		}
		m.state = state
	}

	trimCap, capErr := effectiveTrimCap()
	if capErr != nil {
		tlog("[proxy][trim] warn: could not read the trim target: %v\n", capErr)
		trimCap = 0
	}
	if trimCap > 0 && len(all) > trimCap {
		urlStateForGrades, _ := readProxyURLState()
		gradeFor := buildTrimGradeResolver(state, urlStateForGrades)
		// Earnings protect a proven earner from being held at startup: the
		// decayed store score puts a proxy that earned before this process
		// started ahead of an idle one with the same health and grade (v33
		// startupTrimSelection).
		earningsFor := func(key string) float64 { return proxyEarningsScore(key, time.Now()) }
		launch, held = startupTrimSelection(all, trimCap, state.Proxies, gradeFor, earningsFor)
		importantLogf("[proxy][trim] startup: cap=%d, launching %d of %d desired, holding %d worst-graded until the cap is raised\n",
			trimCap, len(launch), len(all), len(held))
		primeTrimCapSeen(trimCap, trimCapOperator)
	} else {
		launch = all
		if trimCap > 0 {
			primeTrimCapSeen(trimCap, trimCapOperator)
		}
	}
	// The h3 count setting ranks over the proxies this box runs, so the
	// candidates are noted under every setting: switching to a count later
	// ranks over the real set at once instead of an empty one. A ranking that
	// moved relaunches the identities that joined or left it (see
	// h3_proxy_set.go); the reload path itself starts nothing.
	setting := resolveH3Mode(globalControlState, currentProxyResolution())
	if noteH3Candidates(launch, setting) {
		relaunchH3ProxiesNow(setting)
	}

	// Settle the resolution state (v33 proxy_reload.go): the direct-only H3
	// default, the status line and the snapshot all read it. Only a node with
	// no proxy source configured AND direct providing enabled settles as
	// zero-valid — the state that auto-enables H3 for an operator who never
	// touched the key. A source that was configured but yielded nothing is
	// not a completed zero-proxy setup, and neither is a node that serves
	// nothing at all.
	anySourceConfigured := state.Source != "" || (urlErr == nil && urlState != nil && len(urlState.Sources) > 0)
	switch {
	case len(launch) > 0 || len(held) > 0:
		setProxyResolutionStatus(proxyResolutionOK, "")
	case anySourceConfigured:
		setProxyResolutionStatus(proxyResolutionEmpty, "source returned no usable proxies")
	case urlErr != nil:
		setProxyResolutionStatus(proxyResolutionFailed, "could not read proxy_url.json")
	case !isDirectEnabled():
		setProxyResolutionStatus(proxyResolutionNoSource, "direct transport disabled and no proxy source configured")
	default:
		setProxyResolutionStatus(proxyResolutionZeroValid, "direct-only providing; no proxy source configured")
	}
	// The launch provenance is what the warmth launch ordering consumes
	// (proxy_manager_warmth.go): it must be the REAL tag ("file", "internal",
	// "url") the desired set was built with, not a uniform placeholder, or a
	// URL-sourced identity would be ranked like a curated one. Held and
	// backed-off identities are recorded too; the ordering only looks up the
	// launch keys.
	m.sourceOf = sourceOf
	return launch, held, writeState, nil
}

// sortProxySettingsByKey orders settings by identity key.
func sortProxySettingsByKey(settings []*connect.ProxySettings) {
	for i := 1; i < len(settings); i++ {
		for j := i; j > 0 && settings[j].Key() < settings[j-1].Key(); j-- {
			settings[j], settings[j-1] = settings[j-1], settings[j]
		}
	}
}

// start imports/persists state and launches the background loops. The provide
// loop reads the desired set separately (desired(false)) so the import happens
// once, before any child reads.
func (m *proxyManager) start(parent context.Context) ([]*connect.ProxySettings, []*connect.ProxySettings, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		return nil, nil, fmt.Errorf("proxy manager already started")
	}
	launch, held, writeState, err := m.desired(true)
	if err != nil {
		return nil, nil, err
	}
	if writeState {
		if err := writeProxyState(m.state); err != nil {
			tlog("[proxy] warning: could not write proxy.state: %v\n", err)
		}
	}
	// Mirror the legacy <state>/proxy store back so the existing miner readers
	// keep working. URL-sourced proxies deliberately stay out of the legacy
	// file: it is the operator's curated config, and writing discovered
	// entries into it would make them indistinguishable from curated ones.
	m.mirrorLegacyProxyConfig()
	// m.sourceOf was recorded by desired() above with each identity's real
	// provenance; the warmth launch ordering reads it through
	// proxySourceOf().

	// URL-sourced proxies only launch on the next start until P2 wires the hot
	// reload watcher; the loops below still keep proxy_url.json and the trigger
	// current.
	proxyWarmupDone.Store(true)

	// Recovery scouts: install the provider's ranking (v33 main.go provide()).
	// The connect-side install is a seam until the pinned connect exports the
	// hook (proxy_manager_scout.go).
	installScoutScorer()

	// Command audit ring: the process lifecycle (initAuditRing +
	// recordProcessStart) is owned by the provide run path beside the control
	// socket (run.go), which starts before this manager and records commands
	// from the first one served. Only the shutdown persist stays here, so a
	// standalone manager still writes what it appended.

	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	m.started = true
	// The startup grading gate: the active probe loops below (the paid/file
	// grader, the URL fetch + stage-1 probe cycles, the stale-reaper pass)
	// wait on it until the provide run's workers are up. It decides from the
	// launch set here and the run's published worker accounting, and it never
	// blocks launching, admission, the control socket or the import
	// (proxy_manager_gate.go).
	m.gate = newGradingGate(len(launch))
	processGradingGate.Store(m.gate)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.gate.run(ctx)
	}()
	if len(m.cfg.Urls) > 0 {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			runProxyURLFetcher(ctx, m.cfg.Urls, m.cfg.Refresh, m.cfg.Max, m.cfg.ApiHost, m.cfg.ApiPort, m.cfg.SelfHeal, m.gate)
		}()
	}
	// AIMD pool controller (v33 runPoolController, T-07): computes and
	// persists the URL pool's TargetPoolSize every poolControlInterval, and
	// sheds the worst URL proxies back to the cache on a sustained-pressure
	// cut. The fetcher and cleanup loops read the target back (gated on
	// self-heal, see resolveEffectiveProxyURLMax), which is the half that
	// makes the learned operating point real; v33 starts this loop
	// unconditionally beside the fetcher, and so does the miner.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runPoolController(ctx, m.cfg.Max, m.cfg.SelfHeal)
	}()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runURLProxyReaper(ctx, m.cfg.ApiHost, m.cfg.ApiPort, m.gate)
	}()
	// Paid/file-list proxy grading (v33 proxy_grade_paid.go): rides the
	// reaper ticker cadence and grades non-URL proxies read-only on the
	// paid stale sweep, so proxy.state carries the Score/Graded fields the
	// trim, audit and summary read.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runPaidProxyGrader(ctx, m.cfg.ApiHost, m.cfg.ApiPort, m.gate)
	}()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		pruneURLProxyBlacklist(ctx)
	}()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runProxyURLCleanup(ctx, m.cfg.DeadCleanupScope, m.cfg.DeadCleanupInterval, m.cfg.SelfHeal)
	}()
	// Health cadence: writes proxy_health.state / proxy_health.log /
	// proxy_traffic.state / usage_history.jsonl and persists the overlaid
	// health back into proxy.state, so the proxy subcommands and the reaper
	// see live health (v33's health heartbeat).
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runProxyHealthLog(ctx, proxyHealthLogInterval)
	}()
	// Earnings recording loop (v33 runEarningWindows, recording half): folds
	// the live per-identity billable counters into the per-proxy earn tracker
	// (the paid grader's earn-skip and the audit's park decision) and the
	// persisted earnings store (the launch ranking, the trim tiebreak and the
	// scout) once a minute.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runProxyEarningsRecorder(ctx)
	}()
	// Direct-path egress grade (v33 proxy_direct_grade.go): read-only, persisted
	// to its own direct_grade.json, gated on the operator's direct toggle
	// (isDirectEnabled). v33 starts it unconditionally beside the reaper loops
	// with the same cadence; the same here.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runDirectGrader(ctx)
	}()
	// Grade summary (v33 proxy_grade_summary.go): a pure-read A-F snapshot of
	// the running set every proxy_grades.json interval, on the important/disk
	// logs and <state>/grades. It never probes, so it is safe at fleet size.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runProxyGradeSummary(ctx)
	}()
	// Proxy audit (v33 proxy_audit_run.go): parks proven-junk paid/file proxies
	// when the audit switch is on AND hot restart is on, and only logs
	// would-park otherwise (the miner's hot restart is P2, so it observes). 5
	// minute cadence, like v33.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runProxyAudit(ctx, proxyAuditParkCancelMap, &proxyAuditParkCancelMu, proxyAuditStartupEnabled())
	}()
	return launch, held, nil
}

// proxyAuditParkCancelMap is the per-address cancel map proxy audit parks
// through. v33 builds it in its provide loop (proxyCancelMap/proxyCancelMu);
// the miner's provide loop does not register per-proxy cancels yet (the hot
// reload reconciler is P2), so the map stays empty and a park finds nothing to
// cancel until P2 fills it. The audit still evaluates grades and reports.
var (
	proxyAuditParkCancelMap = map[string]context.CancelFunc{}
	proxyAuditParkCancelMu  sync.Mutex
)

// stop cancels the loops and waits for them.
func (m *proxyManager) stop() {
	m.mu.Lock()
	cancel := m.cancel
	gate := m.gate
	m.started = false
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
	// This manager's gate is done with the process: a later provide run in the
	// same process registers its own.
	if gate != nil {
		processGradingGate.CompareAndSwap(gate, nil)
	}
	// Flush the command audit ring (v33 main()'s shutdown persist).
	forceAuditPersist()
}

// mirrorLegacyProxyConfig rewrites <state>/proxy (the miner's legacy internal
// config) atomically. The content is the config as read: the mirror does not
// invent entries, it guarantees that the file a pre-port reader sees is whole
// and parseable after the manager has touched the state directory.
//
// Which curated source is authoritative is decided by proxy.state.Source, not
// by this file (v33 workflow selection): a configured source file (Workflow A)
// is what the manager, summary, refresh and remove-dead read, and the legacy
// internal config is only the Workflow B source. The mirror stays so a reader
// that predates the Source field still sees the internal config whole.
func (m *proxyManager) mirrorLegacyProxyConfig() {
	cfg := readProxyConfig()
	b, err := json.Marshal(cfg)
	if err != nil {
		tlog("[proxy] warn: could not marshal the legacy proxy mirror: %v\n", err)
		return
	}
	path, err := providerStatePath("proxy")
	if err != nil {
		return
	}
	if err := atomicWriteFile(path, b, 0700); err != nil {
		tlog("[proxy] warn: could not write the legacy proxy mirror: %v\n", err)
	}
}

// proxyManagerReadSettings is the manager-backed body of readProxySettings()
// (run.go). proxy.state absent means the port has never run here, so the
// legacy <state>/proxy file is the whole answer. With proxy.state present, the
// desired set is primary source + URL cache + operator trim cap, and the
// legacy file remains the operator's store for curated proxies.
func proxyManagerReadSettings() []*connect.ProxySettings {
	path, err := proxyStatePath()
	if err != nil {
		return readProxySettingsLegacy()
	}
	if _, err := os.Stat(path); err != nil {
		return readProxySettingsLegacy()
	}
	m := newProxyManager(defaultProxyManagerConfig())
	launch, _, _, err := m.desired(false)
	if err != nil {
		tlog("[proxy] warning: could not read proxy.state: %v\n", err)
		return readProxySettingsLegacy()
	}
	return launch
}
