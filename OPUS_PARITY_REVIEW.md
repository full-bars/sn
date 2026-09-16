# Opus Parity Review: h3-provider vs urnetwork-3.23-fix provider

Date: 2026-09-16
Reviewer: Claude Opus 5

| | Path | Revision |
|---|---|---|
| OLD (production) | `urnetwork-3.23-fix/provider/` | working tree as of 2026-09-14 |
| NEW (migrated) | `h3-provider/provider/` | `8ef8ac93` |
| Connect (as built) | `full-bars/connect@065bcdd` (go.mod `replace`) | matches `h3-workspace/connect` HEAD `065bcdd` |

All line references are to those revisions. OLD `main.go` = `urnetwork-3.23-fix/provider/main.go`.

---

## 0. Verdict

**Not at parity. Do not deploy to fleet.** The migration kept every function *name*, but
the behaviour of the core `provide` path has regressed. Six of the findings each break
production by themselves:

| # | Finding | Effect |
|---|---|---|
| C1 | `provide()` exits after 30 s | Provider process `os.Exit(0)`s 30 seconds after startup |
| C2 | Bandwidth wrapper replaces the proxy dialer | **Every "proxy" connects directly from the host IP.** Proxies are not used at all |
| C3 | Control socket closed at end of setup | `urnet-tools` live control dead from second 0 |
| C4 | Proxy health state never updated | Every proxy reads "connecting" then "dead"; pacing, reaper, revocation, hub and cleanup all act on false data |
| C5 | HotSwap IPC closed before ACK | Every hotswap aborts |
| C6 | Reloaded proxies all get index 0 | Bandwidth, contracts and health for every hot-added/URL proxy are credited to `direct` |

C1 hides most of the others in any quick smoke test: the process dies before the
65-minute health windows, the reaper and the cleanup ever run.

Method: an AST pass over both trees mapped every non-test function (OLD 648, NEW 781).
Only `main` (renamed `Main`) and `backoffPacer` (unused) are missing by name. So parity
has to be judged on function bodies. Everything moved out of OLD `main.go` was diffed
with comments stripped. The ~20 shared files that differ by more than the package line
were diffed the same way, and the connect call sites were checked against the pinned
module.

Build status: `GOOS=linux go build ./provider ./cmd/provider` OK, `go vet` clean.
`GOOS=windows` and `GOOS=darwin` **do not compile** (H6). `go test ./provider` prints
`PASS` and then `FAIL` because a test closes the testlog descriptor.

---

## 1. CRITICAL

### C1. `provide()` exits 30 seconds after startup
- NEW `provider/provide.go:77-92`
- OLD `main.go:3989` (`wg.Wait()`, blocks for the process lifetime)

```go
provideLauncherLoop(st)   // non-blocking: launches goroutines, returns
provideStatusServer(st)   // non-blocking
done := make(chan struct{})
go func() { st.wg.Wait(); close(done) }()
select {
case <-done:
case <-time.After(30 * time.Second):   // starts counting at startup, not at shutdown
    tlog("[provider] timed out waiting for goroutines to exit\n")
}
... closeAllCaches(st); os.Exit(0)
```
The 30 s timer starts as soon as startup finishes, not after `ctx` is cancelled. Thirty
seconds after "Ready" the provider logs "timed out waiting for goroutines", runs
shutdown and exits with code **0**. systemd `Restart=on-failure` treats that as a clean
stop and does not restart it. With `Restart=always` it becomes a 30-second restart loop
that hammers the auth API.

**Why:** a bug from splitting `provide()` into helpers. The intent was probably a bounded
drain after cancel.
**Fix:** `<-st.ctx.Done()` first, then the bounded `wg.Wait()`.

### C2. The bandwidth wrapper bypasses the SOCKS5 proxy for all traffic
- NEW `provider/provide.go:341-344`, `provider/bandwidth/wrap.go:24-54`
- connect `net.go:104-116` (pinned module)
- OLD: bandwidth passed into `NewLocalUserNat`/`NewRemoteUserNatProvider` (`main.go:3607-3616`) and into connect's `trackedConn` (connect `net.go:436-440`)

