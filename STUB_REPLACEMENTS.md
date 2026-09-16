# Stub Replacements

Working log for replacing bridge stubs in `provider/` with real implementations, one
stub file at a time. Fork source: `/tmp/wt-h3/provider/` (and connect internals at
`/tmp/wt-h3/*.go`). Connect v2026 library checked at `/home/klets/h3-workspace/connect/`.

## Batch 1: metrics_stubs.go, stubs_batch_m.go, stubs_batch_h.go, stubs_phase15.go, main_stubs.go

### prometheusLabelValue — REPLACED with real fork logic
Fork: `metrics_prometheus.go:278` (`connect.PrometheusLabelValue`). Local version only
escaped `\` and `"`; fork also scrubs invalid UTF-8 (`strings.ToValidUTF8`) and escapes
newlines. Ported the full fork behavior.

### prometheusHandlerStub — REPLACED, fixes a real bug
Was returning `nil`. `control_socket.go:serveMetrics` does
`&http.Server{Handler: prometheusHandlerStub()}` — a nil Handler falls back to
`http.DefaultServeMux`, which has nothing registered, so every `/metrics` request
served through this listener silently 404'd. `providerExtraMetrics()`
(metrics_provider.go:227) is already the complete Prometheus payload for this codebase
(the fork's `connect.PrometheusHandler` + `SetExtraMetricsProvider` bridge collapsed
into one function during porting), so the handler now just serves that string with the
correct Content-Type.

### stubMetricsProvider — REMOVED (dead code)
Defined but never called anywhere in the package. Deleted rather than "implemented":
nothing links to it, and `prometheusHandlerStub` above calls `providerExtraMetrics()`
directly.

### stubSetPersistentErrorFunc / stubSetExtraMetricsProvider — LEFT as no-ops
These correctly model a hook `connect` removed in v2026. The provider registers its own
equivalents locally (`IncrPersistentError`, `providerExtraMetrics`) and calls them
directly from `control_socket.go`, so these two no-op setters are genuinely vestigial
bridging shims, not missing logic. No change needed.

### getDohFailureCountStub — LEFT returning 0 (verified, not lazy)
Fork: `net_http_doh.go:33` `GetDohFailureCount()`, an atomic counter incremented at DoH
query call sites. v2026 `connect.DohCache` (`net_http_doh.go` in the v2026 lib) has no
failure-counting method at all, AND — more importantly — h3-provider never actually
calls `SharedDohCache().Query/QueryResult/Forward` anywhere yet (grepped, zero call
sites). DoH resolution isn't wired into the proxy path yet, so there is no failure
signal to count regardless of how this function is implemented. Returning 0 is accurate
today, not a placeholder. Revisit when DoH resolution is wired into proxy target
resolution.

### pqeTotalCounts / PQETotalCounts — LEFT returning zero value (blocked upstream)
Fork: `pqe_tracker.go` + `EncryptionSessionManager.PQECounts()`
(`transfer_encrypt.go:2509`), summed across `encryptionManagers` in main.go:1389-1410.
v2026 connect's `EncryptionSessionManager` (`transfer_encrypt.go:3065`) has no PQE
session tracker at all — this is a removed upstream capability, not a rename.
`encryptionManagers` registry itself is already ported and real
(`encryption_tracking.go`); only the per-manager counts method is missing from v2026
connect. Documented the gap; genuinely blocked until v2026 connect re-exposes
per-session PQE accounting.

### messagePoolSummary / ResizeMessagePoolsPerClass — LEFT as no-ops (verified)
v2026 connect's `messagePool` type (`message_pool.go:81`) is unexported with no public
resize or stats API. Fork's `connect.MessagePoolSummary`/`connect.ResizeMessagePoolsPerClass`
don't exist in v2026 connect under any name. Genuinely unavailable, not a wiring gap.

### activeConnectionCount / activeProxyConnections — REPLACED with real local data
Fork sourced both from atomic counters buried in connect's IP/transport layers
(`ip.go:31`, `transport.go:93`) that v2026 connect removed entirely. Implemented using
data this package already tracks for real: `ProxyHealthSnapshot()`'s
`bwMap map[string]*bandwidth.ProxyBandwidth` (proxy_health.go), specifically
`bw.Clients.Load()` per proxy (already used by `bandwidth_reporter.go` for the
bandwidth-report `Clients` field). `activeConnectionCount` sums clients across all
proxies; `activeProxyConnections` counts proxies with `Clients > 0`.

### contractMetricsSnapshot — REPLACED with real local data (partial)
Fork sourced `(acquired, denied, utilSum)` from atomics in connect's
`transfer_contract_manager.go`, swap-and-reset on each call. v2026 connect has none of
this. `acquired`/`denied` are now real: sourced from `globalContractMetrics.totals()`
(contract_metrics.go, already ported and real), diffed against the previous call's
cumulative totals to reproduce the same since-last-scrape delta semantics the fork had.
`utilSum` (per-contract byte utilization) stays 0 — nothing in this codebase tracks
that; it lived entirely inside connect's contract manager and was never ported. Callers
already guard divide-by-acquired, so this degrades to `avg_util=0%` rather than
crashing.

### proxyWarmupDone — REAL BUG FIXED (was a busy-loop hazard)
`proxyWarmupDone atomic.Bool` (main_stubs.go) was declared but **nothing ever called
`.Store(true)`** outside tests. `proxy_url_source.go:1118` and `proxy_reload.go:648`
both gate on it, so URL-sourced/hot-reloaded proxies would wait on it indefinitely in
production. Root cause: `paceMonitor` (stubs_provide.go) — the function responsible for
flipping it — was a no-op stub, even though it was wired up and running
(`go connect.HandleError(func() { paceMonitor(st.ctx) })` in provide.go:299).
Ported the real `paceMonitor` from fork main.go: polls `ProxyHealthSnapshot()`/
`ProxyHealthCount()` (both already ported, real) every 30s, flips `proxyWarmupDone`
once the fleet is small (<5 proxies), >90% up, force-completes after 60m, and triggers
a proxy reload signal on completion via the existing `proxyReloadPath()`/
`writeReloadTrigger()` helpers.

### VersionStamp — verified real, not a stub
`var VersionStamp string`, set via `-ldflags "-X main.VersionStamp=..."` at link time.
Matches fork's approach 1:1; no logic needed — Go string literals aren't affected by
`-trimpath` the way buildinfo is, so no extra dead-code-elimination guard is required.

## Verification
- `go build ./provider/...` — clean
- `go vet ./provider/...` — clean
- `go test ./provider/...` — one pre-existing flaky failure
  (`TestHotSwapParentDrainExitsCleanlyRepeatedly`, times out under full-suite CPU
  contention but passes in isolation both before and after this batch — confirmed via
  `git stash`); unrelated to these changes.
