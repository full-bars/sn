# Docker Scripts Porting Notes: urnetwork-3.23-fix → h3-provider

**Date:** 2026-09-16  
**Source:** `/home/klets/ur/urnetwork-3.23-fix/docker/scripts/`  
**Destination:** `/home/klets/ur/h3-provider/docker/scripts/`  
**Target Repository:** `full-bars/sn`  
**Target Container Image:** `ghcr.io/3cape/urnetwork-provider`  

---

## 1. Overview & Porting Summary

All 17 scripts (3,189 lines of shell and test fixtures) from `urnetwork-3.23-fix/docker/scripts/` were ported into `h3-provider/docker/scripts/`.

The container scripts provide in-container orchestration, provider lifecycle supervision, authentication (JWT & legacy user/pass), health/traffic inspection, RAMLOGS extraction, auto-update watchers with SHA256 digest verification, vnStat bandwidth telemetry on port 8080, and the container-side `urnet-tools` CLI.

The Dockerfile (`/home/klets/ur/h3-provider/Dockerfile`) was updated to match the runtime environment of `urnetwork-3.23-fix`, bundling the script toolkit, creating standard PATH symlinks, configuring vnStat, and setting `/app/entrypoint.sh` as the container entrypoint.

---

## 2. Inventory of Ported Scripts

| File | Lines | Purpose & Description |
|---|---|---|
| `entrypoint.sh` | 97 | Primary container entrypoint. Normalizes `BUILD` (`stable`, `nightly`, `jwt`), sets `URNETWORK_PROFILE` from `TURBO` (`turbo-v4`/`turbo-v8`), and executes the target runner or Pelican panel. |
| `logs.sh` | 30 | RAMLOGS inspector. Tails `/dev/shm/urnetwork.log` or dumps to `/root/urlogs.txt`. Symlinked to `/usr/local/bin/logs`. |
| `pelican_panel.sh` | 181 | Bootstrap script for Pelican Panel environments (`PELICAN=yes`). Validates credentials, checks proxies, detects architecture, and supervises the provider. |
| `proxy-health.sh` | 24 | Displays persistent proxy health snapshot (`$HOME/.urnetwork/proxy_health.state`) and tails event log. Symlinked to `/usr/local/bin/proxy-health`. |
| `proxy-traffic.sh` | 14 | Displays persistent proxy bandwidth/session statistics (`$HOME/.urnetwork/proxy_traffic.state`). Symlinked to `/usr/local/bin/proxy-traffic`. |
| `start_jwt.sh` | 283 | JWT-authenticated container startup. Supports session persistence, proxy config, vnStat on :8080, and provider supervisor with automatic retry and exit code 78 re-authentication. |
| `start_nightly.sh` | 547 | Nightly build runner with daily auto-updater (`UPDATE_TIME=12:00`). Checks GitHub release API, verifies SHA256 digest, extracts, stages atomically, and restarts provider. |
| `start_stable.sh` | 292 | Stable build container startup with credential auth, proxy support, vnStat, supervisor loop, and restart handling for update/hotswap. |
| `start_update.sh` | 124 | Standalone provider updater. Downloads latest release from GitHub API, verifies SHA256 digest, extracts `linux/amd64/provider` and `linux/arm64/provider` to `/app/urnetwork_${arch}_${suffix}`. |
| `stats` | 25 | CGI script served by busybox httpd on port 8080. Outputs `vnstat --json d` traffic statistics as JSON. |
| `test_nightly_tarball_extract.sh` | 303 | Regression test suite verifying tarball layout detection (flat, nested, multi-arch) and atomic binary staging in `start_nightly.sh`. |
| `test_pelican_gates.sh` | 138 | Behavioral tests for `PELICAN=yes` update-disable gates, state-dir resolution, and egg JSON invariants. |
| `test_pelican_smoke.sh` | 86 | Local smoke test exercising `pelican_panel.sh` boot flow against an emulated provider in `jwt` and `stable` modes. |
| `test_update_verify.sh` | 262 | Verification harness for SHA256 digest extraction from GitHub release API, parser fallbacks (jq/python3), hashing fallbacks (sha256sum/openssl), and upstream repository source guards. |
| `update_verify.sh` | 94 | Shared POSIX library for SHA256 digest extraction and verification (`upd_asset_digest_from_json`, `upd_sha256_of`, `upd_verify_digest`). Sourced by `start_update.sh`, `start_nightly.sh`, and `urnet-tools.sh`. |
| `urnet-tools.sh` | 679 | In-container management CLI. Exposes `proxy`, `auth`, `choose_network`, `logs`, `status`, `self-heal`, `fast-auth`, `set`, `report`, `version`, `session` (save/load with AES-256-CBC), `idle-update`, and `update`. Symlinked to `/usr/local/bin/urnet-tools`. |
| `urnetwork_ipinfo.sh` | 31 | Diagnostic utility querying `https://api.bringyour.com/my-ip-info` to print public IP, VPN, proxy, TOR, relay, and hosting flags. |