connect v2026 `ConnectSettings.DialContext`:
```go
if self.DialContextSettings != nil {
    dialContext = self.DialContextSettings.DialContext        // used INSTEAD of proxy
} else {
    if self.ProxySettings != nil { dialContext = self.ProxySettings.NewDialContext(...) }
```
`DefaultConnectSettings()` leaves `DialContextSettings` nil. `WrapDialContextSettings(nil, ...)`
therefore wraps a plain `&net.Dialer{}` and assigns it. From then on, every connection
built from `clientStrategySettings` ignores `ProxySettings`:
- API auth / client JWT minting (`provideAuth`), so all N proxies' auth comes from one host IP
- the platform websocket transport (`NewPlatformTransportWithDefaults`)
- user egress, because `provide.go:368-369` copies the same `ConnectSettings` into `localUserNatSettings.Tcp/UdpBufferSettings`

Effect: the provider runs N identities that all egress from the host IP. The paid proxy
fleet carries no traffic, and the host IP is exposed on every client flow. This arrived
in `6fb2cdd7` today ("wire bandwidth tracking ... critical billing path").

Even once the dialer is fixed, the accounting has different meaning:
- OLD `BillableRx/Tx` counted IP packets through the user NAT (connect `ip.go:3036,3123`) and `Clients` came from NAT sessions (`ip.go:928-1793`).
- NEW counts every TCP byte on every dialed connection, including API calls and platform control. That is an overcount.
- UDP/QUIC is not counted (`wrap.go:45-49`, PacketConnFactory TODO).
- `Clients` is never incremented anywhere (no `Clients.Add` or `AddSession` callers in non-test code). So `activeConnectionCount()`/`activeProxyConnections()` (`stubs_batch_h.go:17`, `stubs_batch_m.go:62`) always return 0.

**Fix:** capture the proxy dialer inside the wrapper (`ProxySettings.NewDialContext(ctx, NetDialer())`) when `ProxySettings != nil`. Separately, decide what "billable" means in v2026 and find a hook near the user NAT instead of the dialer.

### C3. The control socket is closed as soon as setup returns
- NEW `provider/provide.go:226-245` (inside `provideSetupSignals`)
- OLD `main.go:2893-2911` (inside `provide`, runs at process exit)

```go
defer func() { if st.cleanupControlSocket != nil { st.cleanupControlSocket() } }()
unregSocketCloser := RegisterCoordinatorCloser(...)
defer unregSocketCloser()
```
These defers were moved into a helper that returns at the end of startup. The socket is
closed and unlinked immediately, and the coordinator closer is unregistered. Every
`urnet-tools set/clear/get/shutdown/hotswap/metrics` call falls back to
`pending_overrides.json` and is not applied until restart.
`globalControlState.shutdownFn` still exists, but nothing can reach it.

Same defect class in the same function:
- `defer flushRetentionEvents()` (`provide.go:210`) now runs at startup. It **closes `retentionEventCh`**, so all later retention events are dropped (OLD `main.go:2865` runs at exit). `closeAllCaches` never calls it at exit either (OLD `main.go:3999`).

**Fix:** return cleanups from the helpers and defer them in `provide()`. Better: store them on `provideState` and call them from `closeAllCaches`.

### C4. Proxy health state is never updated (up/down/auth-failure)
- NEW `provider/proxy_health.go:158-248` (`MarkProxyUp`, `markProxyUp`, `MarkProxyDown`, `RecordProxyAuthFailure`, `RecordProxyTransportDrop`): **zero non-test callers**
- OLD: called from connect `transport.go:753,866,872-873,1344,1409`

The fork's connect transport stamped health transitions. v2026 connect has no hook for
this, and the port did not replace it. Every registered proxy stays `connecting` until
`connectingStaleAfter` (65 min, `proxy_health.go:69`). After that it is `dead`, because
`everUp` is never set. Downstream effects:

| Consumer | NEW location | Effect |
|---|---|---|
| `paceMonitor` warmup | `stubs_provide.go:115` | `up` always 0, so with ≥5 proxies warmup only ends through the 60-min force. URL-sourced proxies are held back for an hour (`proxy_reload.go:648`) |
| URL cleanup (default scope `url`, every 6 h) | `proxy_url_source.go:1289` | after 65 min uptime **every URL proxy is `dead` and removed** from the URL cache |
| `urnet-tools proxy remove-dead` | `cmd_remove_dead.go:53` | offers to delete every proxy |
| degraded reaper | `degraded_reaper.go:156` | never sees degradation (fails safe) |
| revoked-identity watcher | `provider_auth.go:455-459` | `ProxyEverUp` false and auth-fail count 0, so it never evicts a revoked client_id |
| renewal watcher auth-fail trigger | `renewal_watcher.go:360-386` | never fires |
| `[health]` heartbeat, hub reporter, `proxy_health.log`, systemd STATUS | `health_heartbeat.go:111` etc. | report 0 up for the whole fleet |

