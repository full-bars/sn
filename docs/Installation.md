# 📦 Installation Guide

This guide covers the Linux installer, user-level systemd service, post-install commands, and host optimization tools. Alpine with OpenRC, FreeBSD and OpenBSD use a different service manager. They are covered under Other Platforms below.

## 🚀 Quick Start

The provider is designed to run as a **non-privileged user service** for maximum security and reliability.

> [!IMPORTANT]
> Recommended: run this command as your normal non-root user. If run as root, the installer will guide you through creating a dedicated service user named `urnet`.

Install:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
```

Uninstall:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Uninstall_Linux.sh | sh
```

### 🔑 Post-Install Authentication

After installation, source your terminal profile so the new commands are available in the terminal you installed from (new terminals, `ssh host urnet-tools ...`, cron and root find them without this step: the installer links `urnet-tools` and `urnetwork` into `~/.local/bin` and `/usr/local/bin` and writes the PATH block to `~/.bashrc`, `~/.profile` and `~/.zshenv`), and authenticate the provider. Then you can load your proxy list:

A direct unattended `provide` on a new state directory needs `--allow-client-registration` or an allowing `ALLOW_CLIENT_REGISTRATION` value such as `Y` or `auto`. With the variable unset or empty, a direct `provide` refuses registration. Docker instead defaults the variable to `auto`, which permits unattended registration, including when an identity is missing. Stop and restore the original state directory rather than registering a replacement. Interactive `provider start` asks before a first registration. The supplied systemd provider unit passes the flag; other service wrappers can differ.

```bash
source ~/.bashrc
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
```

> [!TIP]
> **Path Formatting**
> You can use either `~/proxies.txt` or `/home/you/proxies.txt`. Both syntaxes work.

> Full proxy-loading walkthrough (including Windows): [Adding Proxies](/full-bars/sn/wiki/Adding-Proxies).

### 🔒 What the installer verifies and how it replaces the binary

Before it extracts anything, the Linux and FreeBSD installers look up the release in the GitHub API and compare the SHA-256 of the downloaded provider tarball with the digest the release lists for it. A mismatch stops the install and prints the asset name, the expected digest and the digest it got. The check fails closed: if the release metadata cannot be fetched, or does not list the provider tarball (which is what a rate-limited API returns), the installer stops with `Refusing to install an unverified provider binary.` instead of installing without a check. Passing an explicit tag does not skip the lookup. The only case that continues is a release that lists the tarball without a digest because it predates digests; that prints a warning once and installs unverified. A digest that is listed but is not 64 hex characters (or is not a string) is not treated as absent: the install stops with `Refusing to install an unverified provider binary.`. The digest of the `urnet-tools-<os>-<arch>` asset is validated the same way, from the same metadata, before the provider is replaced or the service stopped, and a malformed one stops with `Refusing to install an unverified urnet-tools.`; a tool asset that lists no digest still falls back to the shell `urnet-tools`. Reading the metadata needs `jq` or `python3`; the Linux installer installs one when it can, and the FreeBSD installer stops and asks you to.

The new binary is staged beside the installed one and renamed into place, so the configured executable path is never missing. The previous binary is copied to `bin/urnetwork.old` first (a hard link to `bin/.urnetwork.bak.<pid>` on FreeBSD), and the installer refuses to replace the binary when that copy cannot be made. If the service then fails to start, the installer puts the previous binary back, restarts the service on it and exits with an error; the backup is deleted once the service has started. The `.version` and `.date` files (`version` on FreeBSD) are written only after that start, so they never name a release that failed to start.

## 🧭 Other Platforms (Quick install)

