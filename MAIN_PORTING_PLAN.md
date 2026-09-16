# main.go Porting Plan

> **Source**: `/tmp/wt-h3/provider/main.go` (6427 lines, 167 `connect.*` references)
> **Target**: `/home/klets/ur/h3-provider/provider/main.go`
> **Generated**: 2026-09-16

---

## 1. connect.* Symbol Inventory (67 unique symbols)

### 1A. EXISTS in v2026 — can use directly (40)

These symbols are defined in `/home/klets/h3-workspace/connect/` and can be imported as-is:

| Symbol | v2026 Location |
|--------|---------------|
| `ApiCallbackResult` | api callback types |
| `AuthCodeLoginArgs` | api args |
| `AuthCodeLoginResult` | api result |
| `AuthLoginWithPasswordArgs` | api args |
| `AuthLoginWithPasswordResult` | api result |
| `AuthNetworkClientArgs` | api args |
| `AuthNetworkClientResult` | api result |
| `ByteCount` | net.go |
| `ClientAuth` | transport types |
| `ClientSettings` | client.go |
| `ClientStrategy` | strategy.go |
| `DefaultClientSettings` | client.go |
| `DefaultClientStrategySettings` | strategy.go |
| `DefaultEncryptionSettings` | encryption types |
| `DefaultLocalUserNatSettings` | nat types |
| `DefaultRemoteUserNatProviderSettings` | nat types |
| `EncryptionModeOpportunistic` | encryption constants |
| `EncryptionSessionManager` | encryption session |
| `GetClientKeyResult` | api result |
| `HandleError` | error handling |
| `HttpGetWithStrategy` | api.go |
| `Id` | id types |
| `IsDegraded` | backend_degraded |
| `LocalUserNatSettings` | nat types |
| `NewApiOutOfBandControl` | api_control types |
| `NewBlockingApiCallback` | api callback |
| `NewBringYourApi` | api.go |
| `NewClient` | client.go |
| `NewClientStrategy` | strategy.go |
| `NewClientStrategyWithDefaults` | strategy.go |
| `NewContractManagerWithDefaults` | contract types |
| `NewEventWithContext` | event types |
| `NewId` | id types |
| `NewLocalUserNat` | nat types |
| `NewNoopApiCallback` | api callback |
| `NewPlatformTransportWithDefaults` | transport types |
| `NewRemoteUserNatProvider` | nat types |
| `NewRouteManager` | route manager |
| `ParseByteCount` | net.go |
| `ParseId` | id types |

### 1B. CHANGED — exist in v2026 but structurally different (3)

| Symbol | Difference | Impact |
|--------|-----------|--------|
| **`ProxySettings`** | v2026: `{Network, Address, Auth}` — **no `Index` field** | Fork uses `proxySettings.Index` extensively (bandwidth tracking, contract callbacks, proxy registration). Must add an adapter/wrapper. |
| **`AuthCodeCreateArgs`** | Only in API spec YAML (`api/bringyour.yml`), not a Go type in the connect package root | Need to generate or manually define the Go struct, or use a raw HTTP call |
| **`AuthCodeCreateResult`** | Same as above | Same |

### 1C. REMOVED from v2026 (24)

Most of these are **proxy health/metrics/registration** functions that lived in the old WebSocket-based connect package. The fork's `proxy_health.go`, `contract_metrics.go`, `metrics_stubs.go`, and `main_stubs.go` already contain partial replacements for many of them.

