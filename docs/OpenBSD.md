# OpenBSD

This page covers running the URnetwork provider and `urnet-tools` on OpenBSD (amd64 and arm64). For FreeBSD, see the [FreeBSD guide](/full-bars/sn/wiki/FreeBSD). For other platforms, see the [Installation Guide](/full-bars/sn/wiki/Installation).

The provider runs as an `rc.d` service named `urnetwork` under an unprivileged user. There is no hosted one-liner for this fork. You fetch this repository's script and run it, and it downloads the release for your architecture.

> [!IMPORTANT]
> The OpenBSD installer, the `rc.d` script and the release assets are checked by reading the scripts and by cross-compiling. They have not been run on an OpenBSD host. The OpenBSD and FreeBSD installers are different scripts, so run each one only on its own platform.

> [!WARNING]
> **OpenBSD has no durable client registration in this line.** Registration is refused with `durable client registration requires Unix custody support`, because the unsupported-platform stub in `clientauth/registration_store_unsupported.go` is built for every platform except Linux, macOS and Windows. Legacy authentication remains available. Do not deploy OpenBSD nodes for production until that gap is closed.

## Before you start

The base system has `ftp(1)`, `sha256(1)`, `tar`, `awk` and `rcctl`, so downloads need no extra package. The installer uses `ftp` first, then `curl`, then `wget`.

The installer does need one JSON parser. It installs the provider tarball only after checking its SHA-256 against the digest in the release metadata, and reading that metadata takes `jq` or `python3`. With neither, the install stops before it downloads anything. Install `jq`:

```sh
doas pkg_add jq
```

The installer needs root for the install tree and the `rc.d` script. It uses `doas` first and `sudo` as a fallback. If neither is configured, add a rule to `/etc/doas.conf` (for example `permit persist youruser`) or run from a root shell.

## Install

Choose an existing non-root service account and replace `urnet` in these commands with its name. Pass it explicitly so an existing `urnetwork` account is not selected instead:

