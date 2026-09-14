# Report 2: oracle correction and renewal preparation

This bundle records closed preparation results on 14 September 2026. It does
not establish successful renewal, reserve repair, traffic or final acceptance.
The native renewal apply started separately at 07:35:36 UTC and was still
running before its first transaction at the 07:42:16 UTC observation. Its live
files are deliberately absent from this closed bundle.

The preceding setup command exited 1 at 06:41:20 UTC: the historical
`fleet.refresh.batch.1` check expected binding version count 2, but fleets 5
and 6 had legitimately advanced to generation 3 through recorded lifecycle
transactions. `failed-pre-renewal-setup/` retains that actual failure and the
unchanged journal/supervisor hashes. The supported recovery performs the
already-required stopped fleet renewal first. Completed renewals can then
authenticate historical refresh and lifecycle state at their original
checkpoints. No version-count predicate or receipt was weakened.

The first renewal preview exited 1 at 06:48:23 UTC because it required the
original oracle with no pending reroute. `failed-old-renewal/` retains that
result. The pinned LAN observation in `oracle-snapshot/` shows a completed
restore: at EVM block 8,002,200, hash
`0xefd219a0f812985c190ceda95241545617dceed9c6a06cd77d324b41d2f4e1eb`,
the active, immutable and stored pending oracle were all
`0x0422ac5e1ea3997d2a06f8268d4834380609738c`; the stored effective epoch was
273 and the current epoch was 356. The contract retains its scheduled fields
after activation. Nonzero stored fields therefore did not indicate a future
reroute in this state.

Astra's correction, commit `536dba6541498238a339c958358ab1b4b50e8aac`,
changes the shared predicate used by planning and per-action execution. It
requires the nonzero original active and immutable oracle, and accepts either
an empty schedule or a schedule of that same oracle which has already matured.
Foreign, future and inconsistent schedules remain refused. `source/` contains
the patch, source identity, handoff and root review. Terra ran all seven
affected roots normally and with race detection; both commands exited 0.
Actual outer completion times were 07:01:39 and 07:13:15 UTC, with package
durations 39.724 and 352.899 seconds. Commands and complete outputs are in
`focused/`; these are focused qualification results, not full-gate passes.

The candidate source and matching release lock were published. Final source
`a59294e98ea02d05125015ae02cf32f2c0059c8a` differs from the code correction
only in the SN Go source-hash field of the release lock. Reusing the older
executable for lock apply first failed its revision attestation; that refusal
is retained in `native-lock-apply-536dba/`. A matching driver applied the
reviewed lock successfully. The final runtime executable was then built
successfully, with unchanged before/after observations across all 13
repositories. Its SHA-256 is
`998777328adfad2c394a3aec9c9df92f3bb477d3c49a7bc509d9457251fc4efd`;
its clean VCS revision is `a59294e9`, with trimpath and buildvcs enabled.
The source observation SHA-256 is
`3136a9900882718cd012465ae2826108da9c687c2a17b5b1bb361411b5fffe5e`.
`final-cli/` retains actual build commands, outputs, terminal records and
metadata. The raw result's field named `private_gomodcache` refers to the
existing global cache `/home/by/go/pkg/mod`; it is not a private cache.

Native setup preview exited 0 at 07:21:09 UTC. All 2,309 actions and all
spending limits were unchanged; the revision bound the corrected release
lock and preserved prior lineage. Native setup then wrote the complete new
plan and effective inputs and was intentionally canceled at 07:30:04 UTC
before the known failing carried-history audit could reach action execution.
It joined at 07:30:05 UTC with native exit 1: **CANCELED, not PASS**. Actual
post-join checks confirmed the exact old/new archives, new lock/resolved-input
hashes, and unchanged journal, identities and supervisor files. This was
native plan admission, not successful setup or a bypass of transaction checks.

The corrected renewal preview exited 0 at 07:31:28 UTC. Its exact plan hash is
`0xbf3a911a6e8ccdac1eb9af016d257d7bbac389395b93613e09f753e7a53facc5`.
The private plan SHA-256 is
`375a0c5b933322d0c9bc5989697486b8661224f53dca4359fd98ca6fa36dd1a7`.
It contains 202 commitments, 202 mirrors and 808 bindings, for 202 fleets,
with validity epochs 359–390, 25 gwei and at most 10 operations in flight.
No revokes are needed because the prior bindings have expired. The new gas
ceiling of 9.09 TAO is taken from the existing campaign reserve; the native
fee ceiling is 0.606 TAO. Total maximum plus superseded liabilities remain
within 200 TAO, 180 EVM, 37,250 alpha, 262 registrations and zero new subnets.
Thirty-two unexecuted future lifecycle actions retain authenticated successor
consent; the two future generation-4 commitments correctly advance to 5.
The first operator review assertion incorrectly expected those two parameters
to remain at 4; its refusal occurred before apply and is retained separately.

All actual testnet RPC in this bundle uses `192.168.1.162:9944`, without pacing
or public fallback. Its provenance is `independent_rpc=false`. The private
plans, signed transaction bytes, executable, vault and caches are omitted.
Closed raw files are copied unchanged; original absolute paths identify their
source captures. Verify portable bundle bytes with:

```sh
sha256sum -c SHA256SUMS
```
