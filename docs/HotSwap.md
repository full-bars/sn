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

## Requirements

1. The running provider and the replacement binary are both **v3.23.0-fix.31.0 or newer**.
2. The provider's systemd unit is **`Type=notify` with `NotifyAccess=all`**. Check:

   ```bash
   systemctl --user show urnetwork.service -p Type,NotifyAccess,MainPID   # drop --user for a system unit
   ```

   `Type=notify` and `NotifyAccess=all` means eligible. `Type=simple` means the update will restart instead.

3. `urnet-tools` on your PATH. A user-mode install keeps it in `~/.local/share/urnetwork-provider/bin/`, which is not on PATH by default. Either add that directory to PATH or call the tool by its full path.

## How to update with HotSwap

```bash
urnet-tools update            # latest release; hotswaps when the requirements above are met
urnet-tools update --tag v3.23.0-fix.31.2   # a specific release, same rules (also works for a downgrade)
```

`update -n` is a dry run, but it currently prints only the target version. It does not tell you whether the update will hotswap or restart.

### A node that is still `Type=simple`

The installer writes a unit with no `Type=` line, which systemd treats as `simple`. The **first** `urnet-tools update` on a build that includes the unit migration rewrites the unit to `Type=notify` and `NotifyAccess=all`, reloads systemd, and performs a **normal restart** (the running process was started without a notify socket, so it cannot hand off). From the next update on, updates hotswap.

> [!WARNING]
> Releases up to and including v3.23.0-fix.32.0 do **not** perform this migration on units without an explicit `Type=` line. On those, `update` reports that hotswap is unavailable and restarts every time. Update to a release that contains the fix, or convert the unit by hand once:
>
> ```bash
> mkdir -p ~/.config/systemd/user/urnetwork.service.d
> printf '[Service]\nType=notify\nNotifyAccess=all\n' > ~/.config/systemd/user/urnetwork.service.d/notify.conf
> systemctl --user daemon-reload
> urnet-tools restart -f        # one normal restart so the process gets a notify socket
> ```
>
> Use `systemctl` without `--user` and a file under `/etc/systemd/system/urnetwork.service.d/` for a system unit. To undo, delete the file, run `daemon-reload` and restart.

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
