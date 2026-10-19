package miner

import (
	"fmt"
	"maps"
	"runtime"
	"runtime/metrics"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// metrics_provider.go ports the 3.23-fix v33 provider's metrics_provider.go
// onto the miner: the operational counters the control socket increments, the
// startup/restart diagnostics, and minerExtraMetrics, the provider-side half
// of the scrape.
//
// v33 serves connect's PrometheusHandler and appends providerExtraMetrics
// through connect.SetExtraMetricsProvider. The pinned connect fork exports
// neither hook, so the miner serves the whole payload from minerExtraMetrics
// (see metrics_server.go) — the same adaptation the legacy provider in this
// repository made (provider/metrics_stubs.go, provider/metrics_provider.go).
//
// Families the miner cannot source yet are gated on hooks in metrics_seams.go
// and omitted while their hook is nil: a missing series means "not
// implemented", never a fabricated zero.

// --- Operational counters (in-process, exposed via /metrics) ---

var (
	controlCmdsAcked atomic.Int64
	controlCmdsSet   atomic.Int64
	controlCmdsGet   atomic.Int64
	controlCmdsClear atomic.Int64
)

// IncrControlCmd increments the appropriate counter based on the command type.
// The control socket calls it once per handled request, exactly as v33's
// handleControlRequest does.
func IncrControlCmd(cmd string) {
	controlCmdsAcked.Add(1)
	switch cmd {
	case "set":
		controlCmdsSet.Add(1)
	case "get":
		controlCmdsGet.Add(1)
	case "clear":
		controlCmdsClear.Add(1)
	}
}

// minerMetricsStartTime stands in for the run start time when no provide run
// has published one (a scrape served before the run begins, or in a test).
var minerMetricsStartTime = time.Now()

// minerExtraMetrics generates the Prometheus text-format payload the miner
// serves on /metrics. Called by the handler on every scrape.
func minerExtraMetrics() string {
	var b strings.Builder

	// --- Provider info (version) ---
	// A gauge fixed at 1 that carries the version as a label. The classic
	// text format this handler serves has no "info" type, and Prometheus
	// rejects the entire scrape on one. Uptime and the proxy count are their
	// own metrics (urnet_uptime_seconds, urnet_proxy_pool_size); as labels
	// they changed on every scrape and created a new series each time.
	version := minerControlVersion()
	writeSeamDiskBench(&b)
	fmt.Fprintf(&b, "# HELP urnet_info Provider identity: the version label, value always 1.\n")
	fmt.Fprintf(&b, "# TYPE urnet_info gauge\n")
	fmt.Fprintf(&b, "urnet_info{version=%s} 1\n", prometheusLabelValue(version))

	// --- Startup diagnostics (crash/restart/upgrade) ---
	writeStartupMetrics(&b, version)

	// --- Control socket commands ---
	fmt.Fprintf(&b, "# HELP urnet_control_commands_total Lifetime control socket commands acked.\n")
	fmt.Fprintf(&b, "# TYPE urnet_control_commands_total counter\n")
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"get\"} %d\n", controlCmdsGet.Load())
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"set\"} %d\n", controlCmdsSet.Load())
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"clear\"} %d\n", controlCmdsClear.Load())
	fmt.Fprintf(&b, "urnet_control_commands_total{cmd=\"any\"} %d\n", controlCmdsAcked.Load())

	// --- Proxy audit ---
	// Counts only; proxy addresses stay on the control socket. Emitted at zero
	// before the first tick so panels and alerts have a stable series.
	writeProxyAuditMetrics(&b)

	// --- Hotswap decline/success counters (written by the urnet-tools CLI) ---
	if hsCounts := readHotswapDeclinesFromDisk(); len(hsCounts) > 0 {
		fmt.Fprintf(&b, "# HELP urnet_hotswap_outcomes_total HotSwap attempt outcomes since last install.\n")
		fmt.Fprintf(&b, "# TYPE urnet_hotswap_outcomes_total counter\n")
		for _, reason := range slices.Sorted(maps.Keys(hsCounts)) {
			fmt.Fprintf(&b, "urnet_hotswap_outcomes_total{reason=%s} %d\n", prometheusLabelValue(reason), hsCounts[reason])
		}
	}

	// --- Sessions, contracts and the persisted lifetime store ---
	// v33 keeps these in connect and the provider's lifetime store; the
	// grading/earnings port owns the miner's equivalents (metrics_seams.go).
	writeSeamSessions(&b)
	writeSeamContracts(&b)
	writeSeamLifetime(&b)

	// --- Persistent error counts (survive restarts) ---
	writeSeamErrors(&b)

	// --- Traffic: proxy pool composition and aggregate byte counters ---
	writeTrafficMetrics(&b)

	// --- Resource pressure ---
	// currentPressure reads calm until the pressure monitor port lands
	// (proxy_manager_pressure.go), which is the v33 behavior on a box whose
	// monitor has not sampled yet.
	pressure := currentPressure()
	fmt.Fprintf(&b, "# HELP urnet_pressure_score Resource pressure (0=ok, 1=emergency).\n")
	fmt.Fprintf(&b, "# TYPE urnet_pressure_score gauge\n")
	fmt.Fprintf(&b, "urnet_pressure_score %g\n", pressure)

	// --- H1+ upgrades ---
	// The switch is the miner's; the fork keeps H1PlusStats per client
	// settings with no process-global accessor, so the counters are a seam.
	enabled := 0
	if controlH1PlusEnabled() {
		enabled = 1
	}
	fmt.Fprintf(&b, "# HELP urnet_h1plus_enabled 1 while H1+ upgrades are allowed (control key h1_plus, urnet-tools set h1-plus).\n")
	fmt.Fprintf(&b, "# TYPE urnet_h1plus_enabled gauge\n")
	fmt.Fprintf(&b, "urnet_h1plus_enabled %d\n", enabled)
	writeSeamBlock(&b, metricsSeamH1Plus)

	// --- DoH failures ---
	writeSeamDohFailures(&b)

	// --- Proxy grades ---
	writeProxyGradeMetrics(&b)

	// --- Restart reason and process resources ---
	writeNodeGauges(&b)
	writeResourceSignalGauges(&b)

	// --- Runtime memory, GC and goroutine gauges ---
	writeRuntimeGauges(&b)

	// --- Supervised background loops ---
	writeSeamLoops(&b)

	// --- Enforcement log ---
	writeSeamBlock(&b, metricsSeamEnforcement)

	// --- Peer key cross-check ---
	writeSeamBlock(&b, metricsSeamPeerKeyCrosscheck)

	// --- Dual-stack family transport ---
	writeSeamBlock(&b, metricsSeamDualstack)

	// --- Reconnect governor, transfer accounting and dead-client release ---
	writeSeamBlock(&b, metricsSeamGovern)

	// --- H3 identity set and DATAGRAM accounting ---
	writeSeamBlock(&b, metricsSeamH3)

	// --- connect stream and transport accounting ---
	// The pinned fork's own exposition: stream sequence kinds and lifetimes,
	// open requests, control frames, p2p transport churn, TLS handshakes by
	// dial path and the long dial backoff tier.
	b.WriteString(connect.StreamMetricsPrometheus())

	return b.String()
}

