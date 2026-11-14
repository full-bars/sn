<!-- DRAFT. At tag time: rename this file to releases/<tag>.md, put the tag in the title and today's date in Released, then delete this comment. -->

# NEXT_TAG

**Released:** NEXT_DATE.

Previous release: `v2026.11.9-1094091330-sn`.

This release adds OpenBSD provider and `urnet-tools` support. It moves the `sdk` pin to a commit on the fork main branch and moves the build to Go 1.27.4. The `connect` pin does not move.

Update and restart your provider. OpenBSD support is experimental, so read its limits before you install there.

Legend: **[redeploy]** needs the new provider, tool or container image. **[installer]** needs the updated installer or script. **[no redeploy]** covers documentation, CI and tests.

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

OpenBSD support is experimental. The installer and the service have not run on an OpenBSD host. Durable client registration is unavailable there. Do not deploy OpenBSD providers to production yet.

On OpenBSD, the installer needs `jq` or `python3` to read the release metadata. Install `jq`, download the installer and run it with an existing non-root service account (replace `urnet` with its name):

```sh
doas pkg_add jq
ftp -o urnet-install.sh https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_OpenBSD.sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

The installer does not sign the provider in. Sign in as the service account with a single-use auth code from https://ur.io, then restart the service. The OpenBSD guide has the exact commands.

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
- **OpenBSD provider and `urnet-tools` support. [redeploy] [installer]** The provider and `urnet-tools` build for OpenBSD on amd64 and arm64. There is an installer, `rcctl` service setup, cron auto-update and release assets.
  - `urnet-tools` tells an absent service from an ambiguous process match before it runs `rcctl`.
  - It keeps manually started providers on the direct stop path.
  - It removes the cron entry of a bare provider.

### Build and dependencies
- **The `sdk` pin moves to the fork main at `5c905e84`. [redeploy]** It has the same code as the previous pin, which was on no branch, and it keeps the extender activation pacing hook that the miner uses.
- **The build is Go 1.27.4. [redeploy]**

### Fixes
- **The `urnet-tools` root help lists `extender about`. [redeploy]**
- **The BSD cron log path uses forward slashes on every build host. [no redeploy]**
- **The extender setup elevation test skips on platforms that keep every provider. [no redeploy]** macOS does not narrow the provider list, so the test failed there.
- **A shutdown during URL source probing no longer logs a source failure. [redeploy]** The probe reports every new line dead once its context is cancelled. The provider took that for a failing source and logged a warning. It now logs nothing.

### CI [no redeploy]
- The churn soak compares settled samples per running slot and names the goroutines that grew. A failure keeps the goroutine profiles as an artifact.
- The distro matrix checks the shipped `Type=notify` unit and runs a full Arch upgrade. The matrix is green.

### Documentation [no redeploy]
- The monitoring guide covers `urnet-tools autopilot log` and `urnet-tools enforcement`. The configuration guide covers the `oom-cap`, `gomemlimit` and `metrics-listen` keys. The install guide lists `urnet-tools lowmode`.
- The operations guide lists the operator notices that touch a provider or miner, as of 2026-11-13.
- The extender guide says what the extender pool announced for epoch 8 means for the role.
- The CI guides match the workflows: the PR soak churn check, the installer check on pull requests and the release build matrix.

## Known limits

- The OpenBSD runtime was not run on a real OpenBSD host, and durable client registration is unavailable there.
- The `connect` pin stays at the fork main `8e078bbca`. The merged fork main with upstream connect `0d6d5d001` is held back until more soak runs confirm its memory use. Two runs on the new soak check passed.
- The source-graph tests for SCTP, gVisor and the substrate RPC fork need sibling checkouts. They fail in a plain worktree.
- `scripts/mainnet-release-build/pin.sh` is the network operator's release tooling. It still expects the upstream `sdk` module path. The fork's workflows do not run it.
- The upstream `sdk` features that need new sdk code, such as the extender WebRTC carrier and router port mapping, are not in this release. The sdk pin is the fork main without the upstream sdk sync.