**Fix:** derive up/down from something v2026 does expose. Candidates: platform transport
connect/disconnect callbacks, `ContractManager` status callbacks (already used by
`registerContractCallback`), or a periodic probe of `RouteManager` transports. This is the
largest missing piece of connect integration and needs a design decision.

### C5. HotSwap IPC is closed before the candidate's ACK
- NEW `provider/provide.go:138` `defer st.hotSwapIPC.Close()` inside `provideSetupMemory`
- OLD `main.go:2797` (deferred in `provide`, runs at exit)

The candidate finishes the handshake (READY, then receives TAKEOVER) and returns from
`provideSetupMemory`, and the IPC handle closes. The parent's ACK wait
(`hotswap.go:205-220`) sees EOF, calls `session.Kill()`, and logs "Aborting handoff; live
provider retained". Later, `runHotSwapChildAck` (`provide.go:612`) writes to a closed
file and the `_ =` hides the error.

Effect: **every hotswap fails**. It fails safely, since the parent keeps serving, but
zero-downtime upgrades cannot happen.

### C6. Proxies spawned by the reloader get proxy index 0
- NEW `provider/proxy_reload.go:652-655`: `stableID := resolveProxyID(...); _ = stableID`
- OLD `proxy_reload.go` `settings.Index = stableID`

v2026 `ProxySettings` has no `Index` field. The port replaced it with the
`proxyIndexByAddr` sync.Map (`provide.go:29-40`), but only the startup loop calls
`setProxyIndex` (`provide.go:891`). Any proxy added by `reload()` (file edits,
`proxy add`, URL fetch merges, relaunch after give-up/backoff, trim re-adds) resolves
`getProxyIndex(addr) == 0` in `provideWithProxy`. The consequences:
- `RegisterProxyBandwidth(0)` returns the **direct** connection's counters, so bytes are misattributed. `registerContractCallback(0, ...)` does the same for contract metrics.
- The renewal watcher, revocation watcher and every `proxy[%d]` log line report `proxy[0]`.
- `registerProxyV2026(stableID, ...)` registers health under the real ID, but the bandwidth goes to a different entry, so per-proxy earnings and grading see 0 bytes.

**Fix:** `setProxyIndex(settings.Address, stableID)` before spawning, in `proxy_reload.go` at both the add path and the direct re-enable path. Also consider passing the index to `spawnProxy` explicitly.

---

## 2. HIGH

### H1. 17 Prometheus metric families removed; Grafana dashboard broken
- NEW `metrics_stubs.go:69-75` (`prometheusHandlerStub` serves only `providerExtraMetrics()`)
- OLD `connect.PrometheusHandler()` (connect `metrics_prometheus.go`) wrote the base families, then appended provider extras

Present in OLD, absent in NEW:
`urnet_uptime_seconds`, `urnet_goroutines`, `urnet_mem_heap_bytes`, `urnet_mem_sys_bytes`,
`urnet_gc_cycles_total`, `urnet_bytes_total`, `urnet_billable_bytes_total`,
`urnet_clients_active`, `urnet_connections_active`, `urnet_contracts_total`,
`urnet_errors_total`, `urnet_pool_latency_ms`, `urnet_proxy_pool_size{status}`,
`urnet_proxy_bytes_total{proxy,direction}`, `urnet_proxy_billable_bytes_total{proxy,direction}`,
`urnet_proxy_clients{proxy}`, `urnet_proxy_session_age_seconds{proxy}`.

`monitoring/grafana/dashboards/urnetwork-providers.json` queries 8 of them (billable, bytes,
clients_active, contracts, errors, goroutines, proxy_billable, uptime/pool_size).
The comment at `metrics_stubs.go:62-67` says `providerExtraMetrics` "absorbed the whole
payload". That is not true: `metrics_provider.go:234` mentions two of them only in a
comment.

The families that survive and keep the same labels are the lifetime, earnings, hotswap,
grades, health distribution, DoH and URL-grade metrics.

