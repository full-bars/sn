# H3/QUIC Fleet Rollout Strategy

**Project:** URNetwork provider migration to connect v2026 (H3/QUIC + IPv6 dual-stack)
**Fleet:** ~120 provider nodes on Tailscale
**Date:** 2026-09-16

---

## 0. Executive Summary

This document covers the phased rollout of the H3/QUIC-capable provider binary
across the URNetwork provider fleet. The migration replaces the WebSocket-only
connect transport with connect v2026, which adds HTTP/3 over QUIC, native IPv6
dual-stack, and a rewritten bandwidth wrapper that counts both TCP streams and
UDP/QUIC packets. The fleet is orchestrated over Tailscale, managed via
`urnet-tools`, and updated through systemd units.

**Current status:** Ten provider modules ported and tested locally. Four modules
(sn.go, proxy_state, pool_health, metrics) remain in progress. The server-side
connect-v4 endpoint (`connect-v4.bringyour.com`) has an expired TLS cert — this
is the critical blocker for live H3 traffic.

---

## 1. Migration Status

### 1.1 Ported Modules (Ready for Canary)

| Module | File(s) | What Changed | Risk |
|--------|---------|-------------|------|
| Bandwidth wrapper | `bandwidth/wrap.go`, `bandwidth/tracker.go` | Wraps both `DialContext` (TCP) and `PacketConnFactory` (UDP/QUIC) for byte counting | Medium — new UDP path |
| Proxy health | `proxy_health.go` | Adapted for `bandwidth.ProxyBandwidth.Snapshot()` API | Low |
| Resource pressure | `resource_pressure.go` | Unchanged core; reads PSI/cgroup metrics | Low |
| Control socket | `control_socket.go` | Ported to `provider` package namespace | Low |
| Lifetime metrics | `lifetime_metrics.go` | Persisted across restarts, delta-guarded | Low |
| Contract metrics | `contract_metrics.go` | Acquired/denied/utilization tracking | Low |
| DOH cache | `doh_cache.go` | Adapted for v2026 connect API (`cache.Warm` signature change) | Medium |
| Renewal watcher | `renewal_watcher.go` | JWT renewal with serialization mutex (stampede prevention) | Low |
| Audit ring | `audit_ring.go` | Circular buffer for security-critical log events | Low |
| Hotswap | `hotswap.go` + platform files | Zero-downtime binary replacement via IPC socketpair | High — process lifecycle |

### 1.2 Modules Still In Progress

| Module | Blocker / Notes |
|--------|----------------|
| `sn.go` | Subnet protocol state machine — requires connect v2026 `protocol` package changes |
| `proxy_state.go` | Proxy state persistence — serialization format may differ with H3 fields |
| `pool_health.go` | Pool health aggregation — depends on proxy_state |
| `metrics` | Prometheus exposition — needs H3-specific counters (QUIC streams, 0-RTT, migration) |

### 1.3 Server-Side Blockers

| Blocker | Impact | ETA |
|---------|--------|-----|
| `connect-v4.bringyour.com` TLS cert expired | H3 endpoint unreachable; providers fall back to WSS | Must be renewed before canary |
| IPv6 AAAA records for connect endpoint | Required for true dual-stack H3 | Depends on cert renewal |

---

## 2. Phased Rollout Plan

### Phase 0: Pre-flight (Week 1)

**Goal:** Validate the binary builds, runs, and connects without breaking anything.

1. **Build verification**
   - `go build -o provider-h3 ./provider/` against local connect v2026 replace
   - Cross-compile for all fleet targets: `linux/amd64`, `linux/arm64`
   - Verify binary size delta (H3/QUIC adds ~8-12MB from quic-go + pion/webrtc)

2. **Unit test sweep**
   - Run full test suite: `go test ./provider/... -race -count=1`
   - Key test files to watch: `hotswap_test.go`, `proxy_health_test.go`,
     `renewal_watcher_test.go`, `bandwidth/wrap_test.go`

3. **Local integration smoke test**
   - Run provider against the existing `wss://connect.bringyour.com` (WSS)
   - Verify proxy warmup completes (paceMonitor reaches warmup-done)
   - Verify `[health]`, `[profit]`, `[earn]` heartbeat lines appear
   - Verify hotswap: `urnet-tools hot-swap` on the running local provider

