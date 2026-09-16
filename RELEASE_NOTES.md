# URNetwork Provider — v2026.9.16-meso

First release of the H3/QUIC-capable provider, forked from upstream and
ported with full feature parity from the v3.23 maintenance fork.

## What's New

- **H3/QUIC + IPv6 dual-stack** — native HTTP/3 transport via connect v2026,
  automatic transport negotiation (H3, H2, TCP), and full IPv6 support.
- **Bandwidth tracking** — per-proxy Rx/Tx billing with billable rate windows.
- **Resource pressure monitoring** — adaptive GC, memory budget, PSI-aware
  load shedding, and pool controller for proxy concurrency.
- **Proxy health grading** — real probe host table (127 health-class hosts
  + 22 DNS resolvers), URL proxy source auto-fetch with cooldown, and
  dead-proxy pruning.
- **Control socket IPC** — hot-restart (SIGUSR1), session save/load, and
  urnet-tools integration.
- **Hotswap** — client identity preservation across restarts, IPC handle
  lifecycle management.
- **Metrics** — Prometheus /metrics endpoint, lifetime counters, PQE
  measurement gating.

## Ported from v3.23-fix

All critical production features ported and verified:

- JWT 401 renewal with out-of-band callback
- `seedEnvFromControlState` for environment persistence
- `RecordProxyAuthFailure` for auth failure tracking
- `markProxyUp` / `markProxyDown` health callbacks
- `stableID` via `setProxyIndex` for proxy identity
- `flushRetentionEvents` at process scope
- DialContextSettings removed to preserve proxy routing
- Real probe host table (removed `probe.invalid` stubs)

## Cross-Platform

Binaries available for:
- Linux amd64 / arm64
- macOS amd64 / arm64
- Windows amd64 / arm64

All binaries are GPG signed (`.asc` detached signatures).

## Verification

```bash
# Download and verify signature
gpg --verify provider-linux-amd64.asc provider-linux-amd64
chmod +x provider-linux-amd64
./provider-linux-amd64 provide --help
```

## Known Limitations

- `connect-v4.bringyour.com` certificate expired (server-side) — end-to-end
  H3 transfer testing blocked until upstream resolves.
- PacketConnFactory (UDP/QUIC bandwidth wrapping) not yet re-enabled — pinned
  connect version doesn't expose the interface.
- 34 stubs remain across non-critical paths (metrics edge cases, platform-
  specific socket ACLs).

## CI

- Build and test: passing
- GAUNTLET auth: passing
- VirusTotal + ClamAV: non-blocking scan on release
- CFAA blocklist sync: lives in connect library, not this repo
