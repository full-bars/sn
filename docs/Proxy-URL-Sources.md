# Proxy URL Sources

This guide covers feeding the provider a live proxy list URL instead of (or
alongside) a static `proxy.txt` file. It is useful when you pull proxies from a
service that publishes a rotating list of fresh entries.

## The problem

Some proxy-list endpoints publish new `ip:port` entries on a rolling basis,
some every few minutes. Without this feature, picking up fresh entries means
manually re-downloading the list, re-importing it, and hoping you don't
duplicate proxies you already added. There is also no way to automatically
prune entries that go dead without touching proxies you added by hand.

## What it does

Point the provider at a URL and it will:

- Fetch on an interval (default every hour) and add any genuinely new proxies.
  Entries already running, by address, are skipped. This never disturbs
  already-warmed-up proxies.
- Clean up dead entries on a separate, slower cadence (default every 6 hours),
  and only the ones that came from a URL, by default. Proxies you added
  yourself via a file or `proxy add` are left alone unless you explicitly
  widen the cleanup scope.
- Coexist with your existing proxy file. A URL source is additive: you can run
  `--proxy_file` and `--proxy_url` at the same time. They share one hot-reload
  pipeline (see [Proxy Management](Proxy-Management.md)).

## Stage-1 quality gate

Proxies that pass stage-0 (the SOCKS5 greeting and API `CONNECT`) are not yet
trusted with client traffic. Each one is graded against a sampled block of the
backend's destination table, dialed through the proxy itself at `:443`, before
it reaches the auth queue.

**Scoring**: a pass samples `sample_width` hosts (default 12) from the
backend's health table, disjoint from the previous pass, and dials each at
`:443` with a per-target timeout of `timeout_ms` (default 4000ms). The proxy's
score is the fraction of successful dials in the pass. Only a completed TCP
handshake counts as success; a timeout or refusal is not treated as proof the
proxy is bad, since the target host itself could be unreachable for unrelated
reasons. Hostnames are resolved through the box's own DNS, not through the
proxy being probed, so a proxy with broken DNS is not penalized for a
resolution failure that is not its fault.

A grade is only written when the pass is decidable: a quorum of the sampled
hosts answered and the probe context was not cancelled. An empty, cancelled,
or resolver-gutted pass leaves the proxy's previous grade in place rather than
overwriting it with a false failure.

**`pass_bar`**: a proxy must score at or above `pass_bar` (default 0.6) to be
admitted to the auth queue. A score at or above `preferred_bar` (default 0.9)
marks the proxy as preferred tier. Proxies that clear stage-0 but fail stage-1
never spawn, and are reported separately from dead proxies in the auth gate's
summary.

## A-F quality tiers

Every URL-source proxy that clears the stage-1 gate is assigned a letter grade
from its score:

| Grade | Score    |
|-------|----------|
| A     | `>= 0.9` |
| B     | `>= 0.8` |
| C     | `>= 0.7` |
| D     | `>= 0.6` |
| F     | `< 0.6`  |

- **Admission funnel**: candidates from all sources are pooled and added
  best-first up to the cache cap, instead of per-source in whatever order the
  sources happen to be processed.
- **Best-overall cache eviction**: when the cache is full, eviction compares
  candidates across all sources by tier, so a full cache keeps the fleet's
  highest-tier proxies regardless of source.
- **Per-cycle grade breakdown** is logged (`admitted by tier`, `probe grade
  breakdown`, `cap eviction`, `reaper: refreshed grade`).
- **Fetch probing**: each cycle probes only newly-seen addresses; the reaper's
  stale sweep refreshes grades on already-cached proxies.
- **Cross-source duplicates** are table-probed once per cycle.

## Paid and file-list proxy grading

Proxies from `--proxy_file` or the internal config bypass the URL admission
gate by construction. A background sweep grades every non-URL proxy the box
serves with the same stage-1 table probe, persisting score and grade into
`proxy.state`. Grading is read-only by construction: a grade never gates
admission, never evicts, and never feeds cleanup. A graded F keeps serving
exactly as it did before. `proxy_probe.json enabled=false` skips the sweep
entirely.