// writeStartupMetrics emits the crash/restart/upgrade gauges from the run
// marker beginControlRun consumed (control_runtime.go), which carries the same
// facts v33's detectStartup reads from its shutdown marker and version file.
func writeStartupMetrics(b *strings.Builder, currentVersion string) {
	state := readMinerRestartState()
	wasCleanStr := "0"
	if state.CleanShutdown {
		wasCleanStr = "1"
	}
	restarted := "0"
	if !state.CleanShutdown && state.PreviousVersion != "" {
		restarted = "1" // had a previous run that did not shut down cleanly
	}
	upgraded := "0"
	if state.PreviousVersion != "" && state.PreviousVersion != currentVersion {
		upgraded = "1"
	}
	fmt.Fprintf(b, "# HELP urnet_startup_clean_shutdown Whether the previous shutdown was clean.\n")
	fmt.Fprintf(b, "# TYPE urnet_startup_clean_shutdown gauge\n")
	fmt.Fprintf(b, "urnet_startup_clean_shutdown %s\n", wasCleanStr)

	fmt.Fprintf(b, "# HELP urnet_startup_restarted Whether the provider restarted (dirty or clean).\n")
	fmt.Fprintf(b, "# TYPE urnet_startup_restarted gauge\n")
	fmt.Fprintf(b, "urnet_startup_restarted %s\n", restarted)

	fmt.Fprintf(b, "# HELP urnet_startup_upgraded Whether the version changed since last run.\n")
	fmt.Fprintf(b, "# TYPE urnet_startup_upgraded gauge\n")
	fmt.Fprintf(b, "urnet_startup_upgraded %s\n", upgraded)

	if state.PreviousVersion != "" {
		fmt.Fprintf(b, "# HELP urnet_startup_previous_version Version before last upgrade.\n")
		fmt.Fprintf(b, "# TYPE urnet_startup_previous_version gauge\n")
		fmt.Fprintf(b, "urnet_startup_previous_version{version=%s} 1\n", prometheusLabelValue(state.PreviousVersion))
	}
}

