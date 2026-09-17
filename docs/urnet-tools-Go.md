# urnet-tools (Go): Provider-Aware Operations

> Applies to v2026 releases. The legacy shell tool (POSIX `Provider_Install_Linux.sh` plus Windows `urnet-tools.ps1`) is replaced by a single provider-aware Go binary. Subcommand names and usage are preserved and expanded. What changed is **how the tool decides which provider it operates on**.

This page is the condensed overview. The full command reference, targeting rules, and deep dives live on [urnet-tools-go](urnet-tools-go).

## Why this exists

The legacy `urnet-tools` resolved its target from a hardcoded path with zero awareness that other providers exist on the box. On a multi-provider machine it could act on the **wrong provider entirely**. The Go rewrite makes the tool's single most important guarantee structural: **it never guesses which provider you mean.**

## The two binaries

| Binary | What it manages |
|---|---|
| `urnet-tools` | Process/systemd providers (`--proxy_file`, internal config, systemd units) |
| `urnet-docker` | Docker-deployed providers (discovers containers, delegates via `docker exec`) |

Both are cross-compiled from one Go source. The shell/PowerShell drift is gone.

---

## Command Reference

### Core and Lifecycle Commands

| Command | What it does |
|---|---|
| `providers` (`list`, `ps`) | List providers with JWT identities, systemd units, and state directories. |
| `status [target]` | Show detailed status. On Linux it shows a live `systemctl status` view; on Windows/macOS it renders a styled panel. |
| `start [target]` | Start provider service/process. |
| `stop [target]` | Stop provider service/process. |
| `restart [target]` | Restart provider service/process. |
| `hot-restart [target]` | Restart provider unit behind a confirm gate (`-f` skips the prompt). |
| `reinstall [target]` | Cleanly reinstall the provider binary (delegates to the latest updater). |
| `uninstall [target]` | Uninstall provider, unit files, and optional state. Confirm-gated. |
| `update [target]` | Update provider to the latest release (or `--tag <version>`). Digest-verified. |
| `hotswap` (`hot-swap`) | Zero-downtime in-process binary reload: hands the live service to a verified candidate with no restart. |
| `self-update` (`selfupdate`) | Update the tool binary itself without touching running providers. |
| `logs [target] [N]` | Stream provider logs (N lines). RAMLOGS-aware. |
| `version` (`--version`, `-v`) | Print stamped binary version and build metadata. |

### Provider and Session Commands

| Command | What it does |
|---|---|
| `auth <code> [target] [-f]` | Authenticate provider with an auth code. `-f` forces overwrite of an existing JWT. Drops privileges to run as target user when called by root. |
| `direct [on\|off\|status] [target]` | Toggle or report direct/local IP providing state. Takes effect immediately via reload. |
| `show-ip [on\|off\|status] [target]` | Control whether the provider appends its public IP to the dashboard label. Renamed from `ip-detect`, which is kept as an alias. |
| `sn-status [--json] [target]` | Query and display mining and node telemetry: rank, tier eligibility, bandwidth provided, registered coldkey, epoch blocks, and payout share. |
| `usage [graphs\|graph <view>] [target]` | Display traffic and billing accounting: billable relay bytes vs control-plane protocol overhead. |
| `choose-network <api> <connect> [target]` | Point provider to custom API and WebSocket signaling endpoints. Use `--reset` to restore defaults. |
| `fast-auth [on\|off\|status] [target]` | Toggle or check `~/.urnetwork/fast_auth` marker to bypass the auth rate limiter. Confirm-gated. |
| `set [help \| <key> <val> \| <key> off \| <key>] [target]` | Get, set, or clear runtime provider state overrides. Sent live over the provider control socket, or queued to `pending_overrides.json` if the provider is stopped. Confirm-gated. |
| `rename <name> [target]` | Set the dashboard identity label on the backend. Alias for `set node-name <name>`. Writes `~/.urnetwork/node_name`, re-read on next tick. No restart. |
| `session save <file> [target]` | Export an encrypted identity bundle (AES-256-GCM) of provider JWT identity and state. Prompts for passphrase. |
| `session load <file> [target] [--allow-different-account]` | Decrypt and load an identity bundle into the provider. Automatically backs up current state first. |
| `self-heal [on\|off\|status] [target]` | Toggle or query the resource-pressure self-healing monitor (`~/.urnetwork/proxy_self_heal`). |
| `default [set <target> \| show \| clear]` | Persist, inspect, or clear the default provider target for the current user. |

### Proxy Management Commands

| Command | What it does |
|---|---|
| `proxy add <file\|url> [target]` | Merge proxies from text file (`host:port[:user:pass]`) or live URL. |
| `proxy paste [target]` | Stream raw proxies from stdin or a pipe without creating host files. |
| `proxy clear [target]` | Remove all proxies and URL sources. Confirm-gated (`-f` bypasses the prompt). |
| `proxy remove [addresses...] [target]` | Remove specific proxies or patterns. Use `--match=<pattern>` or `--all`. |
| `proxy trim <N> [target] [--preview]` | Set a persistent hard cap of `<N>` running proxies. `proxy trim off` clears the cap. |
| `proxy refresh [target] [--force]` | Reload the proxy list into the running provider without restarting. |
| `proxy add-source <url> [target]` | Add a live URL proxy source. Fetched and probed immediately. |
| `proxy remove-source <url> [target]` | Remove a URL proxy source. |
| `proxy ids [target]` | Show the `client_id` the platform assigned to each proxy. Bearer tokens are never printed. |
| `proxy health [target]` | Display live health state (Up, Down, Dead, Degraded). |
| `proxy traffic [target]` | Display bandwidth, billable traffic, and active NAT sessions per proxy. |
| `proxy remove-dead [target]` | Interactively prune dead and degraded proxies. Honors `--dry-run`. |
| `summary [target]` | Fleet-style summary of proxy counts by source (url, file, internal). Top-level command. |

