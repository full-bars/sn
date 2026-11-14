# 🐳 Docker Deployment

This page keeps the copy-paste Docker examples from the README in one place. Use either `docker run` or Docker Compose, depending on how you prefer to manage containers.

## What `:latest` is

`ghcr.io/full-bars/sn:latest` (and the `3cape/sn:latest` mirror) is built from the repository `Dockerfile`: the subnet miner (`cli/miner`) installed as `provider`, plus `urnet-tools` and `urnet-docker`. `:miner` is the same packaging with the same entrypoint, kept as an alias for one release. The entrypoint is `docker/miner/entrypoint.sh`, which validates `AUTH` and then runs `docker/miner/start_miner.sh`. See [Miner Image](/full-bars/sn/wiki/Miner-Image) for the full variable list.

> [!WARNING]
> **vnStat is gone from this image.** The `Dockerfile` installs no vnStat, copies no vnStat startup path and publishes no port, so there is no dashboard on port 8080 and `ENABLE_VNSTAT` does nothing. An existing `:latest` container that you pull and recreate loses the dashboard. The old nightly updater (`start_nightly.sh`), the `start_stable.sh` and `start_jwt.sh` scripts and the Pelican panel script (`pelican_panel.sh`) are not in the image either. The same goes for `TURBO` and `ENABLE_IP_CHECKER`.

Authentication is chosen with `AUTH=authcode` (with `AUTHCODE` or `URNETWORK_AUTH_CODE`, or a trailing auth-code argument) or `AUTH=userpass` (with `USER_AUTH` and `PASSWORD`). When `AUTH` is not set, it is inferred from what is present. The old `BUILD=stable` and `BUILD=jwt` still start: they map to `AUTH=userpass` and `AUTH=authcode` with a deprecation warning, and an explicit `AUTH` wins. `BUILD=nightly` has no equivalent and is ignored. The examples below use `AUTH`.

## Extender networking

`extender setup` does not configure containers. Publish TCP 443 and, if wanted, UDP 443 and UDP 4053, and configure the host firewall and any router separately. UDP 53 is optional and user-managed. For Compose IPv6, enable IPv6 on the Compose network and configure routing and port publishing; changing Docker's default bridge alone does not configure a Compose network. See [Extender](/full-bars/sn/wiki/Extender).

With Docker RAM logs, the log follower may replay retained lines after a trim.

## 🛠️ Managing Containers with `urnet-docker`

> [!IMPORTANT]
> **Host-Side vs. In-Container Tooling:**
> - **`urnet-docker`** runs **on the Docker host** (outside the containers). It discovers provider containers, reads their in-container JWTs, and dispatches management tasks directly.
> - **`urnet-tools`** runs **inside the container** (accessible via `docker exec -it <container> urnet-tools <command>`).

Install `urnet-docker` once on the host (SHA-256 verified against the release API):

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/install-urnet-docker.sh | sh
# installs /usr/local/bin/urnet-docker (or ~/.local/bin when not root)
# GitHub fallback: curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

The tool is self-updating afterwards:

```sh
urnet-docker update                  # update the tool binary itself
urnet-docker update --unit urfix     # update a provider container in place (no recreate)
urnet-tools self-update              # same, for the process/systemd tool
```

Common host-side commands:

```sh
urnet-docker providers                          # list provider containers
urnet-docker status --unit urfix                # status of one container
urnet-docker direct status --unit urfix         # show direct IP providing state
urnet-docker direct off --unit urfix            # toggle direct IP (proxies only)
urnet-docker usage --unit urfix                 # show aggregate billable vs control traffic
urnet-docker usage graphs --unit urfix          # time-series usage graphs (day/hour/month)
urnet-docker proxy add --unit urfix ~/p.txt     # add proxies from host (or URL)
urnet-docker proxy paste --unit urfix < p.txt   # stream raw proxies from stdin
urnet-docker proxy trim --unit urfix 500        # hold running proxies at cap (A-F worst first)
urnet-docker proxy refresh --unit urfix         # reload proxies without restart
urnet-docker restart --unit urfix               # restart a container
urnet-docker logs --unit urfix 100              # stream logs (RAMLOGS-aware)
```