4. **Cert renewal gate**
   - **No canary deploys until `connect-v4.bringyour.com` cert is renewed.**
   - Fallback: providers using the v2026 binary can still connect via WSS to
     `connect.bringyour.com` — the QUIC path is negotiated, not forced.

**Exit criteria:** Binary builds clean, all tests pass, hotswap handshake verified
locally, cert renewal confirmed (or fallback WSS path confirmed working).

---

### Phase 1: Canary (Week 2-3)

**Goal:** Deploy to 2-3 nodes. Validate runtime stability, earnings continuity,
and hotswap reliability under real traffic.

#### 1a. Canary node selection

Pick 3 nodes that maximize coverage of the fleet's heterogeneity:

| Node Profile | Why | Example |
|-------------|-----|---------|
| **High-proxy box** (40+ proxies, turbo-v8 profile) | Stress-tests the bandwidth wrapper, QUIC packet counting, and memory pressure under load | Primary earning node |
| **Low-mem box** (<512MB RAM, eco/lowmem profile) | Validates GOMEMLIMIT, pool auto-sizing, and pressure system on constrained hardware | Edge/ARM node |
| **Medium box** (default profile, 15-25 proxies) | Baseline — what 80% of the fleet looks like | Typical VPS provider |

#### 1b. Deploy method

```bash
# On each canary node, via Tailscale SSH or urnet-tools:
# 1. Backup current binary
cp /usr/local/bin/provider /usr/local/bin/provider.pre-h3

# 2. Stage new binary
cp provider-h3 /usr/local/bin/provider
chmod +x /usr/local/bin/provider

# 3. Trigger hotswap (zero-downtime)
urnet-tools hot-swap
```

**Hotswap is the primary deploy mechanism.** The hotswap subsystem:
- Parent spawns candidate child with `URNETWORK_HOTSWAP=1`
- Child completes preflight (JWT validation, proxy state read, socket bind)
- Child sends `READY` → parent sends `TAKEOVER` → child sends `ACK`
- Parent drains in-flight streams for 30s, then exits
- If hotswap fails, the parent is untouched (no downtime)

If hotswap is disabled or fails, fall back to a full restart:
```bash
systemctl restart urnetwork-provider
```

#### 1c. Monitoring window (7 days minimum)

Monitor these signals daily:

| Signal | Tool / Source | Pass Condition |
|--------|--------------|----------------|
| **Process alive** | `systemctl status urnetwork-provider` | No crash-restart loops |
| **Proxy count** | `urnet-tools proxy summary` → `up` count | Stable or growing (match pre-deploy) |
| **Earnings** | `[profit] earning=yes` log lines | Billable traffic moving (no `reason=no_proxies` or `no_traffic`) |
| **Memory** | `[health] heap=... sys=...` | No sustained growth trend; stays under GOMEMLIMIT |
| **Hotswap** | `urnet-tools hot-swap` + verify via `[health][build]` line | New version in build tag, no downtime >5s |
| **QUIC negotiation** | Connect logs (if H3 endpoint is live) | `h3` or `quic` appearing in transport negotiation |
| **Contract health** | `[profit] contracts=N denied=N avg_util=N%` | denied count not spiking vs baseline |
| **Billable rate** | `cat ~/.urnetwork/health/billable_rate` | Non-zero during business hours |
| **Lifetime metrics** | `cat ~/.urnetwork/lifetime_metrics.json` | Monotonic growth, no reset |

#### 1d. Canary exit criteria

- **7 days** with zero crash loops, earnings within ±20% of pre-deploy baseline
- Memory stable (no sustained growth >10% above pre-deploy)
- Hotswap at least once mid-canary to verify process replacement
- No Tailscale connectivity disruption (Tailscale health check)

---

### Phase 2: Staging Wave (Week 4-5)

**Goal:** Deploy to 10-15 nodes (~10% of fleet). Validate fleet-scale hotswap
and catch edge cases across diverse hardware.

#### 2a. Wave composition

