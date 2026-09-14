# Report 2: corrected server qualification, 14 September 2026

These are the 12 completed focused captures on server
`0f095a639e111f71d231cd6f792a191cbc6b0a69`, run by Terra max. Every capture
has actual outer, build/reuse, list, body, conversion, verification, source
fence, service and cleanup exits 0. Exact compiled membership and raw Go
JSON events are retained. Before/after observations match for all 13 recorded
repositories across all 12 captures.

The recorded SN dependency is `e3d3539dd761f3e04d2c28e281b01735fa297f9c`.
These passes are scoped to that component-qualification graph. They are not
executions on the later integrated SN `ca42812`, not a passed complete release
gate, and not live testnet acceptance. The two full gates on the integrated
candidate were still running when this bundle was assembled.

The server change stages and syncs one private source for both existing
conditional evidence writes, while preserving both exact immutable readbacks,
cleanup, cancellation behavior, all 1,000 clients, both operators, 2,000
published objects, SQL/authentication checks and the original 30-second
operation deadline. The monitor change retains upstream migrations 662–668
and their monitoring tests and adds the real PostgreSQL 661-to-668 regression
with its rollback-fault matrix. The earlier passing diagnostic did not
reproduce the original timeout or measure fsync wall time; it is not claimed
as failure closure. The original full-gate failures remain in their original
bundle.

| Capture | Passed roots | Passed descendants | Actual outer completion UTC |
| --- | ---: | ---: | --- |
| `cohort1-normal-p2` | 1 | 0 | 2026-09-14T00:01:27.980317278Z |
| `cohort1-normal-p3` | 1 | 0 | 2026-09-14T00:03:28.676450651Z |
| `controller25-normal-p1` | 25 | 0 | 2026-09-13T23:59:27.966783514Z |
| `controller25-race-p1` | 25 | 0 | 2026-09-14T00:03:12.426456324Z |
| `monitor15-normal-p1` | 15 | 12 | 2026-09-14T00:03:23.165502503Z |
| `monitor15-race-p1` | 15 | 12 | 2026-09-14T00:03:48.145365585Z |
| `monitorfailed2-normal-p2` | 2 | 0 | 2026-09-14T00:04:28.184256343Z |
| `monitorfailed2-normal-p3` | 2 | 0 | 2026-09-14T00:05:33.695114781Z |
| `published-monitor1-normal-p1` | 1 | 0 | 2026-09-14T00:05:19.591920270Z |
| `published-monitor1-race-p1` | 1 | 0 | 2026-09-14T00:05:52.212425817Z |
| `startifact49-normal-p1` | 49 | 9 | 2026-09-13T23:58:16.759184410Z |
| `startifact49-race-p1` | 49 | 9 | 2026-09-13T23:59:55.599284699Z |

The full 25-root controller normal run supplies the first normal confirmation
of the original cohort failure. The two later one-root runs reuse its exact
binary in fresh processes; the full 15-root monitor normal run and its two
later two-root processes similarly supply three consecutive normal passes.
The normal/race affected matrices and real PostgreSQL normal/race root are
separate retained obligations, not substitutes for those confirmations.

The cohort root is `TestStClientKeyRegistrationCohortActualThousandClientsTwoOperators`. Its three actual pass events are:

- `controller25-normal-p1`: 2026-09-13T23:59:06.932501253Z, root elapsed 31.61 seconds.
- `cohort1-normal-p2`: 2026-09-14T00:01:07.027853711Z, root elapsed 34.98 seconds.
- `cohort1-normal-p3`: 2026-09-14T00:03:10.161630741Z, root elapsed 29.47 seconds.

The two monitor confirmation roots are `TestMigrationArtifactCatalogCoversEveryVersion614ThroughHead`, `TestMigrationsSignalReportsMissingPublishedArtifacts614ThroughHead`.
The real database root is `TestPublishedMigrationMonitorExtenderArtifacts`.

Raw files are copied byte-for-byte, including Go test2json control prefixes.
Commands and source-check results retain original capture paths. Test binaries,
Go caches, private service environments, service storage, vault contents and
full private source inventories are omitted; their recorded identities remain
in the receipts. No tests or chain calls were rerun to assemble this bundle.
The relative `SHA256SUMS` checks the portable files:

```sh
sha256sum -c SHA256SUMS
```