New: `urnet_pqe_supported 0` replaces the `urnet_sessions_*` PQE families (intentional, see M1).

### H2. URL stage-1 table probe and paid grader cannot grade anything
- NEW `stubs_batch_b.go:14-24`: `probeHostCount()` returns a hard-coded 200, and `sampleProbeTargets` returns `"probe.invalid"` × n
- OLD `connect.SampleProbeTargets` / `ProbeHostCount` (connect `ip_probe_targets_api.go:21`, real host table)

Every probe target fails DNS, so `res.Total == 0` and every pass is undecidable
(`proxy_table_probe.go:447-480`). The A-F tiering, the quality admission bar
(`PassBar` 0.6, default `Enabled: true`) and `runPaidProxyGrader` all become
non-functional. Each pass also wastes DNS lookups on `probe.invalid`. The comment
admits "real implementation requires the probe table".

**Fix:** port `ip_probe_targets.go` (host and resolver tables plus the disjoint-block sampler) into the provider package. It is pure data and code with no transport dependency.

### H3. Hub `node_id` is empty when `URNETWORK_NODE_NAME` is unset
- NEW `provide.go:296-305`:
```go
watcherName := st.nodeName
if watcherName == "" {
    watcherName, err := os.Hostname()   // := shadows the outer variable
    ...
}
```
- OLD `main.go:2986-2992` (`watcherName, _ = os.Hostname()`, plain assignment)

The outer `watcherName` stays `""`, and it is passed as `nodeID` to
`runBandwidthReporter`/`runHeartbeatReporter` (`bandwidth_reporter.go:255,376,442`).
Every node without an explicit name reports `node_id=""`. On the hub they collide into a
single node. (`Host` recovers through `resolveNodeName`, but `NodeID` does not.)

### H4. `seedEnvFromControlState()` is never called
- OLD `main.go:185` (package `init()`, before `initGlog()`)
- NEW: defined at `startup_env_seed.go:40`, no callers. The new `init()` (`hotswap.go:796`) only installs a default `hotSwapTrigger`.

A `profile` or `ramlogs` value persisted with `urnet-tools set` is not visible to
`Main()`'s audit and ramlog decision (`cmd_main.go:27-60`). Operators who switched to RAM
logs or the auto profile through the control socket silently lose that setting at the
next start.

### H5. Renewal-on-401 path is dead
- NEW `renewal_watcher.go:56-160` adds `RenewalOOB`, which counts 401s only inside its own `SendControl`
- NEW `provide.go:564-565,655`: connect's `NewClient` gets the raw `oob`, and only the watcher gets the `renewalOOB` wrapper

connect never calls `RenewalOOB.SendControl`, so `audit401Count` stays 0 and `on401` never
fires. The OLD fork had `SetOn401` inside connect's `ApiOutOfBandControl`, so contract/audit
401s triggered an immediate renewal. Now renewal happens only on the 12 h-before-expiry
schedule. A server-side early expiry or rotation leaves the proxy returning 401 until
the next hourly check. With C4 in place, the auth-failure-count trigger is dead too.

**Fix:** pass `renewalOOB` to `connect.NewClient` if it satisfies the OOB interface. Otherwise accept schedule-only renewal and remove the dead wrapper.

### H6. Non-Linux builds fail
- `hotswap.go:792` declares `func restoreStdioBeforeExec() {}` with no build tag, which collides with `shmlog_fallback.go:20` (`//go:build !linux`). Windows and darwin fail.
- `resource_pressure.go:295-296` uses `syscall.Rlimit/Getrlimit/RLIMIT_NOFILE` with no build tag. OLD split this into `read_fd_frac_unix.go` / `read_fd_frac_windows.go`, which are not ported.

Verified with `GOOS=windows go build -gcflags=-e ./provider/`.

### H7. On Linux, ramlog stdio is not restored before in-place exec (Docker hotswap)
- NEW `hotswap.go:792` untagged no-op; the Linux `restoreStdioBeforeExec` was **deleted** from `shmlog_linux.go`
- OLD `shmlog_linux.go:62` restored the original stdout/stderr descriptors

`hotswap.go:678` calls this before `syscall.Exec`. With ramlogs on, the exec'd process
inherits the pipe as stdout/stderr, but the reader goroutine is gone after exec. When
the pipe buffer fills (64 KiB), every log write blocks and the provider hangs. This only
matters once C5 is fixed, and only on the Docker/PID-1 path.

