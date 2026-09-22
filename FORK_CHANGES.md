# URNetwork H3/QUIC Fork — Custom Changes

This document tracks all modifications made to the upstream URNetwork `sn` (connect v2026) codebase in this fork. Use this as a reference when rebasing to newer upstream versions.

**Fork Based On**: urfoundation/sn (connect v2026 — H3/QUIC transport + IPv6 dual-stack)
**Repository**: github.com/full-bars/sn
**Binary Versioning**: v2026.<month>.<day>-<epoch>-meso

---

## Porting Status

This fork is a fresh repo forked from `urfoundation/sn`, with the fork feature set carried over from the mature `full-bars/urnetwork-3.23-fix` fork. All ported modules were adapted to the v2026 connect API (H3/QUIC, IPv6-first, `go mod` manifest system).

**Notable v2026 API differences handled during porting**:
- `golang.org/x/net/context` is a deprecated alias — use `context` from the stdlib
- `syscall.Umask` split into platform-specific files (`umask_unix.go` / `umask_windows.go`) for Windows cross-compile
- `NewPlatformTransport` (with custom settings) replaces `NewPlatformTransportWithDefaults` so UDP bandwidth federation can be injected
- Connect v2026 ships no `internal/` directory — `internal/urnettools` (urnet-tools CLI) is fork-owned

---

## 1. Proxy Health Tracking & Management

**Files**: `provider/proxy_health.go`, `provider/proxy_health_log.go`, `provider/proxy_warmth.go`, `provider/proxy_slow_retry.go`

**Changes**:
- `RegisterProxy` / `UnregisterProxy` implemented locally (not in connect library) with generation guards
- Generation-guarded `UnregisterProxySafe` in all production goroutines — a stale deferred unregister can no longer nuke a freshly registered entry with the same address
- `RecordProxyAuthFailure` (local `proxy_health.go`) tracks per-proxy auth failure counters
- `classifyProxyHealth` helper ensures consistent dead/degraded/up thresholds across `ProxyHealthSnapshot`, `ProxyHealthByAddress`, and heartbeat paths
- `globalProxySlowRetryState` uses `atomic.Pointer` for thread safety

**Status**: ✅ Shipped

---

## 2. Proxy Hot-Reload, URL Sources & Probing

**Files**: `provider/proxy_reload.go`, `provider/proxy_url.go`, `provider/proxy_url_source.go`, `provider/proxy_probe.go`, `provider/proxy_table_probe.go`, `provider/probe_targets.go`

**Changes**:
- Hot-reload engine via `.reload` trigger files; stable monotonic `proxy[N]` slot IDs (`proxy_id.go`)
- URL proxy sources: `--proxy_url` / `PROXY_URL`, interval refresh, `--proxy_url_max` cap, `--proxy_dead_cleanup_scope=url|all|none`
- `urnet-tools proxy add-source` / `remove-source` runtime source management (atomic add + immediate fetch)
- Dual-stage SOCKS5 probe (greeting + CONNECT to the API endpoint), background URL reaper, persistent blacklist with 24h prune
- Slow-retry give-up backoff (15m→24h escalating, +jitter) and permanent eviction after 4 give-up cycles
- `SampleProbeTargets` + `ProbeHostCount` exported as wrappers for test seams

**Status**: ✅ Shipped

---

## 3. Bandwidth Tracking — TCP + UDP/QUIC Federation

**Files**: `provider/bandwidth/tracker.go`, `provider/bandwidth/wrap.go`, `provider/provide.go`

**Changes**:
- Both TCP and UDP/QUIC data-plane paths feed the same `ProxyBandwidth` counters
- TCP: `WrapConnectSettings` wraps `ConnectSettings` so the provider's own dials still route through the proxy (the v2026 `DialContextSettings` path bypasses the proxy buffer settings, so it is wrapped explicitly)
- UDP/QUIC: the platform transport is built with `NewPlatformTransport` + a custom `H3PacketConnFactory` closure that captures the per-proxy `ProxyBandwidth` and counts every H3 QUIC datagram
- `InitialContractTransferByteCount` defaults to 1 MiB (contract cap parity, user-approved)

