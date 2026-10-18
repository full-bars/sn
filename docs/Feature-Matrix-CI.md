# Feature Matrix CI (release-only)

The release distro CI (`.github/workflows/distro-matrix.yml`) runs two
complementary harnesses inside each distro's own userland:

| Harness | Job | What it proves |
| --- | --- | --- |
| `.github/scripts/distro-smoke.sh` | `distro-smoke` | the static payload loads on that libc, `miner provide` binds the control socket (mode 0600), `urnet-tools version/status` reach it, SIGTERM stops the miner cleanly |
| `.github/scripts/distro-features.sh` | `distro-features` (snippet in [§Wiring](#wiring-the-feature-matrix-job-into-distro-matrixyml)) | the risky operator surfaces, the URL-source pipeline and the broad command surface behave on that distro |

Both scripts are **POSIX sh** on purpose: they run in the distro image, where
there is no bash assumption, no jq and no python3 (Alpine, debian-slim,
openSUSE and Arch images all lack at least one of them). The feature harness
only needs the payload (`miner`, `urnet-tools`), core POSIX utilities, and —
for the raw control-socket probes — a netcat that supports `nc -U`. The CI
job installs `netcat-openbsd` where the base image does not carry it (Alpine's
busybox `nc` has no unix-socket mode; without the install those checks
degrade to `SKIP-UNSUPPORTED`, they never fail).

Everything reads and writes only inside `STATE_HOME` (`HOME`): the harness
creates its own sandbox, points the invoking user's passwd home at it (a
container-local adaptation so `urnet-tools` discovery can find the provider),
and never touches the ambient home. It never pushes, tags, opens PRs,
comments, or reruns workflows.

## Blocks and coverage

### Block 1 — risky surfaces (runs first, on purpose)

The destructive/sensitive checks run before anything that adds state, so a
release blocker in them is not masked by proxy churn.

| Check | What it asserts |
| --- | --- |
| `binaries` | the miner answers `provide --help`; `urnet-tools version` answers |
| `miner-start` | `provider.sock` binds within `FEATURE_START_TIMEOUT` (90s) |
| `socket-perm-0600` | the control socket is owner-only (mode 600) |
| `socket-owner-req` | a raw owner request over `nc -U` is answered (`{"ok":true}`) |
| `socket-peer-uid` | a foreign uid (`nobody`/65534) is denied — `Permission denied` at the filesystem or a structured refusal |
| `socket-oversize-set` | a 4100+ byte `set` value is refused with `value too large`, not accepted |
| `socket-oversize-line` | a 70KB line does not kill the socket; it still answers |
| `socket-hostile-input` | 5 hostile inputs (non-JSON, `{}`, traversal-shaped cmd, bad version, truncated JSON) get bounded JSON and the socket survives |
| `control-key-live` | `set gogc=250` applies live, reads back, is logged, and changes `provider_state.json` (before/after fingerprint) |
| `key-h3-limit` | `set h3=3` either applies live or is classified as persisted-but-inert with its owning port |
| `key-ramlogs` | `set ramlogs=on` either applies live or is classified as persisted-but-inert with its owning port |
| `metrics-bind` | when the exporter exists: `metrics on` applies and no wildcard public bind is reported; otherwise classified unsupported |
| `identity-mint` | a provider client identity (`.provider.jwt`) is minted against the disposable JWT |
| `identity-key-material` | `.provider.key` exists next to the identity |
| `clean-sigterm` | SIGTERM exits rc=0 and removes the socket file |
| `identity-restart-reuse` | the client identity is carried across a restart when the stable client-JWT store exists; otherwise classified with the owning port |
| `hot-restart-off` | `URNETWORK_HOT_RESTART=0` mints new ids when the client-JWT store exists; otherwise classified with the owning port |
| `hotswap-handoff` | a raw `{"cmd":"hotswap"}` trigger hands the socket to a successor; otherwise classified with the owning port |
| `identity-gate-fresh` | a fresh state dir without `--allow-client-registration` must not allocate (no socket) |
| `identity-gate-adopt` | `--adopt-legacy-provider-key` against a fresh dir must not allocate either |
| `exit-75-quarantine` | a rejected sign-in with `--quarantine-rejected-sign-in` exits 75 (EX_TEMPFAIL; environmental when not reproducible) |
| `state-paths` | provider state exists under `STATE_DIR` |
| `state-paths-sandbox` | no `.urnetwork` grows under the pre-adaptation passwd home (the sandbox-leak canary) |
| `state-integrity-audit.json` / `state-integrity-proxy.state` / `state-integrity-proxy_url.json` | each state file replaced with garbage: the provider either recovers (socket up) or refuses with a clean diagnostic; a panic/SIGSEGV is a FAIL |

### Block 2 — URL source pipeline + operator manipulation

| Check | What it asserts |
| --- | --- |
| `url-fetch-source1` | `add-source` of the monosans socks5 list fetches, probes and admits: the `[proxy][url] probed …` line matches the documented shape (`N/M new qualified (K cached, skipped), …`), the cycle count agrees with the cache (`proxy_url.json`), and an admitting cycle names its tier (`admitted by tier`) |
| `url-fetch-source2` | the documented second source (proxifly socks5, 68k lines) exercised when enabled via `FEATURE_URL_SOURCE2`; off by default (see below) |
| `url-state-coherence` | a successful fetch leaves a `sources` + cache in `proxy_url.json` |
| `url-cached-skip` | a second cycle against the cached list is add-only: the `skipped N already-cached` / `already known` line is present and the cache does not shrink |
| `url-remove-source` | `remove-source` drops the second list by URL |
| `url-log-vocabulary` | the pipeline lines the run's own outcomes imply are present (`probed`, `admitted by tier`, `fetch failed`, `all unparseable or dead`) |
| `proxy-add` | `proxy add <file>` adds every synthetic entry; the proxy file holds them; the tool reports `added server …` |
| `trim-preview-no-write` | `proxy trim N --dry-run` prints a preview and the state files stay byte-identical |
| `trim-apply` | `proxy trim N -f` persists the cap (`proxy_trim`) |
| `proxy-remove` | `proxy remove <host:port> -f` removes exactly one address and the state diff is present |
| `remove-dead-preview` | `proxy remove-dead --preview` either lists candidates without writing, or reports the warmup gate (exit 61: `need 1h … uptime`) without writing |
| `remove-match` | `proxy remove --match=<pattern>` removes by pattern when the surface exposes it |
| `refresh-add-only` | `proxy refresh -f` reports the curated list up to date or an add-only diff (`0 proxies will be removed`) and a non-interactive run writes nothing |
| `proxy-paste` | `proxy paste` accepts a stdin list when the grammar exposes it |
| `proxy-clear` | `proxy clear -f` removes every configured proxy |

The synthetic entries are IPv4 TEST-NET-1 addresses (`192.0.2.x:1080`) that
can never be live proxies, so manipulation checks are deterministic and
cannot accidentally add working proxies to the cache.

### Block 3 — broad surface

| Check | What it asserts |
| --- | --- |
| `surface-version` | `urnet-tools version` answers |
| `surface-status` | `status` reports the provider (`running: …`) |
| `surface-proxy-health` / `-traffic` / `-ids` | the proxy sub-surfaces answer, or are classified as not exposed |
| `surface-summary` | `summary` renders the proxy summary |
| `surface-config` | `config` renders the settings table |
| `surface-audit` | the proxy-audit surface answers, or is classified with its owning port |
| `surface-metrics` | the metrics surface answers, or is classified with its owning port |
| `surface-exclude` | `proxy exclude <pattern>` is accepted when exposed |

## Classification and detection rules

Every check prints exactly one line:

```
PASS <name>: <detail>
FAIL <name>: <detail>
SKIP-UNSUPPORTED <name>: <detail> [. Owning port: <branch> (<worktree>)]
SKIP-NO-NETWORK <name>: <detail>
```

- **PASS** — the documented behavior was observed.
- **FAIL** — a product defect. Every FAIL is a finding: the detail carries the
  raw tool output and the state fingerprints that back it.
- **SKIP-UNSUPPORTED** — runtime-detected absence at this head. The detail
  names the owning port when one exists (see the table below), so a skip reads
  as "another port owns this", never as a silent pass.
- **SKIP-NO-NETWORK** — environmental: no network, no readable JWT, or the
  provider could not stay up. Never a product verdict. If the miner leaves
  during the raw socket probes the remaining probes are downgraded from FAIL
  to SKIP-NO-NETWORK (a failed network sign-in takes the provider down seconds
  after the socket binds); if the miner log shows `panic:`, `SIGSEGV` or
  `fatal error` the FAIL stands. A provider that leaves mid-block is retried
  once with a settle window before anything is recorded against it.

Exit codes: `0` = no FAIL lines; `1` = FAIL lines present; `75` = ENV_BLOCKER
(a missing mandatory input such as an unexecutable binary). A 75 never means
the product failed.

The summary block prints the counts and repeats every FAIL item, so a CI log
is self-contained:

```
PASS=34 FAIL=0 SKIP-UNSUPPORTED=8 SKIP-NO-NETWORK=0
```

## Conditional map (owning ports)

Checks whose code is not in the tree at this head classify themselves as
`SKIP-UNSUPPORTED` and name the port that owns them. When that port lands the
same check flips to PASS without a harness change (the condition is detected
from behavior/output, not from a commit hash).

| Check | Condition at this head | Owning port / worktree |
| --- | --- | --- |
| `identity-restart-reuse` | identity changes across restart and no `.client_jwts.json` store exists | `feat/p2-hotswap` (`wt-sn-hotswap`, client-JWT store) |
| `hot-restart-off` | `URNETWORK_HOT_RESTART=0` is accepted but reuse-vs-new is not observable without the store | `feat/p2-hotswap` (`wt-sn-hotswap`) |
| `hotswap-handoff` | raw `hotswap` trigger answers `hotswap trigger not available` | `feat/p2-hotswap` (`wt-sn-hotswap`) |
| `key-h3-limit` | `h3` persists but reports `requires a restart to take effect` | `feat/p2-h3` (`wt-sn-h3`) |
| `key-ramlogs` | `ramlogs` persists but reports `requires a restart to take effect` | `feat/p2-ops` (`wt-sn-ops`) |
| `metrics-bind` | `metrics on` is persisted but refused live (`the miner does not implement the v3.23 Prometheus /metrics exporter yet`) | `feat/p2-metrics` (`wt-sn-metrics`) |
| `surface-metrics` | same exporter gap | `feat/p2-metrics` (`wt-sn-metrics`) |
| `surface-audit` | proxy-audit surface not wired into the tool/provider path at this head | the audit-ring port (`wt-sn-audit`) |
| `remove-match` | `remove --match` surfaced by the provider grammar but not routed by the `urnet-tools` CLI at this head | `feat/p2-proxy-cli` (`wt-sn-proxy-cli`) |
| `proxy-paste` | `proxy paste` not exposed by the tool at this head | `feat/p2-proxy-cli` (`wt-sn-proxy-cli`) |
| `url-fetch-source2` | second source disabled by knob; the documented proxifly list is 68k lines and cannot clear in one 900s pass | `FEATURE_URL_SOURCE2` (documented source; see below) |
| `exit-75-quarantine` | only observable when the environment actually rejects a sign-in; otherwise SKIP-NO-NETWORK | advertised and exercised by `wt-sn-exit75` |

## Stagger (rate politeness)

Public proxy-list hosts and shared runner egress IPs are rate-sensitive: if
every cell in a matrix fetches at the same instant, the run pattern is
indistinguishable from a scrape and the lists/API may throttle the whole
runner. Every container therefore waits before Block 2:

- **Jitter**: `0..FEATURE_URL_JITTER_MAX` seconds (default `180`; `0`
  disables), seeded from `hostname | cksum` plus 4 bytes from `/dev/urandom`,
  and always logged with both halves of the seed:
  ```
  URL-STAGGER: seed=7946111031 (hostname-cksum=3696323312 + urandom=4249787719) jitter=166s max=180s
  ```
- **Source gap**: sources are fetched `0..30s` apart
  (`(seed / 7) % 31`); the gap is logged before each source.
- **Randomized order**: which of the two sources goes first depends on the
  seed (`as-listed` / `swapped`).
- **Randomized independent checks**: the Block-2/3 groups (`url`, `manip`,
  `surface`) are shuffled with the same seed — state-dependent pairs stay
  ordered, the independent groups do not land in the same order in every
  cell.

All three are logged on one line so a CI log can prove the stagger ran:

```
URL-STAGGER: source-gap=13s source-order=swapped independent-order=manip url surface
```

## Environment knobs

| Variable | Default | Meaning |
| --- | --- | --- |
| `BIN_DIR` | — | dir holding `miner` + `urnet-tools` (required unless `MINER_BIN`/`TOOLS_BIN` are set) |
| `MINER_BIN` | `$BIN_DIR/miner` | miner binary |
| `TOOLS_BIN` | `$BIN_DIR/urnet-tools` (or `urnet-tools-323`) | urnet-tools binary |
| `JWT_FILE` | — | disposable JWT; staged into `$STATE_DIR/jwt` (mode 600, never printed); without it the live blocks are `SKIP-NO-NETWORK` |
| `STATE_HOME` / `HOME` | `HOME`, else `/root` | sandbox home; `STATE_DIR` and the report live under it |
| `STATE_DIR` | `$STATE_HOME/.urnetwork` | provider state dir |
| `TIMEOUT` | `900` | generic timeout (seconds) |
| `FEATURE_START_TIMEOUT` | `90` | wait for `provider.sock` |
| `FEATURE_SETTLE` | `12` | settle window before the live checks trust a provider |
| `FEATURE_START_ATTEMPTS` | `2` | provider start attempts |
| `FEATURE_IDENTITY_TIMEOUT` | `90` | wait for the minted identity |
| `FEATURE_SOCKET_TIMEOUT` | `6` | per raw socket probe |
| `FEATURE_URL_TIMEOUT` | `$TIMEOUT` | per `add-source` fetch (900s in CI) |
| `FEATURE_URL_SOURCE1` | monosans `proxies/socks5.txt` | the first URL source |
| `FEATURE_URL_SOURCE2` | empty (off) | the second URL source; set to the proxifly socks5 list `https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt` to exercise the two-source path |
| `FEATURE_URL_CACHED_PROBE` | `1` | run the second (cached-skip) cycle; `0` disables |
| `FEATURE_EXIT75_TIMEOUT` | `60` | budget for the exit-75 probe |
| `FEATURE_URL_JITTER_MAX` | `180` | stagger ceiling in seconds; `0` disables the wait |
| `FEATURE_REPORT` | `$STATE_HOME/feature-matrix-report.txt` | report file |
| `FEATURE_WORK` | `$STATE_HOME/.feature-matrix-work` | scratch dir |

Why the proxifly source is off by default: the list publishes ~68k lines
(68,714 measured on 2026-10-18) and the pipeline probes every new address
before admission, so one 900s pass cannot clear it on a shared runner; an
enabled-but-unreachable source is classified `SKIP-NO-NETWORK`, never FAIL.
Enable it when running with a large budget or on a private runner:
`FEATURE_URL_SOURCE2=https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt`.

## Wiring the feature-matrix job into distro-matrix.yml

`.github/workflows/distro-matrix.yml` is owned by the release-distro work; do
not edit it from this branch. Three changes wire the harness in:

1. one line in the existing `build-binaries` job (next to the
   `cp .github/scripts/distro-smoke.sh out/distro-smoke.sh` line):

   ```sh
   cp .github/scripts/distro-features.sh out/distro-features.sh
   ```

2. the job below (release-only like the rest of the matrix: it inherits the
   workflow's `v*` tag + `workflow_dispatch` triggers, and has no
   pull-request trigger).

```yaml
  # --------------------------------------------------------------------------
  # Tier 1b: feature matrix in the two representative libc families (glibc +
  # musl). Runs .github/scripts/distro-features.sh in the distro's own
  # userland against the same static payload: risky control surfaces first,
  # then the URL-source pipeline, then the broad surface. netcat-openbsd is
  # installed because the raw control-socket probes use `nc -U`; without it
  # those checks degrade to SKIP-UNSUPPORTED instead of failing.
  # --------------------------------------------------------------------------
  distro-features:
    name: features (${{ matrix.slug }})
    needs: build-binaries
    runs-on: ubuntu-latest
    timeout-minutes: 75
    permissions:
      contents: read
      actions: read # the token is used below to fetch this run's payload artifact
    strategy:
      fail-fast: false
      matrix:
        include:
          - image: debian:stable-slim
            slug: debian-stable-slim
          - image: alpine:latest
            slug: alpine-latest
    container:
      image: ${{ matrix.image }}
    steps:
      - name: Install curl + unzip + netcat in the distro image
        run: |
          set -eu
          if command -v apt-get >/dev/null 2>&1; then
            apt-get update -qq
            DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
              curl unzip ca-certificates netcat-openbsd
          elif command -v apk >/dev/null 2>&1; then
            apk add --no-cache curl unzip ca-certificates netcat-openbsd
          else
            echo "::error::no supported package manager in ${{ matrix.image }}"; exit 1
          fi
          nc -h 2>&1 | head -n 1 || true

      - name: Fetch the payload (artifact REST API; no JS action, so musl works)
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          set -eu
          mkdir -p /tmp/payload
          url="https://api.github.com/repos/${{ github.repository }}/actions/artifacts/${{ needs.build-binaries.outputs.artifact-id }}/zip"
          curl -fsSL -H "Authorization: Bearer ${GITHUB_TOKEN}" -o /tmp/payload.zip "$url"
          unzip -q -o /tmp/payload.zip -d /tmp/payload
          chmod +x /tmp/payload/miner /tmp/payload/urnet-tools /tmp/payload/distro-features.sh
          ls -l /tmp/payload

      - name: Mint the disposable CI JWT (refuses the production account)
        env:
          GAUNTLET_USER: ${{ secrets.GAUNTLET_USER }}
          GAUNTLET_PW: ${{ secrets.GAUNTLET_PW }}
        run: |
          set -eu
          mkdir -p /tmp/mint /tmp/fixtures
          HOME=/tmp/mint /tmp/payload/miner auth \
            --user_auth="$GAUNTLET_USER" --password="$GAUNTLET_PW" \
            --api_url=https://api.bringyour.com -f >/tmp/mint/auth.log 2>&1 || true
          test -s /tmp/mint/.urnetwork/jwt || {
            echo "::error::JWT mint failed (verification required?)"; tail -n 20 /tmp/mint/auth.log; exit 1; }
          cp /tmp/mint/.urnetwork/jwt /tmp/fixtures/jwt
          chmod 600 /tmp/fixtures/jwt
          # Production-account refusal guard, decoded with POSIX tools: these
          # images have no python3 and no jq. The token is never printed.
          payload="$(cut -d. -f2 /tmp/fixtures/jwt | tr '_-' '/+')"
          pad=$(( (4 - ${#payload} % 4) % 4 ))
          i=0; while [ "$i" -lt "$pad" ]; do payload="$payload="; i=$((i + 1)); done
          if printf '%s' "$payload" | base64 -d >/tmp/mint/jwt-payload.json 2>/dev/null &&
             [ -s /tmp/mint/jwt-payload.json ]; then
            if grep -q 'mesocyclone' /tmp/mint/jwt-payload.json; then
              echo "::error::REFUSING: the minted JWT is the production account"; exit 1
            fi
            echo "minted a disposable JWT (account guarded, token never printed)"
          else
            echo "::warning::could not decode the JWT payload; the production-account guard was not applied"
          fi
          rm -rf /tmp/mint

      - name: Run the feature matrix (the harness creates its isolated HOME)
        env:
          BIN_DIR: /tmp/payload
          JWT_FILE: /tmp/fixtures/jwt
          STATE_HOME: /tmp/feature-matrix/home
          TIMEOUT: "900"
        run: |
          # POSIX sh on purpose: this runs in the distro's own shell. The
          # harness never prints the token; its report file stays inside
          # STATE_HOME.
          set +e
          sh /tmp/payload/distro-features.sh >/tmp/feature-matrix.log 2>&1
          rc=$?
          cat /tmp/feature-matrix.log
          echo "harness rc=$rc"
          exit "$rc"

      - name: Publish the feature summary
        if: always()
        run: |
          {
            echo "### feature matrix — ${{ matrix.image }}"
            echo
            echo '```'
            sed -n '/SUMMARY/,$p' /tmp/feature-matrix.log 2>/dev/null || echo "(no harness output)"
            echo '```'
          } >>"$GITHUB_STEP_SUMMARY" || true
          # Make the skips/findings visible inline even when the job fails.
          grep -E '^(FAIL|SKIP-NO-NETWORK|SKIP-UNSUPPORTED) ' /tmp/feature-matrix.log 2>/dev/null || true
