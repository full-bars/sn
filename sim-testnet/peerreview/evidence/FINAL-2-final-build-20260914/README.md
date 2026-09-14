# Final candidate build and native lock evidence

This closed bundle records publication and native preparation for SN candidate
`abae9a6a8410a54a560335b16888d2ad0d4fc51f`. It does not claim a full gate pass,
a finalized reserve transfer, or live campaign acceptance.

The temporary `2a8cf09` binary built with exit 0 and clean, unchanged observations
across 13 repositories. Its matching native release-lock preview and apply both
closed with exit 0. Only `repositories.sn_go_source_hash` changed, to
`sha256:6215cfe3b055ef3edc453bc688f6abafe4397f8315631883593d7a1ac1131a98`.
These release-lock operations issue no chain RPC calls or transactions.
The resulting lock bytes have SHA-256
`40345bafbc6a6fce4b3e8dafec7b1126141f1915c1fc67318ce4beb4f0cdca02`.
The lock-only successor was published as `abae9a6`.

The final native binary built at 10:04:30–10:05:16 UTC on 2026-09-14, with outer
completion at 10:05:17 UTC and exit 0. It is stamped with clean VCS revision
`abae9a6`, `trimpath=true`, and SHA-256
`71fca036f0aa6876565fbc38f1d138f0ed2268e19f9488d280eaf95c49ae191e`.
Its before/after 13-repository pair is byte-identical, SHA-256
`f7871bf5e6b53a939b8ea8add2b2b3abbbfd0bf58f9c1bdc3474fd42248b0089`.
Executables and mutable caches are not included; original paths, byte counts,
source identities, build commands, outputs and hashes are retained.

The read-only setup preview closed at 10:08:56 UTC with exit 0 and unchanged
runtime state. Its private plan bytes remain private. The included review
records exact equality of all 3,521 actions and preservation of 1,212 completed
renewals and every spending limit in replacement plan
`0xf6348cfdb032e658b8ce46d764ab7c65476250959fdbabc2a658563759e17585`.
The approved repair is 6,000 alpha with minimum credit 5,999,999,999,999 alpha-rao
within 37,250 alpha lifetime. The included runtime-scope review is the historical
10:14 decision to proceed with that authorized repair while two test-only
metadata roots completed independently. Those three race recoveries later
closed at 10:19:30 UTC; see the separate mirror qualification bundle.
No live repair capture is copied here, and review/adoption alone is not proof
of a submitted or finalized transfer. All actual testnet RPC uses the owned
`192.168.1.162:9944` endpoint without harness pacing or public fallback.

`MANIFEST.json` maps retained raw bytes to their original sources. `SHA256SUMS`
seals the manifest, this scope note, and all copied evidence. Failed and canceled
qualification attempts remain in the separate correction bundles.