**Fix:** move the no-op into `shmlog_fallback.go` only and restore the Linux body.

---

## 3. MEDIUM

### M1. PQE session accounting removed (intentional, but visible)
`metrics_stubs.go:106` returns `Measured:false`, and the `[pqe]` line and
`urnet_sessions_*` are suppressed (OLD `main.go:1390` summed `EncryptionSessionManager.PQECounts()`).
v2026 connect has no tracker. This is a genuine upstream removal, handled honestly.
Note that `encryptionManagers` (`encryption_tracking.go`) is still maintained but has no
reader: dead bookkeeping.

### M2. Shutdown and cancel classification differences in `provideHandleAuthFailure`
- NEW `provide.go:712`: `errors.Is(err, context.Canceled)`
- OLD `main.go:3363`: `errors.Is(err, context.Canceled) || proxyCtx.Err() != nil`

`proxyCtx` is no longer passed in. A cancellation that surfaces as a wrapped non-Canceled
error (for example an admission-gate or rate-limiter error returned on cancel) is now
counted as a give-up. `RecordGiveUp` can then **evict a healthy URL proxy** after trim,
reap or reload cycles, which is exactly what the OLD comment warns about.

### M3. `directStartupDone` not wired to the reloader
- NEW `provide.go:753-780` (no done channel) and `:958` `directDone: nil`
- OLD `main.go:3763-3776, 3907`

OLD notes that without it, a `direct off` before the first hot-toggle "would unregister
proxy[0] out from under a still-running direct transport". That race is back.

### M4. DoH cache is closed at startup and never used by connect
- NEW `provide.go:879-880` `defer closeDohCache()` inside `provideLauncherLoop`, so the cache is closed and `SetSharedDohCache(nil)` runs as soon as launch finishes. At exit there are no final saved scores, since `closeAllCaches` does not call it (OLD `main.go:3998`).
- NEW `doh_cache.go:36-49`: `SharedDohCache()` has **no readers**. OLD called `connect.SetSharedDohCache` so connect's resolver used the cache. In NEW, the cache warms and then does nothing.
- NEW `doh_cache.go:62` `IncrDohFailure()` has **no callers**, so `urnet_doh_failures`/`[health]` DoH failures are always 0. Commit `8ef8ac93` says "doh failure counting" was fixed; the counter exists but nothing increments it.

### M5. `refreshJWT` bypasses the connect client strategy
- NEW `jwt_refresher.go:42+`: step 1 (`/auth/code-create`) uses a bare `http.Client{Timeout:30s}` with `Authorization: Bearer <account JWT>`
- OLD `main.go:2304`: `connect.NewBringYourApi(...).AuthCodeCreate` through `ClientStrategy`

This loses connect's pinned TLS roots, extender and resilient fallbacks, and the
`--api_url`-aware strategy. On networks where direct API access is blocked,
account-JWT refresh now fails while steps 2-3 still use the strategy. It also sends the
long-lived account JWT with Go's default transport and proxy environment.

### M6. Stubs that log success and do nothing
| Stub | NEW | OLD behaviour | Visible symptom |
|---|---|---|---|
| `EnableProfiling` | `stubs_provide.go:36` | pprof + `/metrics/pool` + `/metrics/errors` on `URNETWORK_PPROF` | logs "[profile] enabling diagnostics", nothing listens |
| `TriggerPulse` | `stubs_provide.go:32` | hourly wake of stalled transports | `[hourly-maintenance] reconnecting stalled transports` logged, no-op |
| `ApplyAutoTuning` | `stubs_provide.go:26` | auto buffer sizing from system resources | none |
| `ResizeMessagePoolsPerClass` | `stubs_phase15.go:11` | `--max-memory` / 8 pool cap | `--max-memory` only sets GOMEMLIMIT |
| `messagePoolSummary` / `[health][pool]` | `health_heartbeat.go` (`_ = newPoolHealthWindow`) | pool leak detector line | line gone |
| `contractMetricsSnapshot` utilSum | `stubs_batch_m.go:39` | contract byte utilization | `avg_util=0%` |
| `RetentionEventCallback` | `provider_profiles.go:62` (removed from `applyTurboSettings`, OLD `main.go:353`) | retention telemetry to `proxy_health.log` | no retention events (also C3) |

Each is individually defensible where v2026 truly lacks the hook, but the log lines should
not claim the action happened.

