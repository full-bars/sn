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
> `URNETWORK_RAMLOGS=1` and Docker `--log-opt` are **mutually exclusive**. When RAM logging is active, nothing is written to stdout, so Docker's log driver has nothing to capture: remove `--log-driver`/`--log-opt` if you enable it. See [Docker Deployment → RAM Logging](Docker-Deployment#-ram-logging) for the full section.

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
| `ENABLE_VNSTAT` | `true` | Enables the web-based traffic monitor on port 8080. |
| `ENABLE_IP_CHECKER` | `false` | If `true`, queries and logs your public IP on startup. Useful for debugging NAT issues. |
| `BUILD` | `stable` | `stable`, `nightly`, or `jwt`: selects the startup script/auth mode. |

For the complete env-var reference, see [Docker Deployment](Docker-Deployment).

## 4. System Privileges

*   **`--cap-add=NET_ADMIN` & `NET_RAW`**: Required for the provider to manage the network stack and create necessary tunnels.
*   **`--sysctl net.ipv4.ip_forward=1`**: Enables kernel-level IP forwarding, required for tunneling traffic between your proxies and the URnetwork fabric.