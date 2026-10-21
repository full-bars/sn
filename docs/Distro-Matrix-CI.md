# Distro Matrix CI (release-only)

The release distro CI proves that a tagged release runs on the Linux
distributions we ship to — plus Windows, macOS and FreeBSD — before operators
see it. It is deliberately **release-only**: the cells are slow (privileged
systemd containers, a FreeBSD VM, a Windows runner), so they run on release
tags and on manual dispatch, never on pull requests or a nightly schedule. The
per-PR quality gate remains `build.yml`.

| Workflow | Trigger | Purpose |
| --- | --- | --- |
| `.github/workflows/distro-matrix.yml` | `v*` tags, `workflow_dispatch` | The matrix: the real installer, container smoke, systemd, Windows, macOS, FreeBSD, summary |
| `.github/workflows/freebsd.yml` | `v*` tags, `workflow_dispatch` | Real FreeBSD kernel verification (restored, adapted to the miner/tools line) |
| `.github/workflows/openrc-smoke.yml` | `v*` tags, `workflow_dispatch` | Alpine/OpenRC service smoke + cross-OS vet (restored) |
| `.github/workflows/portable-core.yml` | `v*` tags, `workflow_dispatch` | Cross-OS build/vet matrix + native Windows/macOS tests (restored) |

## The matrix cells

`distro-matrix.yml` builds the static `linux/amd64` payload once
(`build-binaries`, `CGO_ENABLED=0` so it runs on musl and on glibc 2.34+) and
fans it out to independent cells:

- **`installer-fast` / `installer-lifecycle` — the real installer**: runs
  `scripts/Provider_Install_Linux.sh` against a local fake release
  (`scripts/ci/fake-release-server` + `.github/scripts/installer-lifecycle.sh`).
  Four cells on installer-path pull requests; thirteen on tags (debian 12 and
  13, almalinux 8/9/10, fedora, amazonlinux 2 and 2023, leap 16, tumbleweed,
  arch, alpine, alpine 3.19). Each cell asserts the install tree, `.version`,
  the binary's own reported version, a reinstall, a tag switch and an
  uninstall, in a seeded (reproducible) order.
- **`installer-arm64` — 2 cells under QEMU**: debian 12 and alpine from a
  per-arch payload, which exercises the installer's `uname -m` mapping and the
  arm64 tarball layout.
- **`installer-variants` — 6 cells**: noexec `/tmp`, IPv6 disabled, non-root
  with sudo, the `URNET_NO_AUTOINSTALL=1` refusal, an Ubuntu-shaped Debian
  (systemd-resolved stub, needrestart, ufw, netplan, `/snap/bin`) and a 256 MiB
  cgroup cap.
- **`lsm-selinux` / `lsm-apparmor` — VMs, non-gating**: SELinux enforcing on
  AlmaLinux and AppArmor on Ubuntu. Containers cannot exercise either (the LSM
  is a host-kernel facility), and each harness first asserts the LSM is
  actually active, so an inactive host is a CONDITIONAL rather than a silent
  pass.
- **`distro-smoke` — 7 container images**: `debian:stable-slim`,
  `almalinux:9`, `fedora:latest`, `opensuse/leap:16.0`,
  `opensuse/tumbleweed`, `archlinux:latest`, `alpine:latest`.
  No Ubuntu, no Rocky. Each cell runs `.github/scripts/distro-smoke.sh` in the
  distro's own userland: the binaries load, `miner provide` binds
  `$STATE/provider.sock` (mode 0600), `urnet-tools version/status` reach it,
  and SIGTERM stops the miner cleanly.
- **`systemd` — 6 images**: the same families minus Alpine, which has no
  systemd: `debian:stable-slim`, `almalinux:9`, `fedora:latest`,
  `opensuse/leap:16.0`, `opensuse/tumbleweed`, `archlinux:latest`. A
  privileged container with `/sbin/init` as PID 1 installs
  `.github/scripts/systemd-smoke.sh`'s unit and drives
  start/socket/status/stop/restart/crash-restart through `systemctl`.
  Before the container starts, the job runs
  `sh .github/scripts/systemd-smoke.sh --self-test` on the runner: the
  regression fixtures for the exit-75 rejection matcher, which must not read
  a `:40150` UDP port as a 401 while still matching `HTTP/1.1 401` and the
  miner's `network_sign_in_rejected` event.
  The image build installs **curl where the family provides it** (apt, dnf,
  zypper, pacman) with **netcat as the documented fallback**; the assertions
  themselves talk to the control socket through `urnet-tools`.
- **`windows` — `windows-latest`**: build + vet + `./internal/urnettools`
  unit tests, then the miner runs natively and the control socket is asserted
  (path, listen, `urnet-tools status`, `urnet-tools version`). If the
  transport is not available at the checked-out commit the step records the raw
  evidence and a `TODO(windows-control-socket)` warning instead of faking a
  pass.
- **`macos` — `macos-15-intel` (x64) + `macos-15` (arm64)**: build, vet, unit tests and the launchd
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
rg -n 'max-parallel|needs:|concurrency' \
  .github/workflows/distro-matrix.yml .github/workflows/freebsd.yml \
  .github/workflows/openrc-smoke.yml .github/workflows/portable-core.yml
