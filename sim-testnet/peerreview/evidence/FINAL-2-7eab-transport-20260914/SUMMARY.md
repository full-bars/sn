# First 7eab native-apply transport refusal

The first native apply using source `7eab04905dbc274ae6d2546b6802f1097c1fc3fe`, CLI `feb8890a60efff80cada37a9015b8edeb2d49f20476b1e7ee2c9d4c0a3176b9e`, and approved plan `0x2b5527989bd2ca38b58d3046e82f2d7fdb5ab1b5b86f3af74ba58e5957ced95d` closed exit 1 at `2026-09-14T15:47:17.649733061Z`.

It completed the batched historical audit at `1000/1000` and reached carried-history marker `2450/3455`. The recorded failure was the current postcondition of `fleet.renew.1.34.bind.2`: `carried plan history preflight: action fleet.renew.1.34.bind.2: current postcondition: 429 Too Many Requests: <html>`.

The closed admission comparison records the same reviewed plan hash, 3,521 actions, equal action/successor/renewal sets, equal maximum and superseded spend fields, and equal limits. The result records plan/config admission changes only; it records no new journal rows, unchanged supervisor and public-identity state, and no submitted repair.

Two closed transcript-derived direct guarded-LAN observations are recorded in `probes/`: a chain-ID read returned HTTP 200 and chain ID 945, while a pinned canonical block and latest finalized receipt read returned HTTP 200 with matching block hashes and receipt status `0x1`. They bound the diagnosis to an intermittent transport refusal: the retained log does not identify the particular HTTP request that returned 429. These reduced reads do not replay the complete binding postcondition or establish a source defect. The live retry is excluded.