> [!NOTE]
> `urnet-docker update` with a target flag (such as `--unit`) updates a provider container in place. The container ID stays the same. Plain `urnet-docker update` with no target updates only the host tool binary. Containers can also be updated by pulling a new image and recreating the container (e.g. via Docker Compose or Watchtower).

## 🗄️ Image Registries

Primary image:

```text
ghcr.io/full-bars/sn:latest
```

Docker Hub mirror:

```text
3cape/sn:latest
```

Use the Docker Hub mirror if GHCR returns `denied` errors or rate-limiting.

## 🔑 Persistent JWT

The JWT is stored inside the container at `/root/.urnetwork/jwt`. Mount a named volume at `/root/.urnetwork` so the state survives when you recreate the container. Without a named volume, Docker creates an anonymous one, and a recreated container starts empty. It then needs re-authentication, and the original auth code is single-use, so it fails on the second attempt.

All examples below mount a config volume at `/root/.urnetwork`. With this volume in place, the startup script detects the existing JWT and skips authentication on later starts. Auth codes are only consumed once: on first run or after manually removing the config volume.

> [!WARNING]
> The same volume holds the provider identity (`.provider.key` and the custody files). Back up and retain the volume. If an existing identity is missing, stop and restore the original volume rather than registering a replacement. An unset or empty `ALLOW_CLIENT_REGISTRATION` defaults to `auto`, which permits the unattended container to register a new identity when one is missing. Set `ALLOW_CLIENT_REGISTRATION=N` to withhold that permission; an explicit registration flag can still grant it.

## 🏃 Docker Run - GHCR

The examples below use `urfix` as the container name.

#### With proxy benchmarking:

```bash
docker run -d --name urfix \
  -v ~/.urnetwork:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -e URNETWORK_PROXY_BENCHMARK=true \
  -e URNETWORK_PROXY_BENCHMARK_ENDPOINT=connect.bringyour.com:443 \
  ghcr.io/full-bars/sn:latest
```

| Env var | Purpose |
|---|---|
| `PROXY_URL` | Live proxy list URL, fetched and merged on interval (see [Proxy URL Sources](/full-bars/sn/wiki/Proxy-URL-Sources)) |
| `URNETWORK_PROXY_BENCHMARK=true` | **Not implemented in this line.** Nothing reads it; setting it changes nothing. |
| `URNETWORK_PROXY_BENCHMARK_ENDPOINT` | **Not implemented in this line.** Nothing reads it. |

The two `URNETWORK_PROXY_BENCHMARK*` variables in the example above are ignored by this image. See [Configuration.md](/full-bars/sn/wiki/Configuration).

For additional containers, change the container name and volumes together.

### 🔐 JWT Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=authcode \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest AUTH_CODE_HERE
```

Replace `AUTH_CODE_HERE` with your token from [ur.io](https://ur.io). Auth codes are single-use; the token is saved to the `urfix_config` volume on first run and reused on later starts.

Alternative method:

```bash
-e URNETWORK_AUTH_CODE=YOUR_CODE
```

Node labels are **not implemented in this line**. `URNETWORK_NODE_NAME` is not read by anything in the tree, so it does not label this server in webhook alerts, health logs or the dashboard. The `-e URNETWORK_NODE_NAME=...` flags in the examples on this page are ignored. See [Node-Identity.md](/full-bars/sn/wiki/Node-Identity).

### ✉️ Email/Password Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=userpass \
  -e USER_AUTH=you@example.com \
  -e PASSWORD=yourpassword \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest
```

## 🏃 Docker Run - Docker Hub

The commands are identical to GHCR except for the image name.