**Credentialed URL entries**: RFC 1929 auth (`host:port:user:pass`) is carried
through both the stage-0 and stage-1 probes, so paid and credentialed
URL-source entries are graded on the same footing as free ones.

**Kill switch**: create `~/.urnetwork/proxy_probe.json` with
`{"enabled": false}` to disable stage-1 entirely and fall back to
stage-0-only admission. The file also accepts `sample_width`, `timeout_ms`,
`pass_bar`, and `preferred_bar` overrides.

**Persistence**: the score, grade, and last-graded time are stored per proxy
in `proxy_url.json`, alongside the existing cache fields.

## Setting it up

### Binary / Linux service

Start the provider with a live source:

```sh
urnetwork provide --proxy_url=https://example.com/your-proxy-list.txt
```

Or manage sources at runtime without restarting:

```sh
urnet-tools proxy add-source https://example.com/your-proxy-list.txt
urnet-tools proxy remove-source https://example.com/your-proxy-list.txt
```

`add-source` triggers an immediate fetch and persists the URL so it survives
restarts. You don't need to keep passing `--proxy_url` by hand.

### Docker

```bash
docker run -d \
  --name=urnetwork \
  --restart=unless-stopped \
  --cap-add=NET_ADMIN \
  --cap-add=NET_RAW \
  --sysctl net.ipv4.ip_forward=1 \
  -e BUILD=jwt \
  -e PROXY_URL='https://example.com/your-proxy-list.txt' \
  -v /path/to/your/proxy.txt:/app/proxy.txt \
  ghcr.io/full-bars/sn:latest YOUR_AUTH_CODE_HERE
```

`PROXY_URL` and `-v .../proxy.txt` can be used together. The URL source adds
on top of whatever is in the mounted file.

> [!TIP]
> **Multiple URLs in Docker:** there is no `PROXY_URL_2` or `PROXY_URL_3`.
> Repeating `-e PROXY_URL=...` just overwrites itself, since Docker env vars
> are not additive. Put all your sources in one comma-separated `PROXY_URL`.
> The repeatable `--proxy_url=<url> --proxy_url=<url>` form is only available
> on the binary and CLI side, where flags can be passed more than once.

## Tuning flags

| Flag | Env var | Default | What it controls |
| :--- | :--- | :--- | :--- |
| `--proxy_url=<url>` | `PROXY_URL` | none | The live source. Pass multiple times (or comma-separate the env var) for more than one source. |
| `--proxy_url_refresh=<dur>` | `PROXY_URL_REFRESH` | `1h` | How often to fetch and add new entries. |
| `--proxy_url_max=<n>` | `PROXY_URL_MAX` | `500` | Caps total URL-sourced proxies. `0` = unlimited. Once hit, new entries are skipped until cleanup or restart frees room. Existing proxies are never evicted to make space. |
| `--proxy_dead_cleanup_scope=url\|all\|none` | `PROXY_DEAD_CLEANUP_SCOPE` | `url` | Which proxies the automatic cleanup may remove. `url` (default) means only URL-sourced dead proxies are cleaned. `none` disables it entirely. `all` treats every source the same. Manual `proxy remove-dead` always works regardless. |
| `--proxy_dead_cleanup_interval=<dur>` | `PROXY_DEAD_CLEANUP_INTERVAL` | `6h` | How often the automatic cleanup runs, when scope is not `none`. |

> [!TIP]
> If you're pulling from a free or public list, the default `url` scope is what
> you want. The provider keeps adding fresh entries on the refresh interval and
> quietly retires ones that never panned out, but it never touches proxies from
> your own hand-curated file.

## Supported list format

Plain-text lists only, one proxy per line: the same format `--proxy_file`
already accepts, plus an optional `socks5://` prefix:

```
1.2.3.4:1080
1.2.3.4:1080:myuser:mypass
socks5://1.2.3.4:1080
socks5://myuser:mypass@1.2.3.4:1080
```

