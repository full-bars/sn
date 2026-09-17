# Project Structure

This document outlines the architecture and directory structure of the **URNetwork H3/QUIC fork** (`full-bars/sn`). It covers the provider binary, the Go management tooling, the Docker/pelican deployment surface, and the fork-overlaid subsystems ported from the 3.23-fix fork.

## Directory Layout

```
sn/                                  # Fork root (forked from urfoundation/sn)
├── provider/                        # Provider binary (the relay node)
│   ├── provide.go                   # Provider entrypoint, goroutine launch, settings wiring
│   ├── provider_auth.go             # Authentication backends (user/pass + JWT)
│   ├── provider_identity.go         # Node identity, names, env seeding
│   ├── provider_profiles.go         # Performance profiles (auto/lowmem/turbo)
│   ├── control_socket.go            # Unix-domain control socket (urnet-tools <-> provider)
│   ├── control_state.go             # Control-plane state machine + hotswap trigger
│   ├── hotswap*.go                  # Zero-downtime restart (unix/windows/socketpair splits)
│   ├── bandwidth/                   # Data-plane accounting
│   │   ├── tracker.go               # ProxyBandwidth counters (TCP + UDP/QUIC federated)
│   │   └── wrap.go                  # WrapConnectSettings — TCP dials kept on-proxy
│   ├── proxy_health.go              # Per-proxy health registry, RegisterProxy/UnregisterProxy
│   ├── proxy_health_log.go          # Durable health state/event log writer
│   ├── proxy_warmth.go              # Warmup + register/unregister helpers
│   ├── proxy_slow_retry.go          # 24h daily gate, 14-day drop, give-up backoff
│   ├── proxy_reload.go              # Hot-reload engine via .reload trigger files
│   ├── proxy_state.go               # On-disk proxy state (proxy.state), stale-lock cleanup
│   ├── proxy_id.go                  # Stable monotonic proxy slot IDs (proxy[N])
│   ├── proxy_url.go                 # URL proxy state + blacklist (proxy_url.json)
│   ├── proxy_url_source.go          # URL fetcher, merge, reaper, blacklist pruner
│   ├── proxy_probe.go               # Dual-stage SOCKS5 probe (greeting + API CONNECT)
│   ├── proxy_table_probe.go         # Batch proxy table probing pipeline
│   ├── probe_targets.go             # SampleProbeTargets/ProbeHostCount test seams
│   ├── proxy_benchmark.go           # Opt-in staggered latency probing
│   ├── proxy_admission_gate.go      # Weighted-lottery admission gate for auth slots
│   ├── auth_rate_limiter.go         # Global adaptive auth rate limiter (AIMD)
│   ├── proxy_failure_history.go     # Persistent per-proxy failure counts
│   ├── proxy_auth_history.go        # Proven-proxy set for limiter gating
│   ├── metrics_listen.go            # Metrics listener + Tailscale auto-discovery
│   ├── metrics_provider.go          # Prometheus exposition
│   ├── metrics_collectors.go        # Runtime metric collectors
│   ├── lifetime_metrics.go          # Process-lifetime counters
│   ├── contract_metrics.go          # Fleet-wide per-proxy contract history
│   ├── health_heartbeat.go          # [health] heartbeat goroutine
│   ├── earn_tracker.go              # Per-minute billable earning windows ([earn])
│   ├── startup_banner.go            # Phased startup + Ready summary
│   ├── doh_cache.go                 # Persistent DNS-over-HTTPS cache
│   ├── ssrf_guard.go                # SSRF blocking (incl. Tailscale CGNAT ranges)
│   ├── client_jwt_store.go          # RWMutex-guarded JWT store
│   ├── jwt_refresher.go             # Proactive JWT refresh (7d primary / 48h fallback)
│   ├── jwt_utils.go                 # JWT parsing + expiry validation
│   ├── renewal_watcher.go           # Token renewal coordinator
│   ├── resource_pressure.go         # FD/memory pressure monitoring
│   ├── pool_health.go               # Message-pool health
│   ├── degraded_reaper.go           # Degraded-state reaper
│   ├── cmd_*.go                     # provider CLI subcommands (auth, proxy, status...)
│   ├── umask_unix.go / umask_windows.go  # Platform umask split (0600 control socket)
│   ├── shmlog_linux.go / shmlog_fallback.go  # RAM log (/dev/shm/urnetwork.log)
│   ├── tlog.go                      # Thread-safe timestamped logging
│   └── *_test.go                    # Deterministic unit tests (run with -race)
│
├── internal/
│   └── urnettools/                  # urnet-tools Go CLI implementation
│       ├── cli.go                   # CLI dispatch, help, target-flag parsing
│       ├── cobra.go                 # Cobra command tree (all subcommands)
│       ├── discover*.go             # Provider discovery (systemd, docker, /proc)
│       ├── lifecycle*.go            # start/stop/restart/uninstall + systemd units
│       ├── update.go                # Provider/tool update, digest verify, backup/prune
│       ├── proxy.go                 # proxy add/clear/summary/remove-dead
│       ├── hotswap.go               # hotswap trigger (v2026 version acceptance)
│       ├── session_cmds.go          # Encrypted identity session save/load (AES-256-GCM)
│       ├── provider.go              # Provider struct, version-from-buildinfo
│       ├── target.go / select_multi.go  # Target resolution + --all/--include batches
│       ├── release.go               # GitHub release + digest resolution
│       └── ...                      # platform-specific + test files
│
├── cmd/                             # Go command entrypoints
│   ├── provider/                    # provider binary main
│   ├── urnet-tools/                 # manager/CLI binary
│   ├── urnet-docker/                # Docker wrapper binary
│   └── sp2/                         # (inherited upstream entrypoint)
│
├── docker/
│   └── scripts/                     # Container runtime scripts
│       ├── entrypoint.sh            # BUILD-mode dispatch (stable/nightly/jwt)
│       ├── start_stable.sh / start_nightly.sh / start_jwt.sh
│       ├── urnet-tools.sh           # Docker-side tools wrapper + update digest verify
│       ├── update_verify.sh         # Signature/digest verification
│       ├── proxy-health.sh / proxy-traffic.sh / logs.sh
│       ├── pelican_panel.sh         # Pelican panel bootstrap
│       └── test_*.sh                # CI tests (pelican gates/smoke, update verify)
│
├── pelican/
│   └── egg-urnetwork-h3.json        # Pelican panel egg (image: ghcr.io/full-bars/sn)
│
├── third_party/
│   └── npipe/                       # Local replace target for gopkg.in/natefinch/npipe.v2
│
├── .github/workflows/
│   ├── build.yml                    # Test + lint + Docker multi-arch build/push (GHCR+DockerHub)
│   └── release.yml                  # Tagged release binaries (tarballs per platform)
│
├── Dockerfile                       # Multi-stage Alpine build (3 binaries), multi-arch
├── CHANGELOG.md                     # Release changelog
├── FORK_CHANGES.md                  # This fork's modifications (this doc's sibling)
├── PROJECT_STRUCTURE.md             # This document
└── go.mod / go.sum                  # Go module manifest + checksums
```

