<!-- DRAFT. At tag time: rename this file to releases/<tag>.md, put the tag in the title and today's date in Released, then delete this comment. -->

# NEXT_TAG

**Released:** NEXT_DATE.

Previous release: `v2026.11.9-1094091330-sn`.

This release adds OpenBSD provider and `urnet-tools` support, moves the `connect` and `sdk` pins to the merged fork main branches, and moves the build to Go 1.27.4. The `sdk` pin also puts the extender activation pacing hook back on the fork main. Update and restart.

## Quick start

Already running a provider? Update in place:

```sh
urnet-tools update
```

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

On FreeBSD, download the installer and run it with a non-root service account selected:

```sh
fetch -qo urnet-install.sh https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_FreeBSD.sh
sudo sh urnet-install.sh install
```

Start a new provider container:

```sh
docker run -d --name urnetwork --restart unless-stopped \
  -e URNETWORK_AUTH_CODE=YOUR_AUTH_CODE_HERE \
  -e ALLOW_CLIENT_REGISTRATION=Y \
  -v urnetwork_config:/root/.urnetwork \
  ghcr.io/full-bars/sn:latest
```

Auth codes are single-use. Keep the state volume. If an existing identity is missing, restore its original volume instead of authorizing a replacement. For optional extender ports, see the [extender guide](https://github.com/full-bars/sn/wiki/Extender).

## What changed

### OpenBSD
- **OpenBSD provider and `urnet-tools` support. [redeploy] [installer]** The provider and `urnet-tools` build for OpenBSD on amd64 and arm64. There is an installer, `rcctl` service setup, cron auto-update and release assets. `urnet-tools` tells an absent service from an ambiguous process match before it runs `rcctl`, keeps manually started providers on the direct stop path, and removes the cron entry of a bare provider.

### Build and dependencies
- **The `connect` pin moves to the fork main at `cb9728fb5`. [redeploy]** It is the fork merged with upstream connect main `0d6d5d001`, 207 upstream commits past the previous pin. The miner does not run the multi client window, so the provider selection change reaches a miner through the network operator's server. This pin does not carry it.
- **The `sdk` pin moves to the fork main at `c364d632`. [redeploy]** It names the same `connect` commit and brings the extender activation pacing hook back onto the fork main. The previous pin pointed at a commit that was on no branch.
- **The build is Go 1.27.4. [redeploy]** The new `connect` requires it.

### Fixes
- **The `urnet-tools` root help lists `extender about`. [redeploy]**
- **The BSD cron log path uses forward slashes on every build host. [no redeploy]**

### Documentation [no redeploy]
- The operations guide lists the operator notices that touch a provider or miner, as of 2026-11-13.
- The extender guide says what the extender pool announced for epoch 8 means for the role.

## Known limits

- The OpenBSD runtime was not run on a real OpenBSD host, and durable client registration is unavailable there.
- The upstream `sdk` features that need new sdk code, such as the extender WebRTC carrier and router port mapping, are not in this release. The sdk pin is the fork main without the upstream sdk sync.