Select nodes to cover:

| Category | Count | Purpose |
|----------|-------|---------|
| All ARM64 nodes | All of them | Cross-arch validation |
| All nodes with `eco` or `lowmem` profile | All of them | Constrained-memory validation |
| 3-5 random `turbo-v4`/`turbo-v8` nodes | 3-5 | High-throughput validation |
| 2-3 nodes with the most proxies | 2-3 | Max concurrency stress |

#### 2b. Deploy cadence

Deploy in **sub-waves of 3 nodes**, 24 hours apart:

- **Wave 2a** (Day 1): 3 nodes
- **Wave 2b** (Day 2): 3 more nodes
- **Wave 2c** (Day 3): 3 more nodes
- **Wave 2d** (Day 5): Remaining staging nodes (if 2a-2c clean)

**Stop rule:** If any node in a sub-wave shows a new crash loop, earnings
drop >50%, or memory leak, **halt the next sub-wave** until root-caused.

#### 2c. Additional staging checks

| Check | Method | Pass |
|-------|--------|------|
| **Docker provider variant** | Test on a Docker-deployed node | Containerized hotswap works |
| **Slow-disk auto-RAM-logs** | Verify on a box with slow storage | `[ramlogs]` redirect fires correctly |
| **Control socket** | `urnet-tools status`, `urnet-tools set gomemlimit ...` | All control socket commands work |
| **Session staging** | `urnet-tools session load` + restart | `.session-pending` → staged apply works |
| **Root PATH sanitization** | Deploy as root | No exec hijack; PATH filtered correctly |
| **Concurrent hotswap** | Hotswap 2 staging nodes simultaneously | No Tailscale/coordination conflict |

---

### Phase 3: Full Fleet Rollout (Week 6-8)

**Goal:** Deploy to all ~120 nodes.

#### 3a. Fleet segmentation

| Segment | Size | Deploy Method | Timeline |
|---------|------|---------------|----------|
| **Segment A** — Already on staging binary | ~15 | Already done | Week 6 |
| **Segment B** — Standard nodes (default profile, amd64) | ~70 | Hotswap via urnet-tools fleet command | Week 6-7 |
| **Segment C** — ARM64 / non-standard | ~20 | Hotswap (if ARM binary verified in staging) | Week 7 |
| **Segment D** — Docker / containerized | ~15 | Container image rebuild + rollout | Week 7-8 |

#### 3b. Fleet deploy procedure

For Segment B, use the fleet-wide hotswap:

```bash
# On the orchestration box (Tailscale-admin node):
for node in $(tailscale status --json | jq -r '.Peer[] | select(.HostName | startswith("provider-")) | .HostName'); do
    echo "=== $node ==="
    tailscale ssh "$node" -- "urnet-tools hot-swap /path/to/provider-h3"
    sleep 30  # stagger to avoid thundering herd on connect endpoint
done
```

**Stagger 30s between nodes** to avoid all providers reconnecting to the
connect endpoint simultaneously (the `backoffPacer` in main.go handles
per-proxy stagger, but endpoint-level load also matters).

#### 3c. Fleet exit criteria

- **All 120 nodes** on the H3 binary, confirmed by `urnet-tools version` grep
- **Fleet-wide earnings** within ±10% of pre-migration weekly total
- **Zero** unrecovered crash loops
- **Hotswap verified** on at least 50% of nodes (mid-deploy or post-deploy)

---

## 3. Hotswap vs. Restart Decision Matrix

| Scenario | Method | Why |
|----------|--------|-----|
| **Normal upgrade** (no breaking API changes) | **Hotswap** | Zero downtime; in-flight streams drain gracefully |
| **Breaking change in control socket protocol** | **Restart** | Old parent and new child speak different IPC; hotswap handshake would fail |
| **Changing `URNETWORK_PROFILE` or memory limits** | **Restart** | Profiles are applied at init; hotswap inherits parent's runtime settings |
| **Security patch** (urgent, no time for hotswap soak) | **Restart** | Faster; trade uptime for speed of deployment |
| **Provider with 0 proxies** (idle or warmup) | **Either** | No in-flight traffic to drain; restart is simpler |
| **Provider with 50+ proxies under load** | **Hotswap** | Must preserve active sessions; 30s drain window |
| **First canary deploy** | **Hotswap** | If hotswap fails → parent is still running (safe rollback) |
| **Docker PID 1 mode** | **Hotswap (CANARY_DONE)** | Special IPC path: child validates then exits, parent execve replaces itself |
| **connect v2026 library update** (major dependency bump) | **Restart** | New binary may have different memory layout; fresh start is cleaner |
| **go.mod changes** (dependency version bump only) | **Hotswap** | Binary-compatible; hotswap is sufficient |

