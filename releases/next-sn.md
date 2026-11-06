# Next sn release

## Quick start

Install on Linux or Alpine:

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
```

Install on macOS:

```sh
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Mac.sh | sh -s -- install
```

Install on Windows in PowerShell:

```powershell
irm https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Win32.ps1 | iex
```

Install on FreeBSD as root. Set `SERVICE_USER_OVERRIDE` to an existing non-root account:

```sh
fetch -qo urnet-install.sh https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_FreeBSD.sh
SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

Start a new provider container:

```sh
docker run -d --name urnetwork --restart unless-stopped \
  -e URNETWORK_AUTH_CODE=YOUR_AUTH_CODE_HERE \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  -v urnetwork_config:/root/.urnetwork \
  ghcr.io/full-bars/sn:latest
```

The `:miner` tag uses the same image configuration but can point to a different build.

Keep the state volume. If an existing identity is missing, restore its original volume instead of authorizing a replacement. For optional extender ports, see [Extender](https://github.com/full-bars/sn/wiki/Extender).

### Downloads

Asset names use the release tag in place of `<tag>`. Download them from the release's Assets section.

| What | Asset |
| --- | --- |
| Provider, Linux amd64 | `urnetwork-provider-<tag>-linux-amd64.tar.gz` |
| Provider, Linux arm64 | `urnetwork-provider-<tag>-linux-arm64.tar.gz` |
| Miner, Linux amd64 | `urnetwork-miner-<tag>-linux-amd64.tar.gz` |
| Miner, Linux arm64 | `urnetwork-miner-<tag>-linux-arm64.tar.gz` |
| Linux amd64 tools | `urnet-tools-linux-amd64`, `urnet-docker-linux-amd64` |
| Monitoring configuration | `urnetwork-monitoring-<tag>.tar.gz` |

## What changed

**We gate new identity registration.** [redeploy] A direct unattended `provide` needs the registration flag or an allowing environment setting. Interactive start asks first. Docker defaults to `ALLOW_CLIENT_REGISTRATION=auto`, which permits registration without a prompt, including if an identity is missing. Stop and restore existing state rather than creating a replacement. (#378)

**We preserve settings and identity evidence.** [redeploy] We added schema checks, durable extender identity and persisted legacy import proofs. We refuse a newer state schema rather than silently creating identities. We added migration primitives but no operator migration command. (#357, #374, #376, #351)

**We added live diagnostic capture.** [redeploy] Run `urnet-tools diag` as the provider user before restarting to collect heap and goroutine profiles, a summary and a CPU profile when available. The bundle can contain secrets. See [Capture Diagnostics](https://github.com/full-bars/sn/wiki/Capture-Diagnostics). (#360)

**We made extender setup and status visible.** [redeploy] The management tool checks host setup and displays the running extender state. The Go updater, Linux installer and container wrappers restore bind capability after a binary swap when possible. Setup repairs a disabled or blocking Windows firewall rule, verifies the TCP 443 bind as the provider user, and treats a missing FreeBSD `sysctl.conf` as empty. With no flags, `urnet-tools extender setup` configures every provider. Activation errors shown on the control reply are redacted. Check the [extender guide](https://github.com/full-bars/sn/wiki/Extender) for ports and platform limits. (#358, #365, #366, #368)

**We hold the proxy launch for one extender attempt.** [redeploy] With the extender role on, proxy slots wait up to 20 seconds for a single extender activation attempt, then start. The hold is skipped when direct is off, there are no proxies, the provider store opens slowly, or legacy import adopted identities. The `[startup] extender head start` line says how the hold ended. (#342)

**We report the extender and store state on metrics.** [redeploy] The metrics endpoint gains `urnet_extender_*` gauges for the role, listening, activation, refusal and revocation. It also gains `urnet_state_store_degraded` and a parked-proxy pool count. (#340, #357)

**We corrected the proxy health line and slot numbers.** [redeploy] The health line counts only the proxies the pool runs, counts a parked proxy as parked rather than degraded, and drops the duplicate `down` field. The direct connection is slot 0 and proxies number from 1 in their configured order. (#343, #347, #348, #369)

**Degraded URL proxies leave only when the threshold is set.** [redeploy] A degraded URL proxy is retired only when `degraded_cleanup_threshold` is set. Its down clock starts when it is first observed and persists across restarts. An unset threshold removes nothing. (#344)

**We made urnet-tools safer to run.** Restart refuses a call with no unit name. `urnet-tools proxy trim` shows its cancel path and previews it. The macOS installer forwards its arguments to proxy refresh and remove-dead. These take effect when you update the tools, not when you restart the provider. (#367, #345, #372)

**We pinned connect and sdk to the 2026-11-05 fork mains.** [redeploy] The pins include the carrier-flap guard and the extender activation budget. (#364, #371, #373)

**We carry client UDP through proxied slots.** [redeploy] Client UDP now uses the proxy's SOCKS5 UDP relay. A failed relay check refuses the flow rather than falling back to the host, and IPv6 destinations are refused because the check proves only IPv4. Idle proxied flows close after 120 seconds; direct flows keep their 300-second default. (#387, #388)

**We made proxy refresh previews reflect cached sources.** [redeploy] Review additions and removals before confirming the refresh. Cached URL proxies and current admission rules are included. (#379)

**We bounded shutdown and made pressure recovery gradual.** [redeploy] The watchdog starts before the dump. A recovered thrash cap relaxes in stages based on health, not just elapsed time. (#350, #352, #363)

**We reduced recurring work and improved status.** [redeploy] Per-slot sampling shares a ticker, unchanged state writes are skipped, and discovery warnings reach the operator. Extender activation retries and token renewal are better spread. (#353, #375, #346, #341)

`[redeploy]` takes effect when you update and restart the provider.
