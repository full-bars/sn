# Monitoring with Prometheus and Grafana

Every provider can serve Prometheus metrics at `/metrics`. This page covers enabling the endpoint and running the included Grafana stack.

## Enable the metrics endpoint

The provider listens on auto ports by default. It tries ports 9100, 9101, 9102, and 9103 in order and binds the first free one. On a container the listener binds `0.0.0.0` when every address the container holds is private or unique-local; on a native host it binds `127.0.0.1` plus a Tailscale auto-bind when a tailnet interface is present. A container that shares a publicly addressed network keeps loopback plus Tailscale, so the endpoint never lands on a public interface.

Control the endpoint with the manager tool:

```sh
urnet-tools metrics status
urnet-tools metrics on
urnet-tools metrics off
urnet-tools metrics listen auto
urnet-tools metrics listen 0.0.0.0:9100
```

`urnet-tools metrics off` is the same control-socket toggle as the runtime tuning override. The `URNETWORK_METRICS` environment variable also sets a fixed listen address at startup. The provider logs the chosen address:

```text
[metrics] enabling Prometheus /metrics on 127.0.0.1:9100
```

## Included Grafana stack

The `monitoring/` directory contains a full compose stack: Prometheus scrapes your providers, Grafana shows them.

Run it from the repository checkout:

```sh
cd monitoring
./setup.sh          # writes .env with the Grafana admin password, then starts the stack
docker compose up -d
```

Services:

- Prometheus on `127.0.0.1:9090`, 30-day retention by default (`PROMETHEUS_RETENTION`).
- Grafana on `:3000` (`GRAFANA_BIND` default `0.0.0.0`, `GRAFANA_PORT` default 3000). Login with `admin` and the password from `.env`.

The Grafana dashboard auto-loads from `monitoring/grafana/dashboards/urnetwork-providers.json`. Provisioning files live under `monitoring/grafana/provisioning/`.

## Scrape configuration

The Prometheus config in `monitoring/prometheus/` targets your provider nodes. Refer to `monitoring/prometheus/prometheus.yml` and the `targets/` directory for the exact scrape job definitions. The same bundle ships with every release as `urnetwork-monitoring-<version>.tar.gz`.

## New dashboard panels

The providers dashboard has a **Lifecycle** row (restarts by reason, uptime per node, HotSwap outcomes per hour, and version skew: each version in the fleet and how many nodes run it) and two **Node health** panels that plot memory and file descriptors against their limits (the limit is a dashed line).

The memory, descriptor and restart-reason panels need a provider that exports `urnet_mem_limit_bytes`, `urnet_rss_bytes`, `urnet_open_fds`, `urnet_fd_limit` and `urnet_restart_reason`. On older providers those panels stay empty and the rest of the dashboard works as before. Descriptor metrics are Linux only.

## Alerts

The bundle ships alert rules in `monitoring/prometheus/rules/urnetwork.yml`. Prometheus loads them at startup, and `docker compose up -d` mounts the `rules` directory for you. Firing alerts show at `http://127.0.0.1:9090/alerts`.

> [!NOTE]
> The bundle has no Alertmanager. Alerts are visible in Prometheus and as the `ALERTS` series, but nothing is emailed or posted until you add an Alertmanager and point Prometheus at it.

### Enabled by default

| Alert | Fires when | Does not detect |
|---|---|---|
| `UrnetworkNodeDown` | A node's `/metrics` endpoint has not answered for 5 minutes (`up == 0`). | A stopped provider looks the same as metrics switched off, a blocked port or a Tailscale outage. A node removed from `targets/providers.yml` never fires. It says nothing about whether a reachable node is earning. |
| `UrnetworkRestartLoop` | More than 3 restarts of one node in an hour. | A restart that hides between two scrapes, and the cause of a restart. Check `urnet_restart_reason` and the node's logs. |

`UrnetworkRestartLoop` counts drops of `urnet_uptime_seconds` with `resets()`. It does not use `urnet_startup_restarted`, because that gauge keeps one fixed value for the life of a process, so it cannot be counted with `increase()`.

### Optional rules

These are in the same file, commented out, because they need thresholds that suit your fleet.

