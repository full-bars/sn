# HotSwap: updating the provider without a restart

HotSwap replaces the running provider with a new binary without stopping the service. The old process keeps serving what it already has while a new process starts, takes over, and the old one drains and exits. This page is the procedure, what you will see, and what it costs. Everything here was observed on a live node (Linux, systemd user unit, about 1,400 proxies), not just in tests.

> [!NOTE]
> On a plain restart the provider is down for a few seconds (about 2 to 3 seconds on the test node). The README figure of 20 to 60 seconds is the worst case, when a node is slow to bring proxies back up. HotSwap avoids that stall, but it is not free: see [What it costs](#what-it-costs).

## What HotSwap is, and is not

- The new provider is a **separate OS process** started by the old one (`fork` and `exec` of the binary on disk). The two talk only over a private control socket, using READY, TAKEOVER and ACK messages.
- **Nothing is migrated.** Goroutines, connections and streams stay with the old process. The new process builds its own from scratch.
- The old process keeps its existing streams for a **graceful drain of up to 30 seconds**, then exits. New work goes to the new process.
- systemd is told about the new main PID, so `systemctl` and `Restart=on-failure` follow the new process (a crash after a swap is restarted normally).

> [!IMPORTANT]
> A stream that is still open when the old process exits after its 30-second drain is cut, and the client has to reconnect through the new process. Short flows finish on the old process. This drain cut has not been measured per stream yet.

## How to update with HotSwap

There is nothing to set up and no script to run. Use the normal update command:

```bash
urnet-tools update                          # latest release
urnet-tools update --tag v3.23.0-fix.31.2   # a specific release (a downgrade works the same way)
```

`urnet-tools update` decides for itself: it hands off to the new binary when the node is eligible and falls back to a normal restart when it is not, and it prints which one it did and why. `update -n` is a dry run, but it currently prints only the target version, not whether the update will hotswap or restart.

> [!NOTE]
> `urnet-tools` is reachable from every kind of shell without any setup. The installer links `urnet-tools` and `urnetwork` into `~/.local/bin` (found by the shell behind `ssh host urnet-tools ...` and by cron, neither of which reads `~/.bashrc`), into `/usr/local/bin` so root and other users find them (directly when run as root, otherwise through passwordless `sudo`), and writes the PATH block to `~/.bashrc`, `~/.profile` and `~/.zshenv`. A node installed before that is repaired by its next `urnet-tools update`. If root still cannot find it, the box has no passwordless `sudo` for the installing user; the installer and `update` cannot write `/usr/local/bin` unprivileged, so run the single `sudo ln -sfn <install>/bin/urnet-tools /usr/local/bin/urnet-tools` line the installer prints.

### Which update hotswaps

A node qualifies when the running provider and the new binary are both **v3.23.0-fix.31.0 or newer** and the provider was started by a systemd unit that is `Type=notify`. The installer writes a unit with no `Type=` line (systemd's default, `simple`), so a fresh or older node is not eligible yet. `urnet-tools update` fixes that for you:

| Update | What `urnet-tools update` does | Downtime |
|---|---|---|
| The first update on a build that includes the unit migration | Converts the unit to `Type=notify` (and reloads systemd), then restarts the provider once so it starts with a notify socket | a few seconds (a normal restart) |
| Every update after that | Hands off to the new binary | none (see [What it costs](#what-it-costs)) |

You can see whether a node is ready at any time:

```bash
systemctl --user show urnetwork.service -p Type,NotifyAccess   # drop --user for a system unit
```

`Type=notify` and `NotifyAccess=all` means the next update will hotswap.

> [!IMPORTANT]
> Builds up to and including v3.23.0-fix.32.0 do not perform the unit migration on units with no `Type=` line, so on those the update always restarts. Once a node is on a build with the fix, the sequence above applies: the next update migrates and restarts, and the one after that hotswaps. If you would rather a node never hotswap, use `urnet-tools restart` after installing an update by hand.

## What you will see

`urnet-tools update` prints the download, the checksum check, the binary swap, then:

```text
triggered zero-downtime HotSwap handoff (SIGUSR2 sent to PID <old>)
provider urnetwork.service handed off (pid <old> -> <new>), waiting for version <tag>...
verified urnetwork.service running <tag> (pid <new>; running image ... matches)
```

If the unit is not eligible you get `hotswap trigger unavailable (...); falling back to service restart` and a normal restart instead. The provider log shows the handoff:

```text
[hotswap] Initiating zero-downtime handoff (live parent PID <old> ...)
[hotswap] Candidate PID <new> passed pre-flight -> announced READY to parent
[hotswap] TAKEOVER received from parent ... -> candidate assuming live traffic
[hotswap] Candidate PID <new> confirmed active takeover (ACK received)!
[hotswap] systemd updated: MAINPID=<new>
[hotswap] Parent PID <old> entering graceful stream drain (max 30s)...
```

## What it costs

Measured on a node with about 1,400 proxies, twice (downgrade and upgrade):

| Item | Observed |
|---|---|
| Candidate takes over live traffic | about 0.15 s after it starts, before most proxies are up |
| Old process lifetime after takeover | about 30 s (the drain), then it exits |
| New process build-up | goroutines and file descriptors ramp from near zero to a steady state over about 35 s (goroutines from about 4,500 at +4 s to about 28,000 at +30 s) |
| Memory | **both processes are alive for about 30 s**, so peak memory is roughly old plus new |
| Provider log | no gap longer than about 1.3 s across the swap |
| `urnet-tools update` wall time | about 10 s, mostly a fixed 3-second poll while it waits to see the new PID |

> [!CAUTION]
> Because the candidate takes over before most proxies have authenticated, capacity is reduced while it builds up, and the larger the proxy list the longer that takes. On a node that cannot fit two provider processes in RAM, prefer `urnet-tools restart` for updates.

## Diagnostics after a swap

Set `URNETWORK_PPROF=127.0.0.1:6060` to expose pprof on loopback. During a swap the old process holds that port, so the new one retries the bind in the background until the old one exits, then serves pprof. (Before that retry existed, diagnostics disappeared on every other hotswap.)

## Related

- [Troubleshooting: HotSwap declines on an existing node](Troubleshooting.md#5-hotswap-declines-on-an-existing-node)
- [Monitoring: `urnet_hotswap_outcomes_total`](Monitoring.md)