### 🔐 JWT Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=authcode \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  3cape/sn:latest AUTH_CODE_HERE
```

Alternative method:

```bash
-e URNETWORK_AUTH_CODE=YOUR_CODE
```

### ✉️ Email/Password Auth

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  --log-driver=json-file \
  --log-opt max-size=10m \
  --log-opt max-file=3 \
  -e AUTH=userpass \
  -e USER_AUTH=you@example.com \
  -e PASSWORD=yourpassword \
  -e HOST_HOSTNAME=$(hostname) \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  3cape/sn:latest
```

## 🐙 Docker Compose

For another single-container deployment on the same host, copy your `docker-compose.yml` to a new folder and replace the `urfix` prefix with a unique name in `container_name`, `volumes`, and the top-level `volumes:` section.

For 3, 5, or 10 nodes in one Compose file, use the [Multi-Container Scaling](/full-bars/sn/wiki/Multi-Container-Scaling) guide.

### 🔐 JWT Auth

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix
    restart: unless-stopped
    pull_policy: always
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=authcode
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urfix_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urfix_config:
```

On first start, provide your auth code using one of these methods:

1. Trailing argument: `docker compose run --rm urnetwork AUTH_CODE_HERE`
2. Environment variable: add `URNETWORK_AUTH_CODE=AUTH_CODE_HERE` to the `environment:` section, then run `docker compose up -d`

After the JWT is saved to the volume, subsequent starts need no auth code:

```bash
docker compose up -d
```

### ✉️ Email/Password Auth

```yaml
services:
  urnetwork:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix
    restart: unless-stopped
    pull_policy: always
    cap_add:
      - NET_ADMIN
      - NET_RAW
    sysctls:
      - net.ipv4.ip_forward=1
    environment:
      - AUTH=userpass
      - USER_AUTH=you@example.com
      - PASSWORD=yourpassword
      - HOST_HOSTNAME=${HOSTNAME}
    volumes:
      - urfix_config:/root/.urnetwork
      - ./proxy.txt:/app/proxy.txt
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

volumes:
  urfix_config:
```


## 💾 RAM Logging

Setting `URNETWORK_RAMLOGS=1` redirects provider logs to `/dev/shm/urnetwork.log` inside the container, a RAM-backed filesystem, instead of stdout. This keeps log I/O entirely off disk.

> [!TIP]
> **Live Monitoring (RAMLOGS)**
> If you have `URNETWORK_RAMLOGS=1` enabled, your logs are stored in high-speed memory instead of standard output. Use this command to live-tail them:
> ```bash
> docker exec -it <container_name> tail -f /dev/shm/urnetwork.log
> ```

> [!NOTE]
> With `URNETWORK_RAMLOGS=1` the miner writes its log to `/dev/shm/urnetwork.log` and not to stdout. The container supervisor can still write startup and failure messages to Docker logs, so keep a log rotation policy if you need to bound them.

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e URNETWORK_RAMLOGS=1 \
  -e URNETWORK_NODE_NAME=urfix \
  -e AUTH=authcode \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest YOUR_AUTH_CODE
```

View logs live:

```bash
docker exec -it urfix tail -f /dev/shm/urnetwork.log
```

RAM logs are capped at 1MB with automatic rotation and are lost when the container restarts.

## 📊 Proxy Summary

View a single-pane fleet overview showing proxy counts by source (file vs URL), health breakdown, and URL cache status:

```sh
docker exec -it <container> urnet-tools summary
```

(The legacy PowerShell wrapper `urnet-tools.ps1` has been retired; the Go binary replaces it on every platform.)

## ♻️ Hot-Restart

Client JWTs are reused across restarts by default. The write path and the read path both persist JWTs, so a container restart keeps existing client identities. What is stored, how to inspect it, and what mints a new identity: [Hot-Restart.md](/full-bars/sn/wiki/Hot-Restart).