**Status**: ✅ Shipped

---

## 4. Control Socket & Hotswap

**Files**: `provider/control_socket.go`, `provider/control_state.go`, `provider/hotswap*.go`, `internal/urnettools/hotswap.go`

**Changes**:
- Control socket created with `syscall.Umask(0o177)` before `net.Listen` for 0600 permissions from the start (`umask_unix.go` / `umask_windows.go`)
- Hotswap (zero-downtime restart) trigger get/set implemented; v2026 release versions accepted by the version check
- Hotswap socketpair platform split (`hotswap_socketpair_linux.go` / `_other.go`) for cross-platform builds

**Status**: ✅ Shipped

---

## 5. Metrics, Observability & Telemetry

**Files**: `provider/metrics_listen.go`, `provider/metrics_provider.go`, `provider/metrics_collectors.go`, `provider/lifetime_metrics.go`, `provider/contract_metrics.go`, `provider/health_heartbeat.go`, `provider/startup_banner.go`, `provider/earn_tracker.go`

**Changes**:
- Prometheus exposition endpoint (`metrics_provider.go`)
- Metrics listener with Tailscale auto-discovery (`metrics_listen.go`)
- Lifetime + contract metrics collectors
- Health heartbeat with uptime/heap/sys/connections
- Startup banner with phased startup + clean "Ready" summary line
- Per-minute earning windows (`earn_tracker.go`)

**Status**: ✅ Shipped

---

## 6. Security Hardening

**Files**: `provider/ssrf_guard.go`, `provider/proxy_url_source.go`, `provider/client_jwt_store.go`, `provider/renewal_watcher.go`

**Changes**:
- SSRF guards extended: 100.64.0.0/10 (Tailscale CGNAT), 198.18.0.0/15 (benchmarking), 64:ff9b::/96 (NAT64) added to blocked ranges
- `sanitizeURLForDisplay` masks query params (API keys) in proxy source URLs before logging — paid proxy services authenticate via URL query params, raw URLs must never reach logs
- `globalClientJWTStore` race fixed with `sync.RWMutex` + accessor functions (`loadGlobalClientJWTStore` / `storeGlobalClientJWTStore`)
- `atomicWriteJSON` uses `os.CreateTemp` with random suffix (matches `atomicWriteFile` pattern)

**Status**: ✅ Shipped

---

## 7. JWT, Auth & Renewal

**Files**: `provider/provider_auth.go`, `provider/jwt_refresher.go`, `provider/jwt_utils.go`, `provider/client_jwt_store.go`, `provider/renewal_watcher.go`

**Changes**:
- Proactive JWT refresh (7-day primary, 48h-expiry fallback, startup jitter)
- Renewal watcher with testable deadlines; `setRenewalTestHome` sets `HOME` env for CI determinism
- Exit code 78 on invalid/expired JWT (startup scripts intercept to re-authenticate)

**Status**: ✅ Shipped

---

## 8. Resource Pressure & Memory Management

**Files**: `provider/resource_pressure.go`, `provider/pool_health.go`, `provider/degraded_reaper.go`

**Changes**:
- FD-pressure + memory-pressure monitoring built into the provider (matches 3.23-fix behavior, single-file layout for the v2026 tree)
- Message pool resizing, degraded-state reaper

**Status**: ✅ Shipped

---

## 9. DoH Cache & DNS

**Files**: `provider/doh_cache.go` (removed), `provider/net_http_doh.go`

