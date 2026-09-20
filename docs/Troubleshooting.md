# Troubleshooting

## Incident quick diagnosis

> [!NOTE]
> On Docker deployments, run these commands inside the container via
> `docker exec -it <container> urnet-tools <command>`, or directly from the
> host using `urnet-docker <command> --unit <name>`.

| Symptom / Error | Probable Cause | Action |
| :--- | :--- | :--- |
| Repeated auth errors or login failures | Backend outage, expired credentials, or invalid auth code | Run `urnet-tools status`; monitor self-healing status with `urnet-tools self-heal status`. Verify `USER_AUTH` and `PASSWORD`, or `URNETWORK_AUTH_CODE`. |
| Container exits with code `78` | JWT expired, invalid, or not persisted | Ensure `/root/.urnetwork` is mounted as a volume. Check `USER_AUTH`/`PASSWORD` or `URNETWORK_AUTH_CODE` in the environment, or re-authenticate (auth codes are single-use). |
| Memory ballooning / OOM kills | High proxy count without a memory profile | Set `URNETWORK_PROFILE=auto` or `eco`; enable `URNETWORK_SELF_HEAL=1`. |
| `proxy refresh` refused shortly after a restart | Warmup lockout | Run `urnet-tools proxy refresh --force` to bypass the warmup gate. |
| Disk space exhaustion | Unrotated logs filling the disk | Enable `URNETWORK_RAMLOGS=1`, or set Docker log rotation with `--log-opt max-size=10m --log-opt max-file=3`. |
| Proxies marked dead or degraded | Proxy failure or dropped connections | Run `urnet-tools proxy health` and prune with `urnet-tools proxy remove-dead`. |
| `urnet-tools set` seems not to apply | The change was rejected, queued, or applied only on restart | Check the provider log for `[control]` lines. See Confirming a settings change below. |
| `status` shows a running process but no control socket | Startup failure, or a second provider for the same OS user | The socket is the liveness signal, not the PID. Check the log for a startup error and `urnet-tools providers --all` for a collision. |

## Exit codes

Every time the provider binary exits with a non-zero code, it prints a
`FATAL [exit <code>]: ...` line to both stderr (visible in `docker logs`) and
the ramlog file (visible via the `logs` command). The message describes the
failure and what happened.

| Code | Meaning |
|------|---------|
| 0 | Clean shutdown (SIGTERM or manual stop). |
| 78 | The JWT is expired or invalid. The container startup script intercepts this code, deletes the stale JWT, and re-authenticates when an auth code is available (see Container troubleshooting). |

## Container troubleshooting

If your container exits unexpectedly:

1. **Check the exit code**: `docker inspect <name> --format '{{.State.ExitCode}}'`
2. **Look up the code** in the table above for the likely cause.
3. **Read the ramlogs**: `docker exec <name> logs` (requires
   `URNETWORK_RAMLOGS=1`).
4. **Exit 0** means a clean shutdown (SIGTERM or manual stop).
5. **Exit 78** means the JWT expired. The script attempts automatic
   re-authentication when `URNETWORK_AUTH_CODE`, `USER_AUTH`, or `PASSWORD`
   are set correctly.
6. **All other non-zero codes** indicate a configuration or environment
   problem. The fatal message describes the specific issue.

### Fatal messages always write to both logs and stderr

The fatal-exit path writes the `FATAL [exit <code>]` line directly to the
ramlog file before exiting. This bypasses the normal log pipe, so the message
is never lost to a scheduling race. It also writes to the original stderr, so
the message appears in `docker logs` regardless of the ramlog setting. You do
not lose the error message no matter how you view logs.

## Resource exhaustion

### Disk space (log ballooning)

Logs are chatty. Without management, they can grow to several gigabytes in
hours.

- **Symptoms**: "No space left on device" errors, system instability.
- **Fix**: use Docker log rotation flags
  (`--log-opt max-size=10m --log-opt max-file=3`) or enable ramlogs
  (`URNETWORK_RAMLOGS=1`) to redirect output to `/dev/shm`.

### CPU starvation

In high-volume environments, a pegged CPU delays the processing of signaling
packets.

- **Symptoms**: frequent timeout errors despite a stable network.
- **Fix**: if you use `--cpus`, make sure the limit is high enough to handle
  the signaling overhead of your proxy list.

## HotSwap declines on an existing node

**Symptom**: `urnet-tools hotswap`, or a `urnet-tools update` on a HotSwap-capable
release, restarts the provider normally instead of handing off with no downtime.
The update prints `hotswap trigger unavailable (...); falling back to service restart`.

**Cause**: the systemd handoff requires a `Type=notify` unit. The retiring
process aborts whenever systemd started it (`INVOCATION_ID` is set) but
`NOTIFY_SOCKET` is empty, which is exactly what a `Type=simple` unit looks like.
`Provider_Install_Linux.sh` deliberately writes a unit with **no `Type=` line**
(systemd's default, `simple`), because `Type=notify` blocks `systemctl start`
until the provider is ready and can wedge every start when the binary and the
unit come from different releases. Instead, `urnet-tools update` migrates the
unit to `Type=notify` when the binary it installs can signal readiness
(v3.23.0-fix.31.0 or newer). That first update is a normal restart; later ones
hotswap.

> [!WARNING]
> Releases up to and including v3.23.0-fix.32.0 skipped that migration for units
> with no `Type=` line, so those nodes stay on restart-only updates
> indefinitely, and the "update migrates it" wording in the decline message was
> not true for them. Fixed in the release after v3.23.0-fix.32.0.

```bash
# Confirm what the unit actually is
systemctl --user show urnetwork.service -p Type,NotifyAccess   # drop --user for a system unit
```

**Fix**: nothing manual. Run `urnet-tools update` on a build that includes the
migration fix: the first update converts the unit to `Type=notify` and restarts
once, and the updates after that hotswap (see [HotSwap](HotSwap.md#which-update-hotswaps)).
If the update prints `note: ... is Type=simple but its unit file cannot be
migrated automatically`, `Type=` is set by a drop-in you control: change it
there to `Type=notify` and `NotifyAccess=all`, then `systemctl daemon-reload`.
A `hotswap unavailable: the running provider was started before its systemd
unit became Type=notify` message means the unit was converted but the provider
has not restarted since; the update restarts it for you and later ones hotswap.

> [!IMPORTANT]
> The check deliberately fails closed. An earlier revision gated only on the
> version string, so the trigger fired, the internal handoff silently aborted,
> and the "hotswap triggered" path skipped the restart fallback entirely,
> turning every update on a pre-existing node into a permanent no-op. Losing
> zero-downtime is the safe failure; a bricked update is not.

## Confirming a settings change

**Symptom**: `urnet-tools set <key> <value>` returns success, but the
provider does not appear to be using the new value.

The CLI's exit code only says the request was accepted. The provider logs what
it actually did, and that is the authoritative signal:

```bash
urnet-tools logs | grep -E '\[control\]'
```

| Log line | Meaning |
| :--- | :--- |
| `[control] set <key>=<v> ...` | Applied. Nothing further needed. |
| `[control] ... rejected: <reason>` | Not applied at all. The message says why. |
| `[control] ... failed to persist, rolled back` | The change could not be stored and was rolled back. |

**No `[control]` line at all** means the provider never received the request.
Check that it is running and that its socket is bound:

```bash
urnet-tools status          # reports control socket liveness
urnet-tools providers --all # as root, to rule out a same-user collision
```

Larger deployments that hit repeated signaling timeouts should review the
guidance in High-Volume-Performance-Tuning.