**Disabling:**

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -e URNETWORK_HOT_RESTART=0 \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest YOUR_AUTH_CODE
```

> [!NOTE]
> To opt out, set `-e URNETWORK_HOT_RESTART=0` at container creation. A saved `hot_restart` control setting takes precedence over this variable, so check it with `urnet-tools set hot-restart` if the behavior does not match. If you set nothing, hot-restart is active by default.

## 💾 Session Save/Load

Export a provider's full identity state (client JWTs, account JWT, signing keys, proxy lists) as an encrypted, portable bundle for cross-machine transfer.

**Save** (two steps: save inside container, then copy out):

```bash
docker exec -it urfix urnet-tools session save /root/.urnetwork/nyc.urnsession
docker cp urfix:/root/.urnetwork/nyc.urnsession .
```

**Load** (two steps: copy in, then load inside container):

```bash
docker cp atlanta.urnsession urfix:/root/.urnetwork/
docker exec -it urfix urnet-tools session load /root/.urnetwork/atlanta.urnsession
```

The load will prompt for the passphrase, check the network_id against the current account, backup existing files, stage the new session, and ask "Restart now? (Y/n)". If yes, it kills the provider process: the container's start script crash loop picks it up with the new identity.

> [!IMPORTANT]
> Save and load require an interactive TTY (`docker exec -it`, not just `docker exec`). The script will fail with a clear error if `-it` is omitted.

## 🩺 Viewing Proxy Health

You can view the full list of never-up and dropped proxies, as well as a live event log of proxy state transitions. These files persist on the config volume and survive container restarts, even if RAM logging is active.

- Persistent (always): `docker exec -it <container> proxy-health`
- Live-tail RAMLOGS on: `docker exec -it <container> sh -c "tail -f /dev/shm/urnetwork.log | grep -E '\[health\]\[proxies\]|\[pulse\]'"`
- Live-tail RAMLOGS off: `docker logs -f <container> 2>&1 | grep -E '\[health\]\[proxies\]|\[pulse\]'`

## 🏎️ Auto-Tune Performance

The Auto-Tune feature (`URNETWORK_PROFILE=auto`) automatically optimizes the provider based on server hardware.

It detects total system RAM, assigns buffer sizes, enables Eco mode on very low-RAM machines, and benchmarks disk speed on startup. If storage is too slow, it can automatically enable RAM logging to prevent disk I/O from bottlenecking the network stack.

Example Docker Run:

```bash
docker run -d \
  --name=urfix \
  --pull=always \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e AUTH=authcode \
  -e URNETWORK_PROFILE=auto \
  -e URNETWORK_NODE_NAME=urfix \
  -e HOST_HOSTNAME=$(hostname) \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v urfix_config:/root/.urnetwork \
  -v /path/to/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest YOUR_AUTH_CODE
```

> [!NOTE]
> Because Auto-Tune may enable RAM logging if it detects a slow disk, omit `--log-driver` and `--log-opt` from this command to avoid conflicts.

## 🔄 Automatic Updates

[Watchtower](https://containrrr.dev/watchtower/) can automatically pull new image versions and restart your container when an update is published. Add it to your `docker-compose.yml` alongside the `urnetwork` service:

```yaml
  watchtower:
    image: containrrr/watchtower
    container_name: watchtower
    restart: unless-stopped
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
    command: --cleanup --interval 3600 urfix
