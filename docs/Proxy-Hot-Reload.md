# Proxy Hot-Reload

The provider can add and remove proxies from a running instance without
restarting or dropping existing connections. Changes take effect within a
couple of seconds of issuing the reload command.

## How it works

The reload system uses a trigger file at `~/.urnetwork/proxy.reload` that
contains a sequence number. A background goroutine polls that file every 2
seconds. When it detects the sequence number has changed, it diffs the current
proxy list against the live set and applies the delta.

```
urnet-tools proxy refresh
        │
        ▼
~/.urnetwork/proxy.reload  (seq incremented)
        │
        ▼  (within a couple of seconds)
reload watcher goroutine detects change
        │
        ▼
reload() diffs source vs running set
        │
        ├─ Added proxies   → goroutines launched with staggered startup
        └─ Removed proxies → goroutines cancelled, connections cleaned up
```

The reload is serialized by a mutex. Two simultaneous reloads cannot race. If
a reload is already in progress when a second trigger fires, the second call
returns immediately with an error logged (`reload already in progress`).

## Self-healing: the hourly reload reconciler

The provider also runs an hourly reload reconciler. It fires a reload trigger
once per hour unconditionally, whether or not anything else requested one.

**Why it exists:** `reload()` only ran on an explicit trigger (add-source,
remove-dead, proxy refresh, URL fetch merge, reaper change). A mass-failure
event, such as a transient backend outage, could leave a batch of still-desired
proxies stuck out of the running set with no future event scheduled to bring
them back. This was observed live on a production node where ~3,300 proxies
stayed offline for ~22 hours until an unrelated `add-source` forced a reload,
at which point all of them recovered in one cycle.

The reconciler is cheap when nothing is wrong: if the running set already
matches the desired set, `reload()` just logs `+0 added, -0 removed` and
returns. It is unconditional, not gated behind `URNETWORK_SELF_HEAL`, and
sits in the same tier as the degraded-proxy reaper.

## State reconciliation: no ghost entries

`reload()` also prunes `proxy.state` to the full desired set (config/file +
URL cache), not just the running diff. Previously, a dead or offline proxy
whose goroutine had already exited was never in the running set, so its stale
`proxy.state` entry was never deleted. That accumulated ghost entries that
`proxy remove-dead` re-reported as "removed" on every run, forever. Now those
are pruned on the next reload, and `proxy remove-dead` reports accurate
removal counts.

## Triggering a reload

### Native (Linux service)

```bash
# Edit your proxy file, then:
urnet-tools proxy refresh
```

### Docker

The `~/.urnetwork` directory must be mounted as a volume for the trigger file
to be reachable from outside the container:

```bash
docker run -v urnetwork_data:/root/.urnetwork ...
```

Then trigger from the host:

```bash
docker exec urfix urnet-tools proxy refresh
```

Or write the trigger file directly from the host if you have access to the
named volume mount point.

## What happens during a reload

1. The watcher reads the proxy source file (the `--proxy_file` flag or the
   default path).
2. It computes the diff: which proxy addresses are new, which have been
   removed.
3. **Added proxies**: new goroutines are launched with the same jittered
   startup stagger as initial proxies, to avoid a thundering herd on the
   platform.
4. **Removed proxies**: their context is cancelled, connections drain and
   close cleanly.
5. **Unchanged proxies**: untouched, no interruption to live sessions.
6. The updated proxy set is written to `proxy.state`.

Reload output:

```
[proxy] reloaded: +12 added, -3 removed
```

## What counts as a change

Proxies are identified by address (`host:port`) and credentials. A proxy that
appears in the new file with the same address and credentials as a running
proxy is treated as unchanged. Only additions and removals are applied.

## Edge cases

| Situation | Behavior |
|---|---|
| Proxy file missing or unreadable at reload time | Reload is skipped with a warning log; running proxies are unaffected |
| Reload already in progress | Second reload is skipped; logged as `reload already in progress` |
| No usable proxies found in source | Reload is skipped as a safety guard. Removing every proxy is treated as an accidental wipe, not an intent to stop all proxies |
| Provider restarted | Reads the proxy source fresh on startup; `proxy.state` is used as a secondary consistency check |

## Environment variables

| Variable | Purpose |
|---|---|
| `--proxy_file` (flag) | Path to the proxy list file (default: `~/.urnetwork/proxy.txt`). This is a CLI flag, not an environment variable. |