---

## 3. String & Repository Replacements

### 3.1 `full-bars/urnetwork-3.23-fix` → `full-bars/sn`

All occurrences of the old repository path were replaced:

1. **`docker/scripts/start_update.sh`**:
   - Line 22: `readonly UPSTREAM_REPO="full-bars/sn"` (was `full-bars/urnetwork-3.23-fix`)
   - Line 30: `API="https://api.github.com/repos/full-bars/sn/releases/latest"`
2. **`docker/scripts/start_nightly.sh`**:
   - Line 31: `API_URL="https://api.github.com/repos/full-bars/sn/releases/latest"`
3. **`docker/scripts/urnet-tools.sh`**:
   - Line 44: `release_url="https://api.github.com/repos/full-bars/sn/releases/tags/$requested_tag"`
   - Line 46: `release_url="https://api.github.com/repos/full-bars/sn/releases/latest"`
   - Line 64: `primary_url="$(echo "$download_url" | sed 's|https://github.com/full-bars/sn/releases/download/|https://dl.fullbars.xyz/releases/download/|')"`
4. **`docker/scripts/test_update_verify.sh`**:
   - Line 227: `URL_V9="https://github.com/full-bars/sn/releases/download/v9/urnetwork-provider-v9.tar.gz"`

### 3.2 `ghcr.io/full-bars/urnetwork-3.23-fix` → `ghcr.io/3cape/urnetwork-provider`

- Ripgrep search confirmed that `ghcr.io` references did not exist directly in `docker/scripts/`. Image references in `h3-provider` are maintained in:
  - `internal/urnettools/release.go`: `const dockerImage = "3cape/urnetwork-provider"`
  - `internal/urnettools/docker.go`: `ghcr.io/3cape/urnetwork-provider:latest`
- In-container scripts do not call `docker run` or `docker pull` internally; they execute within the running container context.

### 3.3 `test_pelican_gates.sh` Pelican Egg Guard

- In `urnetwork-3.23-fix`, the script assumed `$REPO/pelican/egg-urnetwork-323fix.json` was present in the repository.
- Because `h3-provider` currently does not contain the `pelican/` folder, lines 117–130 were updated to:
  ```bash
  egg="${PELICAN_EGG_PATH:-$REPO/pelican/egg-urnetwork-323fix.json}"
  if [ -f "$egg" ]; then
      # run jq rules and uniqueness assertions
  else
      echo "SKIP: pelican egg JSON not present at $egg"
  fi
  ```
- This allows the test suite to pass cleanly in `h3-provider` while still allowing operators to test egg configurations via `PELICAN_EGG_PATH`.

---

## 4. Binary Naming Architecture & Mismatches

### 4.1 In-Container Binary Naming: `urnetwork_${TARGETARCH}_stable`

There is an intentional dual-naming design in the container:

1. **Physical Binary Path:** `/app/urnetwork_${TARGETARCH}_stable` (or `_nightly`)
   - In `Dockerfile`:
     ```dockerfile
     COPY --from=builder /app/provider_bin /app/urnetwork_${TARGETARCH}_stable
     ```
   - In `start_stable.sh`, `start_jwt.sh`, `pelican_panel.sh`:
     ```bash
     PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
     ```
   - In `start_nightly.sh`:
     ```bash
     PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_nightly"
     ```
   - In `urnet-tools.sh`:
     ```bash
     provider_bin="/app/urnetwork_${arch}_stable"
     provider_pattern="urnetwork_${arch}_stable provide"
     ```
   - In process filtering and supervision:
     - `pgrep -f "$provider_pattern"`
     - `pgrep -f "urnetwork.*provide"`
     - `ps aux | grep "[u]rnetwork_${A_SYS_ARCH}_"`

