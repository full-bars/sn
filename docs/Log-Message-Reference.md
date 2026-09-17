# Log Message Reference

This page lists the log line prefixes the provider emits. Prefixes mirror the subsystem that produced the line. Emoji marks the high-value frequent lines.

## Standard prefixes

| Prefix | Meaning |
|--------|---------|
| `[audit]` | System auditor warnings: host limits below recommended values |
| `[auth]` | Login and token authentication events |
| `[billable_rate]` | Billable transfer rate sampling |
| `[control]` | Control-socket command handling, applied values, rejected values |
| `[direct]` | Direct providing (no proxy) status |
| `[earn]` | Per-minute earning windows: billable_1m, billable_5m, billable_15m, billable_60m |
| `[encryption]` | Packet encryption subsystem events |
| `[health]` | Health heartbeat: uptime, profile, heap, sys, connections |
| `[hot-restart]` | Hot-restart enablement and trigger events |
| `[hotswap]` | Zero-downtime binary reload handoff: candidate, promos, takeovers |
| `[hourly-maintenance]` | Hourly maintenance tasks |
| `[init]` | Startup initialization phases |
| `[jwt]` | JWT lifecycle: mint, refresh, expiry warnings |
| `[jwt-store]` | Client JWT store: persist, prune, reload |
| `[lifetime]` | Process-lifetime metric snapshots |
| `[mem]` | Memory pressure monitor |
| `[metrics]` | Prometheus metrics endpoint events |
| `[no-direct]` | Direct providing disabled notices |
| `[pace]` | Warmup pace monitor: progress toward fleet up |
| `[persist]` | State persistence warnings |
| `[pool]` | Message pool size and resize events |
| `[pqe]` | Post-quantum encryption events |
| `[profile]` | Tuning profile application |
| `[provider]` | Provider identity and version lines |
| `[proxy]` | Proxy lifecycle: add, remove, health, reload |
| `[proxy-quality]` | Proxy quality grading events |
| `[relay]` | Relay connection events |
| `[report]` | Hub bandwidth report posts and results |
| `[session]` | Identity session events |
| `[profit]` | Profit heartbeat: earning yes/no, clients, rate |
| `[startup]` | Startup phase markers and Ready summary |
| `[status]` | Status server events |
| `[systemd]` | systemd integration events |
| `[traffic]` | Data-plane traffic: rx/tx rates, clients |

## Exit codes

All nonzero exit codes write `FATAL [exit N]: ...` to stderr and the shm log.

| Code | Meaning |
|------|---------|
| 0 | Clean shutdown |
| 10-17 | Auth failures: no home (10), login request failed (11), credentials rejected (12), account verification required (13), auth code request failed (14), auth code rejected or reused (15), state dir creation failed (16), JWT write failed (17) |
| 20 | Proxy file could not be read |
| 21 | Proxy file contained no valid entries |
| 40 | Ramlog file not found (is `URNETWORK_RAMLOGS=1` set?) |
| 50-56 | Proxy refresh failures: state file missing, provider not running, warmup threshold, proxy lock, source read, trigger path, trigger write |
| 60-62 | Proxy remove-dead failures: provider not running, dead-confirmation threshold, source update |
| 70-76 | URL-source and direct-providing failures: missing URL (70), proxy lock (71, 75), direct providing enable/disable (72), proxy_url.json write (73-74), proxy_url.json read (76) |
| 72 | Could not enable or disable direct providing |
| 78 | JWT expired or invalid; startup scripts intercept this code to re-authenticate |
| 80-82 | Proxy paste failures: input file, temp file, executable path |

## Log files

- Standard output: the provider logs to stdout and systemd journal (or Docker logs).
- Shm log: `/dev/shm/urnetwork.log` when `URNETWORK_RAMLOGS=1` is set. Read it with `urnet-tools logs`.
- Important events mirror to `/dev/shm/urnetwork-important.log` when ramlogs are enabled.