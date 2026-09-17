# Installer Porting Notes: Provider_Install_Linux.sh

Ported installer script from `urnetwork-3.23-fix` to `h3-provider` (`full-bars/sn`).

- **Source:** `/home/klets/ur/urnetwork-3.23-fix/scripts/Provider_Install_Linux.sh`
- **Destination:** `/home/klets/ur/h3-provider/scripts/Provider_Install_Linux.sh`

---

## 1. Summary of Changes Made

| Line(s) | Description of Change | Previous Value | New Value |
|---|---|---|---|
| **2** | Script header banner updated to identify v2026 | `# urnet-tools: URnetwork provider manager` | `# urnet-tools: URNetwork Provider (v2026)` |
| **5** | Upstream repository URL updated | `# https://github.com/full-bars/urnetwork-3.23-fix` | `# https://github.com/full-bars/sn` |
| **16** | Help menu banner added to `show_help()` | *(None)* | `echo "URNetwork Provider (v2026)"` |
| **69** | Support URL in help text updated | `https://github.com/full-bars/urnetwork-3.23-fix` | `https://github.com/full-bars/sn` |
| **103** | GitHub API base endpoint updated | `https://api.github.com/repos/full-bars/urnetwork-3.23-fix` | `https://api.github.com/repos/full-bars/sn` |
| **110** | Default installer script bootstrap URL updated | `https://raw.githubusercontent.com/full-bars/urnetwork-3.23-fix/refs/heads/main/scripts/Provider_Install_Linux.sh` | `https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_Linux.sh` |
| **461–462** | `show_version()` GitHub `/releases/latest` redirect trick fallback URL | `https://github.com/full-bars/urnetwork-3.23-fix/releases/latest` | `https://github.com/full-bars/sn/releases/latest` |
| **1090–1091** | `do_install()` GitHub `/releases/latest` redirect trick fallback URL | `https://github.com/full-bars/urnetwork-3.23-fix/releases/latest` | `https://github.com/full-bars/sn/releases/latest` |
| **1168** | Fallback GitHub release mirror tarball download URL | `https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/urnetwork-provider-$tag.tar.gz` | `https://github.com/full-bars/sn/releases/download/$tag/urnetwork-provider-$tag.tar.gz` |
| **1243** | Inline comment updated regarding standalone Go `urnet-tools` asset releases | `# (urnet-tools-<os>-<arch>, v3.23.0-fix.28+). Prefer it — digest-verified` | `# (urnet-tools-<os>-<arch>, v3.23.0-fix.28+ / v2026+). Prefer it — digest-verified` |
| **1252** | Go `urnet-tools` binary mirror URL from GitHub release assets | `https://github.com/full-bars/urnetwork-3.23-fix/releases/download/$tag/$tool_asset` | `https://github.com/full-bars/sn/releases/download/$tag/$tool_asset` |
| **1369** | Post-install summary banner heading | `printf "\e[1;32mCustom Build Improvements:\e[0m\n"` | `printf "\e[1;32mURNetwork Provider (v2026):\e[0m\n"` |

### Search Verification
- `ghcr.io/full-bars/urnetwork-3.23-fix`: Searched using `rg`. No occurrences were present in `Provider_Install_Linux.sh` (image references reside in container tooling and documentation).
- `dl.fullbars.xyz`: Preserved intact across all 5 occurrences (lines 469, 1103, 1167, 1251) as the primary CDN endpoint.

---

## 2. Architecture & Layout Verification

### 2.1 Systemd Unit Name: `urnetwork.service`
The installer installs a **user systemd service** at:
- `~/.config/systemd/user/urnetwork.service`
- `~/.config/systemd/user/urnetwork-update.service`
- `~/.config/systemd/user/urnetwork-update.timer`

Controlled via `systemctl --user {start,stop,restart,status,enable,disable}`.

> [!IMPORTANT]
> **Unit Name Conflict:** The repository also contains `/home/klets/ur/h3-provider/deploy/urnetwork-provider.service`, which defines a **system-level** unit (`ExecStart=/usr/local/bin/provider provide`, `Type=simple`).
> However:
> 1. The installer `Provider_Install_Linux.sh` is built around user unit `urnetwork.service`.
> 2. The Go `urnet-tools` discovery (`internal/urnettools/discover.go`) enumerates user-level units under `~/.config/systemd/user/urnetwork*.service`.
> 3. Zero-downtime restarts (`internal/urnettools/hotswap.go`) expect `Type=notify` with `NOTIFY_SOCKET` isolation, whereas `deploy/urnetwork-provider.service` specifies `Type=simple`.
> 
> **Recommendation:** Standardize on `urnetwork.service` as the canonical user unit. Either align `deploy/urnetwork-provider.service` or retire it to prevent operator confusion.

### 2.2 Install and State Layout
- **Binaries:** Installed to `~/.local/share/urnetwork-provider/bin/`:
  - `urnetwork` (core provider daemon binary)
  - `urnet-tools` (manager CLI: either the compiled Go standalone binary or this shell script)
- **Environment:** Appended to `~/.bashrc`:
  ```sh
  # == urnetwork-provider start
  export URNETWORK_PROVIDER_INSTALL="$HOME/.local/share/urnetwork-provider"
  export PATH="$PATH:$URNETWORK_PROVIDER_INSTALL/bin"
  # == urnetwork-provider end
  ```