- **Alpine Linux (OpenRC):** the same install command, run from a root shell, installs a system service under `supervise-daemon`. See the [Alpine Linux (OpenRC) guide](https://github.com/full-bars/sn/wiki/OpenRC).
- **FreeBSD:** installs an `rc.d` service from the FreeBSD installer script. The base system is enough to download: the installer uses `curl` when it is installed, then `fetch`, then `wget`. From a root shell (`su -`) name a non-root account that already exists, because the installer refuses to run the service as root and will not create the account: `fetch -qo urnet-install.sh https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_FreeBSD.sh && SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install`. As a normal user, install `sudo` first (`pkg install -y sudo`) and prefix the second command with it. See the [FreeBSD guide](https://github.com/full-bars/sn/wiki/FreeBSD).
- **OpenBSD:** installs an `rc.d` service managed with `rcctl` from the OpenBSD installer script. It downloads with `ftp` from the base system and needs `jq` or `python3` to read the release digests (`pkg_add jq`). Install `doas` or `sudo` access first, or run from a root shell with `SERVICE_USER_OVERRIDE=<user>`. See the [OpenBSD guide](https://github.com/full-bars/sn/wiki/OpenBSD).

## 🍎 macOS Installation

The macOS installer is the equivalent of the Linux installer but uses `launchd` instead of `systemd`:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Mac.sh | sh -s -- install
```

Uninstall (manual: macOS uninstall script not yet available):

```bash
# Remove binary and service files
rm -rf ~/.local/share/urnetwork-provider
launchctl unload ~/Library/LaunchAgents/com.urnetwork.provider.plist 2>/dev/null
rm -f ~/Library/LaunchAgents/com.urnetwork.provider.plist
# Remove identity and proxy data (⚠️ deletes all JWTs and proxy state)
rm -rf ~/.urnetwork
# Remove PATH additions from ~/.bashrc / ~/.zshrc
```

### What gets installed

| Component | Location |
|-----------|----------|
| Provider binary | `~/.local/share/urnetwork-provider/bin/urnetwork` |
| launchd plist | `~/Library/LaunchAgents/com.urnetwork.provider.plist` |
| Logs | `~/Library/Logs/com.urnetwork.provider/stdout.log` + `stderr.log` |
| State directory | `~/.urnetwork/` (same as Linux) |

### Post-install commands

Most `urnet-tools` commands work the same as on Linux. See the note below for `eco`, `ramlogs` and `optimize`:

```bash
# Start/stop
urnet-tools start
urnet-tools stop
urnet-tools restart
urnet-tools status

# Zero-downtime binary reload
urnet-tools hotswap

# Session save/load
urnet-tools session save backup.urnsession
urnet-tools session load backup.urnsession

# Auth and proxy
urnetwork auth
urnet-tools proxy add ~/proxies.txt
urnet-tools proxy refresh
urnet-tools summary
```

> [!NOTE]
> `optimize` applies the macOS (BSD) kernel settings with `sysctl`. It sets the socket buffers, the file limits, the ephemeral port range and TIME_WAIT recycling. It needs root or `sudo`. The settings apply at once and do not survive a reboot. `eco` and `ramlogs` are not supported on macOS.

## 🔐 User-Level Systemd Service
Unlike traditional services that run as root, this build defaults to a **systemd user unit**.

- **Security:** the provider binary does not need root privileges.
- **Isolation:** configuration and JWT tokens are stored in the user's home directory.
- **Linger:** the installer enables `loginctl enable-linger`, so the provider starts automatically on boot and keeps running after logout.
- **Root guard:** if installed as root, the script can create a restricted `urnet` user and add it to the appropriate admin group.

## 🛠️ Post-Install Commands

The installation includes the `urnet-tools` suite for management, a provider-aware Go binary. On multi-provider machines, pass a target (`--unit` / `--user` / `--network` / `--state-dir`) or the tool refuses. See [urnet-tools-go.md](/full-bars/sn/wiki/urnet-tools-go).

| Command | Description |
| :--- | :--- |
| `urnet-tools status` | Check service health and uptime. |
| `urnet-tools logs` | Stream logs, automatically detecting RAM vs disk logging. |
| `urnet-tools auto on` | Enable Smart Auto. Recommended for most hosts. |
| `urnet-tools optimize` | Full host optimization for many-proxy deployments and high-volume traffic. Add `-f` to skip prompts. |
| `urnet-tools turbo v4` | Enable Turbo V4 mode. |
| `urnet-tools turbo v8` | Enable Turbo V8 mode. |
| `urnet-tools eco on/off` | Toggle Eco mode. |
| `urnet-tools ramlogs on/off` | Toggle RAM-disk logging independently. |
| `urnet-tools lowmode on/off` | Turn low-memory mode on or off. It reduces buffers to save RAM and restarts the provider, so it asks for a typed "yes". Add `-f` to skip the question. |
| `urnet-tools autopilot log` | Show the newest capacity decisions of the provider. See [Monitoring](Monitoring#records-of-capacity-decisions-and-enforcement). |
| `urnet-tools enforcement` | Show what the governors did: a summary per day, or the newest events with `history N`. |
| `urnet-tools update` | Upgrade to the latest version (prompts before restarting the provider). |
| `urnet-tools update -f` | Non-interactive upgrade: stop, update, and restart the provider with no prompts. Use in scripts/automation. |

When a newer tool is available, `urnet-tools update` installs it first and runs it again to update the provider, so the new release's own update logic does the work. The second run keeps the target and the approval you already gave. It also keeps the exact provider set the first run inspected, and it stops without a change if that set differs. If the tool install or the second run fails, the running tool carries on. The marker `URNET_TOOLS_UPDATE_REEXEC` stops a loop. The first update to this release still runs in the old tool. A provider with the port-443 file capability can make that old tool report a false failure once, because it cannot read the running image. Run `urnet-tools self-update` before the provider update to avoid it.

For the Linux installer, restart confirmation reads the terminal even when the script arrives through a pipe. Without a terminal, the installer stops and tells you to pass `-y`, before or after the subcommand. Approval carries through `sudo`. OpenRC tree preparation must finish before the service is installed, and a failure stops the install. The installer writes a systemd unit with `Type=notify` and `NotifyAccess=all`. It prepares the new unit and keeps the old one as `<unit>.old` before it swaps the binary. If the restart fails, the installer restores the previous unit with the previous binary, and it prints the exact command when a step cannot finish. The optimizer lists the steps it finished, skipped and failed, with a retry command for each failure. On an interactive terminal, the installer offers to grant the extender's port-443 file capability, and it preserves any other file capabilities when it does. It prints the manual `setcap` command when you decline or no terminal exists.

## 🪟 Windows Installation

Install via PowerShell (no admin required):

```powershell
irm https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Win32.ps1 | iex
```

Windows Defender may flag this one-liner. See the note below.

> [!NOTE]
> Windows Defender may flag the Windows install one-liner, and it may flag the downloaded
> binaries. What we see are machine-learning heuristics (the `!ml` suffix), not signatures;
> for the binaries we publish they are false positives. Recent release pages record the scan
> results for the published binaries. If Defender blocks the one-liner, download the script,
> review it, and run it from disk instead. If Defender quarantines an extracted binary,
> allow it from Windows Security > Virus & threat protection > Protection history. Both lines
> go in PowerShell:
>
> ```powershell
> irm https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Win32.ps1 -OutFile "$env:TEMP\install-win.ps1"
> powershell -NoProfile -ExecutionPolicy Bypass -File "$env:TEMP\install-win.ps1"
> ```

<details>
<summary>The detections you may see, and what each one means</summary>

- `Trojan:Script/Wacatac.B!ml`, `Trojan:Script/Wacatac.C!ml`: Defender's machine-learning label for the PowerShell installer script fetching and extracting a remote payload.
- `Trojan:Win32/Wacatac.B!ml`, `Trojan:Win32/Wacatac.C!ml`: Defender's machine-learning label for files whose shape looks like a packed trojan. Our Go binaries are stripped, statically linked and unsigned, which reads as a packed payload. The B and C variants are different model generations, so one binary can be flagged under more than one name. `Trojan:Win32/Execution.A!ml` is another label from the same family on some builds.
- `Trojan:Win32/Commando.A!ml`: fires on the download-and-run command line itself (the `irm ... | iex` one-liner), not on the installed files. Fetching a remote script and piping it into execution reads as a trojan-downloader pattern to the model.
- `Trojan:Win32/Bearfoos.A!ml`: a behavioural label for scheduled-task activity. The installer registers Task Scheduler tasks — a weekly update task (on `latest` installs) and, if you accept auto-start, a logon task so the provider starts at login — and a behavioural model cannot tell that apart from persistence malware.

</details>

Uninstall via PowerShell (no admin required):

```powershell
irm https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Uninstall_Win32.ps1 | iex
```

### What gets installed

| Component | Location |
|-----------|----------|
| Provider binary | `%LOCALAPPDATA%\urnetwork\provider\urnetwork.exe` |
| Management tool | `urnet-tools` (Go binary) |
| State directory | `%USERPROFILE%\.urnetwork\` |
| Startup (optional) | Task Scheduler logon task `urnetwork-autostart` |
| PATH | User PATH updated to include `%LOCALAPPDATA%\urnetwork\provider\` |

### Post-install commands

```powershell
# Authenticate
urnetwork auth

# Start in foreground
urnetwork provide

# Start in background
urnet-tools start

# Manage proxies
urnet-tools proxy add "$env:USERPROFILE\Downloads\proxies.txt"
urnet-tools proxy refresh
urnet-tools summary

# View logs
urnet-tools logs

# Session save/load
urnet-tools session save C:\Users\You\backup.urnsession
urnet-tools session load C:\Users\You\backup.urnsession

# Update
urnet-tools update
```

Zero-downtime `urnet-tools hotswap` is not available on Windows in this release. Both the CLI and the running provider decline it before anything is started, with an error that says supervisor adoption is not implemented on Windows, and the running provider stays as it is. A Windows update is a normal stop and start. The reason is that the bundled supervisor waits on the original miner command and returns when it exits cleanly, so a successor provider started by a handoff would not be supervised.

> See [Adding Proxies](/full-bars/sn/wiki/Adding-Proxies) for per-OS proxy-loading instructions and the Windows `.txt.txt` extension trap.

### `urnet-tools` on Windows

`urnet-tools` runs natively on Windows. It needs no `tail`, no `sudo` and no WSL: the log views use a built-in follower that survives log rotation, `urnet-tools start` creates `%USERPROFILE%\.urnetwork` before it launches the provider, and the commands that would need `sudo` on Linux are skipped. Rotating, clearing or updating a log no longer fails with "being used by another process", because the follower opens it with delete sharing.

When the provider runs under its per-user service (`urnetwork-provider`, set up by `provider service install`), `urnet-tools start`, `stop` and `restart` act on that service, not on the provider process alone. This matters because the service starts the provider again whenever it exits: stopping only the process would last about 15 seconds. `urnet-tools stop` stops the service and the provider together, and `urnet-tools start` starts the service again. A provider that the service does not run is still started and stopped directly.

These rules apply to a provider that the service runs:

- `start` and `restart` wait up to 30 seconds for the provider to come up. They report success only when it is up. If it does not come up in time, they say so and show the path to the service log.
- `stop` makes sure the provider process ended. If the provider is still running after the service stopped, `stop` ends it. If it cannot end it, `stop` reports an error.
- `--force` still goes through the service. It gives the service a short wait instead of the normal one, then ends the provider if it is still running.
- `uninstall` removes nothing if the provider did not stop.

`urnet-tools` must know whether the service runs the provider. If it cannot tell (for example, the service settings cannot be read), `start`, `stop`, `restart` and `uninstall` change nothing. They show the problem and what to do next.

`uninstall` also refuses when another running provider uses the same program file or the same state folder. It names that provider. Stop that provider first, then run `uninstall` again.

### 📦 Release archives

The [GitHub release page](https://github.com/full-bars/sn/releases) also offers provider archives and standalone `urnet-tools` binaries for Windows. The PowerShell installer above downloads the release assets; it is not an offline installer.

## 📊 System Auditor & Host Optimization

When the provider starts, it logs a **System Auditor** report that checks kernel limits and disk I/O performance:

```text
[audit] Conntrack Max: 262144 (Suboptimal! Target: 2097152)
[audit] Hint: Container detected suboptimal host limits. Run 'urnet-tools optimize' on the HOST to fix.
```

> [!WARNING]
> The provider cannot modify host-level kernel settings from inside a container. Run `urnet-tools optimize` on the host machine when deploying many proxies, or whenever you see `Suboptimal!` warnings.

For Docker-only users who do not want the systemd provider service, run the installer on the host to install the tools:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
```

Then optimize the host:

```bash
urnet-tools optimize -f
```

`optimize` re-executes itself under `sudo` with its own resolved binary path when it needs root, so you don't have to type `sudo /path/to/urnet-tools` (and it does NOT work as bare `sudo urnet-tools`: the binary lives on a per-user path, not root's PATH).

The `-f` flag skips interactive prompts. This applies:

- Conntrack max: `262144` -> `2097152`
- Conntrack timeout: `432000s` -> `5400s`
- TCP established timeout: 5 days -> 1 hour
- BBR congestion control and Fair Queuing
- Auto-install of `zram` and `conntrack-tools`
- Boot persistence for kernel modules

After optimization, your Docker container should restart and report:

```text
[audit] Conntrack Max: 2097152 (Optimal!)
```

> [!NOTE]
> If you only run Docker and do not want the systemd provider service, run the installer as root on a systemd host and choose the tools-only option. Without a terminal, a root install installs the tools and skips the service by itself.