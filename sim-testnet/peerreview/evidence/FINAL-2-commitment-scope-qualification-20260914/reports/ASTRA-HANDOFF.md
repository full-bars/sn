# Historical commitment consumer scope correction

Candidate: `6271dcfceb1af8257b626aba79a6864eb3b7c986`, clean isolated checkout
`sn/`, based on published production `abae9a6a8410a54a560335b16888d2ad0d4fc51f`.
Apply commits `2129727627fbcd3f631045a28a2871740cc0b427` and
`6271dcfceb1af8257b626aba79a6864eb3b7c986` onto root's documentation successor.
Root reviewed the production change and tests. Terra formatted four files;
the only formatter delta wrapped four test table closures and is committed.
Astra did not execute tests, builds, formatters, RPC, or native writes.

## Cause and scope

The retained repair failed before any repair transfer at
`fleet.commitment.34`. Canonical native block and historical commitment-write
checks had passed before `validatedFleetCommitmentGeneration` reached its
current-state check. `fleetRenewalHistoricalSource` correctly selected the
original source plan for the action receipt, but that plan could not see the
later approved plan where its failed consumer finally succeeded. Consequently,
`consumedFleetCommitmentGeneration` incorrectly reported the original generation
unconsumed and required that old generation to remain the current native value.

The original consumer `fleet.install.batch.4` failed at sequence 6190 in source
plan `0xf2f5dd2d5c78b12bc131bb57ddeb26c1960309721353eac032a32873b7b55ba3`.
The exact same consumer intent later finalized at 6226 and verified at 6227 in
approved descendant `0x8092a5b32c06fbdadff9e795c497fe77bb9e800aa5bcb14bc6b7c6ea615188d0`.
The original commitment verified at 6158. The active plan explicitly approves
both source and descendant; the original source does not approve its future
descendant. No chain data discrepancy is needed to explain the actual error.

The correction attaches an immutable consumer scope only after the existing
completed-renewal/source authentication succeeds. The replay executor retains
the original plan and the exact original action observation/hash. Only the
affected fleets may consult the approved continuation, and only after ordinary
source-plan consumption lookup fails. Its fallback requires the original and
current consumer intents to match, approved source/consumer ancestry, the same
deployment, an exact verified consumer intent, and the existing persisted-receipt
identity/hash authentication. Accepted prior intent aliases cannot substitute
for the original consumer. Current native-state requirements outside this
authenticated historical path are unchanged.

## Deterministic regressions

Seven new top-level `TestFleetCommitmentHistory*` roots use synthetic identities.
The main integration test calls the actual historical-source constructor, then
the actual consumption predicate: original failed consumer, exact approved
descendant completion, complete renewal, and preserved source receipt/hash.
Removing each commitment/mirror/binding successor refuses historical scope.
Predicate controls cover initial install, generation-two refresh, challenger
mirror, independent reader cloning, unapproved plans, unrelated fleets/actions,
changed intents/approval, incomplete or missing consumer, missing/tampered
receipt, duplicate actions, and a later accepted-intent alias. The alias journal
is ordered: exact completion sequence 4, alias sequence 5.

`causal-restore-source-consumer.patch` restores the entire consumption predicate
byte-for-byte from `abae9a6`; its new scope type/wiring remain to compile the
regressions. Apply only in a separate disposable checkout. The causal selector is:

```text
^Test(FleetCommitmentHistory(ConsumesApprovedDescendant|ScopeRequiresCompletedRenewal|ScopeRetainsAdjacentActionFamilies)|FleetCommitmentConsumptionRequiresTheExactGenerationConsumer)$
```

Expected actual body outcomes: `ConsumesApprovedDescendant` and
`ScopeRequiresCompletedRenewal` FAIL because the descendant remains invisible;
`ScopeRetainsAdjacentActionFamilies` and the existing exact-generation consumer
control PASS. Preserve raw output, compiled four-root census, exits, and fences.
These outcomes are expectations until Terra executes the causal candidate.

## Terra qualification

Use one admitted dependency graph and explicit package-main output binaries.
Build/list normal and race binaries in the existing capture layout; retain
the existing per-process 10-minute deadline, resource bounds, source/binary
fences, and raw output. Source inspection expects a disjoint 33 + 69 = 102
runtime union; the compiled census is authoritative. Selectors:

