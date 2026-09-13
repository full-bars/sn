# Sim-testnet finalization report 2

**Status: in progress; `final_acceptance=false`.** This report covers the next
full finalization of testnet chain **945**, subnet **521**, under
[FINALIZE.md](../FINALIZE.md). The fleet and soak remain stopped. No additional
reserve repair, renewal or campaign transaction has been made during the current
preparation. Successful historical payments do not establish full acceptance.

[FINAL.md](FINAL.md) remains report 1, preserved at SHA-256
`489fe5a367af6ce17541a0626fc052455f373d7792316593cc752d501cefd996`.
Later finalizations will use `FINAL-3.md`, `FINAL-4.md`, and so on. This report
records corrections to report 1 without rewriting its original results.

## Peer-review findings and required closure

The peer reviewer supplied an independently queried chain review and committed
the [review scripts and instructions](peerreview/verify/README.md) at
`580831d0b483229d4d41d44d9dabfe04bc906035`, alongside
[the first review's HTML report](final.html). Their supplied narrative reports
67 of 70 assertions reproduced; the committed two-stage suite has a different
declared census of **59 passing checks out of 62**, with the three findings
below. These are separate populations, not interchangeable totals. Terra
reproduced **59/62** against the public endpoint at **07:08 UTC**, with exactly
the same three findings and actual zero exits for both stages. This establishes
reproduction of the findings, not an all-check pass.
[Fresh complete results](peerreview/evidence/FINAL-2-independent-review-20260913/results.json)
(`sha256:987037ccea00ee0bdc8653ad815c40d56659518751221aa1f593712cf4de1efa`),
[actual endpoint and working directory](peerreview/evidence/FINAL-2-independent-review-20260913/environment.tsv),
[stage 1 output](peerreview/evidence/FINAL-2-independent-review-20260913/stage1.stdout),
[stage 2 output](peerreview/evidence/FINAL-2-independent-review-20260913/stage2.stdout).

| Finding | Understanding | Closure required for this finalization |
| --- | --- | --- |
| Production cadence was never scheduled | The first run used 300/50/150/5. A `production_cadence` YAML entry does not prove scheduling or activation. | Retain the successful policy-scheduling transaction, effective epoch, finalized policy state showing **360/60/180/6**, and **three consecutive fully observed epochs** under that active policy. The five accelerated epochs remain a separate prerequisite. Pending. |
| `max_allowed_validators=64`, target ≤56 | The [whitepaper](../WHITEPAPER.md) calls this root-controlled/runtime-dependent. The [compatibility policy](../deploy/testnet/hyperparams.yml) already requires exactly 64. The user has explicitly directed this run to work with the real limit. | **Use 64; reaching 56 is not a testnet prerequisite.** Retain finalized value, actual permits, UID occupancy and 200-head selection evidence from the run. Report the difference from the whitepaper target without claiming ≤56 compliance. No parameter change is needed. |
| Reserve 61.449%, below 65% target | The historical 60% floor passed; the repair target did not. The old repair is already credited. | Apply the approved additional **3,750 alpha** only through the bound plan, then retain its finalized debit/credit and a new complete stake census proving ≥65%. Continue monitoring the 60% floor and report the end-of-run share separately. Pending; the approved lifetime limit is **35,000 alpha**, with **6,000 per repair**. |
| Epoch 309 paid despite capturing zero | `RootMissed(308)` carried each operator's funded amount into its own epoch-309 entitlement. | The missing historical transition is reproduced below from both nodes. Every new paid epoch must similarly explain its funding source, carry, payments and remainder per operator. Historical reporting omission closed; fresh-run accounting pending. |
| Artifact signers differ from registered root signers | A recoverable artifact signature establishes provenance. The coordinator authorizes the root commitment transaction using the epoch's registered `rootSigner`; these are separate checks. | Preserve each recovered artifact signer, committed artifact hash/root, transaction sender and epoch-specific registered root signer. Correct the full semantic collector/verifier's conflation of these roles before the new run. Implementation review in progress; retained keys and old signatures stay unchanged. |
| Chain verification cannot establish off-chain usage or lifecycle | A committed hash authenticates bytes, not the truth of usage, restart or gate assertions within them. | Label chain-reproduced, independently recomputed, artifact-only, and locally executed evidence separately. Link exact artifacts, executable/source identity, commands, actual exits, process generations and shutdown outcomes. Pending full-run evidence. |

The runtime-455 source pinned by the release lock is commit
`67dcf7f791dc495064c293f080a0702cb433e51e`. Its
[`sudo_set_max_allowed_validators` implementation](https://github.com/RaoFoundation/subtensor/blob/67dcf7f791dc495064c293f080a0702cb433e51e/pallets/admin-utils/src/lib.rs#L888)
calls `ensure_root(origin)`. This is a chain-root governance dependency; owning
the subnet or serving its RPC does not satisfy that origin check. The existing
testnet compatibility policy and the user's explicit direction allow the bounded
experiment to proceed at 64; the ≤56 target remains unmet and is not a blocker
for this run. Actual validator permits and native head selection
must still be reported; a configured maximum of 64 is not evidence of 64 active
validators or a fixed 64-slot partition.

## Missing epoch-308 to epoch-309 funding transition

On **2026-09-13 at 06:52 UTC**, read-only calls to both
`http://192.168.1.162:9944` and `https://test.finney.opentensor.ai` reproduced the
same historical results. Both identify chain 945 and genesis
`0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105`.
The comparison covers **19 identical historical/identity results**, including
four canonical blocks, eight `carry(noId)` reads, the settlement logs, both
epoch-309 entitlements and both successful `RootMissed` receipts. Each endpoint
also supplied its current finalized block; moving heads are recorded separately.
[Exact requests](peerreview/evidence/FINAL-2-carry-20260913/requests.json),
[owned-node responses](peerreview/evidence/FINAL-2-carry-20260913/owned.json),
[public-node responses](peerreview/evidence/FINAL-2-carry-20260913/public.json),
[comparison](peerreview/evidence/FINAL-2-carry-20260913/comparison.json).

All amounts in this table are **alpha-rao**; 1 alpha = 1,000,000,000 alpha-rao.

| Transition / finalized EVM block | Operator 1 | Operator 2 |
| --- | ---: | ---: |
| Epoch-308 `EmissionCaptured`, 7,988,077 | 51,653,232,130 | 51,667,423,224 |
| `carry(noId)` before missed-root handling, 7,988,226 | 0 | 0 |
| `RootMissed(308)` and resulting carry, 7,988,227 | 51,653,232,130 | 51,667,423,224 |
| Epoch-309 `EmissionCaptured`, 7,988,377 | 0 | 0 |
| Carry immediately before entitlement finalization, 7,988,526 | 51,653,232,130 | 51,667,423,224 |
| Epoch-309 entitlement total, 7,988,527 | 51,653,232,130 | 51,667,423,224 |
| Carry after entitlement finalization, 7,988,527 | 0 | 0 |
| Rounding residue after the 16 retained claims | 5 | 3 |

The missed-root transactions are
`0x7c39d45b0c4d6f31db3d322bfe7a6171a2a688576d58f1d940646760170e510c`
and
`0xb771d9f296eaab244e7255765ce5762ec2046ac14616bc6fe4dd2e82500ebb1e`,
both in block **7,988,227**, hash
`0x31daecc50a6f78ddb9904da3b22c800c9632c5fd7abad7fb8d8d84d0080af8ee`.
Their complete receipts are retained under `root_missed_1_receipt` and
`root_missed_2_receipt` in the [public RPC capture](peerreview/evidence/FINAL-2-carry-20260913/public.json).

Thus **103.320655354 alpha** was captured in epoch 308, carried per operator,
and allocated to epoch 309. The [16 retained payments and pinned vault state](peerreview/evidence/epoch309-paid-claims-20260912.json)
account for **103.320655346 alpha paid plus 8 alpha-rao residue**. This is
value-preserving carry, not fresh epoch-309 emission or a second capture.
The [vault implementation](../evm/src/STSettlementVault.sol) records
`carry[noId] += record.funded` on a missed root and consumes only that same
operator's carry when finalizing its next entitlement.

## Independent reproduction and its limits

The committed review suite requires Python 3.9+ and an archive RPC. Its default
endpoint is the public Opentensor service. Run stages in order in an isolated
copy because they write `results.json` and `topics.json`:

```sh
cd sim-testnet/peerreview/verify
SN_RPC_URL=https://test.finney.opentensor.ai python3 verify_all.py
SN_RPC_URL=https://test.finney.opentensor.ai python3 verify2.py
```

Retain the two actual exits, exact source revision, endpoint and complete check
IDs. A zero process exit does not mean all assertions passed. The expected
three findings are `wp-cadence`, `hp-maxval`, and `reserve-target`; a different
census or failure set requires explanation. Do not overwrite report 1's
`final.html` with `build.py` during report-2 execution.

Source inspection found three scope limits to address when reporting the next
run. The script's `meta.endpoint` is hardcoded to the LAN address even though
its RPC client defaults to the public endpoint, so the external execution
record must preserve the actual endpoint. `wp-cadence` checks whether any
scheduled policy has a 360-block epoch; it does not establish all four policy
values, activation or three observed epochs. `reserve-target` replays the
historical block-7,992,355 census and will remain false after a later repair.
The old root, entitlement and receipt constants also belong to epoch 309.
Historical reproduction remains useful; new acceptance requires fresh,
explicitly identified epochs, finalized state, roots and transactions.

The successful rerun used exact script revision `580831d`; script hashes
remained unchanged. Its committed baseline outputs were preserved separately
and removed from the working copy before execution, so stage 2 could append
only to newly emitted stage-1 results. An earlier command mistakenly added
port 19 to the endpoint and failed; its
[failed exits and stale summary](peerreview/evidence/FINAL-2-independent-review-20260913/rejected-endpoint-attempt/)
are retained as a rejected attempt and do not establish any chain result.

The independent Python Keccak and secp256k1 implementations retain their
separation from the project under review. A second cryptographic implementation
can cross-check them without substituting the project's own Merkle calculation
for the independent result. Script output, direct chain reads and off-chain
artifact claims will be reported with their actual verification scope.

## Current execution and remaining acceptance

The current full producer gate on SN `9133805` recorded an actual **25-minute
semantic race-package timeout** and a **10-minute capture race-package timeout**.
It remains a failed gate. The aggregate gate
is still running. The retained failed campaign also needs a separately signed,
approved successor before the full run can begin; its original result and
signed attempt remain preserved. These corrections and their qualification are
in progress. There is no final source/gate or new live-acceptance pass yet.

The next run must retain both complete gate results on its final candidate,
five accelerated epochs, the activated production policy and three consecutive
complete production epochs, both validators' fresh native applications,
required traffic/proof/adversarial and lifecycle evidence, final accounting,
independent replay and actual shutdown results. This section will link their
terminal artifacts as they become available. Pending or failed work stays
visible; this report cannot establish acceptance until that evidence exists.