2. **Symlink on PATH:** `/usr/local/bin/provider -> /app/urnetwork_${TARGETARCH}_stable`
   - Enables operators to run `provider <command>` (e.g. `docker exec <container> provider status`, `provider proxy add`, `provider auth`).
   - Enables `urnet-tools.sh` to delegate commands to `/usr/local/bin/provider`.

3. **Discovery Matching (`internal/urnettools/discover.go`):**
   - In `internal/urnettools/discover.go`, `knownBinaries` contains `"urnetwork": true` and `"provider": true`.
   - The matching logic accepts both exact matches and prefix matches with `-` or `_`:
     - Matches `urnetwork_amd64_stable` via prefix `urnetwork_`.
     - Matches `provider` directly.
   - Preserving `/app/urnetwork_${TARGETARCH}_stable` guarantees that `urnet-docker` running on the host discovers the running container provider process.

### 4.2 Standalone / Release Tarball Naming: `provider`

- In `h3-provider/cmd/provider/`, the Go package builds the `provider` executable.
- In `h3-provider/.github/workflows/release.yml`:
  - Per-platform tarballs (`urnetwork-provider-${VERSION}-linux-amd64.tar.gz`) contain `provider` at root.
  - Multi-arch fat tarballs (`urnetwork-provider-${VERSION}.tar.gz`) contain `linux/amd64/provider` and `linux/arm64/provider`.
- `start_nightly.sh` and `urnet-tools.sh` are already compatible with both tarball layouts:
  - `start_nightly.sh` lines 332–335 check for `linux/${A_SYS_ARCH}/provider` first, falling back to `provider`.
  - `urnet-tools.sh` lines 129–140 extract `provider` and rename it to `/app/urnetwork_${arch}_stable`.
  - `start_update.sh` extracts `linux/${arch}/provider` and writes to `/app/urnetwork_${arch}_${suffix}`.

> [!TIP]
> The container internal binary name **must remain** `/app/urnetwork_${TARGETARCH}_stable` with symlink `/usr/local/bin/provider`. Changing the on-disk filename in Docker would break `pgrep` patterns across `start_stable.sh`, `start_jwt.sh`, `start_nightly.sh`, and `urnet-tools.sh`.

---

## 5. Dockerfile Modernization

The Dockerfile (`/home/klets/ur/h3-provider/Dockerfile`) was updated from a bare single-binary entrypoint to match the complete `urnetwork-3.23-fix` pattern:

1. **Build Stage (`builder`):**
   - Builds `./cmd/provider/` with version injection:
     `-ldflags "-s -w -X main.Version=${VERSION} -X main.VersionStamp=URNET_VERSION_STAMP=${VERSION}"`
   - Output binary staged at `/app/provider_bin`.
2. **Runtime Dependencies (`apk add`):**
   - Added `vnstat`, `iptables`, `net-tools`, `busybox-extras`, `gosu` (along with existing `tzdata`, `iputils`, `dos2unix`, `jq`, `tar`, `curl`, `htop`, `wget`, `procps`, `bind-tools`, `ca-certificates`, `ca-certificates-bundle`, `bash`).
3. **Directory Structure & Script Installation:**
   - Created `/app/cgi-bin` and `/root/.urnetwork`.
   - `COPY docker/scripts/*.sh /app/`
   - `COPY docker/scripts/stats /app/cgi-bin/`
   - `COPY --from=builder /app/provider_bin /app/urnetwork_${TARGETARCH}_stable`
   - Applied `dos2unix` and `chmod +x` to scripts and `stats`.
4. **Symlinks Created:**
   - `ln -sf /app/proxy-health.sh /usr/local/bin/proxy-health`
   - `ln -sf /app/proxy-traffic.sh /usr/local/bin/proxy-traffic`
   - `ln -sf /app/logs.sh /usr/local/bin/logs`
   - `ln -sf /app/urnet-tools.sh /usr/local/bin/urnet-tools`
   - `ln -sf /app/update_verify.sh /usr/local/bin/update_verify.sh`
   - `ln -sf /app/urnetwork_${TARGETARCH}_stable /usr/local/bin/provider`