- **State Directory:** `~/.urnetwork/`
  - `jwt` / `jwt_last_refresh` / `.client_jwts.json`: Identity and credentials
  - `proxy` / `proxy_url.json` / `proxy.state`: Proxy configurations and peer state
  - `pending_overrides.json`: Runtime tuning overrides
  - `fast_auth`, `proxy_self_heal`: Feature marker flags
  - `provider.sock`: Unix control domain socket
- **RAM Logging:** `/dev/shm/urlog` and `/dev/shm/urnetwork-important.log` (when enabled via `urnet-tools ramlogs on`)

### 2.3 Tarball Asset Layout
- Download target: `urnetwork-provider-$tag.tar.gz`
- Expected internal layout: `linux/$arch/provider`
- Destination installation:
  - Extracted binary `workdir/linux/$arch/provider` is copied to `$HOME/.local/share/urnetwork-provider/bin/urnetwork`.
- Verification against root artifact `urnetwork-provider-v2026.9.16-1789602650-meso.tar.gz`:
  - `linux/amd64/provider` matches.
  - `linux/arm64/provider` matches.

### 2.4 Binary Naming & Discovery Parity
- Host installations: Binary is named `urnetwork`.
- Container installations: Binaries are named `urnetwork_amd64_stable` and `urnetwork_arm64_stable`.
- Discovery parity verification (`internal/urnettools/discover.go`):
  - `knownBinaries` map includes `"urnetwork": true`.
  - `isProviderArg()` matches `base == known || strings.HasPrefix(base, known+"-") || strings.HasPrefix(base, known+"_")`.
  - Suffix exclusion (`nonProviderSiblingSuffixes`: `hub`, `update`, `sentinel`, `dashboard`) does not match `amd64-stable` or `arm64-stable`.
  - Confirmed by `internal/urnettools/discover_docker_test.go` covering `urnetwork_amd64_stable` and `urnetwork_arm64_stable`.

### 2.5 Version Comparison Logic
Version evaluation during `urnet-tools update` operates via:
1. **API Metadata:** Fetches release payload from `$api_base/releases/latest` or `/releases/tags/$tag`.
2. **Metadata Extraction:**
   - `version_to_install`: parsed from `tag_name`.
   - `release_date`: parsed from `published_at` and converted to Unix epoch timestamp.
3. **Fallbacks:**
   - Redirect URL scraping via `curl -Ls -o /dev/null -w %{url_effective} "https://github.com/full-bars/sn/releases/latest"` -> extracts `${tag_url##*/}`.
   - CDN endpoint: `https://dl.fullbars.xyz/latest-version`.
4. **Comparison Mechanism:**
   - If release dates are present and numeric: checks `[ "$install_release_date" -lt "$release_date" ]`.
   - If release date missing or invalid: falls back to string inequality `[ "$version_to_install" != "$installed_version" ]`.
5. **Disk vs Process Drift (`show_version`):**
   - Compares on-disk version (`$install_path/bin/urnetwork --version`) with active running process version (`/proc/$running_pid/exe --version`).
   - Flags drift if binary was replaced on disk without restarting the service.

---

## 3. Items Requiring Operator Attention

### 3.1 Fleet Cross-Grade Path
Existing fleet nodes currently run `urnetwork-3.23-fix` with `urnet-tools` pointing to `full-bars/urnetwork-3.23-fix`. They will not self-update to `v2026` automatically because their updater queries the old repository API.
- **Option A (Manual Bootstrap):** Run the bootstrap installer on fleet nodes:
  ```sh
  curl -sSf https://raw.githubusercontent.com/full-bars/sn/main/scripts/Provider_Install_Linux.sh | sh
  ```
- **Option B (Bridge Release):** Publish a transition release in `full-bars/urnetwork-3.23-fix` whose update mechanism redirects discovery to `full-bars/sn`.

### 3.2 CDN Route Migration (`dl.fullbars.xyz`)
- The script preserves `dl.fullbars.xyz` URLs.
- The Cloudflare Worker / CDN origin routing behind `dl.fullbars.xyz/releases/download/...` and `dl.fullbars.xyz/latest-version` must be configured to point to `full-bars/sn` releases when cut.

### 3.3 Hardcoded Assumptions That May Break with v2026
1. **Tag Format & Semver Comparison:**
   - The installer does not perform semver arithmetic; it uses epoch date comparison or string inequality (`!=`).
   - If an operator manually forces an update to an older tag, date comparison prevents downgrade unless `-f`/`--force` is passed.
2. **Architecture Support:**
   - `get_arch()` maps `i386`/`i686` to `386`. If v2026 releases do not build `386` assets, 32-bit x86 downloads will fail with a 404.
3. **Drop-in vs Socket State:**
   - `show_logs` checks `$HOME/.config/systemd/user/urnetwork.service.d/*.conf` for `URNETWORK_PROFILE` and `URNETWORK_RAMLOGS`.
   - v2026 supports runtime socket-based controls (`provider.sock`); if systemd drop-ins are deprecated in favor of runtime control state, `show_logs` RAM-log detection will need updating.