```

> [!IMPORTANT]
> The `urfix_config` volume is required when using Watchtower. Without it, Watchtower will pull a new image, recreate the container, and the auth code will be consumed again, which fails because auth codes are single-use. With the volume mounted, the existing JWT is reused.
>
> **JWT Smart Refresh**: The container includes "smart refresh" logic. If the JWT stored in your volume expires, the provider will automatically detect this, delete the stale file, and attempt to re-authenticate when `AUTH=userpass` (or an auth code is supplied) gives it a way to. This ensures your nodes stay online even if a JWT is revoked or corrupted during an update.

> [!NOTE]
> **Pelican Panel deployments**: With `PELICAN=yes`, `urnet-tools update` refuses to run (`docker/miner/urnet-tools.sh`), so the panel manages updates by re-pulling the published image. See [Pelican Panel](#-pelican-panel).

## ⏳ Idle Update

`urnet-tools idle-update` lets you apply a pending provider update without interrupting active client sessions: it waits for a quiet traffic window before swapping the binary, instead of updating immediately and cutting off whatever's in flight:

```sh
docker exec -it <container> urnet-tools idle-update
```

It polls `billable_rate` every 10s and waits until traffic stays at or below a threshold for a sustained window, then double-checks with 1s polling for 10s before actually applying the update: so a brief lull doesn't trigger an update while traffic is still fluctuating.

| Flag | Default | Meaning |
|---|---|---|
| `--threshold <bytes/sec>` | `5120` (5 KiB/s) | Traffic at or below this is considered "quiet". |
| `--window <seconds>` | `300` (5 min) | How long traffic must stay quiet before updating. |

```sh
# Wait for a 10-minute quiet window under 10 KiB/s
docker exec -it <container> urnet-tools idle-update --window 600 --threshold 10240

# Skip waiting entirely and update immediately
docker exec -it <container> urnet-tools idle-update --window 0
```

> [!NOTE]
> If `billable_rate` isn't available yet (provider hasn't written its first sample), `idle-update` treats the node as **not idle** rather than assuming it's safe to update: it fails closed.

## 📊 (Optional) Running with vnStat Bandwidth Monitor

**vnStat is not part of this image.** `:latest` used to start a vnStat web page on port 8080 (`ENABLE_VNSTAT`, default on). The current `Dockerfile` installs no vnStat and the miner entrypoint never starts one, so `ENABLE_VNSTAT`, `-p <port>:8080` and a `/var/lib/vnstat` volume have no effect. Remove them from existing run commands and compose files. The container makes all p2p tunnels and relay connections outbound and needs no inbound port.

For traffic graphs use the Prometheus `/metrics` endpoint and the monitoring bundle below, or run vnStat on the Docker host. The `/metrics` endpoint is a separate listener, enabled by default. Inside a bridged container it binds `0.0.0.0` on port 9100 (9101-9103 if that port is taken), so publish it with `-p` to scrape it. A container that shares a publicly addressed network stays on loopback and Tailscale instead (see [Configuration.md](/full-bars/sn/wiki/Configuration)).

### Multi-Container Rules: Isolate Volumes

When running more than one container on the same host:

> [!CAUTION]
> **Isolate provider config volumes (`/root/.urnetwork`).** Never share `/root/.urnetwork` between multiple running containers. The provider maintains process-local SQLite databases (`proxy.state`, `.client_jwts.json`, `lifetime`) and identity keys inside this directory. Sharing it across containers leads to lock contention and state corruption. Each container requires its own config volume (`ur_config_1`, `ur_config_2`).
>
> If you publish `/metrics`, also give each container a unique host port (for example `9100:9100` and `9101:9100`).

```yaml
services:
  node-1:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix-1
    environment:
      - AUTH=authcode
    volumes:
      - ur_config_1:/root/.urnetwork    # ISOLATED identity & state volume
      - ./proxy-1.txt:/app/proxy.txt

  node-2:
    image: ghcr.io/full-bars/sn:latest
    container_name: urfix-2
    environment:
      - AUTH=authcode
    volumes:
      - ur_config_2:/root/.urnetwork    # ISOLATED identity & state volume
      - ./proxy-2.txt:/app/proxy.txt

volumes:
  ur_config_1:
  ur_config_2:
