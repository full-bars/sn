# Proxy Management

This guide covers how to monitor, update, and prune your proxy list
dynamically without restarting the provider.

## The problem with restarts

> [!WARNING]
> Proxies on the platform take roughly 8-12 hours to fully warm up and reach
> maximum traffic allocation. Previously, if you discovered a single dead
> proxy in your list, removing it meant restarting the entire provider. That
> restart wiped the warm state of every other live proxy on the machine,
> causing a temporary dip in earnings and throughput while the backend slowly
> re-ramped them.

## Proxy hot-reload

You can add and remove proxies from a running instance without a restart.
Changes to your proxy list are applied live:

- **Dead or removed proxies** are cancelled and their connections drained
  gracefully.
- **New proxies** are started with a staggered delay to prevent a burst of
  simultaneous authentication attempts at the API.

When a reload completes, the log prints a summary line in the form
`[proxy] reloaded: +N added, -M removed`, so you can confirm a `proxy
refresh` or `--proxy_url` pickup actually applied what you expected.

> [!NOTE]
> **Stable proxy identities**
>
> Proxy identities are stable across reloads. An identity is assigned once
> and persisted to `proxy.state` in your configuration directory. If you
> remove a proxy and later re-add it, the provider recognizes it and restores
> its original identity, so your logs and traffic reports remain consistent
> over time.

See [Proxy Hot-Reload](Proxy-Hot-Reload.md) for the full mechanics, the
trigger file, and the edge-case behavior, including the safety guard that
skips a reload when the source yields no usable proxies.

## Modifying the proxy list

To update your proxy list:

1. Edit your proxy configuration file (for example `proxies.txt` or
   `proxy.txt`) as you normally would.
2. Run the `refresh` command to apply the changes live.

### Binary / Linux service

```sh
urnet-tools proxy refresh
```

### Docker

```sh
docker exec -it urfix urnet-tools proxy refresh
```

The refresh reads your updated configuration and applies the diff to the
running provider. Proxies that are unchanged keep serving without
interruption.

## Removing dead proxies interactively

If you want to clean up failing proxies without manually editing files, use
the interactive cleanup tool.

### Binary / Linux service

```sh
urnet-tools proxy remove-dead
```

### Docker

```sh
docker exec -it urfix urnet-tools proxy remove-dead
```

> [!IMPORTANT]
> The provider continuously monitors proxy health. This command queries the
> live health state and groups failing proxies into two categories:
>
> 1. **Dead proxies:** proxies that have never successfully authenticated
>    (likely bad credentials or unreachable IPs).
> 2. **Inactive or degraded proxies:** proxies that were previously working
>    but have been offline for an extended period.
>
> The tool prompts you separately for each category, allowing you to
> selectively remove dead proxies while keeping inactive ones (in case they
> are just suffering a temporary network blip), or wipe all failing proxies
> at once.

Proxies that fail continuously are also cleaned up automatically. File-sourced
proxies that fail for 14 days are dropped from the active pool, persisted
across reboots. URL-sourced proxies are cleaned up after 65 minutes of
provider uptime. See [Proxy URL Sources](Proxy-URL-Sources.md) for the
cleanup scope controls. Use `proxy remove-dead` for immediate cleanup of
known-bad entries.

## Removing proxies by pattern

Remove every proxy matching a provider or IP range in one command, no list
reset, no provider restart. Matching is a case-insensitive substring test
against the proxy host.

### Binary / Linux service

```sh
urnet-tools proxy remove --match=dc.decodo.com --preview   # list matches, change nothing
urnet-tools proxy remove --match=dc.decodo.com             # prompt, then remove
urnet-tools proxy remove --match=192.3. --yes              # no prompt (IP prefix match)
```

### Docker

```sh
docker exec -it urfix urnet-tools proxy remove --match=dc.decodo.com
```

> [!IMPORTANT]
> What it does:
>
> 1. Removes matching proxies from all three stores: the internal proxy list
>    (`proxy.state`), your proxy file (`--proxy_file`), and the URL source
>    cache.
> 2. Adds the pattern to a persistent exclude list, so future URL source
>    refreshes silently skip matching proxies. They cannot sneak back in.
> 3. Triggers a hot reload: the running provider drops only the removed
>    proxies; everything else keeps serving.

`--preview` lists the matches without changing anything. `--yes` skips the
confirmation prompt for scripted use.

## Persistent proxy trim (hard cap)

Operators can enforce a hard ceiling on running proxies without restarting
the provider or wiping configuration files:

```sh
# Preview which proxies would be shed without making changes
urnet-tools proxy trim 500 --preview

# Set the running proxy hard cap to 500
urnet-tools proxy trim 500

# Remove the trim cap
urnet-tools proxy trim off
```

### Docker (host-side)

```sh
urnet-docker proxy trim --unit urfix 500 --preview
urnet-docker proxy trim --unit urfix 500
```

> [!IMPORTANT]
> How trim ranking works:
>
> 1. **Health first:** dead, inactive, and offline proxies are always shed
>    first.
> 2. **Earning protection:** proxies with active billable traffic are
>    protected. An active earner is never shed while idle proxies remain.
> 3. **A-F reachability grade:** among idle proxies, worst-first order is
>    proven-failing (F) first, then never-graded, then D, C, B, A. Among
>    active earners, smaller earners shed before larger ones, with grade as
>    the tiebreak.
> 4. **Persistent cap:** the target count is persisted to
>    `~/.urnetwork/proxy_trim`. It stays in effect across restarts and
>    reloads until raised or cleared with `proxy trim off`.
> 5. **Prevents over-budget re-spawning:** during background URL fetch cycles
>    and configuration reloads, new proxy additions are capped to prevent
>    exceeding the trim target while retaining candidate history.

## Pressure-based self-healing

`URNETWORK_SELF_HEAL=1` (or `urnet-tools self-heal on` at runtime) activates
an adaptive resource-pressure controller that protects running nodes from
memory exhaustion and high load:

```sh
urnet-tools self-heal on       # Enable pressure monitoring
urnet-tools self-heal status   # Inspect live score, components, and target pool size
urnet-tools self-heal off      # Disable pressure monitoring
```

When enabled, the controller dynamically paces URL fetches and probe
concurrency, accelerates dead-proxy cleanup and stale-entry re-probing under
pressure, and adjusts the target pool size to shed dead and lowest-grade
proxies first.

## Monitoring proxy health and traffic

To help you decide which proxies to prune, use the built-in telemetry
commands:

### Proxy health

View a live report of proxy states and stream live recovery and loss events:

```sh
urnet-tools proxy health                          # Binary
docker exec -it urfix urnet-tools proxy health    # Docker
```

> [!TIP]
> When you first deploy proxies or add new ones to your list, they appear as
> `connecting` in the health report during an initial startup period. This is
> not a sign they are broken. The backend cannot handle thousands of proxies
> authenticating at once, so the provider staggers the startup of newly added
> proxies. A large deployment takes minutes, not seconds, to fully
> initialize, and proxies show as `connecting` until their staggered slot
> comes up. A proxy that never connects falls back to `dead` after the
> health tracker's confirmation window; investigate those. In the first hour
> after a big add, `connecting` labels are normal and expected.

### Proxy traffic

View a sorted report of cumulative bandwidth per proxy, broken down by
billable vs. total traffic, along with the number of active NAT sessions
currently multiplexed through each proxy:

```sh
urnet-tools proxy traffic                          # Binary
docker exec urfix cat /root/.urnetwork/proxy_traffic.state  # Docker
```