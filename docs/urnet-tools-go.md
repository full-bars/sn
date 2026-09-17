# urnet-tools (Go): Provider-Aware Operations

> Applies to v2026 releases. The legacy shell tool (POSIX `Provider_Install_Linux.sh` plus Windows `urnet-tools.ps1`) is replaced by a single provider-aware Go binary. Subcommand names and usage are preserved and expanded. What changed is **how the tool decides which provider it operates on**.

## Why this exists

The legacy `urnet-tools` resolved its target from a hardcoded path (`$HOME/.local/share/urnetwork-provider`) with zero awareness that other providers exist on the box. On a multi-provider machine it could act on the **wrong provider entirely**. The Go rewrite makes the tool's single most important guarantee structural: **it never guesses which provider you mean.**

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
| `providers` (`list`, `ps`) | List providers. Your own OS user's by default, or all providers on the box with `--all` (JWT identities, systemd units, state dirs). |
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
| `direct [on\|off\|status] [target]` | Toggle or report direct/local IP providing state. Takes effect immediately via reload. Available across `provider`, `urnet-tools`, and `urnet-docker`. |
| `show-ip [on\|off\|status] [target]` | Control whether the provider appends its public IP to the dashboard label set by `rename`. Renamed from `ip-detect`, which is kept as an alias. This is about what the dashboard shows, not which address the provider serves on; for that see `direct`. |
| `sn-status [--json] [target]` | Query and display mining and node telemetry: global rank, tier eligibility, net bandwidth provided, registered coldkey, current epoch blocks, and finalized epoch pool payout share. Available across `urnet-tools`, `urnet-docker`, and `provider`. |
| `usage [graphs\|graph <view>] [target]` | Display traffic and billing accounting: billable relay bytes vs control-plane protocol overhead, with rolling time-series summaries. |
| `choose-network <api> <connect> [target]` | Point provider to custom API and WebSocket signaling endpoints. Use `--reset` to restore the default endpoints. |
| `fast-auth [on\|off\|status] [target]` | Toggle or check `~/.urnetwork/fast_auth` marker to bypass the auth rate limiter. Confirm-gated. |
| `set [help \| <key> <val> \| <key> off \| <key>] [target]` | Get, set, or clear runtime provider state overrides (`node-name`, `report-interval`, `proxy-url-max`, `proxy-url-refresh`, `cleanup-scope`, `cleanup-interval`, `fast-auth`). Sent live over the provider control socket, or queued to `pending_overrides.json` if the provider is stopped. Confirm-gated. |
| `rename <name> [target]` | Set the dashboard identity label on the backend. Alias for `set node-name <name>`. Writes `~/.urnetwork/node_name`, re-read on next tick. No restart. Use `off` to clear. |
| `session save <file> [target]` | Export an encrypted identity bundle (AES-256-GCM) of provider JWT identity and state. Prompts for passphrase. |
| `session load <file> [target] [--allow-different-account]` | Decrypt and load an identity bundle into the provider. Automatically backs up current state first. Verifies account identity unless bypassed. |
| `self-heal [on\|off\|status] [target]` | Toggle or query the resource-pressure self-healing monitor (`~/.urnetwork/proxy_self_heal`). |
| `default [set <target> \| show \| clear]` | Persist, inspect, or clear the default provider target for the current user. |

### Proxy Management Commands

| Command | What it does |
|---|---|
| `proxy add <file\|url> [target]` | Merge proxies from text file (`host:port[:user:pass]`) or live URL. Supports paths with `~`, URLs (auto-routed to `add-source`), and flags (`--file=`, `--proxy_file=`, `--url=`). |
| `proxy paste [target]` | Stream raw proxies from stdin or a pipe without creating host files. Auto-detects formats and URLs. |
| `proxy clear [target]` | Remove all proxies and URL sources. Confirm-gated (`-f` bypasses the prompt). |
| `proxy remove [addresses...] [target]` | Remove specific proxies or patterns. Use `--match=<pattern>` for host substring matches, or `--all` for a complete wipe. |
| `proxy trim <N> [target] [--preview]` | Set a persistent hard cap of `<N>` running proxies. Sheds worst A-F reachability graded proxies first. `proxy trim off` clears the cap. |
| `proxy refresh [target] [--force]` | Reload the proxy list into the running provider without restarting. `--force` bypasses the warmup lockout. |
| `proxy add-source <url> [target]` | Add a live URL proxy source. Fetched and probed immediately. |
| `proxy remove-source <url> [target]` | Remove a URL proxy source. |
| `proxy ids [target]` | Show the `client_id` the platform assigned to each proxy, including the `direct` transport. Read from the provider's local client-JWT store. The bearer tokens themselves are never printed. |
| `proxy health [target]` | Display live health state (Up, Down, Dead, Degraded). |
| `proxy traffic [target]` | Display bandwidth, billable traffic, and active NAT sessions per proxy. |
| `proxy remove-dead [target]` | Interactively prune dead and degraded proxies. Honors `--dry-run`. |
| `summary [target]` | Fleet-style summary of proxy counts by source (url, file, internal). Top-level command, not a `proxy` subcommand. |

