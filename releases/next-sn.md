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

**I made uninstall and capability checks safer.** [redeploy] The Windows PowerShell uninstaller recognizes the legacy install path and refuses to remove data if another provider is running or process checks fail. Linux updates check the bounding set before applying the extender bind capability. The installer preserves unrelated file capabilities, and the miner image includes the capability tools. (#391)

**Proxied UDP guidance is corrected.** An unknown check result can allow an unverified proxy exit. Proxied flows close after 120 seconds without sends or without replies; a known mismatch or failed relay check refuses the flow without falling back to the host address. This is a documentation correction, not a new behavior change. ([d2b00edd9](https://github.com/full-bars/sn/commit/d2b00edd9c22f44b0036fd708a8f3c045f06b2b2))

**SN25 live figures are refreshed.** See the [head-tier page](https://github.com/full-bars/sn/blob/main/docs/Bittensor-Head-Tier.md) for the latest snapshot. ([10e388054](https://github.com/full-bars/sn/commit/10e388054))

`[redeploy]` takes effect when you update and restart the provider.