**Changes**:
- The persistent DNS-over-HTTPS cache with server-score persistence was ported, then REMOVED (commit `01b8e5f5`): a live-fleet probe (ATL/ATL2/honk) showed the cache did nothing even on the 3.23 line — no `.doh_scores` written, zero to one log lines — because the v2026 connect lookup path no longer calls into it. The DoH resolver itself (`net_http_doh.go`) remains.
- Uses system cert pool (not LE-only pinning) so all four DoH providers work

**Status**: ⚠️ Partially shipped — resolver active, server-score cache intentionally removed; do not re-add without new evidence the connect layer reads it.

---

## 10. Docker & Deployment

**Files**: `Dockerfile`, `.dockerignore`, `docker/scripts/*`, `pelican/egg-urnetwork-h3.json`, `.github/workflows/build.yml`, `.github/workflows/release.yml`

**Changes**:
- Alpine base, multi-stage build, single build produces all three binaries: provider, urnet-tools, urnet-docker
- `third_party/` copied before `go mod download` (npipe local-replace target must exist at manifest download time)
- `.dockerignore` excludes real output names (`provider_bin`, `urnet_tools_bin`, `urnet_docker_bin`) — a previous `/provider` pattern silently excluded the `provider/` source dir from the build context
- Multi-arch build (amd64/arm64 via QEMU+buildx) pushes to **both** `ghcr.io/full-bars/sn` and `3cape/sn` (DockerHub) on tag and main pushes
- `:latest` tag gated to non-prerelease tags; a CI assertion fails if a `-rc`/`-alpha`/`-beta` release ever points `:latest`
- Pelican egg (`pelican/egg-urnetwork-h3.json`) for panel deployments: `BUILD=stable|nightly|jwt` modes, admin-only credentials, `PELICAN=yes` disables runtime self-update (image is single source of truth)
- Velero-style lint: shellcheck, pelican JSON/var-contract validation, docker update-source guard (update fetches must target `full-bars/*` only), pelican smoke + gate tests, protocol version assertions

**Status**: ✅ Shipped

---

## 11. Test Determinism

**Files**: `provider/test_helpers_test.go`, `provider/control_state_test.go`, `provider/control_socket_test.go`, `provider/proxy_admission_gate_test.go`, `provider/proxy_health_test.go`, `provider/bandwidth/wrap_test.go`

**Changes**:
- All tests deterministic (`-count=1` verified, 3/3 deterministic runs)
- `withTempHome` resets all process-global caches: `lastReloadTriggerTime`, `probeConfigCache`, `admissionStateCache`, `globalPerProxyEarnTracker`, `controlState`, `USERPROFILE` env
- `writeReloadTriggerDebounce = 0` in `TestMain` prevents flaky debounce behavior
- `wrap_test.go` binds a local listener instead of dialing an unroutable destination (never stalls)

**Status**: ✅ Shipped

---

## 12. CI Pipeline & Installers

**Purpose**: Restore the full CI surface and installer set that the fresh fork was missing. Seven workflows were disabled stubs claiming their scripts did not exist; the scripts had simply not been ported yet.

**Files**: `scripts/Provider_Install_Mac.sh`, `scripts/Provider_Install_Win32.ps1`, `scripts/Provider_Install_Deps.sh`, `scripts/Provider_Uninstall_Linux.sh`, `scripts/Provider_Uninstall_Win32.ps1`, `scripts/install-urnet-docker.sh`, `.github/scripts/*` (shakedown.sh, docker-shakedown.sh, cfaa_sync.sh, stage-wdsi.py, compact_commit_diff.py, create_pr.sh, write_sync_info.sh), `.github/workflows/{dash-compat,unix-lifecycle,windows-lifecycle,cfaa-blocklist-sync,tool-functional-smoke,functional-soak,docker-multi-container,shakedown,docker-shakedown}.yml`, `cmd/fake-provider/`