Exclusion uses `proxy remove --match=<pattern>`. Matching removes proxies and persists the pattern so future URL refreshes skip them. There is no `proxy exclude` subcommand.

### System and Performance Tuning

| Command | What it does |
|---|---|
| `auto [on\|off]` | Enable or disable the Smart Auto hardware profile. |
| `optimize [-f]` | Tune kernel parameters (conntrack, socket buffers, port ranges, BBR). Platform-aware. Self-elevates to root when needed, applies live and persists atomically, or rolls back. |
| `eco [on\|off]` | Enable or disable the Eco profile (RAM-constrained hosts). |
| `turbo [v4\|v8\|off]` | Enable Turbo V4 or Turbo V8 high-throughput modes. |
| `ramlogs [on\|off]` | Enable or disable RAM-disk logging (`/dev/shm`). |
| `report <url>` | Set the live bandwidth reporting URL (`report off` disables). |
| `profile [name]` | Show or set the memory and GC tuning profile (`auto`, `turbo-v4`, `turbo-v8`, `eco`, `lowmem`; `v4` and `v8` are accepted aliases). With no argument, prints the current profile and what each one is for. |
| `metrics [status\|on\|off\|listen <ip:port\|auto>]` | Show where the Prometheus `/metrics` endpoint listens and the address to scrape, turn it on or off, or choose its listen address. Live, no restart, persisted. See [Monitoring](Monitoring). |

### Configuration and Introspection

| Command | What it does |
|---|---|
| `config [--json]` | Show every provider setting with the source it came from (`socket`, `env`, `pending`, `legacy`, `default`). This is what the provider actually believes. |
| `set <key> <value>` | Set a runtime setting over the control socket. Queued to `pending_overrides.json` when the provider is down. |
| `set <key>` | Show one setting's current value. Reads are not logged at the provider, because `status` polls them on every invocation. |
| `set <key> off` | Clear a runtime setting and restore its default. Same queueing behavior. |
| `history [limit]` | Show the provider's command audit trail from its 1000-entry circular ring. Defaults to the last 50, maximum 100. |
| `dashboard` | Rich terminal status panel: state indicators, active settings, proxy sources, restart warnings. Aliases `dash`, `panel`. |

---

## Targeting and Selectors

The tool accepts selectors in both space-separated and equals-separated format (`--flag value` or `--flag=value`):

| Flag | Selects by | Example |
|---|---|---|
| `--unit <name>` or `--unit=<name>` | systemd unit name (system or user) | `--unit=urnetwork-native.service` |
| `--user <user>` or `--user=<user>` | OS user running the provider | `--user=urnet` |
| `--network <name>` or `--network=<name>` | JWT network name (account) | `--network=alpha-fleet` |
| `--state-dir <path>` or `--state-dir=<path>` | Explicit state directory | `--state-dir=/home/urnet/.urnetwork` |

### Targeting Rules

1. **One provider per OS user** is the supported deployment model. An unprivileged `urnet-tools` command with no explicit target resolves to the provider owned by your own OS account.
2. **Unprivileged, no target, exactly one provider for your user = AUTO-SELECT.** The tool acts on it with minimal commentary. It deliberately does not enumerate other users' providers on every command.
3. **Unprivileged, no target, more than one provider for your user = REFUSAL**, with a short inventory of *your* providers (you broke the one-per-user contract), pointing at `urnet-tools providers`, `default set --network <name>`, or `--unit <unit>`. The tool never guesses.
4. **Unprivileged, no provider for your user = error** saying so, pointing at `urnet-tools providers --all` (as root) to see other users' providers.
5. **`providers` shows your providers by default; `providers --all` (root) shows every provider on the box.** It is the single place a multi-user inventory is visible.
6. **Explicit target always wins.** `--unit`, `--user`, `--network`, `--state-dir` resolve exactly, never narrowed.
7. **Root, no target, more than one provider total = REFUSAL** with the full inventory. Root must name a target or pass `--all`.
8. **Persisted default provider:** `urnet-tools default set <target>` pins an implicit target for future no-flag commands, printing a visible notice to stderr. It only fills the "no target" gap.
9. **Conflicting selectors** (for example `--unit foo --network bar` pointing at different instances) = ERROR.
10. **`-f` / `--force` (or `-y` / `--yes`) only skips confirmation prompts.** It **never** selects a provider. To target all providers with force, use `-f --all`.
11. **`--help` always prints help** and never executes actions.