### M7. Default `hotSwapTrigger` installed at package init
- NEW `hotswap.go:796-800` sets `hotSwapTrigger` with `context.Background()` and a **no-op cancel**

In OLD it was nil until `provide()` wired it. In a HotSwap candidate, which does not
overwrite it until ACK, or in any code path that reaches the socket before `provide()`
sets it, a handoff would drain with a cancel that never stops the parent. So both
processes would stay live. Low reachability today because of C3/C5, but it is a trap once
those are fixed.

### M8. `cancelSourceVal` data race
NEW `provide.go:196-201, 254-259`. The shutdown goroutine reads `st.cancelSourceVal`
without synchronization after `ctx.Done()`. If the context was cancelled by a signal and
`st.cancel` is called concurrently afterwards (for example from `closeAllCaches` paths
or the control `shutdown`), the `Once` write races the read. OLD used `atomic.Value`
(`main.go:2842`).

---

## 4. LOW / cosmetic

- **Log formats changed.** Emoji prefixes were stripped from core lines (`✅ Ready`, `♻️ client_id ... (reused)` became `[reuse] client_id:`, `🔥 [startup]`, `💰 [startup]`, `🔑 [jwt]`), and messages were shortened (`earnings ranking`, slow-retry drop, auth give-up text). No in-repo scripts, hub or workers grep these (checked), but external log alerts might.
- **Logger routing.** `controlLog`, `dohLog`, `auditLog`, `renewalLog` and `hotswapLog` are copies of `tlog`, which is harmless. `metricsLog`/`metricsProviderLog` (`metrics_listen.go`, `metrics_stubs.go:15`) write to **stderr** with a different timestamp format (`2006-01-02T15:04:05.000`), so `[metrics]` lines no longer line up with the rest.
- **Removed startup lines.** The `[turbo] profile=turbo-v4/v8 window=... resendQueue=...` line (OLD `main.go:3648-3657`) and the `| logs: urnet-tools logs` hint on Ready (`:3882-3885`) are gone.
- **New:** JWT file permission warning (`provide.go:166`), a small improvement.
- **Dead code.** `atomicBool` in `stubs_batch_d.go:13` is non-atomic and unused. `unregisterProxyStub(interface{})`, `registerProxyV2026` and `proxyBandwidthByAddressV2026` are pass-through indirections worth inlining. `backoffPacer` was dropped (unused in OLD too). `initMetricsStubs` has no callers.
- `startProxyBenchmarks(proxyCtx, nil, ...)` (`provide.go:682`) passes a nil bandwidth record, so latency probes (when `URNETWORK_PROXY_BENCHMARK=true`) cannot record `LatencyNs`. OLD passed `bw`.

---

## 5. Checklist results

