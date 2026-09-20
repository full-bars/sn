# Monitoring with Prometheus and Grafana

Every provider can serve Prometheus metrics at `/metrics`. This page covers enabling the endpoint and running the included Grafana stack.

## Enable the metrics endpoint

The provider listens on auto ports by default. It tries ports 9100, 9101, 9102, and 9103 in order and binds the first free one. On a container the listener binds `0.0.0.0`; on a native host it binds `127.0.0.1` plus a Tailscale auto-bind when a tailnet interface is present.

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