## Build & Test

```sh
# Build all three binaries for the current platform
go build -trimpath -o provider_bin ./cmd/provider/
go build -trimpath -o urnet_tools_bin ./cmd/urnet-tools/
go build -trimpath -o urnet_docker_bin ./cmd/urnet-docker/

# Run the full provider test suite (race detector on, no cache flakes)
go test -short -race -timeout 600s ./provider/...

# Static checks
go vet ./provider/...
gofmt -l provider/            # must be empty
```

## Container Build

```sh
# Local single-arch build
docker build --build-arg TARGETARCH=amd64 --build-arg VERSION=v<ver> -t ghcr.io/full-bars/sn:latest .

# Multi-arch (as CI does) — pushes to GHCR + DockerHub
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/full-bars/sn:latest -t 3cape/sn:latest --push .
```

## Inherited Upstream Directories

The fork is based on the `urfoundation/sn` monorepo, which carries additional upstream subsystems that are **not part of the fork's operational surface** (built but unused by the provider lane): `miner/`, `validator/`, `evm/`, `merkle/`, `ss58/`, `seed/`, `sim-testnet/`, `tools/`, `deploy/`, `monitoring/`, `mainnet/`, `stabi/`, `stctl/`, `crv4/`, `api/`, `cli/`, `clientauth/`, `protocol/`, and `metrics_prometheus.go` at the root. Leave them untouched during provider work.

**Last Updated**: 2026-09-17
**Maintained By**: @full-bars