```

---

### 📊 Prometheus + Grafana Monitoring Bundle

A ready-made Prometheus + Grafana stack that scrapes the provider's built-in `/metrics` endpoint is included in the repository under `monitoring/`:

```bash
cd monitoring
./setup.sh          # generates .env and Prometheus scrape targets
docker compose up -d
```

This brings up:
- **Prometheus**: configured to scrape each provider's `/metrics` endpoint on a fixed interval
- **Grafana**: pre-loaded with a provider dashboard (login `admin`, password from the generated `.env`)

> [!TIP]
> If you run multiple provider containers, update the Prometheus scrape targets in `monitoring/prometheus/targets/` to point at each container's metrics port. Each container needs a unique host-side port mapping (e.g. `-p 9100:9100` on the first, `-p 9101:9100` on the second).

The metrics endpoint is enabled by default: no environment variables are needed. Override the bind address with `-e URNETWORK_METRICS=ip:port`; disable per container with `urnet-tools metrics off`.

See the [Configuration](/full-bars/sn/wiki/Configuration#-monitoring--telemetry) reference for the full list of telemetry variables.

## 🐦 Pelican Panel

The repository still carries a Pelican egg (`pelican/egg-urnetwork-h3.json`). It pulls `ghcr.io/full-bars/sn:latest` and starts `sh /app/entrypoint.sh`, which is the miner entrypoint. The egg was written for the previous image and has **not been re-verified against this one**.

What changed for a panel deployment:

- **No panel script.** `pelican_panel.sh` (the retry loop and the re-auth after 3 crashes) is not in the image. The miner's own supervisor in `start_miner.sh` runs instead.
- **`BUILD` goes through the legacy mapping.** `BUILD=jwt` runs as `AUTH=authcode` with `AUTHCODE`, and `BUILD=stable` as `AUTH=userpass` with `USER_AUTH` and `PASSWORD`, each with a deprecation warning. `BUILD=nightly` is ignored and `AUTH` is inferred from what is set.
- **`PELICAN=yes` has one effect left.** `urnet-tools update` refuses to run (`docker/miner/urnet-tools.sh`). Update by re-pulling the image through the panel (**Settings → Reinstall**) or `docker pull`.
- **`ENABLE_VNSTAT` and `ENABLE_IP_CHECKER` are ignored.** The image has no vnStat and no IP checker.

The provider needs raw packet access on the node: container capability `NET_ADMIN` (and typically `NET_RAW`), IP forwarding on the host, and outbound UDP for WebRTC P2P transport. No inbound ports need to be forwarded.

The egg's CI tests (`docker/scripts/test_pelican_gates.sh`, `docker/scripts/test_pelican_smoke.sh`) exercise the old panel script, not this image. For the old walkthrough see `pelican/README.md`.

## Letting the Thrash Watchdog Restart the Container's Provider

The swap-thrash watchdog ([details](/full-bars/sn/wiki/Configuration#swap-thrash-watchdog)) restarts the provider by exiting with status 75. Inside a container the watchdog counts the container as a supervisor, the same as systemd, so the provider restarts with no extra switch: self-heal is the one switch.

```yaml
services:
  provider:
    image: ghcr.io/full-bars/sn:latest
    restart: unless-stopped
    environment:
      URNETWORK_SELF_HEAL: "1"
    volumes:
      - ur_config_1:/root/.urnetwork   # the thrash cap and its restart ring live here
```

The image's start script (`docker/miner/start_miner.sh`) treats exit 75 as a planned restart: they wait 5 seconds, restart the provider and leave the JWT alone. Without a writable, persistent `~/.urnetwork` the watchdog logs `persist-failed` and never exits. On runtimes that do not create `/.dockerenv` (Podman, Kubernetes) also set `URNETWORK_CONTAINER=1`.

Exit 79 is the terminal stop after repeated authentication failures. The start script keeps the container up but parked: it logs one CRITICAL line and makes no further login attempt until the container is restarted, because a Docker restart policy ignores `RestartPreventExitStatus`. Fix `USER_AUTH`/`PASSWORD` and restart the container.