### 1. `provide()`: startup sequence, goroutines, signals
| Step | OLD | NEW | Status |
|---|---|---|---|
| api/connect URL resolve | 2753-2763 | 98-110 | ✅ same (`resolveApiUrl` moved to `sn.go:167`, with an added empty `ApiUrl` guard) |
| `--max-memory`, pool resize, auto-size | 2765-2777 | 112-125 | ⚠ pool resize no-op (M6) |
| `applyStagedSession` (non-candidate) | 2784 | 129 | ✅ |
| HotSwap candidate handshake | 2793-2802 | 134-144 | ❌ IPC closed early (C5) |
| Identity banner, critLog STARTUP | 2804-2814 | 146-159 | ✅ |
| JWT expiry log | 2817-2835 | 162-183 | ✅ plus perm warning |
| Event, SIGINT/SIGQUIT/SIGTERM | 2837-2838 | 189-190 | ✅ |
| cancel-source capture | 2842-2849 | 196-201 | ⚠ race (M8) |
| SIGUSR2 listener + hotSwapTrigger | 2853-2861 | 203-208 | ✅ |
| flushRetentionEvents at exit | 2865 | 210 | ❌ runs at startup (C3) |
| load control state, merge pending, runtime tuning, errors, audit | 2873-2888 | 212-223 | ✅ |
| control socket + coordinator closer | 2892-2912 | 226-245 | ❌ closed at startup (C3) |
| sd_notify READY + STATUS | 2929-2934 | 247-252 | ✅ |
| shutdown-reason goroutine | 2941-2949 | 254-262 | ✅ |
| `--wallet` subnet set | 2956-2962 | 268-274 | ✅ |
| hourly pulse | 2966-2981 | 277-292 | ⚠ TriggerPulse no-op (M6) |
| watcherName | 2986-2992 | 296-305 | ❌ shadowing (H3) |
| 8 background loops (health, bw reporter, heartbeat, JWT refresher, earning windows, lifetime, profit, billable rate) | 2994-3005 | 307-318 | ✅ all launched |
| paceMonitor | 3022 | 320 | ⚠ launched; fed no data (C4) |
| turbo-v4/v8 log | 3648-3657 | — | missing (LOW) |
| proxy.state, ID counter, source select, URL merge, earnings load, prioritize | 3661-3750 | 793-868 | ✅ |
| direct proxy[0] | 3758-3798 | 753-780 | ⚠ no done channel (M3) |
| write proxy.state, DoH cache, slow-retry state, configured count | 3801-3819 | 874-883 | ❌ DoH closed at startup (M4) |
| per-proxy register + stagger launch | 3822-3880 | 886-934 | ✅ (index via sync.Map) |
| Ready line | 3882-3894 | 936-944 | ✅ minus logs hint |
| ProxyReloader + immediate reload | 3898-3915 | 947-962 | ❌ index lost (C6), directDone nil (M3) |
| 10 maintenance loops (URL fetcher, URL reaper, paid grader, grade summary, blacklist prune, URL cleanup, pressure, pool controller, degraded reaper, reconciler) | 3917-3933 | 973-984 | ✅ all launched; grader/probe hollow (H2) |
| pprof | 3935-3940 | 987-992 | ❌ stub (M6) |
| URNETWORK_METRICS listener (skip candidate) | 3947-3954 | 994-1001 | ✅ served; content reduced (H1) |
| status server `--port` | 3955-3985 | 1005-1034 | ✅ |
| block until shutdown | 3989 `wg.Wait()` | 77-87 | ❌ 30 s exit (C1) |
| exit flush: DoH, retention, errors, audit, metrics, clean marker, socket | 3998-4013 | 1040-1052 | ⚠ DoH + retention missing at exit |

### 2. Proxy lifecycle
- **Registration:** startup ✅; reload ❌ (C6).
- **Auth retry loop:** code-identical except the removed Index field → ✅. Proven/unproven leash, restart-storm guard, admission gate, slow-retry semaphore, 14-day drop, 24h daily gate and URL give-up/evict/backoff are all present.
- **Health checks:** ❌ state never updated (C4). The table probe is hollow (H2).
- **Hot-swap:** ❌ (C5, H7, M7).
- **Removal:** reaper and cleanup logic are ported unchanged but act on false health (C4). URL cleanup would mass-remove after 65 min.
- **Cancellation classification:** ⚠ (M2).

### 3. Auth flow
- `provideAuth` (234 lines), `watchReusedIdentityForRevocation`, `renewClientJWT`, `runJWTRefresher`, `validateJWTExpiry`, the client JWT store and locks: ported, body-identical apart from comments.
- `refreshJWT`: rewritten transport (M5).
- In-process renewal: schedule path ✅; 401-triggered path ❌ (H5); revocation detection ❌ (C4).
- `ErrTokenInvalid` exits with code 78; "Jwt does not exist" retries every 30 s: ✅.
- **All auth traffic egresses directly, not through the proxy (C2).**

### 4. Bandwidth / earnings / grading
- Tracking: ❌ wrong layer, bypasses the proxy, no UDP, no client counts, reloaded proxies credited to index 0 (C2, C6).
- Reporting: `runBandwidthReporter`/`runHeartbeatReporter` are unchanged but get an empty nodeID (H3) and zero health (C4).
- `runEarningWindows`, `runLifetimeCollector`, `runProfitHeartbeat`, `runBillableRateWriter`, the earnings store and `earn_tracker`: logic identical, fed by the counters above.
- Grading: tier/summary/report code identical. Input probe table is stubbed (H2).

### 5. Control socket
Command set, key allowlist (`gomemlimit`, `gogc`, `metrics`, `hot_restart`, `node_name`, …), validation, persistence, rollback, peer-cred checks, accept backoff and `shutdown`/`hotswap`/`metrics` handlers are all **code-identical**. The only changes are the logger swap, `metricsMu` locking (a good fix: removes a race on `metricsServer`), and `RegisterCoordinatorCloser`/`hotRestartEnabled`/`resolveNodeName`/`logDashboardLabel` moving into this file.
**But the socket is closed at startup (C3)**, so in practice none of it is reachable in the live provider.