// writeProxyAuditMetrics emits the audit engine's last tick. The miner runs
// the v33 audit engine (proxy_manager_audit_run.go), so every field has a real
// source; zero is emitted before the first tick so the series are stable.
func writeProxyAuditMetrics(b *strings.Builder) {
	audit := proxyAuditStatusSnapshot()
	if audit == nil {
		audit = &proxyAuditStatus{}
	}
	b2i := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_acting 1 when proxy audit executes parks, 0 when it only observes.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_acting gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_acting %d\n", b2i(audit.Acting))
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_parked Proxies the proxy audit engine is currently holding out.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_parked gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_parked %d\n", len(audit.Parked))
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_would_park Proxies proxy audit would park now (observe mode).\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_would_park gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_would_park %d\n", audit.WouldPark)
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_distrusted 1 when the last pass tripped the correlated-failure breaker.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_distrusted gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_distrusted %d\n", b2i(audit.Distrusted))
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_thin_pass 1 when too few proxies could be graded to trust the last pass.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_thin_pass gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_thin_pass %d\n", b2i(audit.Thin))
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_parks_24h Parks counted against the rolling 24h budget.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_parks_24h gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_parks_24h %d\n", audit.Parks24h)
	fmt.Fprintf(b, "# HELP urnet_proxy_audit_paused 1 while the paid proxy list is unreadable and the audit parks nothing.\n")
	fmt.Fprintf(b, "# TYPE urnet_proxy_audit_paused gauge\n")
	fmt.Fprintf(b, "urnet_proxy_audit_paused %d\n", b2i(audit.Paused))
}

