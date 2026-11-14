# Advanced Deployment

This guide covers advanced Docker flags and environment variables. For the full deployment guide (images, env vars, idle-update, upgrade behavior), see [Docker Deployment](Docker-Deployment).

## 1. Managing Log Ballooning

The provider can log heavily, so log management matters on long-running nodes. Two approaches: pick one:

**Docker log rotation** (keeps logs in stdout):

```bash
--log-driver=json-file \
--log-opt max-size=10m \
--log-opt max-file=3
```

Caps each log file at 10 MB and keeps three rotation files (30 MB total). Trade-off: older history is lost once the cap is hit.

**RAM logging** (recommended for high volume: keeps log I/O off disk entirely):

```bash
-e URNETWORK_RAMLOGS=1
```

Redirects provider logs to `/dev/shm/urnetwork.log`, a RAM-backed filesystem. Live-tail with:

```bash
docker exec -it <container_name> tail -f /dev/shm/urnetwork.log
```

> [!NOTE]
> With `URNETWORK_RAMLOGS=1` the miner writes its log to `/dev/shm/urnetwork.log` and not to stdout. The container supervisor can still write startup and failure messages to Docker logs, so keep a log rotation policy if you need to bound them. See [Docker Deployment → RAM Logging](Docker-Deployment#-ram-logging) for the full section.

## 2. CPU Limiting

The `--cpus` flag caps the container's CPU quota so the provider can't monopolize the host:

```bash
--cpus=1.0
```

> [!IMPORTANT]
> A CPU cap keeps *other* services on the host responsive: it does **not** make the provider more stable. If the cap is too tight for your proxy count, it can starve the signaling layer and cause contract timeouts and connection drops. At **1,000+ concurrent proxies**, do **not** set a CPU limit at all (or set it to `2.0` or higher). Only set a cap you've measured the provider staying under.

## 3. Environment Variables

| Variable | Default | Description |
| :--- | :--- | :--- |
| `AUTH` | inferred | `userpass` (with `USER_AUTH` and `PASSWORD`) or `authcode` (with `AUTHCODE` or `URNETWORK_AUTH_CODE`). Selects the auth path. An explicit `AUTH` always wins. |
| `BUILD` | - | **Deprecated.** `stable` maps to `AUTH=userpass` and `jwt` maps to `AUTH=authcode`, each with a deprecation warning. `nightly` has no equivalent and is ignored. |
| `ENABLE_VNSTAT` | - | **Removed.** The image installs no vnStat and serves no port 8080, so this does nothing. |
| `ENABLE_IP_CHECKER` | - | **Not in this image.** Nothing in the packaged scripts reads it. |

The miner dials out only, so ordinary provider traffic needs no published port. The image serves Prometheus `/metrics` by default. Inside a container the listener binds all interfaces, and only a port you publish is reachable from the host. Publish a port only when you need host access to metrics or when you run an extender. See the [Extender](Extender) guide for its port requirements.

For the complete env-var reference, see [Docker Deployment](Docker-Deployment).

## 4. System Privileges

*   **`--cap-add=NET_ADMIN` & `NET_RAW`**: Required for the provider to manage the network stack and create necessary tunnels.
*   **`--sysctl net.ipv4.ip_forward=1`**: Enables kernel-level IP forwarding, required for tunneling traffic between your proxies and the URnetwork fabric.