### 6. Metrics
- Endpoint: same address handling (`URNETWORK_METRICS`, Tailscale auto-bind `metrics_listen.go`, handoff after takeover). ✅
- Content: 17 families missing (H1). PQE families replaced by `urnet_pqe_supported` (M1). DoH failures always 0 (M4). Labels on surviving families are unchanged.

### 7. Direct connection
`provideDirectSetup` launches proxy[0] with the same `isDirectEnabled()` precedence (toggle file, then `DISABLE_DIRECT_IP`), cancel-map key and stale-unregister guard. It is missing the startup done channel (M3), and with C6, reloaded proxies overwrite its bandwidth counters. `cmdDirect` changed from `docopt.Opts` to `map[string]interface{}` with identical behaviour.

### 8. URL-sourced proxies
- Scheduler: `runProxyURLFetcher` is identical, and cache merge at startup matches.
- Warmup deferral: gated on `proxyWarmupDone`, which only flips at the 60-min force (C4).
- Admission: the SOCKS5 reachability gate works; quality scoring is hollow (H2).
- Failover and give-up: eviction and backoff are identical (M2 aside).
- Cleanup: mass-removal risk after 65 min (C4).
- Sources: `proxy add-source/remove-source/refresh` CLI identical (`cmd_proxy_source.go`).
- Blacklist prune and reaper: identical.

### 9. Functions in OLD not in NEW
By name, only `main` (now exported `Main`, invoked from `cmd/provider/main.go`) and
`backoffPacer` (unused). Files `read_fd_frac_unix.go` and `read_fd_frac_windows.go` are not
ported (H6). Behaviour lost despite a same-named function:

| Function | Nature of loss |
|---|---|
| `init()` | `seedEnvFromControlState` + `initGlog` dropped (H4) |
| `pqeTotalCounts` | returns zero struct (M1) |
| `restoreStdioBeforeExec` (linux) | body deleted (H7) |
| `sampleProbeTargets` / `probeHostCount` | fake data (H2) |
| `EnableProfiling`, `TriggerPulse`, `ApplyAutoTuning`, `ResizeMessagePoolsPerClass` | no-ops (M6) |
| `prometheusHandlerStub` (was `connect.PrometheusHandler`) | base metrics gone (H1) |
| `markProxyUp/Down`, `RecordProxyAuthFailure`, `RecordProxyTransportDrop` | present, never called (C4) |
| `SharedDohCache`, `IncrDohFailure` | present, never read/called (M4) |

Ported unchanged (spot-verified with comment-stripped diffs): all CLI commands
(`auth`, `proxy add/remove/remove-match/exclude/refresh/add-source/remove-source/activity/summary/remove-dead`,
`logs`, `print-network-id`, `network`, `direct`, `wallet`), `sanitizeRootPath`, the
turbo/eco/lowmode profiles, memory limit helpers, identity key and TLS persistence,
`providerDescription`, public IP detection, the degraded reaper scoring, and the docopt
usage string (whitespace-only diff).

---

## 6. Suggested fix order

1. **C1**: block on `ctx.Done()` before the bounded drain. One line; nothing else can be observed until this is fixed.
2. **C3, C5, M4**: move every `defer` out of the setup helpers. One mechanical change fixes the control socket, hotswap IPC, retention flush and DoH lifetime.
3. **C2**: route the wrapper through `ProxySettings.NewDialContext`. Add a test asserting a proxied `ClientStrategy` dial reaches the SOCKS5 listener.
4. **C6**: `setProxyIndex` in the reload paths.
5. **H3, H4, H6, H7**: small, mechanical.
6. **C4**: design decision. Pick the v2026 signal that means "proxy transport up". Until this lands, consider disabling URL cleanup (C4 table) to avoid mass eviction.
7. **H1, H2, H5**: port the probe table and base Prometheus families; decide on 401 renewal.
8. **M-tier**: fix, or make the stub log lines honest.

Regression tests worth adding before the next gauntlet: provider stays up for longer
than 60 s with zero proxies; control socket reachable after startup; hotswap
candidate/parent round-trip over a socketpair; reloaded proxy gets its stable index;
proxied dial goes through a local SOCKS5 fixture.