// writeProxyGradeMetrics emits the provider's proxy grade and health
// distribution from proxy.state, exactly as v33's providerExtraMetrics does.
func writeProxyGradeMetrics(b *strings.Builder) {
	state, stateErr := readProxyState()
	urlState, urlErr := readProxyURLState()

	if stateErr == nil {
		fmt.Fprintf(b, "# HELP urnet_proxies_known Proxies in the provider's proxy state.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxies_known gauge\n")
		fmt.Fprintf(b, "urnet_proxies_known %d\n", len(state.Proxies))

		gradeDist := map[string]int{"A": 0, "B": 0, "C": 0, "D": 0, "E": 0, "F": 0, "ungraded": 0}
		healthDist := map[string]int{}
		var totalAuthFailures int64
		for _, entry := range state.Proxies {
			if entry.Graded {
				tier := proxyGradeTier(entry.Score)
				gradeDist[tier]++
			} else {
				gradeDist["ungraded"]++
			}
			healthDist[entry.Health]++
			totalAuthFailures += entry.AuthFailures
		}
		fmt.Fprintf(b, "# HELP urnet_proxy_grades Count by grade tier.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_grades gauge\n")
		for _, tier := range slices.Sorted(maps.Keys(gradeDist)) {
			fmt.Fprintf(b, "urnet_proxy_grades{tier=%s} %d\n", prometheusLabelValue(tier), gradeDist[tier])
		}

		fmt.Fprintf(b, "# HELP urnet_proxy_health Count by health status.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_health gauge\n")
		for _, health := range slices.Sorted(maps.Keys(healthDist)) {
			fmt.Fprintf(b, "urnet_proxy_health{status=%s} %d\n", prometheusLabelValue(health), healthDist[health])
		}

		if !state.StartedAt.IsZero() {
			providerAge := time.Since(state.StartedAt).Seconds()
			fmt.Fprintf(b, "# HELP urnet_provider_proxy_age_seconds How long managing current proxy set.\n")
			fmt.Fprintf(b, "# TYPE urnet_provider_proxy_age_seconds gauge\n")
			fmt.Fprintf(b, "urnet_provider_proxy_age_seconds %g\n", providerAge)
		}

		// Grading churn
		var recentlyGraded, staleGraded int
		cutoff := time.Now().Add(-1 * time.Hour)
		for _, entry := range state.Proxies {
			if entry.Graded && !entry.LastGraded.IsZero() {
				if entry.LastGraded.After(cutoff) {
					recentlyGraded++
				} else {
					staleGraded++
				}
			}
		}
		fmt.Fprintf(b, "# HELP urnet_proxy_graded_recent Proxies graded in last hour.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_graded_recent gauge\n")
		fmt.Fprintf(b, "urnet_proxy_graded_recent %d\n", recentlyGraded)

		fmt.Fprintf(b, "# HELP urnet_proxy_graded_stale Proxies with stale grade.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_graded_stale gauge\n")
		fmt.Fprintf(b, "urnet_proxy_graded_stale %d\n", staleGraded)

		// A sum over the proxies currently in state: removing a proxy drops
		// it, so this is a gauge, not a counter.
		fmt.Fprintf(b, "# HELP urnet_proxy_auth_failures Auth failures summed over the proxies currently in state.\n")
		fmt.Fprintf(b, "# TYPE urnet_proxy_auth_failures gauge\n")
		fmt.Fprintf(b, "urnet_proxy_auth_failures %d\n", totalAuthFailures)
	}

	// --- URL proxy grades ---
	if urlErr == nil && urlState != nil {
		var urlUngraded int
		urlGradeDist := map[string]int{"A": 0, "B": 0, "C": 0, "D": 0, "E": 0, "F": 0}
		for _, entry := range urlState.Cache {
			if entry.Graded {
				tier := proxyGradeTier(entry.Score)
				urlGradeDist[tier]++
			} else {
				urlUngraded++
			}
		}
		fmt.Fprintf(b, "# HELP urnet_url_proxy_grades URL proxy grade distribution.\n")
		fmt.Fprintf(b, "# TYPE urnet_url_proxy_grades gauge\n")
		for _, tier := range slices.Sorted(maps.Keys(urlGradeDist)) {
			fmt.Fprintf(b, "urnet_url_proxy_grades{tier=%s} %d\n", prometheusLabelValue(tier), urlGradeDist[tier])
		}
		fmt.Fprintf(b, "# HELP urnet_url_proxy_ungraded URL proxies not yet graded.\n")
		fmt.Fprintf(b, "# TYPE urnet_url_proxy_ungraded gauge\n")
		fmt.Fprintf(b, "urnet_url_proxy_ungraded %d\n", urlUngraded)
	}
}

// writeNodeGauges emits the restart-reason and resource gauges. Resource
// figures the platform cannot supply (zero) are left out rather than exported
// as 0, so a missing series means unknown.
func writeNodeGauges(b *strings.Builder) {
	state := readMinerRestartState()
	if state.Reason != "" {
		fmt.Fprintf(b, "# HELP urnet_restart_reason Why the provider last started: 1 for the current reason only.\n")
		fmt.Fprintf(b, "# TYPE urnet_restart_reason gauge\n")
		fmt.Fprintf(b, "urnet_restart_reason{reason=%s} 1\n", prometheusLabelValue(state.Reason))
	}
	res := collectMetricsResources()
	if res.MemLimitBytes != nil && *res.MemLimitBytes > 0 {
		fmt.Fprintf(b, "# HELP urnet_mem_limit_bytes Go memory limit in effect, absent when none is set.\n")
		fmt.Fprintf(b, "# TYPE urnet_mem_limit_bytes gauge\n")
		fmt.Fprintf(b, "urnet_mem_limit_bytes %d\n", *res.MemLimitBytes)
	}
	if res.RSSBytes != nil && *res.RSSBytes > 0 {
		fmt.Fprintf(b, "# HELP urnet_rss_bytes Resident set size of the provider process (Linux).\n")
		fmt.Fprintf(b, "# TYPE urnet_rss_bytes gauge\n")
		fmt.Fprintf(b, "urnet_rss_bytes %d\n", *res.RSSBytes)
	}
	if res.OpenFDs != nil && *res.OpenFDs > 0 {
		fmt.Fprintf(b, "# HELP urnet_open_fds Open file descriptors of the provider process (Linux).\n")
		fmt.Fprintf(b, "# TYPE urnet_open_fds gauge\n")
		fmt.Fprintf(b, "urnet_open_fds %d\n", *res.OpenFDs)
	}
	if res.FDLimit != nil && *res.FDLimit > 0 {
		fmt.Fprintf(b, "# HELP urnet_fd_limit Soft file descriptor limit of the provider process (Linux).\n")
		fmt.Fprintf(b, "# TYPE urnet_fd_limit gauge\n")
		fmt.Fprintf(b, "urnet_fd_limit %d\n", *res.FDLimit)
	}
}

