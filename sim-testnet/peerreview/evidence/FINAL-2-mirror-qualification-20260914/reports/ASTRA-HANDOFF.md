# Historical alias fixture decoder correction

Frozen clean candidate: `4029396ef34fdea2338152cf70cd69c054f5f484`, parent
`c50dbd95649c64887adf372b84441eddfe1d2bbd`, in this directory's `sn` checkout.
Only `sim-testnet/fleet_renewal_history_test.go` changes: 42 insertions and two
replacements. Its blob is `7f7014a5025dae81e6515ba2cdeb10d1838e6a0a`, sha256
`9ef5407bbbb72cf82e500ab36f6a1760da3eacd16802587981e10354e82c348e`.
No tests, builds or formatters were run by Astra. Terra owns this one file's
formatting and all execution after parent admission. Do not modify c50's
failed source, binary, or captures.

## Exact failure and correction

The retained c50 normal failure is at
`<restricted-capture>/sn-mirror-replay-correction-20260914/terra-runtime/capture-mirror-replay-20260914T093947Z/normal/raw/test2json.raw`.
Six new roots failed after the fake node reported unexpected contract
calldata at line 102. The fake decoded only `json:"data"`, leaving that field
empty for the actual request. Its exact calldata lookup therefore refused
the request, and the reader received EOF. Derived and partial/mixed roots did
not need those contract calls and passed.

`sim-testnet/fleet.go:466` calls `ethclient.CallContract` for serial pinned
reads. The pinned go-ethereum v1.17.0 implementation sends `CallMsg.Data` under
JSON `input` in `ethclient/ethclient.go:785`. This is source-confirmed in the
actual dependency; the production calldata and block selection are correct.

The two-line fixture correction decodes `Input hexutil.Bytes` tagged `input`
and uses `message.Input` for the existing full-calldata lookup. It does not
weaken that lookup or numbered-block checks. No production path changes.

`TestFleetRenewalHistoricalAliasRpcDecodesInput` sends input-only JSON directly
through the fake handler, without sockets or sleeps. Two payloads share their
four-byte method prefix but differ afterward; each is read at both the exact
historical block and another numbered block. The test requires all four exact
responses, response ids and the complete block sequence. The old data-only
decoder fails immediately at its exact lookup. The new decoder must preserve
both full calldata and block selection.

## Adjacent review

Searched every `json:"data"` / `json:"input"` call decoder in
`sim-testnet/*test.go`, then checked the matching production encoders:

- `fleet_supersession_test.go` already accepts the serial client's input form.
- `fleet_install_history_cache_test.go`, `fleet_refresh_batch_test.go` and
  `observe_batch_test.go` intentionally decode data: their custom batch writer
  in `fleet.go:503` sends that field.
- `final_semantic_public_state_test.go` and
  `evidence_deployment_runtime_test.go` likewise receive explicit data from
  `final_semantic_public_rpc.go:1443` and `evidence_deployment_runtime.go:62`.
- The doctor, policy, bootstrap, evidence-carry and runtime-activation fake
  readers already include input support. Log payload and disk-fixture data
  fields are separate serialized objects, not contract call arguments.

No adjacent encoder/decoder mismatch was found. The production adapter and
its receipt classifier remain exactly the reviewed c50 bytes.

## Qualification and causal proof

Use the same literal prefix selector and admitted dependency graph as c50:

```text
^Test(FleetRenewal|FleetGenerationOne|HistoricalFleet|CarriedFleet|FleetInstall|FleetRefreshOracle|ConsumedEVMFunding|EVMCheckpoint|CheckpointVisibility|EvmTxManager|IndependentReadExecutor|ProducerGateCaptureSelection)
```

There are now 87 anchored source roots. Independently verify exact compiled
membership, including all nine new historical-alias roots. Run normal and
race with the existing build-once wrappers and `-count=1 -parallel=4
-timeout=10m`. The complete capture census should be 471: ordinary 360 plus
the unchanged 111 separate owners. These are expected censuses, not observed
test passes. No timeout or selector allowance changed.

All six actual c50 normal failures need three sequential fresh-process passes
per root on the same final source/dependency/binary snapshot. The first
successful normal matrix may count as pass one; use this exact six-root
selector for the two remaining confirmation processes:

```text
^TestFleetRenewalHistoricalAlias(ReplaysOriginalMirrorAndBinding|ReplaysIndependentCheckpoint|RejectsChangedObservation|RejectsChangedCheckpoint|RejectsChangedContractState|RejectsChangedSharedClone)$
```

No c50 race result is claimed; parent canceled that attempt. Any actual
race-mode failure discovered in its retained terminal logs requires its own
race confirmations. Completed census/43cc and enumeration/ff62 streaks remain
their scoped evidence and are not reset by this fixture correction.

An accelerated deterministic causal capture can use one disposable checkout
from the final formatted candidate with two disjoint restorations:

1. Restore only `sim-testnet/executor.go` to ff62 bytes for the original
   production receipt-format bug.
2. In the new test file, change only the fake message field's `json:"input"`
   tag back to `json:"data"`, retaining its new direct decoder test.

Run this exact four-root selection normally on the single compiled mutant:

```text
^TestFleetRenewalHistoricalAlias(RpcDecodesInput|ReplaysOriginalMirrorAndBinding|ReplaysIndependentCheckpoint|PreservesDerivedReceipt)$
```

Expected result: three failed roots and one passing derived-format control.
The direct decoder root must report unexpected calldata and an empty response.
The two original-format roots must still report the metadata-only hash mismatch
for both mirror and binding: the old production dispatcher never performs the
fake contract calls on those paths. The derived control must pass. These
orthogonal observations distinguish the fixture error from the production bug
without a second full build or any live RPC. Keep both mutation diffs and exact
source identities. Never mutate the qualification checkout for this proof.

## Source scope

This successor is test-only. `executor.go` retains sha256
`c45f58ccc638ab36f3401bd96cb00d651807466a9db9d25120875c7e15a9f23f`.
There is no additional production digest change beyond the reviewed historical
replay fix. VCS and test-binary identities do change, requiring fresh affected
qualification. The original production fix still requires parent-owned release
lock/build admission before native verification; this fixture correction does
not provide or imply a live retry, transaction, or release-gate pass.