### Hotswap prerequisites check

Before triggering hotswap, verify:

```bash
# 1. Provider is running and healthy
urnet-tools status

# 2. Hotswap is enabled (check control state)
cat ~/.urnetwork/control_state.json | jq '.hot_restart'

# 3. New binary exists and is executable
ls -la /usr/local/bin/provider-h3

# 4. No active session staging in progress
ls ~/.urnetwork/.session-pending 2>/dev/null && echo "BLOCKED: session staging pending"
```

---

## 4. Rollback Procedures

### 4.1 Immediate rollback (< 5 minutes)

If a deployed node shows problems after hotswap:

```bash
# Option A: Restart with the old binary
cp /usr/local/bin/provider.pre-h3 /usr/local/bin/provider
systemctl restart urnetwork-provider

# Option B: If old binary not available, use urnet-tools
# to revert to the last-known-good version
urnet-tools update <previous-version-tag>
```

**Hotswap rollback is naturally safe:** if the hotswap handshake fails or the
child crashes during preflight, the parent process continues running unchanged.
No explicit rollback action is needed — the system self-heals.

### 4.2 Fleet rollback

If a systemic issue is discovered after 10+ nodes are deployed:

```bash
# 1. Halt further deployment
#    (stop the rollout script, kill any in-progress sub-waves)

# 2. On affected nodes, push old binary via Tailscale SSH
for node in $(cat /tmp/h3-affected-nodes.txt); do
    tailscale ssh "$node" -- "
        cp /usr/local/bin/provider.pre-h3 /usr/local/bin/provider &&
        systemctl restart urnetwork-provider
    "
done

# 3. Verify fleet health
for node in $(tailscale status --json | jq -r '.Peer[].HostName'); do
    tailscale ssh "$node" -- "urnet-tools version" 2>/dev/null
done | sort | uniq -c
```

### 4.3 Rollback triggers

| Signal | Threshold | Action |
|--------|-----------|--------|
| Crash loop (restarts > 3 in 10 min) | Automatic | `systemd` reverts; alert operator |
| Earnings drop | >50% sustained for 30 min | Rollback affected node |
| Memory leak | Heap growth >50% over 2 hours with no proxy churn | Rollback, file issue |
| Tailscale disconnect | Provider unreachable via Tailscale for >5 min | Check Tailscale, then rollback if binary-related |
| Hotswap deadlock | TAKEOVER sent but ACK not received in 60s | Parent auto-aborts hotswap; check logs |
| New error class in logs | Any `FATAL` or `SHMLOG_FATAL` not seen on previous version | Rollback, investigate |

---

## 5. Monitoring Checklist

### 5.1 Per-node health (run during each rollout phase)

```
□ Process alive: systemctl status urnetwork-provider → active (running)
□ Version correct: urnet-tools version → matches expected H3 build
□ Build tag visible: grep '\[health\]\[build\]' /dev/shm/urnetwork.log | tail -1
□ Proxy count stable: urnet-tools proxy summary → up count within ±10% of baseline
□ Earning: grep '\[profit\] earning=yes' /dev/shm/urnetwork.log | tail -5
□ Billable rate non-zero: cat ~/.urnetwork/health/billable_rate
□ Memory under limit: grep '\[health\]' /dev/shm/urnetwork.log | tail -1 → heap < GOMEMLIMIT
□ No new error classes: grep -c 'FATAL\|SHMLOG_FATAL' /dev/shm/urnetwork.log
□ Contract metrics: grep '\[profit\] earning' /dev/shm/urnetwork.log → denied not spiking
□ Lifetime metrics: cat ~/.urnetwork/lifetime_metrics.json → monotonic
□ Control socket responsive: urnet-tools status → returns without timeout
□ Tailscale health: tailscale status → all peers healthy
```

