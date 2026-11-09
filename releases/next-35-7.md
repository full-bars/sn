# Next sn release

I added a second way to let the extender use TCP 443. I also made updates verify the new version more safely, and I made installer failures easier to recover from.

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

`[redeploy]` means update the affected binary or image and restart it. `[installer]` means the change is in the install script, so run the new script or use `urnet-tools update`. `[no redeploy]` means documentation or test changes need no restart.

### Extender

- **A sysctl route for TCP 443** [redeploy]. On Linux, `urnet-tools extender setup` can set `net.ipv4.ip_unprivileged_port_start=443` in `/etc/sysctl.d/99-urnetwork-extender.conf`. This is an alternative to a file capability on the binary. It survives binary updates and keeps the provider's process image readable. Interactive setup asks which route to use. Unattended setup needs `--route=sysctl`, `--route=capability` or `--route=skip`, even with `--yes`.
- **Setup keeps a prior drop-in** [redeploy]. A drop-in with the same name is copied with its mode to `99-urnetwork-extender.conf.pre-urnetwork`. The name has no `.conf` ending, so sysctl never loads it. If the copy fails, setup stops before it writes. The printed recovery restores the copy and the prior live value. When the prior live value is unknown, it prints the check to run and does not guess one.
- **Persistence is read the way systemd reads it** [redeploy]. Setup scans `/etc/sysctl.d`, `/run/sysctl.d`, `/usr/local/lib/sysctl.d`, `/usr/lib/sysctl.d` and `/lib/sysctl.d`. A file name in an earlier directory hides the same name in a later one. Setup merges the files in name order, then reads `/etc/sysctl.conf`. The last assignment wins and must be 443 or lower. A file it cannot read, or a value it cannot parse, counts as not persisted. When the live and persisted settings already match, setup leaves them alone.
- **A sudo re-run is bound to one provider** [redeploy]. Root can see providers an unprivileged user cannot. The parent now passes the identity it inspected through `sudo -- /usr/bin/env`. Root refuses to configure anything when it resolves a different provider. The manual sudo command that setup prints carries the same identity. When setup cannot name one provider uniquely, it stops and prints the `providers --all` command to run first.
- **Status shows the port route** [redeploy]. `urnet-tools extender status` and `urnet-tools status` print the sysctl route, a capability on the binary, or neither. With the sysctl route active, they warn when the binary still has a file capability. Such a process is non-dumpable, so the updater may not read its image. The warning gives the `setcap -r` command and says to restart or hand off the provider afterwards. Setup offers the removal itself only to root on a terminal, with a separate yes, and only when the bind capability is the only one.

### Updates and verification

- **Updates run the newer tool first** [redeploy]. `urnet-tools update` installs a newer tool and runs it again, so the new release's own update logic does the work. The second run keeps the target and your approval. It also keeps the provider set the first run inspected, and it stops without a change if that set differs. If the install or the second run fails, the running tool continues. The marker `URNET_TOOLS_UPDATE_REEXEC` stops a loop.
- **Verification does not run the new image under OpenRC** [redeploy]. The old fallback ran the image as root, and the service user can influence that file. The loop now reads the version from build information and the release stamp only. It fails closed when neither holds a version.
- **An unreadable image is confirmed on the control socket** [redeploy]. A provider with the port-443 file capability is non-dumpable. The updater says so once and asks the provider for its version. It checks who answers on Linux and accepts only a new process. The raw permission error is not printed.
- **The verify loop explains a long wait** [redeploy]. It says once when an entry exists but is not ready, or when the state directory is missing. It then prints a short heartbeat.
- **Updates respect the sysctl route** [redeploy]. While the live threshold is 443 or lower, an update or rollback skips the capability check, the restore and the setcap warning. When a rollback needs it, the updater demotes `Type=notify-reload` units to simple.
- **The simple-unit message is accurate** [redeploy]. It says an older installer wrote a `Type=simple` unit. The current installer writes `Type=notify` with `NotifyAccess=all`.

### Installer

- **Restart approval works through a pipe and sudo** [installer]. Restart confirmation reads the terminal even when the script arrives through a pipe. Without a terminal, it stops and tells you to pass `-y`, before or after the subcommand. The approval survives `sudo`.
- **OpenRC preparation fails closed** [installer]. The install tree must be ready before the service is installed. A failure stops the install.
- **A failed restart restores the unit too** [installer]. The new unit is prepared and the old one is kept as `<unit>.old` before the binary swap. A failed restart restores the unit with the binary. When a step cannot finish, the installer prints the exact command to run.
- **The optimizer reports partial success** [installer]. It lists the steps it finished, skipped and failed, with a retry command for each failure.
- **An interactive install offers the port-443 capability** [installer]. It preserves other file capabilities and checks the result. It stops when it cannot verify the capabilities. Without a terminal, it prints the manual `setcap` command.
- **The install body runs once** [installer]. The script held two copies of it. One copy is left.

### Logs

- **The important log follows a replaced file** [redeploy]. A handoff or rotation can leave a process writing to a removed or replaced important log. The provider reopens the active path. It refuses a symlink, a file that is not regular, another user's file and a file with permissions beyond `0600`. It prints `[ramlogs] log file at <path> was replaced or removed; reopened`, at most every five minutes. A failed trim or reopen prints `[ramlogs] warning: cannot maintain <path>: <error>`.

### Documentation [no redeploy]

- The extender, installation, hot swap, troubleshooting and log guides describe the route choice, the update order, the verify messages and the installer behavior. The hot swap guide no longer says the installer writes a unit without `Type=`.

## Known limits

- Nothing ran on a real host. Installer and updater paths were tested with stubs, so real systemd and OpenRC behavior is not confirmed.
- The sysctl route applies to the whole host. Any local user can bind ports 443 and above. Other process restrictions can still make an image unreadable.
- The first update to this release runs in the old tool. A provider with the port-443 file capability can make that tool report a false failure once. Run `urnet-tools self-update` before the provider update to avoid it.
- Elevated setup works on one provider at a time. With several providers and no selector, or when discovery lists no provider, setup prints the inventory command and asks for an explicit `--unit` or `--state-dir`.
- Under OpenRC, an update on a binary with no version stamp fails verification instead of running the image. Release builds carry the stamp.
- Two processes can write the same log during a handoff. The in-place trim uses a lock inside one process. Lines written between a trim's read and truncate can be lost. A real handoff has not been tested.
- FreeBSD extender setup is untested and only prints manual commands.

[Full changelog](https://github.com/full-bars/sn/compare/v2026.11.8-1093262540-sn...main)