| Alert | Fires when | Does not detect |
|---|---|---|
| `UrnetworkOldVersion` | A node runs a different version from most of the fleet for 7 days. | Which version is newer. During a slow rollout it can flag the early upgraders. It does not fire on a fleet that is uniformly out of date. |
| `UrnetworkMemoryNearLimit` | `urnet_mem_sys_bytes` is above 90% of `urnet_mem_limit_bytes` for 15 minutes. | Resident memory, other processes on the machine, and a container limit lower than the Go limit. Nodes with no limit set are skipped. |
| `UrnetworkFileDescriptorsNearLimit` | `urnet_open_fds` is above 85% of `urnet_fd_limit` for 10 minutes. | Which kind of descriptor leaks, and a spike between two scrapes. |
| `UrnetworkNoBillableTraffic` | A node is up, has been up for over 30 minutes, and billed no traffic for 30 minutes. | A broken node versus a quiet one with no demand. It can be noisy on idle nodes. |

To enable one, open `monitoring/prometheus/rules/urnetwork.yml`, read the note above the rule, adjust the threshold, and remove the leading `# ` from every line of that block (the block ends at the next blank line). Then reload Prometheus:

```bash
curl -X POST http://127.0.0.1:9090/-/reload
```

Nodes whose provider does not export a metric never match its rule, so a fleet with mixed versions is safe.

> [!NOTE]
> This provider does not export `urnet_billable_bytes_total`, so `UrnetworkNoBillableTraffic` never fires here until that family is exported. The other rules use metrics listed in the table below.

### Checking your changes

If you have `promtool` (it ships with Prometheus), run these from the `monitoring/` directory after editing the rules:

```bash
promtool check rules prometheus/rules/urnetwork.yml
promtool test rules prometheus/rules/urnetwork.test.yml
```

The test file covers the two enabled rules. `prometheus.yml` lists `urnetwork.yml` by name, so the test file is never loaded as rules.

## Metrics added with the live status snapshot

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `urnet_uptime_seconds` | gauge | | Provider uptime |
| `urnet_mem_sys_bytes` | gauge | | Memory obtained from the OS |
| `urnet_mem_limit_bytes` | gauge | | The Go memory limit in effect; absent when none is set |
| `urnet_rss_bytes` | gauge | | Resident set size (Linux) |
| `urnet_open_fds` | gauge | | Open file descriptors (Linux) |
| `urnet_fd_limit` | gauge | | File descriptor limit (Linux) |
| `urnet_restart_reason` | gauge | `reason` | Value 1 for the reason the current process started; absent for other reasons |
| `urnet_gc_gogc` | gauge | | GOGC value currently in force |
| `urnet_gc_tightening` | gauge | | 1 while the GC governor holds GOGC below its baseline |
| `urnet_gc_cpu_fraction` | gauge | | Share of CPU spent in GC over the last window; absent until a window completes |
| `urnet_conntrack_used_ratio` | gauge | | `nf_conntrack_count` over `nf_conntrack_max` (Linux, when conntrack is loaded). Overflow drops packets silently |
| `urnet_tcp_time_wait` | gauge | | TCP sockets in TIME_WAIT (Linux) |
| `urnet_tcp_orphans` | gauge | | Orphaned TCP sockets (Linux) |

## Metrics for QUIC DATAGRAM

This build offers QUIC DATAGRAM on every H3 connection at the upstream
always-on default: there is no gate to enable, no control key that changes it,
and no environment variable.

The H3 identity set itself is always exported:

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `urnet_h3_mode_info` | gauge | `mode` | Value 1 for the resolved `h3` mode (`off`, `direct`, a proxy count, or `all`) |
| `urnet_h3_set_size` | gauge | | Grand total of running identities that run H3: proxies plus the direct identity. Under an `<int>` cap the direct identity is counted on top of the N proxies. |
| `urnet_h3_proxy_set_size` | gauge | | Running proxies that run H3, excluding the direct identity. `urnet_h3_set_size` is this plus the direct identity when it runs. |

The per-datagram families the 3.23-fix fork exports
(`urnet_h3_datagram_rx_*`, `urnet_h3_datagram_tx_*`,
`urnet_h3_datagram_offered_total`, `urnet_h3_datagram_accepted_total`,
`urnet_h3_datagram_blackholes_total`) are not exported by this provider: those
counters live inside the connect library, and the pinned revision sn builds
against exposes the DATAGRAM data plane but not the counters the fork added.
Until sn moves onto a connect revision that carries them, datagram traffic is
not visible in `/metrics` here.

## Metrics for streams, P2P transports and dial backoff