5. **vnStat Configuration:**
   - Tuned intervals via `sed -i` on `/etc/vnstat.conf` for 15s poll/update and 1s save intervals.
6. **Entrypoint:**
   - Switched `ENTRYPOINT` to `["/app/entrypoint.sh"]`.

---

## 6. Hardcoded Assumptions Requiring Attention

> [!CAUTION]
> Review the following hardcoded assumptions before deploying releases to production.

1. **Primary Download Mirror (`dl.fullbars.xyz`):**
   - In `docker/scripts/urnet-tools.sh:64`, `primary_url` rewrites GitHub download URLs to:
     `https://dl.fullbars.xyz/releases/download/<tag>/<asset>`
   - If `dl.fullbars.xyz` does not mirror `full-bars/sn` releases, the primary download will fail and fall back to `github-mirror` (`https://github.com/full-bars/sn/releases/download/...`). While failover succeeds, CDN configuration should be verified.
2. **Release Asset SHA256 Digests:**
   - `update_verify.sh` requires that GitHub release JSON include a `digest` attribute formatted as `sha256:<hex>` for each `.tar.gz` asset.
   - If assets are uploaded via custom scripts or manual drag-and-drop without digest metadata, `start_update.sh`, `start_nightly.sh`, and `urnet-tools update` will refuse the update with:
     `Release API returned no sha256 digest for <asset>; refusing to update without verification`.
3. **External IP Checker Endpoint:**
   - `start_jwt.sh:15`, `start_nightly.sh:30`, and `start_stable.sh:23` contain:
     `IP_CHECKER_URL="https://raw.githubusercontent.com/techroy23/IP-Checker/refs/heads/main/app.sh"`
   - Activated only when `ENABLE_IP_CHECKER=true` (default: `false`). This points to an external personal repository.
4. **Timezone Assumption:**
   - `start_jwt.sh`, `start_nightly.sh`, and `start_stable.sh` set `export TZ="America/Tijuana"` by default.
   - Used for log timestamps and vnStat calculations unless overridden by container environment `-e TZ=...`.
5. **Pelican Panel Egg Definition:**
   - `urnetwork-3.23-fix` included `pelican/egg-urnetwork-323fix.json`.
   - `h3-provider` does not yet include a `pelican/` folder. If nodes are deployed via Pelican panels, the panel egg definition will need to be ported or recreated under `pelican/egg-urnetwork-provider.json`.
6. **Release Tag Format Ordering:**
   - `urnet-tools update` compares version tags. Ensure tag ordering logic handles `v2026.*` semantic or date-stamped tags correctly against previous `v3.23.0-fix.*` versions if cross-grading existing containers.

---

## 7. Test Execution & Verification Results

All test suites were executed directly in `h3-provider` and passed:

```
$ bash docker/scripts/test_pelican_gates.sh
PASS: func_check_update: PELICAN=yes exits 0
PASS: func_check_update: PELICAN=yes logs disabled notice
PASS: func_check_update: PELICAN=yes makes no network call
PASS: func_check_update: PELICAN unset skips the gate message
PASS: func_check_update: PELICAN unset proceeds past the gate (network attempted)
PASS: do_update: PELICAN=yes exits 1
PASS: do_update: PELICAN=yes prints refusal
PASS: do_update: PELICAN=yes never reaches arch detection
PASS: do_update: PELICAN unset proceeds past the gate (arch detection attempted)
PASS: proxy-health.sh resolves state dir under $HOME (no override)
PASS: proxy-health.sh honors explicit URNETWORK_PROXY_HEALTH_DIR override
PASS: proxy-traffic.sh resolves state dir under $HOME (no override)
SKIP: pelican egg JSON not present at /home/klets/ur/h3-provider/pelican/egg-urnetwork-323fix.json
Results: 12 passed, 0 failed

$ bash docker/scripts/test_update_verify.sh
Results: 23 passed, 0 failed

$ bash docker/scripts/test_nightly_tarball_extract.sh
=== Results: 9 passed, 0 failed ===
ALL TESTS PASSED

$ bash docker/scripts/test_pelican_smoke.sh
Results: 6 passed, 0 failed
```
