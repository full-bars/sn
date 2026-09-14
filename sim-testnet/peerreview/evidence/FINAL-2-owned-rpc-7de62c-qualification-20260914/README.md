# Report 2: owned-node correction qualification, 14 September 2026

These are completed focused captures executed by Terra max for SN
`7de62c78b3553174eef1c1f08ffe81dcd1811826` with server dependency
`b67ea7ad7da80fb55272042a49963c3515d0586c`. All nine build/suite captures have
actual outer exit 0, matched compiled membership, successful raw process
results and byte-identical before/after source observations. The later
integrated `ca42812` candidate has separate full gates; this component evidence
is not an execution or full-gate pass on that later revision.

The correction routes actual testnet operations, historical checkpoints,
adversary checks and final verification through the approved owned LAN node
without request pacing or public fallback. It preserves original signed
inputs and historical provenance, records the new observation profile as
`independent_rpc=false`, and reuses exact already-authenticated inputs within
the current historical invocation. Hermetic localhost fixtures used by these
tests are not actual testnet RPC observations.

| Capture | Passed roots | Passed descendants | Actual outer completion UTC |
| --- | ---: | ---: | --- |
| `active4-race-p1-v3` | 4 | 27 | 2026-09-13T23:22:12Z |
| `guards7-normal-p1-v2` | 7 | 0 | 2026-09-13T23:20:16Z |
| `guards7-race-p1-v2` | 7 | 0 | 2026-09-13T23:25:45Z |
| `owned-history33-normal-p1-v2` | 33 | 30 | 2026-09-13T23:27:05Z |
| `owned-history33-race-p1-v2` | 33 | 30 | 2026-09-13T23:47:25Z |
| `public-profile1-normal-p1-v2` | 1 | 0 | 2026-09-13T23:47:02Z |
| `public-profile1-race-p1-v2` | 1 | 0 | 2026-09-13T23:52:37Z |
| `renderer-authority1-normal-p1-v2` | 1 | 3 | 2026-09-13T23:53:55Z |
| `renderer-authority1-race-p1-v2` | 1 | 3 | 2026-09-13T23:54:24Z |

`active4-race-p1-v3` and the retained v7/v9 confirmations establish three
sequential successful race executions of the four roots active at the earlier
simulator timeout. Each has four passing roots and 27 passing descendants,
actual list/body/conversion/replay/outer exits 0, and the same retained binary:
`b984be08575fa4cda5ef713bb1b3f129d392e037d71b4ea15fe5f855613c5851`.
The confirmation processes reused this binary; they did not recompile it.
Their native outer records are named `active4-race-p2.exit` and
`active4-race-p3.exit`. Earlier launch-attempt records are kept separately
and receive no pass credit. Original source captures remain authoritative
for any omitted inputs.

The complete simulator population is still 2,198 roots, partitioned in the
corrected full aggregate as 426 + 370 + 1,397 + 2 + 3. These focused captures
and confirmations do not establish a completed full partition run; the actual
full aggregate on the final candidate remains required. The original failed
gate is retained separately and is not relabeled by later focused passes.

Raw records, requests, process ownership/join receipts, compiled lists, event
streams, outcomes, source inventories and recorded toolchain identities are
copied unchanged. ELF executables, caches, vault contents and private service
environments are omitted. Original absolute paths identify the original
captures. No test or actual testnet RPC was rerun to assemble this bundle.
Verify the relative portable file hashes with:

```sh
sha256sum -c SHA256SUMS
```
