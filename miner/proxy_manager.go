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
// Design (see /home/klets/ur/scratch/sn-backup/one-program-plan.md section 2):
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

// proxyManagerEnvOrFlagInt is proxyManagerEnvOrFlag for integers.
func proxyManagerEnvOrFlagInt(opts docopt.Opts, flag, env string, def int) int {
	raw := proxyManagerEnvOrFlag(opts, flag, env, "")
	if raw == "" {
		return def
	}
	if n, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && n >= 0 {
		return n
	}
	return def
}

// proxyManagerEnvOrFlagDuration is proxyManagerEnvOrFlag for durations.
func proxyManagerEnvOrFlagDuration(opts docopt.Opts, flag, env string, def time.Duration) time.Duration {
	raw := proxyManagerEnvOrFlag(opts, flag, env, "")
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(strings.TrimSpace(raw)); err == nil && d > 0 {
		return d
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

// proxyManagerConfigFromOpts resolves the manager config from the provide
// command's options and the environment.
func proxyManagerConfigFromOpts(opts docopt.Opts, apiUrl string) proxyManagerConfig {
	cfg := defaultProxyManagerConfig()
	cfg.Urls = resolveProxyURLs(opts)
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
}

func newProxyManager(cfg proxyManagerConfig) *proxyManager {
	return &proxyManager{cfg: cfg, sourceOf: map[string]string{}}
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
		launch, held = startupTrimSelection(all, trimCap, state.Proxies, gradeFor, func(string) float64 { return 0 })
		importantLogf("[proxy][trim] startup: cap=%d, launching %d of %d desired, holding %d worst-graded until the cap is raised\n",
			trimCap, len(launch), len(all), len(held))
		primeTrimCapSeen(trimCap, trimCapOperator)
	} else {
		launch = all
		if trimCap > 0 {
			primeTrimCapSeen(trimCap, trimCapOperator)
		}
	}
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
	m.sourceOf = map[string]string{}
	for _, s := range launch {
		m.sourceOf[s.Key()] = "internal"
	}

	// URL-sourced proxies only launch on the next start until P2 wires the hot
	// reload watcher; the loops below still keep proxy_url.json and the trigger
	// current.
	proxyWarmupDone.Store(true)

	ctx, cancel := context.WithCancel(parent)
	m.cancel = cancel
	m.started = true
	if len(m.cfg.Urls) > 0 {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			runProxyURLFetcher(ctx, m.cfg.Urls, m.cfg.Refresh, m.cfg.Max, m.cfg.ApiHost, m.cfg.ApiPort, m.cfg.SelfHeal)
		}()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runURLProxyReaper(ctx, m.cfg.ApiHost, m.cfg.ApiPort)
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
	// Direct-path egress grade (v33 proxy_direct_grade.go): read-only, persisted
	// to its own direct_grade.json, gated on the operator's direct toggle
	// (isDirectEnabled). v33 starts it unconditionally beside the reaper loops
	// with the same cadence; the same here.
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		runDirectGrader(ctx)
	}()
	return launch, held, nil
}

// stop cancels the loops and waits for them.
func (m *proxyManager) stop() {
	m.mu.Lock()
	cancel := m.cancel
	m.started = false
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	m.wg.Wait()
}

// mirrorLegacyProxyConfig rewrites <state>/proxy (the miner's legacy internal
// config) atomically. The content is the config as read: the mirror does not
// invent entries, it guarantees that the file a pre-port reader sees is whole
// and parseable after the manager has touched the state directory.
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
		tlog("[proxy] warning: could not read the managed proxy state (%v); using the legacy proxy file\n", err)
		return readProxySettingsLegacy()
	}
	return launch
}