```sh
ftp -o urnet-install.sh https://raw.githubusercontent.com/full-bars/sn/refs/heads/main/scripts/Provider_Install_OpenBSD.sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

To install a specific release instead of the latest, add the tag:

```sh
doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install RELEASE_TAG
```

From a root shell, name an existing non-root account:

```sh
env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh install
```

The installer resolves the tag through the GitHub API. The release metadata is needed for the digest check even when you pass a tag, so a rate-limited or unreachable API stops the install with `Refusing to install an unverified provider binary.` Wait for the limit to reset and run it again.

### Service user

The provider must not run as root, and the installer refuses to set that up. It picks the account in this order:

1. `SERVICE_USER_OVERRIDE`, if set.
2. `URNETWORK_USER`, if set.
3. An existing `urnetwork` account.
4. `DOAS_USER`, the account that ran `doas`.
5. `SUDO_USER`, the account that ran `sudo`.
6. `logname`, unless it is `root`.
7. `id -un`, the current user.

A plain root login with none of the above resolves to `root`. The install then stops with `refusing to install a service that runs as root` before it downloads anything.

### What the installer does

- Downloads `urnetwork-provider-<tag>.tar.gz` and verifies its SHA-256 digest before it extracts anything.
- Installs the provider to `/usr/local/libexec/urnetwork-provider/bin/urnetwork`, owned by root.
- Installs `urnet-tools` from the `urnet-tools-openbsd-<arch>` asset after verifying its digest. A tool asset with no digest falls back to the shell `urnet-tools`.
- Writes `/etc/rc.d/urnetwork`, runs `rcctl enable urnetwork` on a fresh install, and starts the service.
- Keeps state in the service account's `~/.urnetwork`.

An update leaves your `rcctl enable` or `disable` choice alone.

### Build from source

On an OpenBSD host:

```sh
doas pkg_add go git
git clone https://github.com/full-bars/sn.git
cd sn
CGO_ENABLED=0 go build -o urnetwork ./cli/miner
CGO_ENABLED=0 go build -o urnet-tools-openbsd ./cmd/urnet-tools
```

Check `go version` against `go.mod` first. Put the binaries in a root-owned directory that the service account cannot modify.

## Authenticate

Run authentication as the same non-root account that will run the provider. Get a single-use auth code from <https://ur.io>:

```sh
/usr/local/libexec/urnetwork-provider/bin/urnetwork auth <code>
```

The provider stores its state in that account's `~/.urnetwork`. Keep this directory private. Restart the service afterwards so the provider picks up the token.

## Service commands

Use `rcctl`:

```sh
doas rcctl start urnetwork
doas rcctl stop urnetwork
doas rcctl restart urnetwork
rcctl check urnetwork
doas rcctl enable urnetwork
doas rcctl disable urnetwork
```

Extra provider flags go in the service flags: `doas rcctl set urnetwork flags "<flags>"`. The provider output goes to syslog at `daemon.info`, which OpenBSD writes to `/var/log/daemon` by default.

The installer script offers the same commands as the other platforms: `install`, `start`, `stop`, `restart`, `status`, `update`, `version`, `hot-restart`, `self-heal`, `session` and `proxy`. Run `sh urnet-install.sh --help` for the full list.

`urnet-tools auto-start on|off` runs `rcctl enable|disable urnetwork`. It needs an installed `rc.d` script and fails with a clear message when there is none.

For a foreground test, run as the service account:

```sh
/usr/local/libexec/urnetwork-provider/bin/urnetwork provide
```

## Updates and auto-update

Run `doas env SERVICE_USER_OVERRIDE=urnet sh urnet-install.sh update` to move to a new release, using the same account as at installation. Use `update` instead of `install` to keep a stopped or disabled service as it is.

`urnet-tools update` installs the `openbsd/<arch>/provider` file from the release tarball and restarts the service with `rcctl`. It needs a discoverable binary (see Platform notes). To schedule updates in root's crontab, run `doas urnet-tools auto-update daily`. Check `urnet-tools version` after the first update, because this path has not run on a real OpenBSD host.

## Sysctl tuning

This repository has no tested OpenBSD tuning targets, and the FreeBSD sysctl names do not carry over. `urnet-tools optimize` is not supported on OpenBSD and returns an error. Inspect the host's limits with `sysctl` and read the OpenBSD manual before you change them.

## Platform notes

- **Process discovery:** `urnet-tools` uses `ps` to find running providers by process name. It does not discover stopped services or infer a binary path from process arguments. It checks the standard libexec location for an executable, and that path is not proof of the running image. If the binary is not there, `update` and the proxy commands that delegate to the provider (`add`, `remove`, `trim`, `paste`) cannot resolve it. There is no `--binary` override.
- **Lifecycle:** `urnet-tools start`, `stop` and `restart` run `rcctl` for the `urnetwork` service and need the `/etc/rc.d/urnetwork` script. `urnet-tools status` runs `rcctl check`.
- **Memory:** Total physical memory comes from `hw.physmem` when available. Available host memory is not reported, so host memory-pressure and swap checks use the unsupported fallback.
- **Control socket:** Peer credential verification does nothing on OpenBSD. The control socket relies on its file permissions, so keep the service account and its state directory private.
- **Disk diagnostics:** Free space uses the OpenBSD `statfs` interface.

`urnet-tools` accepts only `/usr/local/libexec/urnetwork-provider/bin/urnetwork`. The file must be executable by other users and not writable by group or others. Binaries in a home directory are ignored, because `urnet-tools` often runs as root.

## Release assets

Each release publishes `urnetwork-provider-<tag>-openbsd-amd64.tar.gz` and `-openbsd-arm64.tar.gz`, plus `urnet-tools-openbsd-amd64` and `urnet-tools-openbsd-arm64`. The universal provider tarball `urnetwork-provider-<tag>.tar.gz` carries an `openbsd/<arch>/` tree, and the installer reads it. OpenBSD assets exist only in releases cut after this support was added.

## Uninstall

First remove the auto-update entry from root's crontab without deleting provider state. While the provider is running and discoverable:

```sh
doas urnet-tools auto-update off
```

If the provider is already stopped, edit root's crontab with `doas crontab -e` and remove the URnetwork auto-update line. `urnet-tools uninstall` also runs `rcctl stop` and `rcctl disable` as a best-effort step. To do it by hand, stop and disable the service and remove its script and binaries:

```sh
doas rcctl stop urnetwork
doas rcctl disable urnetwork
doas rm /etc/rc.d/urnetwork
doas rm -r /usr/local/libexec/urnetwork-provider
```

`urnet-tools uninstall` also removes the discovered provider's state directory after confirmation, including login and proxy state. The service account's `~/.urnetwork` holds that data, so remove it only when you no longer need it.
