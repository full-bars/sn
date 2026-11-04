# Next full-bars/sn release

Release date and tag are assigned at release time. This draft covers main through `bd3078aed`, after `v2026.11.1-1087168030-sn`.

> [!WARNING]
> On non-Windows systems, this release moves three private settings into `provider_state.db` and removes each original after import. An older build reads the old files, not the migrated values. Back up the complete state directory before upgrading if you need a rollback. The client JWT and shared operator files remain separate. Windows keeps the settings in files.

## Quick start

For an existing native installation, update the provider:

```sh
urnet-tools update
```

For a new installation, see [Installation](https://github.com/full-bars/sn/blob/main/docs/Installation.md). For containers, see [Docker Deployment](https://github.com/full-bars/sn/blob/main/docs/Docker-Deployment.md).

## What changed

### State and recovery

The miner stores `error_counts.json`, `.metrics-counters.json` and `direct_grade.json` in `provider_state.db` on supported platforms. It imports each record before it removes the original file. If an import cannot proceed, that setting stays on its file. If a file conflicts with an existing store value, the store wins and the file remains for inspection. Shared files that `urnet-tools` uses stay on disk. ([#357](https://github.com/full-bars/sn/pull/357))

A build now refuses a store whose schema requires a newer reader before it starts slots or mints identities. The same refusal reaches the parent during a HotSwap handoff. If the store cannot be read, the provider instead continues in degraded mode and reports `urnet_state_store_degraded 1`; it does not claim to have checked the schema. A corrupt store moved aside and replaced with a fresh one also marks the run degraded. ([#357](https://github.com/full-bars/sn/pull/357))

### Shutdown and pressure

A worker that ignored cancellation previously held shutdown indefinitely. The miner now waits 35 seconds for workers, writes a goroutine dump and then arms a five-second exit watchdog. A shutdown timeout exits 1, except that a quarantine keeps exit 75. The watchdog bounds deferred cleanup, but the dump is written before the watchdog starts; a blocked dump can delay that bound. ([#350](https://github.com/full-bars/sn/pull/350))

A thrash cap no longer expires just because its timer elapsed. After the hold window, recovery checks require no recent thrash restart. When watchdog and headroom readings exist, they must show calm and sufficient memory. If headroom is unavailable, the miner checks the heap limit when it can; without memory signals, the quiet hold is the remaining evidence. The cap relaxes by about 20% per clean window toward the operator trim or the pre-cut target, then clears. ([#352](https://github.com/full-bars/sn/pull/352))

Billing and transport health now share one five-second sampler per proxied slot instead of two. Both jobs read local state. ([#353](https://github.com/full-bars/sn/pull/353))

### Proxy pool and tools

The proxy health line counts admitted members separately from held and parked members. It no longer repeats the down count. A parked proxy is not counted as degraded. ([#343](https://github.com/full-bars/sn/pull/343), [#347](https://github.com/full-bars/sn/pull/347), [#348](https://github.com/full-bars/sn/pull/348))

URL proxy cleanup now protects members held by a trim cap. A pool-wide outage freeze restarts the degradation clock instead of letting the outage consume the configured window. Degraded retirement remains opt-in. ([#344](https://github.com/full-bars/sn/pull/344))

`urnet-tools top`, `status` and `providers` no longer wait indefinitely for unit discovery. Discovery bounds property lookups and shares a time budget across its probes. It reports an incomplete discovery rather than presenting unverified units as confirmed. The trim prompt also makes the clear action visible. ([#346](https://github.com/full-bars/sn/pull/346), [#345](https://github.com/full-bars/sn/pull/345))

### Extender and authentication

The direct slot gets a brief head start for an extender activation attempt before proxy launches. A role held by the activation budget is rechecked and can return without a restart. Before an in-place exec, the outgoing image releases its carrier ports. The SDK also retries a busy port quickly before returning to the normal cadence. ([#342](https://github.com/full-bars/sn/pull/342), `d7634fc8f`, `58454832b`, `88eb5014d`)

The metrics endpoint now reports extender observations. A stopped role clears its published reading. ([#340](https://github.com/full-bars/sn/pull/340))

Network tokens schedule renewal at a selected point between 40% and 50% of their lifetime. A token without a usable issue time is due immediately. Retry backoff and renewal spacing still apply. ([#341](https://github.com/full-bars/sn/pull/341))

### Dependencies

The `connect` pin moves from `ca8b9020201a` to `0995412c628f`. The `sdk` pin moves from `6f4d35ee3486` to `0ec902c4a110`. The SDK pin includes the fix that prevents a space opted out of the extender network from building its directory. The module update also lowers the pinned `golang.org/x/crypto`, `x/net`, `x/sys` and `x/term` versions; see [go.mod](https://github.com/full-bars/sn/blob/main/go.mod) for the full set. ([#339](https://github.com/full-bars/sn/pull/339), [#354](https://github.com/full-bars/sn/pull/354))

## Not yet merged

These pull requests were open when this draft was prepared. They are pending and are not part of the main commit range above:

- [#351](https://github.com/full-bars/sn/pull/351): proxy proof persistence.
- [#359](https://github.com/full-bars/sn/pull/359): client JWT write instrumentation.
- [#360](https://github.com/full-bars/sn/pull/360): `urnet-tools diag`.
- [#361](https://github.com/full-bars/sn/pull/361): connect pin update.