### 5.2 Fleet-level dashboard (for phases 2-3)

| Metric | Source | Alert Threshold |
|--------|--------|----------------|
| Fleet online count | `tailscale status \| grep provider \| wc -l` | < 110 of 120 |
| Fleet total billable rate | Aggregate `billable_rate` files across nodes | >30% drop from baseline |
| Crash-loop count | `systemctl list-units --failed \| grep urnetwork` | > 0 |
| Hotswap success rate | `urnet-tools hot-swap` exit code + build line change | < 95% |
| Memory outliers | Heap > 90% of GOMEMLIMIT on any node | Any node |
| Connect endpoint latency | `[health]` line transport handshake times | >2x baseline |
| QUIC vs WSS ratio | Transport negotiation logs (once H3 endpoint is live) | N/A until H3 endpoint |

### 5.3 Post-rollout soak (2 weeks after full fleet)

- [ ] All 120 nodes on H3 binary for 14 days
- [ ] Zero unrecovered crash loops
- [ ] Fleet earnings stable (±10% weekly)
- [ ] Memory profiles stable (no drift)
- [ ] Hotswap exercised on ≥50% of fleet (maintenance updates)
- [ ] H3 endpoint traffic observed (once cert renewed)
- [ ] IPv6 dual-stack verified on nodes with AAAA records

---

## 6. Risk Mitigation

### 6.1 QUIC/UDP port considerations

**Risk:** QUIC uses UDP port 443 (or an ephemeral port). Some providers may have
firewalls or NAT that block UDP egress.