| Symbol | Fork Usage (Line #s) | Already Stubbed in h3-provider? | Porting Action |
|--------|----------------------|-------------------------------|----------------|
| `ActiveConnectionCount` | L2003 | ✅ `stubs_batch_h.go` | Stub already exists; no-op in v2026 |
| `ActiveProxyConnections` | L2003 | ❌ | Return 0; v2026 doesn't track this |
| `ApplyAutoTuning` | L3054 | ❌ | Must stub as no-op or implement equivalent |
| `ContractMetricsSnapshot` | L1834 | ❌ (but `contract_metrics.go` has callback-based approach) | Use `registerContractCallback` from `contract_metrics.go` |
| `CritLogger` | L2749 | ❌ | Assign to `critLog` from `critlog.go` |
| `DegradedProxies` | L2725 | ✅ `proxy_health.go` | Has `DegradedProxies()` replacement |
| `DegradedProxyEntry` | L2593, L2620+ | ✅ `proxy_health.go` | Type defined locally |
| `EnableProfiling` | L3937 | ❌ | Stub as no-op or use `net/http/pprof` directly |
| `GetDohFailureCount` | L1987 | ✅ `metrics_stubs.go` | Stub exists |
| `MessagePoolSummary` | L2013 | ❌ | Return nil; v2026 pool management differs |
| `PQECounts` | L1390 | ❌ | Define locally or return zero struct |
| `ProxyAuthFailureCount` | L4787 | ✅ `proxy_health.go` | Has local replacement |
| `ProxyBandwidth` | L3607-3609 | ✅ `main_stubs.go` | `ProxyBandwidthV2026` stub exists |
| `ProxyEverUp` | L4783 | ✅ `proxy_health.go` | Has local replacement |
| `ProxyHealthByAddress` | L2269 | ✅ `proxy_health.go` | Has local replacement |
| `ProxyHealthCount` | L1640,1765,1871,2031,2972 | ✅ `proxy_health.go` | Has local replacement |
| `ProxyHealthHeartbeat` | L2036 | ✅ `proxy_health.go` | Has local replacement |
| `ProxyHealthSnapshot` | L114,1506,1650,1770,1879,2973 | ✅ `proxy_health.go` | Has local replacement |
| `RegisterProxy` | L3793,3829 | ✅ `main_stubs.go` | `registerProxyV2026` stub exists |
| `RegisterProxyBandwidth` | L3609,3611 | ✅ `proxy_health.go` | Has local replacement |
| `ResizeMessagePoolsPerClass` | L407,1187,2774 | ❌ | Stub as no-op; v2026 uses `memory_budget`/`message_pool.go` |
| `RunSystemAudit` | L261 | ❌ | Stub as returning `(false, false)` |
| `TriggerPulse` | L2978 | ❌ | Stub as no-op |
| `UnregisterProxy` | L3790,3858 | ✅ `sn.go`, `proxy_health.go` | Has local replacement |

---

## 2. Other Dependencies on provider/*.go Files

### 2A. Already Ported (in `/home/klets/ur/h3-provider/provider/`)

| File | Lines | Purpose | Key Functions/Types Used by main.go |
|------|-------|---------|-------------------------------------|
| `proxy_health.go` | ~500 | Health tracking replacing `connect.ProxyHealth*` | `DegradedProxies()`, `ProxyHealthCount()`, `ProxyHealthSnapshot()`, `ProxyHealthByAddress()`, `ProxyHealthHeartbeat()`, `ProxyEverUp()`, `ProxyAuthFailureCount()`, `RegisterProxyBandwidth()` |
| `proxy_state.go` | ~200 | Proxy ID/state persistence | `ProxyState`, `ProxyEntry`, `readProxyState()` |
| `proxy_id.go` | ~100 | Address-stable proxy IDs | `initProxyIDCounter()` |
| `contract_metrics.go` | ~200 | Contract acquired/denied tracking | `registerContractCallback()` |
| `main_stubs.go` | 101 | Temporary stubs for main.go symbols | `readProxySettings()`, `readProxySettingsFromFile()`, `prioritizeAndScheduleProxies()`, `registerProxyV2026()`, `ProxyBandwidthV2026` |
| `critlog.go` | ~100 | Critical event logger | `critLog` variable (replaces `connect.CritLogger`) |
| `doh_cache.go` | ~50 | DoH cache management | `SetSharedDohCache()`, `SharedDohCache()` |
| `proxy_reload.go` | ~100 | Hot-reload signal handling | `proxyReloadPath()`, `writeReloadTrigger()` |
| `proxy_url.go` | ~100 | URL-sourced proxy cache | `mergeProxyURLCache()` |
| `proxy_url_source.go` | ~200 | URL proxy source management | Uses `ProxyHealthByAddress` |
| `hotswap.go` | ~300 | In-process provider restart | `hotSwap()`, IPC, candidate checks |
| `auth_rate_limiter.go` | ~150 | Auth rate limiting | `globalAuthRateLimiter` |
| `proxy_failure_history.go` | ~100 | Failure tracking per proxy | `globalProxyFailureHistory` |
| `proxy_slow_retry.go` | ~100 | Slow retry backoff | `globalProxySlowRetryState` |
| `proxy_admission_gate.go` | ~100 | Auth admission gating | `globalProxyAdmissionGate` |
| `proxy_benchmark.go` | ~100 | Proxy benchmarking | `startProxyBenchmarks()` |
| `proxy_paste.go` | ~50 | Proxy paste/display | Used in CLI |
| `proxy_match.go` | ~50 | Proxy pattern matching | Used in `proxyRemoveMatch` |
| `proxy_earnings_store.go` | ~150 | Earnings persistence | `globalProxyEarningsStore` |
| `proxy_warmth.go` | ~150 | Proxy warmth tiers | Warm/renewable/cold classification |
| `pool_health.go` | ~200 | Connection pool health | Idle floor tracking |
| `resource_pressure.go` | ~100 | RAM pressure monitoring | `runPressureMonitor()` |
| `metrics_provider.go` | ~100 | Prometheus metrics | Metrics exposition |
| `metrics_listen.go` | ~50 | Metrics HTTP listener | |
| `metrics_stubs.go` | ~30 | Stubbed metrics | `GetDohFailureCount` stub |
| `lifetime_metrics.go` | ~200 | Lifetime counters | |
| `earn_tracker.go` | ~100 | Per-proxy earnings tracking | Uses `ProxyHealthSnapshot` keys |
| `bandwidth_reporter.go` | ~100 | Bandwidth rate reporting | |
| `startup_env_seed.go` | ~50 | Env seeding from control state | `seedEnvFromControlState()` |
| `startup_banner.go` | ~50 | Startup banner/logging | |
| `shmlog_linux.go` | ~100 | Shared memory logging | `initSHMLoggerWithHandover()`, `shmLogFatal()` |
| `shmlog_fallback.go` | ~50 | Cross-platform shmlog | |
| `shmlog_trim.go` | ~50 | Log trimming | |
| `tlog.go` | ~50 | Timestamped logging | `tlog()` |
| `important_log.go` | ~50 | Important event logging | |
| `important_logf.go` | ~50 | Formatted important logging | |
| `audit_ring.go` | ~100 | Audit ring buffer | |
| `sn.go` | ~400 | Subnet/network functions | `NetworkGetRankingSync()`, `SnSetWallet()` |
| `sn_rpc.go` | ~200 | SN RPC calls | |
| `network.go` | ~100 | Network state | |
| `network_cmd.go` | ~100 | Network CLI commands | |
| `control_socket.go` | ~200 | Control socket for `urnet-tools` | |
| `control_state.go` | ~200 | Control state persistence | |
| `pending_overrides.go` | ~100 | Pending override management | |
| `direct.go` | ~100 | Direct connection toggle | `isDirectEnabled()` |
| `systemd_status.go` | ~50 | Systemd integration | |
| `ssrf_guard.go` | ~50 | SSRF protection | |

### 2B. NOT Yet Ported (still only in fork)

| Symbol/Type | Used At | Description |
|-------------|---------|-------------|
| `ProxyConfig` | L5243-5387 | Stubbed in `main_stubs.go` as empty struct |
| `ProxyAuth` | L5250 | Stubbed in `main_stubs.go` |
| `removedProxy` | L6072 | CLI removal candidates |
| `removeDeadOptions` | L6081 | Dead proxy removal options |
| `Status` / `WarpStatusResult` | L4812-4835 | HTTP status endpoint |
| `activeProxy` | L5843 | Used in `proxyActivity()` |
| `trafficBytes` | L1312 | Metrics formatting struct |

---

## 3. Major Sections of main.go

### Section A: Initialization (Lines 1-189)
- **Functions**: `init()`, `isLongRunningSubcommand()`, `seedEnvFromControlState()`, `initGlog()`
- **Dependencies**: `shmlog_linux.go`, `startup_env_seed.go`, `tlog.go`
- **connect.* refs**: None directly; runs before connect is imported
- **Portability**: ✅ **Highly portable** — standard Go init, already mostly handled by `provider/` files

### Section B: Pacer & Warmup (Lines 46-158)
- **Functions**: `backoffPacerWithDelay()`, `backoffPacer()`, `paceMonitor()`
- **Dependencies**: `connect.ProxyHealthSnapshot()`, `connect.ProxyHealthCount()` → all replaced by `proxy_health.go`
- **connect.* refs**: 3 (all removed, all have replacements)
- **Portability**: ✅ **Portable** — substitute `proxy_health.go` replacements

### Section C: Startup Audit (Lines 249-262)
- **Functions**: `RunStartupAudit()`
- **Dependencies**: `connect.RunSystemAudit()` → removed
- **Portability**: ✅ **Portable** — stub as no-op or implement disk audit directly

### Section D: Profile Settings (Lines 264-468)
- **Functions**: `applyLowmodeSettings()`, `applyTurboSettings()`, `applyTurboMemoryLimit()`, `applyPoolAutoSize()`, `applyEcoSettings()`, `ensureMemoryLimit()`
- **Dependencies**: `connect.ClientSettings`, `connect.LocalUserNatSettings`, `connect.ByteCount`, `connect.ApplyAutoTuning`, `connect.ResizeMessagePoolsPerClass`
- **connect.* refs**: ~15 (mix of exists + removed)
- **Portability**: ⚠️ **Moderate** — `ApplyAutoTuning` and `ResizeMessagePoolsPerClass` need stubs; rest exist in v2026

### Section E: JWT Utilities (Lines 539-650)
- **Functions**: `validateJWTExpiry()`, `parseJWTExpiryTime()`, `jwtContainsClientId()`, `jwtNetworkId()`, `jwtClientId()`, `accountNetworkId()`, `readAccountJWT()`
- **Dependencies**: Standard library only (JWT parsing, file I/O)
- **connect.* refs**: None
- **Portability**: ✅ **Fully portable** — pure utility functions

### Section F: Session Management (Lines 713-788)
- **Functions**: `isSessionFile()`, `applyStagedSession()`, `sanitizeRootPath()`
- **Dependencies**: Standard library, OS commands
- **connect.* refs**: None
- **Portability**: ✅ **Fully portable**

### Section G: main() & CLI Dispatch (Lines 889-1148)
- **Functions**: `main()`
- **Dependencies**: `docopt`, `initGlog()`, all CLI subcommand functions
- **connect.* refs**: None (dispatches to other functions)
- **Portability**: ✅ **Portable** — standard CLI dispatch, no connect deps

### Section H: Auth Command (Lines 1150-1298)
- **Functions**: `auth()`
- **Dependencies**: `connect.NewEventWithContext`, `connect.NewClientStrategyWithDefaults`, `connect.NewBringYourApi`, `connect.NewBlockingApiCallback`, `connect.AuthLoginWithPasswordArgs/Result`, `connect.AuthCodeLoginArgs/Result`
- **connect.* refs**: ~15 (all exist in v2026)
- **Portability**: ✅ **Fully portable** — all referenced symbols exist in v2026

### Section I: Metrics Utilities (Lines 1299-1458)
- **Functions**: `metricBytesToMiB()`, `fmtRate()`, `fmtBytes()`, `nextMidnight()`, `uDelta()`, `u64At()`, `iDelta()`
- **Dependencies**: Standard library, `runtime/metrics`
- **connect.* refs**: None
- **Portability**: ✅ **Fully portable**

### Section J: Encryption Tracking (Lines 1366-1418)
- **Functions**: `registerEncryptionManager()`, `unregisterEncryptionManager()`, `pqeTotalCounts()`
- **Dependencies**: `connect.EncryptionSessionManager`, `connect.PQECounts` (PQECounts removed)
- **connect.* refs**: 2 exists + 1 removed
- **Portability**: ⚠️ **Moderate** — `PQECounts` needs a local definition or zero-value stub

### Section K: Lifetime/Earnings Collectors (Lines 1459-1934)
- **Functions**: `runLifetimeCollector()`, `runEarningWindows()`, `sumLastN()`, `earningReason()`, `runProfitHeartbeat()`, `runBillableRateWriter()`, `writeRate()`
- **Dependencies**: `connect.ProxyHealthSnapshot()`, `connect.ProxyHealthCount()`, `connect.ContractMetricsSnapshot()`, `connect.ProxyHealthByAddress()`, `connect.GetDohFailureCount()`, `connect.ActiveConnectionCount()`, `connect.ActiveProxyConnections()`
- **connect.* refs**: ~25 (all removed, most have replacements)
- **Portability**: ⚠️ **Moderate** — heavy reliance on removed metrics; proxy_health.go replacements cover most needs. `ContractMetricsSnapshot` replaced by callback-based approach.

### Section L: Health Heartbeat (Lines 1936-2300)
- **Functions**: `runHealthHeartbeat()`
- **Dependencies**: `connect.ProxyHealthCount()`, `connect.ProxyHealthSnapshot()`, `connect.ProxyHealthHeartbeat()`, `connect.ProxyHealthByAddress()`, `connect.MessagePoolSummary()`, `connect.ProxySettings`
- **connect.* refs**: ~15 (all removed, most have replacements)
- **Portability**: ⚠️ **Moderate** — `MessagePoolSummary` needs a stub; rest covered by proxy_health.go

### Section M: JWT Refresh (Lines 2304-2550)
- **Functions**: `refreshJWT()`, `runJWTRefresher()`
- **Dependencies**: `connect.NewClientStrategyWithDefaults()`, `connect.NewBringYourApi()`, `connect.NewBlockingApiCallback()`, `connect.AuthCodeCreateArgs/Result`, `connect.AuthCodeLoginArgs/Result`
- **connect.* refs**: ~10 (mostly exist; AuthCodeCreate* needs resolution)
- **Portability**: ⚠️ **Moderate** — `AuthCodeCreateArgs/Result` need manual type definition

### Section N: Degraded Proxy Reaper (Lines 2552-2747)
- **Functions**: `classifyAuthFailureCause()`, `degradedReaperKeepCount()`, `liveContractsAcquired()`, `scoreDegradedProxies()`, `selectProxiesToReap()`, `onlyCancellableProxies()`, `liveIsDegraded()`, `reapProxies()`, `runDegradedProxyReaper()`
- **Dependencies**: `connect.DegradedProxies()`, `connect.DegradedProxyEntry`, `connect.IsDegraded()`
- **connect.* refs**: ~10 (all removed, all have replacements in proxy_health.go)
- **Portability**: ✅ **Portable** — proxy_health.go already provides `DegradedProxies`, `IsDegraded`, `DegradedProxyEntry`

### Section O: provide() — Core Provider Loop (Lines 2748-4024) ⚠️ CRITICAL SECTION
- **Functions**: `provide()` (1276 lines — largest single function)
- **Sub-sections**:
  - O.1: Memory setup & staged session (2748-2840)
  - O.2: Signal handling, control socket, context setup (2840-2920)
  - O.3: Goroutine launches — health, JWT, metrics, proxy management (2920-3030)
  - O.4: `provideWithProxy()` closure — per-proxy transport setup (3032-3636)
  - O.5: Direct connection setup (3752-3800)
  - O.6: URL proxy scheduler (3800-3860)
  - O.7: Proxy launcher loop (3860-3910)
  - O.8: Background goroutines — reload, reaper, grader, cleanup (3910-3940)
  - O.9: Profiling setup (3937-3940)
- **Dependencies**: This is the **heaviest section**:
  - 40+ `connect.*` calls (mix of exists + removed)
  - All already-ported provider files are wired here
  - `protocol.ProvideMode` (exists in v2026)
  - Heavy use of removed symbols: `ProxySettings.Index`, `RegisterProxy`, `UnregisterProxy`, `RegisterProxyBandwidth`, `ApplyAutoTuning`, `TriggerPulse`
- **Portability**: ❌ **Least portable** — requires resolving ALL removed symbols and all adapter layers

### Section P: Provider Identity & Encryption (Lines 4025-4170)
- **Functions**: `providerStatePath()`, `readProviderClientKeySeed()`, `writeProviderClientKeySeed()`, `enableProviderEncryption()`, `readProviderTlsCertAndKey()`, `writeProviderTlsCertAndKey()`
- **Dependencies**: `connect.ClientSettings`, `connect.DefaultEncryptionSettings()`, `connect.EncryptionModeOpportunistic`
- **connect.* refs**: 3 (all exist)
- **Portability**: ✅ **Fully portable**

### Section Q: Retry Delays & Provider Info (Lines 4175-4458)
- **Functions**: `proxyURLGiveUpRetryDelay()`, `proxyAuthSlowRetryDelay()`, `proxyAuthRetryDelay()`, `providerDescription()`, `logDashboardLabel()`, IP detection/resolution functions
- **Dependencies**: Minimal (standard library, OS)
- **connect.* refs**: None
- **Portability**: ✅ **Fully portable**

### Section R: provideAuth() (Lines 4522-4770)
- **Functions**: `provideAuth()`, `newProviderAuthClientArgsForRenewal()`, `renewClientJWT()`
- **Dependencies**: `connect.NewClientStrategyWithDefaults()`, `connect.NewBringYourApi()`, `connect.NewBlockingApiCallback()`, `connect.AuthNetworkClientArgs/Result`, `connect.ParseId()`, `connect.ProxyEverUp()`, `connect.ProxyAuthFailureCount()`
- **connect.* refs**: ~15 (mix; 2 removed have replacements, rest exist)
- **Portability**: ⚠️ **Moderate** — needs `ProxyEverUp`/`ProxyAuthFailureCount` from proxy_health.go

### Section S: Status HTTP & CLI Commands (Lines 4812-5650)
- **Functions**: `Status.ServeHTTP()`, `Host()`, `RequireHost()`, `RequireVersion()`, `proxyAuthAdd()`, `proxyAuthRemove()`, `expandPath()`, `proxyAdd()`, `proxyRemove()`, `proxyRemoveMatch()`, `proxyExclude()`
- **Dependencies**: Standard library, `connect.ProxySettings` (for config files)
- **connect.* refs**: `ProxySettings` only (CHANGED — no `Index`)
- **Portability**: ⚠️ **Moderate** — `ProxySettings` without `Index` may need local `Index`-carrying wrapper for config file reading

### Section T: Proxy Settings I/O (Lines 5255-5320)
- **Functions**: `readProxySettings()`, `readProxySettingsFromFile()`
- **Dependencies**: `connect.ProxySettings` (with `Index` field in fork)
- **connect.* refs**: `ProxySettings` (CHANGED)
- **Portability**: ⚠️ **Moderate** — must handle missing `Index` field

### Section U: Proxy CLI Operations (Lines 5322-6427)
- **Functions**: `readSHMLog()`, `providerLogs()`, `parseProxyAddress()`, obfuscation helpers, `resolveDuration/Int/String()`, `resolveProxyURLs()`, `proxyRefresh()`, `proxyAddSource()`, `proxyRemoveSource()`, `proxyActivity()`, `proxySummary()`, `collectRemoveDeadCandidates()`, `proxyRemoveDead()`, `formatDuration()`, `classifyHealth()`, `readProxyConfig()`, `writeProxyConfig()`
- **Dependencies**: `connect.ProxySettings` (for config), proxy management types (already in h3-provider)
- **connect.* refs**: Minimal (`ProxySettings` only)
- **Portability**: ✅ **Mostly portable** — straightforward CLI operations

---

## 4. Recommended Porting Order

### Phase 1: Zero-Risk Utility Extraction (~500 lines)
Port in **separate files** (no connect.* dependency):

| Lines | Functions | Target File |
|-------|-----------|-------------|
| 539-650 | JWT utilities | `jwt_utils.go` |
| 1299-1458 | Metrics utilities | `metrics_utils.go` |
| 4175-4458 | Retry delays, IP detection | `provider_info.go` |
| 5387-5470 | Address parsing, obfuscation, resolve helpers | `cli_helpers.go` |
| 678-712 | `atomicWriteFile()` | Already exists as `atomic_write.go` |
| 6333-6341 | `confirm()`, `formatDuration()` | `cli_helpers.go` |
| 282-530 | RAM detection, cgroup, meminfo | `memory_detect.go` |

### Phase 2: Profile & Memory Settings (~300 lines)
Port with stubs for removed symbols:

| Lines | Functions | Target File |
|-------|-----------|-------------|
| 264-468 | `applyLowmodeSettings`, `applyTurboSettings`, etc. | `provider_profiles.go` |
| 1366-1418 | Encryption tracking | `encryption_tracking.go` |

**Required stubs**: `ApplyAutoTuning` (no-op), `ResizeMessagePoolsPerClass` (no-op)

### Phase 3: Metrics & Collectors (~800 lines)
Port with proxy_health.go replacements:

| Lines | Functions | Target File |
|-------|-----------|-------------|
| 1459-1934 | `runLifetimeCollector`, `runEarningWindows`, `runProfitHeartbeat`, `runBillableRateWriter` | `metrics_collectors.go` |
| 1936-2300 | `runHealthHeartbeat` | `health_heartbeat.go` |

**Required stubs**: `MessagePoolSummary` (nil), `PQECounts` (local type), `ContractMetricsSnapshot` (from callback)

### Phase 4: Auth & JWT Refresh (~500 lines)
Port with existing API types:

| Lines | Functions | Target File |
|-------|-----------|-------------|
| 1150-1298 | `auth()` | `cmd_auth.go` |
| 2304-2550 | `refreshJWT`, `runJWTRefresher` | `jwt_refresher.go` |
| 4459-4770 | `provideAuth`, `renewClientJWT`, `newProviderAuthClientArgsForRenewal` | `provider_auth.go` |
| 4771-4810 | `watchReusedIdentityForRevocation` | `provider_auth.go` |

**Required stubs**: `AuthCodeCreateArgs`/`AuthCodeCreateResult` (local type defs)

### Phase 5: Degraded Proxy Reaper (~200 lines)
Port using proxy_health.go replacements:

| Lines | Functions | Target File |
|-------|-----------|-------------|
| 2552-2747 | Degraded proxy scoring, reaping | `degraded_reaper.go` |

### Phase 6: CLI Commands (~800 lines)
Port with minimal connect.* usage:

| Lines | Functions | Target File |
|-------|-----------|-------------|
| 4812-4860 | Status HTTP, Host/Version | `cmd_status.go` |
| 4864-5190 | `proxyAuthAdd/Remove`, `proxyAdd/Remove/Exclude` | `cmd_proxy.go` |
| 5514-5650 | `proxyRefresh`, `proxyAddSource/RemoveSource` | `cmd_proxy_source.go` |
| 5726-5950 | `proxyActivity`, `activitySnapshot` | `cmd_activity.go` |
| 5948-6070 | `proxySummary` | `cmd_summary.go` |
| 6096-6427 | `collectRemoveDeadCandidates`, `proxyRemoveDead`, config read/write | `cmd_remove_dead.go` |

### Phase 7: provide() — Core Loop (~1280 lines) ⚠️ CRITICAL
This must be ported LAST after all dependencies exist:

| Lines | Sub-section | Notes |
|-------|-------------|-------|
| 2748-2840 | Memory/session setup | Depends on Phase 1-2 |
| 2840-2920 | Signal handling, control socket | Depends on existing `control_socket.go` |
| 2920-3030 | Goroutine launches | Depends on Phase 3-5 |
| 3032-3636 | `provideWithProxy()` closure | **Most complex** — depends on ALL removed symbols |
| 3638-3940 | Direct setup, URL scheduler, launcher | Depends on proxy_health.go, hotswap.go |
| 3937-3940 | Profiling | `EnableProfiling` → stub |

### Phase 8: Cleanup
- Remove `main_stubs.go` (101 lines)
- Remove redundant stubs from `stubs_batch_c.go` and `stubs_batch_h.go`
- Delete `stubs_batch_d.go` if `DefaultConnectUrl` is defined in main.go

---

## 5. Required v2026 Stubs Summary

These 12 symbols need stubs or local definitions before main.go can compile:

| Symbol | Recommended Action |
|--------|-------------------|
| `connect.ApplyAutoTuning` | No-op stub (tuning done via profile settings) |
| `connect.ResizeMessagePoolsPerClass` | No-op stub (v2026 has `memory_budget` package) |
| `connect.RunSystemAudit` | Return `(false, false)` |
| `connect.TriggerPulse` | No-op stub |
| `connect.EnableProfiling` | Use `net/http/pprof` directly |
| `connect.ActiveProxyConnections` | Return 0 |
| `connect.MessagePoolSummary` | Return nil |
| `connect.PQECounts` | Define locally as zero-value struct |
| `connect.CritLogger` | Assign to `critlog.go`'s `critLog` |
| `connect.AuthCodeCreateArgs` | Define locally from API spec |
| `connect.AuthCodeCreateResult` | Define locally from API spec |
| `connect.ProxySettings.Index` | Adapter: local `IndexedProxySettings` wrapper or use map for index tracking |

---

## 6. Total Effort Estimate

| Category | Lines | Porting Effort |
|----------|-------|---------------|
| Already ported (provider/*.go) | ~30,000 | ✅ Done |
| Utilities (Phase 1) | ~500 | Low |
| Profiles (Phase 2) | ~300 | Low-Medium |
| Metrics (Phase 3) | ~800 | Medium |
| Auth (Phase 4) | ~500 | Medium |
| Reaper (Phase 5) | ~200 | Low |
| CLI (Phase 6) | ~800 | Low-Medium |
| Core loop (Phase 7) | ~1,280 | **High** |
| **Total remaining** | **~4,400** | |

The core `provide()` function (Phase 7) is the bottleneck. It's a single 1,280-line function with deep coupling to 40+ connect.* calls and all other provider subsystems. Recommend splitting it into sub-functions during porting rather than moving it as-is.
