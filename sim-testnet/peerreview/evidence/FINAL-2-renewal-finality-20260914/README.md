# Finalized fleet renewal and subsequent repair refusal

Native fleet-renew on source a59294e9 completed with exit 0 at 08:56:04 UTC on 14 September 2026. It finalized and verified 1,212 unique transactions across 202 complete fleets: 202 native commitments, 202 EVM mirrors and 808 member bindings, valid from epoch359 through the planned epoch390 boundary. The reviewed plan remains bf3a911a6e8ccdac1eb9af016d257d7bbac389395b93613e09f753e7a53facc5.

The closure index supplies each action's transaction hash, finalized block number/hash, finality and verification journal positions, immutable postcondition hash/path and raw file hash. This is an index of authenticated native receipts. It does not claim a second independent RPC verification of all 1,212 transactions. The individual postcondition files remain at their recorded paths and are not copied in this compact bundle.

The first-inclusion raw RPC reproduces the first native extrinsic's canonical inclusion at block8,002,458, hash0xc03a239da6e042b4bfc583854b38e15b7efd2257dbb8ab41f4f4f4a9274b6d3b, index14, transaction0x9be9f4334b44d9c6f4bcddf2285be953e100c6980b63a24432c9c5b583075ce7. All current chain access uses the owned LAN node, independent_rpc=false. This direct proof covers that one inclusion.

The automatically queued 6,000-alpha repair began after renewal completion and exited1 at09:10:02 UTC before submitting any transaction. Replaying old fleet.mirror.26 added three modern batch-provenance fields and changed its exact historical observation hash, despite matching pinned node state. The six recorded runtime state files were byte-identical before and after refusal. No repair credit or reserve target achievement is claimed. The earlier closure SUMMARY describes the repair as active at its08:59 cutoff; the later repair-refusal result supersedes that status.

Renewal completion is real chain progress. The soak and final acceptance remained pending after the repair refusal. Private plans, signed transaction files, keys, full journal and runtime configuration are excluded. SHA256SUMS binds these unchanged selected native outputs and public transaction indexes.
