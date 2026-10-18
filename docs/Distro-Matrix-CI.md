# Distro Matrix CI (release-only)

The release distro CI proves that a tagged release runs on the Linux
distributions we ship to — plus Windows, macOS and FreeBSD — before operators
see it. It is deliberately **release-only**: the cells are slow (privileged
systemd containers, a FreeBSD VM, a Windows runner), so they run on release
tags and on manual dispatch, never on pull requests or a nightly schedule. The
per-PR quality gate remains `build.yml`.

| Workflow | Trigger | Purpose |
| --- | --- | --- |
| `.github/workflows/distro-matrix.yml` | `v*` tags, `workflow_dispatch` | The matrix: container smoke, systemd, Windows, macOS, FreeBSD, summary |
| `.github/workflows/freebsd.yml` | `v*` tags, `workflow_dispatch` | Real FreeBSD kernel verification (restored, adapted to the miner/tools line) |
| `.github/workflows/openrc-smoke.yml` | `v*` tags, `workflow_dispatch` | Alpine/OpenRC service smoke + cross-OS vet (restored) |
| `.github/workflows/portable-core.yml` | `v*` tags, `workflow_dispatch` | Cross-OS build/vet matrix + native Windows/macOS tests (restored) |

## The matrix cells

`distro-matrix.yml` builds the static `linux/amd64` payload once
(`build-binaries`, `CGO_ENABLED=0` so it runs on musl and on glibc 2.34+) and
fans it out to independent cells:

- **`distro-smoke` — 7 container images**: `debian:stable-slim`,
  `almalinux:9`, `fedora:latest`, `opensuse/leap:15.6`,
  `opensuse/tumbleweed`, `archlinux:latest`, `alpine:latest`.
  No Ubuntu, no Rocky. Each cell runs `.github/scripts/distro-smoke.sh` in the
  distro's own userland: the binaries load, `miner provide` binds
  `$STATE/provider.sock` (mode 0600), `urnet-tools version/status` reach it,
  and SIGTERM stops the miner cleanly.
- **`systemd` — 6 images**: the same families minus Alpine, which has no
  systemd: `debian:stable-slim`, `almalinux:9`, `fedora:latest`,
  `opensuse/leap:15.6`, `opensuse/tumbleweed`, `archlinux:latest`. A
  privileged container with `/sbin/init` as PID 1 installs
  `.github/scripts/systemd-smoke.sh`'s unit and drives
  start/socket/status/stop/restart/crash-restart through `systemctl`.
  The image build installs **curl where the family provides it** (apt, dnf,
  zypper, pacman) with **netcat as the documented fallback**; the assertions
  themselves talk to the control socket through `urnet-tools`.
- **`windows` — `windows-latest`**: build + vet + `./internal/urnettools`
  unit tests, then the miner runs natively and the control socket is asserted
  (path, listen, `urnet-tools status`, `urnet-tools version`). If the
  transport is not available at the checked-out commit the step records the raw
  evidence and a `TODO(windows-control-socket)` warning instead of faking a
  pass.
- **`macos` — `macos-13` + `macos-14`**: build, vet, unit tests and the launchd
  lifecycle checks. The control-socket probe is record-only
  (`continue-on-error`, `TODO(macos-control-socket)`) so it can never fake a
  pass.
- **`freebsd` — `freebsd 14.5` via `vmactions/freebsd-vm`**: cross-builds the
  miner on the runner and runs the shared `.github/scripts/freebsd-smoke.sh`
  on a real kernel. See §Capability gaps for what is a TODO at this commit.
- **`summary`**: fan-in table; fails the workflow when a non-conditional cell
  failed. Release creation is *not* blocked — see §Release gating.

## Parallelism

Every cell is independent by construction. The workflow deliberately has:

- **no per-cell parallelism cap** — no `max-parallel` is set, so all matrix
  cells start as soon as `build-binaries` completes;
- **no serialised dependency chain between cells** — the only job edges are
  the fan-in edges `build-binaries -> distro-smoke`, `build-binaries ->
  systemd`, and `{cells} -> summary`; the Windows, macOS and FreeBSD cells have
  no dependency at all and start immediately;
- **no run-level serialisation group** — no workflow-level `concurrency:` key,
  so a manual dispatch and a tag run can proceed at the same time and no run
  cancels another.

Verify the invariant with:

```sh
rg -n 'max-parallel|needs:|concurrency' .github/workflows/distro-matrix.yml
```

The only expected hits are the four intended fan-in edges (two
`needs: build-binaries`, one `needs: [...]` on the summary) and no
`max-parallel`/`concurrency` line at all.

## Release gating (one switch, currently OFF)

Today the matrix is **report-only**: `summary` fails the workflow, and the
workflow's conclusion is not consulted by `release.yml`, so a red matrix does
not block a release.

The operator-facing switch lives in exactly one place — the clearly marked
`RELEASE GATING SWITCH` block at the bottom of `distro-matrix.yml`:

1. In `distro-matrix.yml`, set `RELEASE_GATE_MODE: block` (env of the
   `Gating placeholder (inactive)` step) and wire the guard step it describes.
2. In `.github/workflows/release.yml`, in the `create-release:` job, make
   release creation wait for this workflow's conclusion (a step that checks
   the workflow run's conclusion for the tag).

The restored `freebsd.yml`, `openrc-smoke.yml` and `portable-core.yml`
workflows carry the same note: they fail themselves on a broken cell but block
no release. Flipping the single switch above is what turns the release matrix
into a gate.

## Disposable JWT handling

Every cell that runs the miner uses a disposable CI account, never a
production one:

- `GAUNTLET_USER` / `GAUNTLET_PW` (repository secrets) are the only inputs.
- The mint follows the `shakedown.yml` pattern:
  `HOME=<isolated> miner auth --user_auth=... --password=... -f`.