```

The only expected hits are the three intended fan-in edges in
`distro-matrix.yml` — two `needs: build-binaries` (the container and systemd
cells) and one `needs: [build-binaries, ...cells]` on the summary — with no
`max-parallel:` or `concurrency:` line in any of the four workflows. Each of
the four declares exactly the same triggers:

```yaml
on:
  push:
    tags:
      - 'v*'
  workflow_dispatch:
```

(no `pull_request`, no `schedule`).

## Release gating (installer tier, report mode until two clean tags)

The gate is implemented in `release.yml`'s `create-release` job, in the step
named "Gate on the installer matrix". It finds the `Distro matrix` run for the
same commit (retrying while GitHub creates it), waits for it to complete
(bounded at 60 minutes), then fails the release when any job whose name starts
with `installer` concluded `failure` or `cancelled`.

It deliberately reads **installer job conclusions**, not the matrix workflow's
own conclusion. That is what makes the gate installer-only: a red smoke,
features, acceptance, systemd, LSM VM, Windows, macOS or FreeBSD cell is
advisory and can never block a release. The reasoning is in the workflow
comment: a red installer cell means a user's first command fails, which is
worth blocking for; the slower cells carry infra flake.

`RELEASE_GATE_MODE` in that step is the switch:

- `report` (current) — prints that the installer matrix is advisory and exits 0.
- `block` — enforces the gate.

Flipping to `block` is a one-line change in each `release.yml`, done only after
**two clean TAG runs**. Every green so far came from `workflow_dispatch`; the
tag path differs (`GITHUB_REF_NAME`, tag-scheme gating, concurrent release
runners) and had never exercised the installer tier when this was written.

`urnetwork-3.23-fix` and `meso-miner` carry the same step in their
`release.yml`, pointing at their `Installer matrix` workflow.

The restored `freebsd.yml`, `openrc-smoke.yml` and `portable-core.yml`
workflows still fail themselves on a broken cell but block no release.

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

Checks whose code is not in the tree at a given commit print
`CONDITIONAL/TODO` with a named tag and switch themselves on when the code
lands (file-existence greps over the synced repository).

State on this line:

- **FreeBSD** — `scripts/Provider_Install_FreeBSD.sh`,
  `internal/urnettools/discover_freebsd.go` and `optimize_bsd.go` are all
  present, and the cell runs the miner and `urnet-tools` on a real kernel. The
  remaining gap is **durable client registration**: the provider refuses with
  "durable client registration requires Unix custody support", which the
  harness records as `CONDITIONAL/TODO(freebsd-durable-registration)` instead
  of two failures.
- **FreeBSD miner tests** — `internal/durablefixture` has no FreeBSD build, so
  `TODO(freebsd-miner-tests)` remains; no miner test binary exists yet.
- **OpenRC** — `scripts/Provider_Install_Linux.sh` does have an OpenRC branch
  (install-tree redirect, init script, `rc_ulimit`, `rc-update` registration).
  The container installer cells have no service manager, so service assertions
  there are skipped; wiring the fake-release install flow through the OpenRC
  branch remains `TODO(openrc-installer)`.
- **Windows / macOS control socket** — recorded per cell
  (`TODO(windows-control-socket)`, `TODO(macos-control-socket)`); promote to
  hard assertions once the transport is verified on those platforms.

## Running the cells locally

All local runs use the static payload and a disposable JWT and an explicit,
isolated `HOME`; nothing reads or writes the ambient home.

The exit-75 rejection matcher fixtures need no container and run on any host
(POSIX sh — dash, busybox ash and bash all pass today):

```sh
sh .github/scripts/systemd-smoke.sh --self-test
```

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
  -e HOME=/state/home -e HOME_DIR=/state/home -e WORK_DIR=/state/work \
  -e STATE_DIR=/state/home/.urnetwork -e OUT_DIR=/state/out -e TIMEOUT_S=90 \
  -v /tmp/distroci/bin:/opt/test/bin:ro \
  -v /tmp/distroci/fixtures:/opt/test/fixtures:ro \
  -v /tmp/distroci/state:/state \
  -v /tmp/distroci/out:/state/out \
  -v "$PWD/.github/scripts/distro-smoke.sh:/opt/test/distro-smoke.sh:ro" \
  <image> /bin/sh /opt/test/distro-smoke.sh
```

(`/bin/sh` works on every matrix image, including Alpine's busybox ash. The
harness itself re-exports `HOME=$HOME_DIR` for every process it starts; the
`-e HOME` above keeps the container-level environment pointing at the same
isolated directory.)

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
  the restored release-only verification workflows; `openrc-smoke.yml` carries
  the dash/bash/shellcheck lint for the installer and for the release-matrix
  smoke scripts.
- `.github/scripts/distro-smoke.sh` — container smoke; mints and guards the
  disposable JWT, isolated `HOME`.
- `.github/scripts/systemd-smoke.sh` — systemd unit lifecycle harness;
  carries the exit-75 rejection matcher and its `--self-test` regression
  fixtures (the `:40150` case is pinned there).
- `.github/scripts/openrc-smoke.sh` — Alpine/OpenRC service lifecycle harness.
- `.github/scripts/freebsd-smoke.sh` — FreeBSD kernel harness shared by
  `distro-matrix.yml` and `freebsd.yml`.