```

The step `exit "$rc"` fails the cell on FAIL (rc=1) and on ENV_BLOCKER
(rc=75), exactly like the harness's own contract; SKIP lines never fail a
cell. `STATE_HOME: /tmp/feature-matrix/home` is the explicit isolated HOME
the harness writes into; the container is disposable, so its /etc/passwd
adaptation (the discovery contract) is container-local.

For the workflow owner (`feat/p2-distro-ci`, which owns `distro-matrix.yml`
and `distro-smoke.sh`), the third change: the workflow's trailing `summary`
job aggregates the cells by name (`needs:` plus a `check` row per cell), so
add `distro-features` there too — otherwise a feature cell that fails
(harness rc=1 or rc=75) is visible in its own job but does not fail the
aggregated summary the way the smoke cells do.

## Running locally

Build the payload from the two lines under test (the same static build as CI):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/fm/bin/miner ./cli/miner/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/fm/bin/urnet-tools ./cmd/urnet-tools/
```

Run it in a distro container with a disposable JWT and an isolated HOME:

```sh
docker run --rm \
  -v /tmp/fm/bin:/opt/fm:ro \
  -v "$PWD/.github/scripts/distro-features.sh:/repo/distro-features.sh:ro" \
  -v /path/to/disposable/jwt:/opt/jwt:ro \
  -e BIN_DIR=/opt/fm -e JWT_FILE=/opt/jwt \
  -e STATE_HOME=/root/fm-home -e TIMEOUT=900 \
  debian:stable-slim /bin/sh /repo/distro-features.sh
```

On Alpine, install `netcat-openbsd` first (`apk add --no-cache
netcat-openbsd`) if the raw socket checks should run rather than skip. The
report is also written to `$STATE_HOME/feature-matrix-report.txt`.

## Files

- `.github/scripts/distro-features.sh` — the feature matrix harness (POSIX sh).
- `.github/scripts/distro-smoke.sh` — the container smoke it complements.
- `.github/workflows/distro-matrix.yml` — the release-only workflow the job
  above belongs to (not edited by this branch).
- This document — coverage, classification, conditional map, stagger
  rationale, knobs, and the exact job snippet.
