# Owned RPC transition evidence

The failed native setup apply ran from 21:45:45 to 22:09:06 UTC on 2026-09-13.
It adopted the approved replacement plan but exited during carried-history
verification, before action execution. Its journal and supervisor hashes are
unchanged. This is a failed preparation attempt, not a repair or live run.
The old error names a public endpoint; the user subsequently required every
new testnet RPC request to use 192.168.1.162:9944 without pacing.

The LAN probes are read-only capability observations. The EVM probe confirms
chain 945, the retained genesis, coordinator code and currentEpoch() at EVM
block 7,897,285. The native storage request in that first probe mistakenly
used an EVM hash and returned UnknownBlock. The corrected native probe obtains
the native hash for native block 7,897,285 and reads MaxAllowedUids=256 there.
These are distinct block namespaces, not a cross-chain height correspondence.
This proves availability of the tested historical state, not every archive
height, independent-node reproduction, reserve repair or final acceptance.

Files are copied byte-for-byte from their named original captures. This bundle
contains neither transaction signing bytes nor private configuration content.
