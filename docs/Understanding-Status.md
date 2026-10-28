# Understand what your provider status means

You can use this page to tell whether a provider is running, whether its local control socket answers, and whether it is carrying traffic. Use it when a status screen looks healthy but you need to know what its fields actually establish.

## Prerequisites

You need shell access to the machine or container running the provider, an installed `urnet-tools`, and an interactive terminal for `top`.

## Steps

Use `status` for a one-shot check of a provider. Use `dashboard` for a panel of its identity and settings. Use `top` to watch it change live.

1. Check the provider once.

   ```bash
   urnet-tools status
   ```

   A running provider with an answering control socket can show a `Live` block after its service status. If several providers are found, select one with the target flags shown by `urnet-tools status --help`.

   Read the fields in this order. This is the shape of the relevant labels across the three surfaces, not a transcript; not every surface shows every label.

   ```text
   Active: / State: / running:
   control-socket: / Socket:
   jwt-expires: / jwt expires: / JWT:
   proxies / Proxies / PROXIES:
   pressure / memory / heap / rss
   billable / total / moved / clients
   ```

   Service state: Linux may show systemd's `Active:`; the fallback shows `running:`, and the panel shows `State:` as `RUNNING` or `STOPPED`. Running is the expected state, but a running process does not prove its socket answers or that it is serving.

   The separate `Live` state can be `STARTING`, `DEGRADED`, `IDLE` or `FLOWING`. `FLOWING` indicates traffic, while `IDLE` can be healthy without demand; read any `why idle` or state reason rather than treating either as a payment verdict.

   Control socket: `control-socket: true` in the fallback or `Socket: reachable` on the panel means the local socket can be reached. That is useful, but it does not prove authentication or network registration. A systemd status view does not necessarily print this field; use the panel for it.

   Authentication: `jwt-expires:`, `jwt expires:` or the panel's `JWT: expires in` describes the stored token's expiry. A future expiry is preferable to an expired token, but it does not prove the network accepts it or that every slot authenticated. A missing expiry is unknown, not a successful login; inspect the logs for `[auth]` and `[startup]`.

   Proxies: the `Live` block and `top` show `up`, `dropped`, `connecting` and `dead`. A pool that you intend to use is healthier when its expected proxies are `up` and few are `dropped` or `dead`; `connecting` may be normal during startup. The Windows and macOS `PROXIES:` summary reads a saved health snapshot and marks it `UP` if even one proxy is up, so it does not prove the whole pool is healthy. Zero proxies can be intentional for direct-only operation.

   Pressure: `pressure` is a combined resource-pressure score, not just memory usage; lower is better, and `top` colors it at 0.7 and 0.9. `memory` or `heap` shows Go heap usage and, when available, its Go limit; `rss` is resident memory. A low score or heap below its limit does not prove the host or container cannot run out of memory, and an omitted resource is unknown rather than zero.

   Traffic: `billable` is the provider's billable rate, `total` is all traffic and `clients` is the current client count. A positive billable rate with clients shows local billable traffic is moving; zero can simply mean no current demand. Neither a positive rate nor accumulated `moved` bytes proves a payout has been received.

   Reachable is not registered, registered is not in a pool, and in a pool is not earning. Network-sourced information does not explain the server's reasoning, and these local views cannot establish each of those steps.

2. Check the panel for the socket and stored identity.

   ```bash
   urnet-tools dashboard
   ```

   The panel prints `State:`, `Socket:` and, when an expiry is available, `JWT:`. It is a one-shot panel, not the live graph, and its `Proxy Sources` list is configuration, not a pool-health count.

3. Watch changes over time.

   ```bash
   urnet-tools top
   ```

   The live screen refreshes, shows throughput graphs and recent events, and exits with `q`; `DISCONNECTED` means the last display can be stale. Use `--interval 500ms` for a faster refresh if needed, or `status` for a non-interactive check.

4. Check the recent provider log.

   ```bash
   urnet-tools logs
   ```

   Look for `[startup]` with `auth ok`, `pending`, `failed` and `control-connected` to see the startup progress; the committed run also writes `STARTUP: version=` to `events.log` in the provider state directory. `[traffic] started` marks a transition to having clients, while `[profit] earning=yes` additionally requires billable bytes moving: startup alone proves neither. For the rest of the lines, see [[Log-Message-Reference]].

## What this does not tell you

`status` is a snapshot, even when it includes small history sparklines; `top` lets you watch rates and events change, but its graph is limited to the last ten minutes. Use [[Monitoring]] for longer-term trends. The panel shows settings and sources, not the live traffic and pool breakdown, so switch to `status` or `top` for those.

None of these views explains why the network selected or did not select a provider, verifies a payout, or establishes server-side pool membership. Use the network's own information for those questions; its decisions come from the network, and this page cannot explain the server's reasoning.

## Two ways it usually goes wrong

**Symptom:** The service is running but `Socket: unreachable`, or `top` says `DISCONNECTED`.

**Cause:** The process may still be starting, may have failed before binding the socket, or the selected provider may be wrong.

**Fix:** Check the provider logs and the selected unit, then follow [[Troubleshooting]]; do not assume a PID means the control socket works.

**Symptom:** Many proxies remain `connecting`, `dropped` or `dead`, or billable traffic stays at zero.

**Cause:** Proxies or authentication may be failing, or a healthy provider may simply have no clients. A zero rate alone does not distinguish these cases.

**Fix:** Check `why idle` or the `[profit]` reason, inspect proxy health and authentication logs, and follow [[Troubleshooting]] before changing the pool.

## See also

- [[Troubleshooting]]
- [[Monitoring]]
- [[Log-Message-Reference]]

<!--
## Visual aid

One screenshot: a real `urnet-tools top` screen showing the throughput graph, Now, Proxies, Resources and Events panels. Annotate the billable rate, proxy states and pressure score; omit network names, IDs, addresses, credentials and other identifying information.
-->