---

## Deep-Dive: Key Features

### 1. Persistent Proxy Trim (`proxy trim <N>`)

`urnet-tools proxy trim <count>` sets a persistent hard cap on the number of running proxies:

```bash
# Preview what proxies would be shed without making changes
urnet-tools proxy trim 500 --preview

# Set running proxy cap to 500
urnet-tools proxy trim 500

# Remove the cap
urnet-tools proxy trim off
```

- **A-F grade ranking:** sheds worst-graded proxies first using the provider's website-reachability probe scores (`dead` then `never-graded`, `F`, `D`, `C`, `B`, `A`).
- **Traffic tiebreaker:** proxies with active billable bandwidth are shed last within their grade tier, preserving active earning connections.
- **Persistence:** stored at `~/.urnetwork/proxy_trim`, surviving provider restarts and reloads.
- **AIMD integration:** clamps the AIMD pool controller `TargetPoolSize` so automated pressure management works within the hard cap.

### 2. Session Save and Load

Securely back up, migrate, or clone provider identities:

```bash
# Save encrypted identity bundle (AES-256-GCM, prompts for password)
urnet-tools session save /path/to/backup.urnsession

# Load identity bundle (automatically backs up existing state directory first)
urnet-tools session load /path/to/backup.urnsession

# Load onto a host with a different account identifier
urnet-tools session load /path/to/backup.urnsession --allow-different-account
```

- **Format:** current bundles use AES-256-GCM (PBKDF2-HMAC-SHA256 key derivation, random salt and nonce). Legacy AES-256-CBC bundles remain loadable.
- **Pre-load safety backup:** automatically creates a timestamped copy of `~/.urnetwork/` (for example `~/.urnetwork.bak.1724288000`) before modifying live files.
- **Permission hardening:** unpacks files with `0700` directory permissions and `0600` file permissions, automatically chowning them to the unit owner when run with elevated privileges.

### 3. Persisted Default Provider

Avoid passing `--unit` or `--network` on every invocation:

```bash
# Set default provider by unit name
urnet-tools default set --unit urnetwork.service

# View current default
urnet-tools default show

# Clear default
urnet-tools default clear
```

### 4. Control Socket and Queued Overrides

Runtime settings no longer go through hand-edited systemd drop-ins. The provider owns a Unix domain socket in the state directory (`~/.urnetwork/provider.sock`, owner-only) and is the single writer of `~/.urnetwork/provider_state.json`.

```bash
# Change a setting on a running provider. No restart.
urnet-tools set report-interval 300

# Read one back
urnet-tools set report-interval

# Clear it
urnet-tools set report-interval off
```

**When the provider is stopped**, the change is written to `~/.urnetwork/pending_overrides.json` instead (flock-guarded, so the CLI and the installer's shell helpers cannot lose each other's writes) and merged atomically on the next start.

**Confirming the change registered.** The CLI's exit code only tells you the request was accepted. The provider logs the change itself, which is the authoritative confirmation:

```text
[control] set report-interval=300 rejected: <error>
[control] set gogc=on failed to persist, rolled back: <error>
```

Rejected changes log too, so a setting that did not take explains itself.

`rename` and `show-ip` do not go through the socket. Both take effect at the next renewal, and the provider reports the resulting dashboard label when it changes.

Reads are deliberately not logged: `urnet-tools status` polls the socket on every invocation, so logging them would bury the writes that matter.

> [!TIP]
> `urnet-tools status` also reports whether the control socket is actually bound. A running PID with no reachable socket means a startup failure or a same-user collision, not a healthy provider.

---

## Safety and Security Guarantees

- **Mandatory digest verification:** `update` verifies downloads against release API SHA-256 checksums.
- **Isolated staging:** temporary files created in private `0700` directories.
- **Atomic binary replacement:** new executables staged as temporary files and renamed into place, preventing truncation of running binaries.
- **Privilege separation:** delegated commands automatically drop root privileges to the provider's UID/GID.

---

## Getting the Tool

Install the process/systemd tool or the Docker host tool via the install script:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/install-urnet-docker.sh | sh
```

For a full provider install (native or container), use the main installer:

```bash
curl -fSsL https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh | sh
```