These families come from the connect library the provider pins. The provider
appends them to its own scrape, so a provider built against an older revision
serves none of them.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `urnet_stream_sequences_by_kind` | gauge | `kind` | Live stream sequences: `single` (this provider is one end of the stream) or `two_ended` (it relays between a source and a destination) |
| `urnet_stream_sequences_opened_total` | counter | `kind` | Stream sequences started |
| `urnet_stream_sequences_closed_total` | counter | `kind`, `reason` | Stream sequences ended: `transport_ended` (its P2P transport gave up on its own) or `cancelled` (platform close, reset, replacement or shutdown) |
| `urnet_stream_sequence_lifetime_seconds` | histogram | `kind` | Lifetime of ended sequences, observed when they end (buckets 1 s, 10 s, 60 s, 5 min, 30 min, 1 h, 6 h, 24 h) |
| `urnet_stream_open_requests_total` | counter | `result` | Platform stream-open requests by what the buffer did: `created` (started a new sequence), `existing` (reused a live one), `replaced` (cancelled an older one for the same stream id) |
| `urnet_stream_control_frames_total` | counter | `type` | Stream control frames received: `open`, `close`, `reset` |
| `urnet_p2p_transports_started_total` | counter | | P2P transports started |
| `urnet_p2p_transports_stopped_total` | counter | | P2P transports stopped |
| `urnet_p2p_connect_failures_total` | counter | | P2P connection attempts that returned an error (a connection that came up and then failed its readiness exchange is not counted) |
| `urnet_tls_handshakes_total` | counter | `path`, `host`, `result` | Client TLS handshakes on the platform and API dial paths. `result` is `full`, `resumed` (resumed handshakes skip certificate verification) or `failed`; `path` names the dial strategy (`normal`, `normal_proxy`, the `resilient_*` modes); `host` is the target class (`api`, `connect`, `other`). 54 bounded series |
| `urnet_dial_backoff_long_tier_active` | gauge | | Identities currently waiting in the long dial backoff tier because their own path keeps failing |
| `urnet_dial_backoff_long_tier_entered_total` | counter | | Times an identity's dial backoff entered the long tier |

### Reading the stream sequence and P2P transport series

Providers carry streams for their tunnel clients, and the P2P transports are
the direct WebRTC hops those streams can use. None of these counters is
touched per packet.

- `urnet_stream_sequences_by_kind`: `single` tracks the client streams the provider is serving. `two_ended` is relay bookkeeping (the provider sits between a source and a destination) and a provider that does not relay should hold it near zero. `two_ended` stuck above 0 for more than half an hour, or either kind climbing while connections do not, means sequences are outliving their purpose.
- `urnet_stream_sequences_closed_total`: in steady state sequences end in step with the connections that carried them. A `cancelled` count that stays near zero while `opened_total` keeps climbing is the shape of sequences that nothing ever closes.
- `urnet_stream_sequence_lifetime_seconds`: healthy streams end with the connection that carried them, so the 1 s to 5 min buckets should dominate. Growth in the 1 h and up buckets is the one to watch; it is the shape of a sequence that outlived its transport. `+Inf` counts every ended sequence.
- `urnet_stream_open_requests_total`: mostly `created` with some `existing` (the platform re-sends opens and the buffer reuses the live sequence). A rising `existing` rate against a flat `by_kind` means the platform is asking for streams the provider is only just reaping. `replaced` spikes during resets.
- `urnet_stream_control_frames_total`: what the platform asked for. `open` counts StreamOpen frames alone; a reset carries its own type and its stream entries surface in `open_requests` (`created` or `replaced`), so read open frames against the sum of `created`, `existing` and `replaced` while remembering that a reset adds requests without adding open frames.
- `urnet_p2p_transports_started_total` minus `stopped_total` is the live transport count; in steady churn the pair should move almost evenly, and a growing gap means transports are not stopping. `urnet_p2p_connect_failures_total` climbing at a fast steady rate means a peer is unreachable and the transport is retrying; a slow trickle is the normal cost of peers going away.
- These counters are process-local: a restart resets them, so alert on rates and ratios, not absolute values.

### Reading the handshake and backoff series

- `urnet_tls_handshakes_total`: full handshakes are the expensive ones (certificate verification happens on each). A large `full` share from one `path` with few `failed` means that path is not taking its resumed-session cache; a rising `failed` share is the real connection problem. Resumed share per path is `sum(rate(urnet_tls_handshakes_total{result="resumed"}[5m])) by (path) / sum(rate(urnet_tls_handshakes_total[5m])) by (path)`.
- `urnet_dial_backoff_long_tier_*`: `active` above zero is normal after trouble (an identity whose own path keeps failing is backed off at the long tier instead of burning the shared attempt budget). An `entered` rate that stays high while `active` is flat is ordinary churn; `active` growing across hours is a node whose paths keep failing.