**Changes**:
- All six installers download release assets exclusively from the official `github.com/full-bars/sn` release. No custom mirror is used anywhere in download paths (help-text contact info unchanged).
- Release pipeline: universal tarball bundles `Provider_Install_Linux.sh` as `urnet-tools`; per-platform tarballs carry their own installer; `urnetwork-monitoring-<ver>.tar.gz` ships the Prometheus/Grafana stack; release body reads `releases/<tag>.md` with auto-generated fallback; VirusTotal + ClamAV run in a parallel non-blocking scan job that posts the verdict and stages Defender submissions.
- Shakedown workflows trigger on `v2026.*` tags (this repo's scheme), not `v3.23.0-fix.*`.
- `cmd/fake-provider/` — 123-line CI test double that listens on the control socket; lets the Windows lifecycle workflow exercise start/stop/restart/logs without a real provider binary.
- Tool smoke CI builds `./cmd/provider/` (the new repo layout).

**Status**: ✅ Shipped

---

## 13. Docker Container Discovery for Renamed Image

**Purpose**: The Go tooling identifies provider containers by image/name substrings (`urnetwork`, `urnet`, `meso`, `miner`). The old image `ghcr.io/full-bars/urnetwork-3.23-fix` matched via `urnetwork`; the new `ghcr.io/full-bars/sn` matched none, so `urnet-tools` could not discover its own containers — breaking daemonized features (status, logs, hotswap, proxy commands) inside Docker.

**Files**: `internal/urnettools/docker.go`

**Change**: `isDockerCandidate` now also matches `full-bars/sn` in the image or container name.

**Status**: ✅ Shipped

---

## Porting Checklist for Future Upstream Versions

- Verify `syscall.Umask` call sites survive platform splits (`umask_unix.go` / `umask_windows.go`)
- Re-check `H3PacketConnFactory` / `WrapConnectSettings` against connect API changes (bandwidth wiring)
- Confirm `RegisterProxy` / `UnregisterProxy` / `RecordProxyAuthFailure` still exist locally (they are NOT in the connect library)
- Check `InitialContractTransferByteCount` (fork default 1 MiB)
- Verify Docker build context: `.dockerignore` must never exclude `provider/` — only real binary output names
- Run `go test -short -race -timeout 600s ./provider/...` and confirm zero ignored tests

---

**Last Updated**: 2026-09-17
**Maintained By**: @full-bars

---

## 14. Audit Ring Survives HotSwap, Lifecycle Audit Trail, Sliding Status (PR #21)

- **Audit ring persists across hotswap (PR #21)**: the old process flushes its audit ring at the handoff commit point, before the takeover message, on every handover path (systemd, Windows, Docker). The successor merges `audit.json` into its live ring after takeover with timezone-safe deduplication (timestamps compared semantically, not by `time.Time` pointer equality). The parent quiesces its control socket at the commit point, so a command accepted after the flush falls back to the pending-overrides queue instead of vanishing in the parent's memory. The merge persist is serialized with the periodic persist gate, and this fork's pre-existing persist mutex covers the same contract.
- **Lifecycle events audited (PR #21)**: `urnet-tools history` now records process start (version + boot/hotswap source), the hotswap handoff, and control-socket shutdown, in addition to `set`/`clear`. The shutdown record is written after this fork's `shutdownFn` guard.
- **Sliding severity scale for provider status (PR #21)**: `active` >= 90%, `partial` 70-89%, `degraded` 50-69%, `critical` < 50% (including zero), exact percentage always rendered and clamped at 100.
- **Start entries persist immediately on normal boots (PR #21)**: only a spawned hotswap successor defers its start entry until the takeover merge; the Docker execve successor (whose ring loads after the parent's pre-exec flush) and normal boots write to disk at once.

## 15. Cross-Platform Compile Gate on every PR (PR #28)

- **PR CI now cross-compiles all shippable binaries** (PR #28): the provider, `urnet-tools`, and `urnet-docker` build for Linux, macOS, and Windows across the amd64 and arm64 architectures as a merge requirement. A change that breaks a non-Linux platform is caught at PR time instead of surfacing only at release time.