Blank lines and `#` comments are ignored. Lines with a non-`socks5://` protocol
prefix are skipped with a warning (this fork is SOCKS5-only), and one bad line
does not fail the whole fetch. CSV and JSON list formats are not supported.

## How cleanup scope works

Every proxy is tagged internally with where it came from: `url`, `file`, or
`internal` (added via `proxy add`). The `--proxy_dead_cleanup_scope` flag
controls which tags the automatic cleanup job is allowed to act on:

| Scope | Behavior |
| :--- | :--- |
| `none` | Automatic cleanup never runs. You prune dead proxies yourself with `urnet-tools proxy remove-dead`. |
| `url` (default) | Automatic cleanup only removes dead proxies that came from a `--proxy_url` source. Your file and `proxy add` entries are never touched automatically. |
| `all` | Automatic cleanup treats every source the same. Equivalent to running `proxy remove-dead --all` on a schedule. |

Manual `urnet-tools proxy remove-dead` is unaffected by this setting. It
always lets you choose interactively, regardless of source.

## Auth give-up backoff and permanent eviction

URL-sourced lists are often noisy. An entry can be reachable on the wire but
fail auth forever, for example a live SOCKS5 endpoint behind a stale password.
Each URL-sourced address gets an escalating per-address backoff after it gives
up on auth. The delay doubles each cycle with up to 20% jitter, capped at a
daily maximum, so hopeless addresses stop hammering the auth rate limiter.

After four give-up cycles, the address is **permanently evicted**: removed
from the URL cache and written to a blacklist persisted in `proxy_url.json`.
Blacklisted addresses are skipped at the merge path, so a hopeless proxy can
never re-enter the auth lottery, even across provider restarts or if the
source list keeps republishing it.

> [!NOTE]
> Eviction is permanent and survives restarts. If a blacklisted address
> genuinely comes back to life (for example the upstream fixes the password),
> remove it from the `blacklist` map in `proxy_url.json` and restart, or it
> will keep being skipped.

This is independent of the dead-proxy cleanup
(`--proxy_dead_cleanup_scope`): backoff and eviction are driven by repeated
auth give-ups, while cleanup acts on proxies marked dead by the health
tracker.

## Overlapping fetch prevention

Concurrent fetch cycles for the same URL are prevented. If a fetch is already
in progress when the refresh interval fires, the new cycle is skipped with a
log line (`[proxy-url] fetch already in progress for <url>, skipping`). This
prevents a thundering herd when multiple triggers fire near-simultaneously,
for example a `--proxy_url` interval coinciding with an `add-source` command.

## FAQ

**Will this duplicate proxies already in my file?**

No. New entries are deduplicated by address against everything currently
running (URL, file, and internal sources share one address space). If the same
`ip:port` shows up in both your file and a URL source, the most recently
applied one wins, same as hot-reload behavior.

**What happens if the URL is unreachable?**

The fetch cycle is skipped with a logged warning. Already-added proxies from
that source keep running. A stale list is better than wiping working proxies
because of a transient network blip. After several consecutive failures the
provider logs a louder warning suggesting the source may be dead, but it will
not remove the source for you.

**What happens when I run `proxy clear`?**

`proxy clear` (which maps to `proxy remove --all`) wipes the entire
`proxy_url.json`: cache, blacklist, and source URLs. After a clear, no
URL-sourced proxies will be fetched unless a source is re-added via
`urnet-tools proxy add-source <url>`. This ensures a clean slate when you want
only the proxies you explicitly add.

**Does this validate proxies before adding them?**

Newly added proxies go through the same warmup, probing, and health-tracking
lifecycle as any other proxy. If a fetched proxy never connects, it shows as
`dead` in `proxy health` and gets swept up by cleanup (if scope allows) or by
a manual `remove-dead`. An address that connects but repeatedly fails auth is
handled separately: it backs off on an escalating schedule and is permanently
evicted after enough give-up cycles (see Auth Give-Up Backoff above).