// writeRuntimeGauges emits the uptime and Go memory gauges the alert rules
// and dashboard panels read; v33's connect handler served them, and the fork
// serves the whole payload from here.
func writeRuntimeGauges(b *strings.Builder) {
	startedAt := controlRuntime.startedAtSnapshot()
	if startedAt.IsZero() {
		startedAt = minerMetricsStartTime
	}
	uptime := time.Since(startedAt)
	if uptime < 0 {
		uptime = 0
	}
	fmt.Fprintf(b, "# HELP urnet_uptime_seconds Provider uptime in seconds.\n")
	fmt.Fprintf(b, "# TYPE urnet_uptime_seconds gauge\n")
	fmt.Fprintf(b, "urnet_uptime_seconds %g\n", uptime.Seconds())

	samples := []metrics.Sample{
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
	}
	metrics.Read(samples)
	fmt.Fprintf(b, "# HELP urnet_gc_cycles_total Cumulative GC cycles.\n")
	fmt.Fprintf(b, "# TYPE urnet_gc_cycles_total counter\n")
	fmt.Fprintf(b, "urnet_gc_cycles_total %d\n", readUint64Metric(samples[0].Value))
	fmt.Fprintf(b, "# HELP urnet_mem_heap_bytes Go heap allocation in bytes.\n")
	fmt.Fprintf(b, "# TYPE urnet_mem_heap_bytes gauge\n")
	fmt.Fprintf(b, "urnet_mem_heap_bytes %d\n", readUint64Metric(samples[1].Value))
	fmt.Fprintf(b, "# HELP urnet_mem_sys_bytes Go runtime total memory obtained from OS.\n")
	fmt.Fprintf(b, "# TYPE urnet_mem_sys_bytes gauge\n")
	fmt.Fprintf(b, "urnet_mem_sys_bytes %d\n", readUint64Metric(samples[2].Value))

	fmt.Fprintf(b, "# HELP urnet_goroutines Number of goroutines.\n")
	fmt.Fprintf(b, "# TYPE urnet_goroutines gauge\n")
	fmt.Fprintf(b, "urnet_goroutines %d\n", runtime.NumGoroutine())
}

// readUint64Metric reads a uint64 runtime/metrics sample, 0 on any other kind.
func readUint64Metric(v metrics.Value) uint64 {
	if v.Kind() != metrics.KindUint64 {
		return 0
	}
	return v.Uint64()
}

// controlH1PlusEnabled reports whether H1+ upgrades are allowed: the h1_plus
// control key when set, else the on default (controlLiveDefaults).
func controlH1PlusEnabled() bool {
	if v, ok := globalControlState.get("h1_plus"); ok {
		return strings.EqualFold(strings.TrimSpace(v), "on")
	}
	return true
}

// The hotswap decline/success counters are read through
// readHotswapDeclinesFromDisk in hotswap.go (shared with the hotswap status
// surface and covered by hotswap_test.go).

// prometheusLabelValue quotes a label value for the text exposition format,
// escaping backslash, double-quote, and newlines per the spec, and scrubbing
// invalid UTF-8 so a malformed label cannot corrupt the scrape. Ported from
// connect's PrometheusLabelValue, which the pinned fork no longer exports
// (see provider/metrics_stubs.go).
func prometheusLabelValue(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	return `"` + s + `"`
}

// --- seam writers (see metrics_seams.go for the hook registry) ---