```text
A: ^Test(FleetCommitment|FleetRefresh|V10FleetCommitment|FleetLifecycleRenewal)
B: ^Test(FleetRenewal|FleetGenerationOne|HistoricalFleet|CarriedFleet|FleetInstall|ConsumedEVMFunding|EVMCheckpoint|CheckpointVisibility|EvmTxManager|IndependentReadExecutor)
```

For each admitted normal/race binary, execute fresh processes using
`-test.run` with A and B, `-test.count=1 -test.timeout=10m -test.v`.
`diagnosis/source-root-expectations.json` records the source-only names.
Do not combine expensive capture metadata roots into these runtime shards.
README's development selected-union mechanism permits exact disjoint shards
while preserving every root and per-process limit. Both complete final gates
remain required on the eventual final source.

Confirm `^TestFleetCommitmentHistoryScopeRequiresCompletedRenewal$` in three
fresh normal processes on the same source and binary; its successful A-run
execution may be p1, followed by fresh p2 and p3. Earlier census/enumeration
confirmation streaks remain completed in their original source scopes.

## Adjacent review and retained graph

Searched `fleet_commitment_recovery.go`, `fleet_refresh.go`, `fleet_install.go`,
`fleet.go`, `fleet_renewal_history.go`, `fleet_renewal_execute.go`,
`fleet_lifecycle.go`, `executor.go`, `postcondition.go`, `plan_revision.go`,
`owned_rpc_history.go`, and their fleet recovery, renewal, lifecycle, batch,
supersession and history-cache test siblings. Also reviewed existing native
canonical-write/pinned-read controls in `crv4/commitment_test.go` and
`crv4/commitment_context_test.go`.

`diagnosis/generation-consumer-review.json` audits 802 generation dependencies:
402 individual commitments and 400 batch-fleet dependencies. Exactly 17 misses
occur in source-only lookup: generation-one fleets 34–40 and 91–100. Every miss
has the exact consumer verified in approved continuation scope; no remaining
generation dependency lacks its approved consumer. Renewal groups cover all
202 fleets: two generation 1→2, 198 generation 2→3, and two generation 3→4.

`diagnosis/adjacent-action-review.json` additionally checks 1,466 verified
historical actions: 202 initial commitments, 200 refresh commitments, 20 install
and 20 refresh batches, 202 mirrors, 808 member bindings, and 14 lifecycle
preparation actions. Their original/current intents, persisted receipt identity,
and completed-renewal successor ordering match. Fourteen future lifecycle
provider/terminal actions have no current-intent verified entry and therefore
do not enter carried replay; the diagnostic lists these separately from the
verified scope. All 17 new-scope consumers also match their actual archived
consumer plans and the current consumer intent. This is static identity/order
analysis, not a claim of replaying all historical chain state.

Lifecycle commitments and the renewal itself use their own generation/variant
artifacts, canonical finalized native proofs, and pinned contract/member reads.
They do not use the failing current-native-value predicate. The historical
classifier excludes the new renewal itself. Existing old-format mirror/binding
receipt adaptation and complete-map replay hash comparison remain unchanged.

## Identities and remaining obligations

Final file SHA256 values:

```text
dafd0d953e65d91186712a54524630b3e2f0997390dfb9af12ee88f133458a68  sim-testnet/executor.go
80423ceabcb443a6a461f191fcea400e1cce643d347f1d7fd4ba1914705d0e3b  sim-testnet/fleet_commitment_recovery.go
a1f9fbb4114542c022ac66f3bf4b60f1764360216858c71c21b57732bb11db71  sim-testnet/fleet_renewal_history.go
a885196dfde1ab8c6b75edfa1c504c87bca5ef2de62e89478d28ff41161e5354  sim-testnet/fleet_commitment_history_test.go
```

Three production files changed, so this is a new runtime source identity and
requires the existing release-lock/build/approval reconciliation by root.
The new test file is excluded from the production Go hash but changes source
and compiled test membership. Earlier gate successes retain only their verified
source/evidence scope; there is no selective complete-gate carry certificate.
Retain the failed native result and canceled old gates. No native repair retry,
transfer, full-gate pass, or final acceptance is claimed by this handoff.