**Mitigation:**
- H3 negotiation is HTTP/2 fallback: if QUIC is blocked, the provider falls
  back to WebSocket over TCP automatically (this is connect v2026's design)
- No firewall changes required for canary/staging phases (WSS-only)
- Document required UDP egress rules for Phase 3 fleet-wide if H3 is enabled

### 6.2 IPv6 readiness

**Risk:** Some fleet nodes may not have IPv6 connectivity or may have broken
IPv6 routes (see `warp-ipv6-broken-route-fix` in the skill catalog).

**Mitigation:**
- connect v2026 dual-stack prefers IPv4 when IPv6 is unavailable
- IPv6 is an enhancement, not a requirement — WSS over IPv4 is the safe path
- Post-deploy audit: check each node's IPv6 capability via
  `tailscale netcheck` or `curl -6 https://api.bringyour.com`

### 6.3 Memory increase from H3/QUIC

**Risk:** quic-go and pion/webrtc add ~8-12MB binary size and additional heap
pressure from QUIC connection state, TLS session tickets, and 0-RTT data.

**Mitigation:**
- `ensureMemoryLimit()` in main.go bounds heap to 80% of RAM (or RAM - 1GiB)
- `eco` and `lowmem` profiles further constrain memory usage
- Canary phase specifically includes a low-memory node to catch this early
- The `pool_auto_size` mechanism scales message pools to RAM/32

### 6.4 Hotswap with H3 connections

**Risk:** In-flight QUIC connections during hotswap may not drain cleanly.
QUIC's 0-RTT data and connection migration are stateful at the transport layer.

**Mitigation:**
- `HotSwapDrainTimeout` is 30 seconds — sufficient for most QUIC idle timeout
  renegotiation (connect v2026 uses 30s default idle timeout)
- QUIC connection state is per-process; the retiring parent's connections will
  timeout and clients will reconnect to the new process
- For the canary phase, test hotswap specifically while QUIC connections are
  active (generate traffic, then hotswap, verify client reconnection)

### 6.5 Tailscale coordination during fleet-wide deploy

**Risk:** Deploying to 120 nodes simultaneously could saturate Tailscale
control plane or cause DNS churn.

**Mitigation:**
- Stagger 30s between node deploys (Phase 3 procedure)
- `backoffPacer` in main.go already staggers per-proxy connect attempts
  with ±50% jitter to avoid thundering herd
- Tailscale itself handles connection migration gracefully

### 6.6 Server cert expiry (BLOCKER)

**Risk:** `connect-v4.bringyour.com` TLS cert has expired. H3 negotiation
requires a valid TLS certificate on the server.

**Mitigation:**
- **This blocks only the H3 endpoint, not the WSS path.** Providers using
  connect v2026 can still connect to `wss://connect.bringyour.com` (WSS)
- Renew the cert BEFORE deploying H3 canary (so H3 traffic is possible)
- If cert renewal is delayed, deploy the H3 binary using WSS fallback and
  enable H3 traffic later via connect URL update
- Monitor: `openssl s_client -connect connect-v4.bringyour.com:443 </dev/null 2>/dev/null | openssl x509 -noout -dates`

### 6.7 Rollback cost asymmetry

**Risk:** If H3 deployment causes fleet-wide issues, reverting 120 nodes takes
time even with Tailscale SSH.

**Mitigation:**
- Phase 1 canary catches 80% of issues before fleet deployment
- Phase 2 staging catches edge cases (ARM64, Docker, low-mem)
- Old binary is always kept at `provider.pre-h3` on each node
- Fleet rollback script is pre-written and tested (Section 4.2)
- The hotswap mechanism itself is the safest rollback: if the new binary
  is bad, the hotswap handshake fails and the old process continues

---

## 7. Timeline Summary

| Week | Phase | Scope | Key Gate |
|------|-------|-------|----------|
| 1 | Pre-flight | Build, test, local smoke | Binary builds + tests pass |
| 2-3 | Canary | 2-3 nodes, 7-day soak | Earnings stable, no crashes |
| 4-5 | Staging | 10-15 nodes, sub-waves | All profiles validated |
| 6-8 | Full fleet | All ~120 nodes | Fleet earnings ±10% |
| 8-10 | Post-rollout soak | 14-day observation | No regressions |
| TBD | H3 endpoint | Server cert renewed, H3 traffic enabled | QUIC connections observed |

**Critical path dependency:** Server cert renewal for `connect-v4.bringyour.com`
must happen before H3 traffic can flow. The binary rollout can proceed on WSS
fallback while waiting for cert renewal.

---

## 8. Appendix: Provider Lifecycle Reference

The provider process lifecycle (from `main.go`) relevant to rollout:

```
main()
  → seedEnvFromControlState()    // reads persisted settings from control socket state
  → initGlog()                   // RAM log redirect if profile=lowmem/eco
  → RunStartupAudit()            // disk speed, memory detection
  → docopt.ParseArgs()
  → provide(opts)
      → hotRestartEnabled()      // check if JWT reuse across restarts is allowed
      → applyStagedSession()     // apply .session-pending if present
      → applyTurboSettings()     // profile-specific buffer/window sizing
      → applyPoolAutoSize()      // scale message pool to RAM/32
      → applyEcoSettings()       // eco profile memory limits
      → ensureMemoryLimit()      // final GOMEMLIMIT guarantee
      → connect.Client(...)      // connect to server (WSS or H3)
      → provideWithProxy(...)    // per-proxy goroutine
          → backoffPacer()       // stagger proxy starts with jitter
          → provideWithProxy()   // actual tunnel establishment
      → paceMonitor()            // warmup progress logging
      → runHealthHeartbeat()     // periodic [health] line
      → runEarningWindows()      // [earn] line with rolling windows
      → runProfitHeartbeat()     // 15s [profit] heartbeat
      → runLifetimeCollector()   // persistent all-time metrics
      → runBillableRateWriter()  // write billable_rate for urnet-tools
```

The hotswap subsystem (`hotswap.go`) operates as a parent-child IPC:
```
Parent → spawn child with URNETWORK_HOTSWAP=1
  → Child preflight (JWT, proxy state, socket bind)
  → Child READY → Parent TAKEOVER → Child ACK
  → Parent drains 30s → Parent exits
  → Child becomes new parent
```

If child preflight fails or times out (20s), parent logs error and continues.
If TAKEOVER ACK not received (60s), parent aborts hotswap and continues.