### System and Performance Tuning

| Command | What it does |
|---|---|
| `auto [on\|off]` | Enable or disable the Smart Auto hardware profile. |
| `optimize [-f]` | Tune kernel parameters (conntrack, socket buffers, port ranges, BBR). Platform-aware, self-elevates when needed. |
| `eco [on\|off]` | Enable or disable the Eco profile (RAM-constrained hosts). |
| `turbo [v4\|v8\|off]` | Enable Turbo V4 or Turbo V8 high-throughput modes. |
| `ramlogs [on\|off]` | Enable or disable RAM-disk logging (`/dev/shm`). |
| `report <url>` | Set the live bandwidth reporting URL (`report off` disables). |
| `profile [name]` | Show or set the memory and GC tuning profile (`auto`, `turbo-v4`, `turbo-v8`, `eco`, `lowmem`). |
| `metrics [status\|on\|off\|listen <ip:port\|auto>]` | Show where the Prometheus `/metrics` endpoint listens, turn it on or off, or choose its listen address. Live, no restart, persisted. See [Monitoring](Monitoring). |

### Configuration and Introspection

| Command | What it does |
|---|---|
| `config [--json]` | Show every provider setting with the source it came from. |
| `set <key> <value>` | Set a runtime setting over the control socket. Queued to `pending_overrides.json` when the provider is down. |
| `history [limit]` | Show the provider's command audit trail from its 1000-entry circular ring. |
| `dashboard` | Rich terminal status panel. Aliases `dash`, `panel`. |

---

## Targeting and Selectors

| Flag | Selects by | Example |
|---|---|---|
| `--unit <name>` | systemd unit name (system or user) | `--unit=urnetwork-native.service` |
| `--user <user>` | OS user running the provider | `--user=urnet` |
| `--network <name>` | JWT network name (account) | `--network=alpha-fleet` |
| `--state-dir <path>` | Explicit state directory | `--state-dir=/home/urnet/.urnetwork` |

### Targeting Rules

1. **Multi-provider box + no target = REFUSAL.** The tool errors and displays an inventory table of available providers. It never guesses.
2. **Single provider + no target = AUTO-SELECT.** Proceeds after echoing the selected target.
3. **Persisted default provider:** if configured via `urnet-tools default set <target>`, the tool uses this target when no flag is passed, printing a visible notice to stderr.
4. **Explicit flags and `--all` override default:** `--unit`, `--user`, `--network`, and `--state-dir` take precedence over persisted defaults.
5. **Conflicting selectors** (for example `--unit foo --network bar` pointing at different instances) = ERROR.
6. **`-f` / `--force` only skips confirmation prompts.** It **never** selects a provider. To target all providers with force, use `-f --all`.
7. **`--help` always prints help** and never executes actions.

---

## Deep-Dive: Key Features

### 1. Persistent Proxy Trim (`proxy trim <N>`)

```bash
# Preview what proxies would be shed without making changes
urnet-tools proxy trim 500 --preview

# Set running proxy cap to 500
urnet-tools proxy trim 500

# Remove the cap
urnet-tools proxy trim off
```

- **A-F grade ranking:** sheds worst-graded proxies first using the provider's website-reachability probe scores (`dead` then `never-graded`, `F`, `D`, `C`, `B`, `A`).
- **Traffic tiebreaker:** proxies with active billable bandwidth are shed last within their grade tier.
- **Persistence:** stored at `~/.urnetwork/proxy_trim`, surviving provider restarts and reloads.
- **AIMD integration:** clamps the AIMD pool controller `TargetPoolSize` so automated pressure management works within the hard cap.

### 2. Session Save and Load

```bash
# Save encrypted identity bundle (AES-256-GCM, prompts for password)
urnet-tools session save /path/to/backup.urnsession

# Load identity bundle (automatically backs up existing state directory first)
urnet-tools session load /path/to/backup.urnsession
```

- **Format:** current bundles use AES-256-GCM (PBKDF2-HMAC-SHA256 key derivation). Legacy AES-256-CBC bundles remain loadable.
- **Pre-load safety backup:** automatically creates a timestamped copy of `~/.urnetwork/` before modifying live files.
- **Permission hardening:** unpacks files with `0700` directory permissions and `0600` file permissions.

### 3. Persisted Default Provider

```bash
# Set default provider by unit name
urnet-tools default set --unit urnetwork.service

# View current default
urnet-tools default show

# Clear default
urnet-tools default clear
```

---

## Safety and Security Guarantees

- **Mandatory digest verification:** `update` verifies downloads against release API SHA-256 checksums.
- **Isolated staging:** temporary files created in private `0700` directories.
- **Atomic binary replacement:** new executables staged as temporary files and renamed into place.
- **Privilege separation:** delegated commands automatically drop root privileges to the provider's UID/GID.

---

## Getting the Tool

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

For a full provider install (native or container), use:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
```