// writeSeamBlock appends one hook's block when the hook is set.
func writeSeamBlock(b *strings.Builder, hook func(*strings.Builder) bool) {
	if hook != nil {
		hook(b)
	}
}

func writeSeamSessions(b *strings.Builder) {
	if metricsSeamSessions == nil {
		return
	}
	pq := metricsSeamSessions()
	if pq == nil {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_sessions_pqe Active PQE sessions.\n")
	fmt.Fprintf(b, "# TYPE urnet_sessions_pqe gauge\n")
	fmt.Fprintf(b, "urnet_sessions_pqe %d\n", pq.ActivePQE)
	fmt.Fprintf(b, "# HELP urnet_sessions_classical Active classical sessions.\n")
	fmt.Fprintf(b, "# TYPE urnet_sessions_classical gauge\n")
	fmt.Fprintf(b, "urnet_sessions_classical %d\n", pq.ActiveClas)
	fmt.Fprintf(b, "# HELP urnet_sessions_opened_total Sessions opened over the provider's lifetime.\n")
	fmt.Fprintf(b, "# TYPE urnet_sessions_opened_total counter\n")
	fmt.Fprintf(b, "urnet_sessions_opened_total{encryption=\"pqe\"} %d\n", pq.PQELifetime)
	fmt.Fprintf(b, "urnet_sessions_opened_total{encryption=\"classical\"} %d\n", pq.ClasLifetime)
	fmt.Fprintf(b, "# HELP urnet_sessions_opened_recent Sessions opened in the last hour, day, or week.\n")
	fmt.Fprintf(b, "# TYPE urnet_sessions_opened_recent gauge\n")
	fmt.Fprintf(b, "urnet_sessions_opened_recent{window=\"hour\",encryption=\"pqe\"} %d\n", pq.PQEHour)
	fmt.Fprintf(b, "urnet_sessions_opened_recent{window=\"day\",encryption=\"pqe\"} %d\n", pq.PQEDay)
	fmt.Fprintf(b, "urnet_sessions_opened_recent{window=\"week\",encryption=\"pqe\"} %d\n", pq.PQEWeek)
	fmt.Fprintf(b, "urnet_sessions_opened_recent{window=\"hour\",encryption=\"classical\"} %d\n", pq.ClasHour)
	fmt.Fprintf(b, "urnet_sessions_opened_recent{window=\"day\",encryption=\"classical\"} %d\n", pq.ClasDay)
	fmt.Fprintf(b, "urnet_sessions_opened_recent{window=\"week\",encryption=\"classical\"} %d\n", pq.ClasWeek)
}

func writeSeamContracts(b *strings.Builder) {
	if metricsSeamContracts == nil {
		return
	}
	acquired, denied, ok := metricsSeamContracts()
	if !ok {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_contracts_total Cumulative contract outcomes.\n")
	fmt.Fprintf(b, "# TYPE urnet_contracts_total counter\n")
	fmt.Fprintf(b, "urnet_contracts_total{result=\"acquired\"} %d\n", acquired)
	fmt.Fprintf(b, "urnet_contracts_total{result=\"denied\"} %d\n", denied)
}

func writeSeamLifetime(b *strings.Builder) {
	if metricsSeamLifetime == nil {
		return
	}
	lt := metricsSeamLifetime()
	if lt == nil {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_lifetime_sessions_total Lifetime sessions opened (persists across restarts).\n")
	fmt.Fprintf(b, "# TYPE urnet_lifetime_sessions_total counter\n")
	fmt.Fprintf(b, "urnet_lifetime_sessions_total{encryption=\"pqe\"} %d\n", lt.SessionsPQE)
	fmt.Fprintf(b, "urnet_lifetime_sessions_total{encryption=\"classical\"} %d\n", lt.SessionsClas)
	fmt.Fprintf(b, "# HELP urnet_lifetime_contracts_total Lifetime contract outcomes.\n")
	fmt.Fprintf(b, "# TYPE urnet_lifetime_contracts_total counter\n")
	fmt.Fprintf(b, "urnet_lifetime_contracts_total{result=\"acquired\"} %d\n", lt.ContractsAcquired)
	fmt.Fprintf(b, "urnet_lifetime_contracts_total{result=\"denied\"} %d\n", lt.ContractsDenied)
	fmt.Fprintf(b, "# HELP urnet_lifetime_proxies_total Lifetime proxy transitions.\n")
	fmt.Fprintf(b, "# TYPE urnet_lifetime_proxies_total counter\n")
	fmt.Fprintf(b, "urnet_lifetime_proxies_total{event=\"recovered\"} %d\n", lt.ProxiesRecovered)
	fmt.Fprintf(b, "urnet_lifetime_proxies_total{event=\"lost\"} %d\n", lt.ProxiesLost)
	fmt.Fprintf(b, "# HELP urnet_lifetime_billable_bytes_total Lifetime billable bytes transferred.\n")
	fmt.Fprintf(b, "# TYPE urnet_lifetime_billable_bytes_total counter\n")
	fmt.Fprintf(b, "urnet_lifetime_billable_bytes_total %d\n", lt.BillableBytes)
}

func writeSeamErrors(b *strings.Builder) {
	if metricsSeamErrors == nil {
		return
	}
	counts := metricsSeamErrors()
	if len(counts) == 0 {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_lifetime_errors_total Persistent error counts by category (survive restarts).\n")
	fmt.Fprintf(b, "# TYPE urnet_lifetime_errors_total counter\n")
	for _, cat := range slices.Sorted(maps.Keys(counts)) {
		fmt.Fprintf(b, "urnet_lifetime_errors_total{category=%s} %d\n", prometheusLabelValue(cat), counts[cat])
	}
}

func writeSeamLoops(b *strings.Builder) {
	if metricsSeamLoops == nil {
		return
	}
	loops := metricsSeamLoops()
	if len(loops) == 0 {
		return
	}
	slices.SortFunc(loops, func(a, b seamLoop) int { return strings.Compare(a.Name, b.Name) })
	fmt.Fprintf(b, "# HELP urnet_loop_restarts_total Times a supervised background loop ended unexpectedly and was restarted.\n")
	fmt.Fprintf(b, "# TYPE urnet_loop_restarts_total counter\n")
	for _, loop := range loops {
		fmt.Fprintf(b, "urnet_loop_restarts_total{loop=%q} %d\n", loop.Name, loop.Restarts)
	}
	fmt.Fprintf(b, "# HELP urnet_loop_up 1 while a supervised background loop is running, 0 while it is stopped or backing off.\n")
	fmt.Fprintf(b, "# TYPE urnet_loop_up gauge\n")
	for _, loop := range loops {
		up := 0
		if loop.Up {
			up = 1
		}
		fmt.Fprintf(b, "urnet_loop_up{loop=%q} %d\n", loop.Name, up)
	}
}

func writeSeamDohFailures(b *strings.Builder) {
	if metricsSeamDohFailures == nil {
		return
	}
	fmt.Fprintf(b, "# HELP urnet_doh_failures_total DNS-over-HTTPS failures.\n")
	fmt.Fprintf(b, "# TYPE urnet_doh_failures_total counter\n")
	fmt.Fprintf(b, "urnet_doh_failures_total %d\n", metricsSeamDohFailures())
}

func writeSeamDiskBench(b *strings.Builder) {
	if metricsSeamDiskBench == nil {
		return
	}
	d := metricsSeamDiskBench()
	if d == nil {
		return
	}
	slow := 0
	if d.Slow {
		slow = 1
	}
	fmt.Fprintf(b, "# HELP urnet_disk_write_mbps Synchronous write speed measured by the startup disk benchmark, MB/s.\n")
	fmt.Fprintf(b, "# TYPE urnet_disk_write_mbps gauge\n")
	fmt.Fprintf(b, "urnet_disk_write_mbps %.1f\n", d.WriteMBps)
	fmt.Fprintf(b, "# HELP urnet_disk_slow 1 when the last disk benchmark judged the disk slow (under 50 MB/s) or it hung.\n")
	fmt.Fprintf(b, "# TYPE urnet_disk_slow gauge\n")
	fmt.Fprintf(b, "urnet_disk_slow %d\n", slow)
	fmt.Fprintf(b, "# HELP urnet_disk_bench_age_seconds Age of the last disk benchmark result.\n")
	fmt.Fprintf(b, "# TYPE urnet_disk_bench_age_seconds gauge\n")
	fmt.Fprintf(b, "urnet_disk_bench_age_seconds %d\n", d.AgeSeconds)
}
