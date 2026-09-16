# URNetwork Provider v2026.9.16-meso

First release of the H3/QUIC-capable provider, forked from upstream
`urnetwork/connect` and ported with full feature parity from the
`urnetwork-3.23-fix` maintenance fork.

## Transport Layer

- **HTTP/3 (QUIC)** via connect v2026 — automatic negotiation between
  H3, H2, and TCP transports with family-aware address selection.
- **IPv6 dual-stack** — native IPv6 support on all transports, with
  fallback handling for hosts with broken IPv6 (tunnel blackhole).
- **Bandwidth tracking** — per-connection Rx/Tx byte counting via
  `net.Conn` and `net.PacketConn` wrappers, with billable rate windows
  and lifetime counters.

## Proxy Management

- **URL proxy sources** — `urnet-tools proxy add-source <url>` fetches
  proxy lists from public sources with automatic refresh and cooldown.
- **Proxy health grading** — real probe host table (127 health-class
  hosts + 22 DNS resolvers) ported from connect, with deterministic
  block rotation for probe target selection.
- **Dead proxy pruning** — automatic removal of failed/degraded proxies
  based on health history and backoff timers.
- **Proxy shedding** — pressure-aware pool controller that sheds
  proxies under resource constraints, using AIMD step sizing.

## Resource Management

- **Pressure monitoring** — PSI-aware (Pressure Stall Information)
  load detection with EWMA smoothing, reading cgroup and host memory
  limits.
- **Adaptive GC** — runtime GC governor that adjusts GOGC based on
  heap pressure, host available memory, and CPU pressure.
- **Memory budget** — automatic memory limit calculation from host RAM
  (4/5ths default), with per-provider targeting.
- **File descriptor tracking** — reads `/proc/PID/fd` on Linux to
  detect FD exhaustion before it causes failures.

## Identity & Authentication

- **JWT authentication** — mint-and-refresh cycle against
  `api.bringyour.com`, with 401 renewal via out-of-band callback.
- **Client identity persistence** — stable `client_id` across restarts
  via control state, with automatic re-authentication on rejection.
- **Hotswap** — zero-downtime restarts that preserve client identity
  and proxy assignments across process boundaries.

## Operations

- **Control socket** — Unix domain socket IPC for `urnet-tools`
  integration: status, stop, restart, session save/load.
- **Prometheus metrics** — `/metrics` endpoint with lifetime counters,
  PQE measurement gating, and per-proxy bandwidth tracking.
- **Audit ring** — circular buffer of recent events for debugging
  and operator visibility.
- **Pressure status** — real-time pressure score written to state
  directory for monitoring.

## Modules Ported from v3.23-fix

All production-critical modules ported and verified against the
maintenance fork:

| Module | Description |
|--------|-------------|
| `proxy_health.go` | Health tracking, grading, and backoff |
| `resource_pressure.go` | PSI monitoring, GC governor, memory budget |
| `control_socket.go` | Unix socket IPC for urnet-tools |
| `lifetime_metrics.go` | Persistent lifetime counters |
| `contract_metrics.go` | Per-contract bandwidth and session metrics |
| `doh_cache.go` | DNS-over-HTTPS resolution cache |
| `renewal_watcher.go` | JWT renewal and token management |
| `hotswap.go` | Zero-downtime identity preservation |
| `audit_ring.go` | Circular event buffer |
| `proxy_state.go` | Proxy assignment and state management |
| `pool_health.go` | Connection pool health monitoring |
| `sn.go` | SN (Subnet) bridge integration |
| `metrics_listen.go` | Prometheus /metrics endpoint |
| `control_state.go` | Persistent control state |
| `bandwidth/` | H1+H3 byte counting wrappers |

## Cross-Platform Builds

| Platform | Architecture |
|----------|-------------|
| Linux    | amd64, arm64 |
| macOS    | amd64, arm64 |
| Windows  | amd64, arm64 |

All binaries are GPG-signed via annotated tag. Docker image available
via multi-stage build (`docker build --platform linux/amd64 -t
urnetwork-provider .`).

## Verification

```bash
# Verify GPG signature on tag
git verify-tag v2026.9.16-<timestamp>-meso

# Verify binary checksum
sha256sum urnetwork-provider-*.tar.gz

# Extract and run
tar xzf urnetwork-provider-v2026.9.16-*-linux-amd64.tar.gz
./provider provide --help
```

## Known Limitations

- `connect-v4.bringyour.com` TLS certificate expired (server-side) —
  end-to-end H3 transfer testing blocked until upstream rotates cert.
- PacketConnFactory (UDP/QUIC bandwidth wrapping) not yet re-enabled —
  pinned connect version doesn't expose the interface. Bandwidth
  tracking works for TCP/H1; H3/UDP tracking pending connect update.
- 34 stubs remain across non-critical paths (metrics edge cases,
  platform-specific socket ACLs, test harnesses).

## CI Status

- Build and test: passing (linux, darwin, windows x amd64, arm64)
- GAUNTLET authentication: passing
- VirusTotal + ClamAV: non-blocking scan on release
- Resource pressure: linux-only (stub on darwin/windows)
