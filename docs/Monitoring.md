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