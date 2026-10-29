# Log Message Reference

This page lists the log line prefixes the provider emits. Prefixes mirror the subsystem that produced the line. Emoji marks the high-value frequent lines.

## Standard prefixes

| Prefix | Meaning |
|--------|---------|
| `[api-token]` | API bearer refresh: scheduled refresh, retry, rejection. Carries `client=<id>` |
| `[audit]` | System auditor warnings: host limits below recommended values |
| `[auth]` | Login and token authentication events |
| `[billable_rate]` | Billable transfer rate sampling |
| `[control]` | Control-socket command handling, applied values, rejected values |
| `[direct]` | Direct providing (no proxy) status |
| `[earn]` | Per-minute earning windows: billable_1m, billable_5m, billable_15m, billable_60m |
| `[encryption]` | Packet encryption subsystem events |
| `[gossip]` | Extender mesh: node startup identity, peer connections, applied records |
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
| `[report]` | Leftover bandwidth report posts. The hub that received them was removed, so you should not see these on a normal setup. |
| `[session]` | Identity session events |
| `[profit]` | Profit heartbeat: earning yes/no, clients, rate |
| `[startup]` | Startup phase markers and Ready summary |
| `[status]` | Status server events |
| `[systemd]` | systemd integration events |
| `[traffic]` | Data-plane traffic: rx/tx rates, clients |

## Verbosity

`-v` is V(1) and `-vv` is V(2). Lines at those levels are trace detail and are absent by default.

- `[s]event=sequence_exit reason=ack_lifetime ...` is V(1). An ack-lifetime expiry is the ordinary outcome for a destination with no route, and a start with hundreds of unreachable identities printed one full line per identity at default verbosity. Every other `sequence_exit` reason stays at info, and the ack-lifetime line keeps every field, so `-v` reproduces the old output exactly.

## Lines added or changed in this release

- **Control-plane tally.** One line per wave replaces one warning per slot: `⚠️ [control] 465 of 465 identities authenticated but not control-connected after 1m; oldest 3m12s; sample: 203.0.113.9:1081, 198.51.100.4:1081`. It prints when the not-connected count changes and repeats an unchanged count at most every five minutes, so a single stuck slot cannot print for the life of the process. `oldest` is the longest wait of a slot that authenticated and has not dialed, not the process's uptime. The sample names each proxy address once: several slots can sit behind one address, and a per-identity sample spent its slots repeating that address and hid how broad the problem was.
- **Identity outcomes are counted, not printed per slot.** The startup summary gains a clause only when an identity was decided in this start: `; identity: 12 minted new, 3 minted replacement, 4 adopted, 0 revoked`. A start that reused every saved identity prints exactly the line it printed before, so nothing new appears on a healthy host. When the platform rejects the identities the pool presented past `max(3, 5% of them)`, one warning follows: `⚠️ [identity] 37 of 500 saved identities were replaced or refused this start (threshold 25); check first that the state directory is the one holding the identities, then whether the platform is refusing identities that used to work`. The decision is made when the last slot resolves, never at the first heartbeat: a rate over the slots that happened to come up first is not a rate over the pool. A start that finds no saved identity at all raises no warning, because a first start mints every identity by design; that case is visible as `0 reused, N new` in the same summary. Per-slot detail — which slot, which outcome, the reason, the time — is appended to the `identity_events` record in `provider_state.db` (bounded at 2000) rather than to the log. A host running with hot restart off mints every identity by contract on every start, so that path writes no per-slot event: there the tally's `0 reused, N new` is the count.
- **Startup summary.** After the first heartbeat interval: `[startup] 500 slots: 465 auth ok, 31 pending, 4 failed (500 reused, 0 new); control-connected 232`.
- **UDP relay refusal.** `[proxy][udp] slot=proxy:65.111.8.178:1081 udp relay unavailable (...); h3 disabled for this slot, tcp only; action: check proxy credentials`.
- **Reload outcome.** The summary line gained a `skipped` count: `✅ [proxy] reload complete: added 2, removed 0, rotated 0, unchanged 480, lost 0, skipped 3`. A reload that cannot trust its input aborts instead: `[proxy] reload failed: reload aborted, running set left as-is: proxy file /home/you/proxies.txt contained no valid proxies (expected one host:port:user:pass per line) (will retry)`. The same abort covers an unreadable `proxy_url.json` (`read proxy_url.json: ...`).
- **Partial file ingest.** `[proxy] warning: /home/you/proxies.txt: loaded 480 proxies, skipped 3 malformed lines`. The per-line message no longer prints the source line: `[proxy] error: proxy 203.0.113.5:1080 (user=present, password=absent) missing credentials`, followed by the required `ip:port:user:pass` format and `skipping`. An unsupported URL-list scheme is reported the same way: `[proxy][url] unsupported scheme "http" (only socks5 is supported); skipping http://203.0.113.5:8080 (user=present, password=present)`.
- **Cached identity refused.** `🔥 [hot-restart] <key>: stored client <id> disagrees with custody client <id>, minting through custody` and `🔥 [hot-restart] <key>: stored JWT client_id "<id>" disagrees with store client_id "<id>", minting fresh`. When the minted credential cannot be written to the store, the slot fails with `failed to persist minted client JWT for <key>` after the existing `⚠️ [jwt-store] failed to persist client JWT` warning, rather than running unrecorded.
- **Aborted handoff.** `❌ [hotswap] Failed to restore control socket after aborted handoff: ...` follows an `Aborting handoff; live provider retained.` line when the parent could not rebind `provider.sock`.
- **Gossip startup.** `[gossip] peer=<peer id> network=<network host> role=<member|extender> listen_addrs=N operator_addrs=N started`.
- **Attributable token lines.** Every `[api-token]` line carries `client=<id>` parsed from the bearer, for example `[api-token] client=019f7191… jwt refresh failed. Will retry in 96.17s. err = http request attempts exhausted`. A legacy bearer without the claim prints no tag rather than a fabricated one.

