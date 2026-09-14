# Runtime 458 adoption evidence

This bundle records source preparation and readonly observations. It does not
establish full release qualification, a deployment rerun, reserve repair or a
completed soak. The affected simulator qualification was still in progress
when these records were copied on September 14, 2026.

The operator reported the nginx change verified at 21:01:55 UTC: no effective
RPC rate-limit directives, successful configuration check and reload, and the
same active master process. Their log is
`/Users/brien/urnetwork/monitor/main-ansible-rollout.2WnFJ05z/nginx-verification-v2.60E9StC2/verify-subtensor-nginx.private.log`.
That remote log was not read by this agent; this is operator-supplied evidence.

`lan-artifacts/` retains three readonly batches sent directly to
`http://192.168.1.162:9944`, without pacing, proxy, redirect or public fallback.
All nine calls returned HTTP 200 and curl exit 0. Finalized block 8,006,567,
hash `0xc814242904668bad31b388b36ed31a0c1ffd3b180b173727f5e3d20ea5c8aba4`,
has runtime `node-subtensor/458/1/1`, expected genesis and EVM chain 945.
Its exact Wasm and metadata bytes, raw replies, request bodies and transport
results are included. This is owned-node evidence, not independent public-node
verification.

`lan-health/` is a separate six-call health observation ending at 22:20:21 UTC.
The node was synced with 16 peers and had advanced another 276 finalized blocks
to 8,006,843. It does not repeat the artifact collection.

`offline-probe/` preserves the completed execution of that exact Wasm. Its
generated metadata matched the captured bytes. No new Cargo build, network
request or RPC was performed for that probe. Its original absolute checksum
manifest is retained inside the directory; this bundle adds relative checksums.

`source-comparison/` contains the reviewed comparison with the independently
authenticated upstream CI artifact. The upstream ZIP digest matches the
official artifact record, but its Wasm is not byte-identical to the LAN code.
All decompressed sections and functions agree except 22 `i64.const` operands
in the hash-state initializer, consistent with the pinned compile-time random
seed dependency. The release pins only the exact LAN code and metadata; it
does not normalize or generally admit other build variants. The source review
and its limits are in `docs/spec/runtime-458-audit.md` in this SN revision.

`xops-qualification/` contains the complete 30-test module, two further fresh
two-test confirmations and the old-pin causal control for source
`446cbdb56e0dc5004b66d7e0cbf05a4d9c49224c`. The causal restores only expected
runtime 455: the runtime behavior test fails and the adjacent network/backend
test passes. The candidate was pulled and fast-forwarded into the primary
checkout, then pushed to `xops` origin/main. The runtime deployment check itself
was not rerun. `SHA256SUMS.original` retains all original absolute locators.

`readonly-renderer/` records the successful runtime-458 CLI build on clean SN
`df98472bd88dc8e29856c172fba4731d52a6308d` and thirteen fixed repositories.
The executable is omitted; its digest, size, build information and unchanged
source observations are included. This linked-worktree build is admitted only
as a readonly renderer. A final write-capable CLI still requires a genuine
canonical VCS stamp. The initial renderer attempt is retained separately as
`readonly-renderer-prebuild-refusal/`: a missing Warp physical-path operand
caused exit 125 before compilation; it created no binary and ran no test.

`lock-render/` contains the native readonly release-lock command, generated
YAML and exact reviews. It changed nine fields relative to FC908: six runtime
fields, two SN source hashes and the node configuration hash. The gateway hash
stayed unchanged because `vars.yml` is bound only by the node file list. All
contract and other dependency fields remain unchanged. The exact generated
YAML was installed only in the idle integration checkout and committed as
`151b515be82cedd02cdae7346b9340cb2cef91aa`; the render's original result describes
its earlier pre-install state. Neither the candidate being tested nor the live
aggregate's source was changed by that local installation.

`aggregate-fc908-partial.stdout` is explicitly an incomplete observation of the
original FC908 aggregate. Its copy contains 20 joined phases, all exit 0.
It is not a terminal result or a full gate pass. The original producer's known
failure and its completed correction are in the separate producer-fc908 and
typed-prior-907 bundles.