- The distro containers mint **inside** `distro-smoke.sh`, into the script's
  isolated `HOME_DIR`; the systemd, Windows, macOS, FreeBSD and OpenRC cells
  mint in their own step into a throwaway directory.
- Each path applies the **production-account refusal guard**: the JWT payload
  is decoded and a `mesocyclone` (production) account makes the cell fail
  before any provider work happens.
- The token is copied with mode 600 into the state directory and is **never
  printed** (no `set -x` around it, no evidence copies of the raw value; the
  evidence records only `network_name`).

## Capability gaps (TODO gates, honest by design)

Checks whose code is not in the tree at this commit are printed as
`CONDITIONAL/TODO` with a named tag and switch themselves on when the code
lands (file-existence greps over the synced repository):

- `TODO(freebsd-urnettools)` — `internal/urnettools` has no FreeBSD port yet,
  so `urnet-tools` cannot be cross-built for FreeBSD and the FreeBSD
  discovery/lifecycle surface is not asserted. The miner itself builds and
  runs, and its control socket is asserted on the real kernel.
- `TODO(freebsd-miner-tests)` — `./miner`'s test files import
  `internal/durablefixture`, which has no freebsd build, so no miner test
  binary exists yet.
- `TODO(freebsd-installer)` / `TODO(freebsd-optimize)` — no
  `scripts/Provider_Install_FreeBSD.sh` and no
  `internal/urnettools/optimize_bsd.go` on this line.
- `TODO(openrc-installer)` — `scripts/Provider_Install_Linux.sh` has no OpenRC
  branch yet; the OpenRC cell writes the equivalent unit itself and marks the
  installer flow as a TODO.
- `TODO(openrc-tools)` — no OpenRC discovery/log backend in
  `internal/urnettools`; the targeted `status --state-dir` path is asserted.
- `TODO(windows-control-socket)` / `TODO(macos-control-socket)` — recorded in
  the workflow; promote to hard cells when the transport is verified.

## Running the cells locally

All local runs use the static payload and a disposable JWT and an explicit,
isolated `HOME`; nothing reads or writes the ambient home.

Build the payload (from a worktree at the commit under test):

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/distroci/bin/miner ./cli/miner/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/distroci/bin/urnet-tools ./cmd/urnet-tools/
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o /tmp/distroci/bin/urnet-docker ./cmd/urnet-docker/
cp <disposable-jwt> /tmp/distroci/bin/../fixtures/jwt && chmod 600 /tmp/distroci/fixtures/jwt
```

Container cells (repeat per image in the matrix):

```sh
mkdir -p /tmp/distroci/out /tmp/distroci/state && chmod 777 /tmp/distroci/out
docker run --rm \
  -e BIN_DIR=/opt/test/bin \
  -e JWT_FILE=/opt/test/fixtures/jwt \
  -e HOME_DIR=/state/home -e WORK_DIR=/state/work \
  -e STATE_DIR=/state/home/.urnetwork -e OUT_DIR=/state/out -e TIMEOUT_S=90 \
  -v /tmp/distroci/bin:/opt/test/bin:ro \
  -v /tmp/distroci/fixtures:/opt/test/fixtures:ro \
  -v /tmp/distroci/state:/state \
  -v /tmp/distroci/out:/state/out \
  -v "$PWD/.github/scripts/distro-smoke.sh:/opt/test/distro-smoke.sh:ro" \
  <image> /bin/sh /opt/test/distro-smoke.sh
```

(`/bin/sh` works on every matrix image, including Alpine's busybox ash.)

systemd cell locally: build the image exactly like the workflow's step does
(`Dockerfile.systemd`), then `docker run --privileged --cgroupns=host
-v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run ... /sbin/init` and
`docker exec -e BIN_DIR=/opt/test -e JWT_FILE=/run/ci/jwt -e REPO_DIR=/opt/repo
-e OUT_DIR=/srv/smoke/out sh /opt/scripts/systemd-smoke.sh`.

OpenRC cell locally: `docker run --rm --cap-add SYS_PTRACE` on
`alpine:latest` with `BIN_DIR`, `REPO_DIR`, `JWT_FILE`, `OUT_DIR` and
`.github/scripts/openrc-smoke.sh` mounted, exactly as the workflow's step does.

FreeBSD cell locally: run `BIN_DIR=out REPO_DIR=$PWD JWT_FILE=out/fixtures/jwt
OUT_DIR=out/freebsd-evidence sh .github/scripts/freebsd-smoke.sh` inside a
FreeBSD 14.5 VM (the CI job uses `vmactions/freebsd-vm`; there is no container
substitute for a real kernel).

## Pinning images

Images are addressed by the operator-approved tags above. To pin digests
(a one-step change per image), replace `image: debian:stable-slim` with
`image: debian@sha256:<digest>` in the matrix `include:` entry; the `slug`
stays the same so artifact names remain stable. Pin all cells at once or none
at all — a mixed matrix makes rollbacks ambiguous.

## Files

- `.github/workflows/distro-matrix.yml` — the release matrix.
- `.github/workflows/freebsd.yml`, `openrc-smoke.yml`, `portable-core.yml` —
  the restored release-only verification workflows.
- `.github/scripts/distro-smoke.sh` — container smoke; mints and guards the
  disposable JWT, isolated `HOME`.
- `.github/scripts/systemd-smoke.sh` — systemd unit lifecycle harness.
- `.github/scripts/openrc-smoke.sh` — Alpine/OpenRC service lifecycle harness.
- `.github/scripts/freebsd-smoke.sh` — FreeBSD kernel harness shared by
  `distro-matrix.yml` and `freebsd.yml`.