## Extender lines

The extender role has its own small vocabulary, and the difference between "bound a port" and "accepted by the network" is the whole of it.

- `[extender] listen tcp 443` and `[extender] listen udp 443 (quic)` and `[extender] listen udp 4053 (dns)` are printed by the carrier layer as each one binds. They report a socket inside this process. They do not report that the port is reachable from outside, and they do not report that the network accepted the host.
- `[extender] listen error (<carrier>): <reason>` means that one carrier did not bind. A failed UDP carrier costs only that carrier; a failed TCP carrier leaves the role off for this process while the provider keeps providing.
- `[extender]activate v4 refused = <reason>` is the network's answer. The text after the equals sign is the network's own wording for the test that failed, so read it as the diagnosis rather than as a local error. The usual cause is that the network could not reach TCP 443 on the public address.
- `[extender] activation refused; next attempt in <duration>` is the hold that a refusal earns: ten minutes, doubling on each further refusal up to one hour. During the hold the provider still provides; it does not listen or try to activate.
- `[extender] activation failed without a refusal (v4 activated=%t, v6 activated=%t); the next attempt is not held` is a failure that never reached the network, such as a dns failure, a dial timeout or a reset. It is deliberately not treated as a refusal and earns no hold, so a flaky path does not spend the account's activation budget.
- `[extender] activation held: <n> of <m> posts in the last hour, next in <duration>` is the rate limit rather than a refusal: the account may post a bounded number of activations per hour, and this attempt was held before it was sent.
- `[extender] activation failed on <family>; the leftover post funds one retry` says the previous attempt left one post unused, so one more try follows without waiting for the hourly window.
- `[extender] extender role off: <reason>` means the role did not start in this process at all. The reason names which precondition failed: an unavailable activation budget, an unavailable refusal barrier, or a hotswap candidate that took the provider state. A role that never started prints no `listen` line either, so look for this line first when the carrier lines are missing entirely.
- The machine-readable form is the diagnostics event: `{"schema":"urnetwork-provider-diagnostics-v1","domain":"extender","event":"observed",...}` carrying `activated_v4`, `activated_v6`, `activation_refused` and `revoked`. It is the only place that records an acceptance as a value rather than as the absence of a refusal.

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
| 79 | Authentication stopped after 30 failed logins over at least 2 hours; fix the credentials and restart. The shipped units set `RestartPreventExitStatus=79`, so systemd does not restart it |
| 80-82 | Proxy paste failures: input file, temp file, executable path |

## Log files

- Standard output: the provider logs to stdout and systemd journal (or Docker logs).
- Shm log: `/dev/shm/urnetwork.log` when `URNETWORK_RAMLOGS=1` is set. Read it with `urnet-tools logs`.
- Important events mirror to `/dev/shm/urnetwork-important.log` when ramlogs are enabled.