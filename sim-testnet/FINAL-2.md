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
| Artifact signers differ from registered root signers | A recoverable artifact signature establishes provenance. The coordinator authorizes the root commitment transaction using the epoch's registered `rootSigner`; these are separate checks. | Preserve each recovered artifact signer, committed artifact hash/root, transaction sender and epoch-specific registered root signer. The collector/verifier correction is integrated into candidate `4fda909` and its affected tests passed normally and under race; retained keys and old signatures stay unchanged. Fresh-run evidence remains pending. |
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

A stopped-state snapshot at **07:45 UTC on 2026-09-13** reproduced byte-identical
results on the owned and public RPC nodes at finalized native block **7,995,269**,
hash `0xe046f55170e00ee58aab564a71cbeb540cd021248477d136adcfb56f311cc821`.
It shows `max_allowed_validators=64`, `max_allowed_uids=256`, and
`SubnetworkN=256`. The 256-entry permit vector has eight true entries, at
UIDs **0, 2, 7, 8, 50, 52, 254 and 255**. Holding a permit does not establish
that a UID submitted weights. This snapshot establishes the actual starting
limits and permit census; the new campaign must still prove its 200-head
selection and capture the permits in effect during that run.
[Exact requests](peerreview/evidence/FINAL-2-real-limits-20260913/requests.json),
[owned responses](peerreview/evidence/FINAL-2-real-limits-20260913/owned.json),
[public responses](peerreview/evidence/FINAL-2-real-limits-20260913/public.json),
[decoded comparison](peerreview/evidence/FINAL-2-real-limits-20260913/summary.json).

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
run. At the reproduced revision `580831d`, `meta.endpoint` was hardcoded to
the LAN address even though the RPC client defaulted to the public endpoint;
the external execution record therefore preserves the actual endpoint. The
current source records the configured RPC client's endpoint directly, without
changing the original rerun's bytes or the verification assertions.
`wp-cadence` checks whether any
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

The full producer gate on SN `9133805` ended with exit 1 at **07:52 UTC on
2026-09-13**. It recorded a **25-minute semantic race-package timeout**, a
**10-minute capture race-package timeout**, and a final source check that
correctly refused the older snapshot after publication of newer source.
The aggregate gate on the same snapshot ended with exit 1 at **09:09 UTC**,
recording a **90-minute simulator race-package timeout**, the infrastructure
errors described below, and the same final source refusal. Its database and
history phases passed. These remain failed gates. None is converted into a pass by later focused
qualification.

Candidate `4fda909` contains authenticated succession for the failed
pre-acceptance campaign, the artifact/root-signer correction, and independent
execution of the producer's observed expensive test groups. The old signed
attempt, failed result and approvals remain intact. Campaign succession's
failed normal scopes completed their required confirmations on their recorded
sources. All **37 adjacent campaign/attempt/analyzer roots** then passed normally
and under race on `4fda909`. The lifecycle, runtime-configuration,
pool-registration and full-artifact race confirmation streaks all closed.
The aggregate's four roots that were active at its timeout completed all
three race confirmations on the same retained binary at **09:49 UTC**.
The scheduling correction splits the two whole-population
tests from the complete complementary selection, retaining the 90-minute
timeout, parallelism and every selected root.

The combined scheduling and infrastructure-scope correction at `f2a87d2`
passed its **12 affected guard roots normally and under race**. The compiled
inventory confirms **2,186 roots**, partitioned into exactly two whole-population
roots and their 2,184-root complement, with no omissions or duplicates. This
inventory check did not execute the complete suite; the final aggregate must
still run both selections. The corresponding xops test correction is
`a9d2eb4`.

At **10:01 UTC**, the clean, pushed `58b251f` native CLI installed the reviewed
release-lock bytes with exit 0. Their SHA-256 is
`ddfd939ac49a465957e3aeaac3ecb49ddd892a2cf6d1821ad9d5d3a583c69249`.
Only the simulator production-source and protocol-script digests changed;
the runtime, EVM artifact, interface and infrastructure pins stayed unchanged.
This local lock update sent no chain transactions. Both final gates and the
live campaign remain pending.

The aggregate alarm occurred while its four active roots had run for only
**37 seconds, 39 seconds, 2 minutes 15 seconds, and 45 seconds**. The log contains
no assertion failure or race report before the alarm, and many tests were
still waiting for their parallel execution slot. This supports shared package
clock exhaustion; it does not establish that any one root hung for 90 minutes.
The correction must preserve the complete selected population and retain the
original timeout. There is no final source/gate or new live-acceptance pass yet.

The same aggregate's infrastructure phase ran **43 Python checks with four
errors**. Three errors required Grafana source outside the simulator's declared
runtime dependencies; the fourth was a Subtensor test using the obsolete
singular `gateway_bind_address` field. The current configuration instead has
`gateway_bind_addresses`, including both management and LAN listeners.
The next SN gate selects the complete **27 Subtensor checks plus
the gateway regression**, with that stale test corrected. All **28 passed**,
and the corrected gateway test completed three fresh successful executions
on the same source, with unchanged source checks before and after. The other 15
infrastructure checks concern services outside this finalization; they are
excluded from its scope, not reported as passing. The original four errors
remain recorded. No node or gateway deployment change follows from this test
correction.

The producer gate on `0dcb5c8` hit another **10-minute capture race-package
timeout at 11:17 UTC**. Its 443 selected roots had passed normally. At the
alarm, `TestFleetRenewalRevisionRefusesCustodyFeeOrLiabilityChanges` was active
and 43 roots were still waiting at `t.Parallel`; the log records runnable
signature verification, with no preceding assertion failure or race warning.
The failed producer was stopped and joined at **11:34 UTC**: 21 phases passed,
one failed, and three were interrupted. The interrupted phases do not establish
test verdicts. [Original timeout](peerreview/evidence/FINAL-2-capture-timeout-20260913/capture.log),
[failure record](peerreview/evidence/FINAL-2-capture-timeout-20260913/failure.json),
[actual phase outcomes](peerreview/evidence/FINAL-2-capture-timeout-20260913/producer-phases.json).

Correction `b78b672` changes only the producer's scheduling and its existing
coverage guards. It assigns the affected 443 roots to four disjoint groups of
**343, 87, 11 and 2**, retaining their five-minute normal and ten-minute race
deadlines. All four groups passed normally and under race, with the native
owner exiting 0 at **12:06:50 UTC** and identical source checks before and
after. The compiled inventory still contains exactly **454 roots**; the eleven
unchanged separately owned roots retain their prior qualification and remain
in the complete gate. They were not re-executed as part of this focused run.
All twelve affected coverage guards also passed normally and under race.
[Exact four-group commands](peerreview/evidence/FINAL-2-capture-qualification-20260913/affected443/capture-affected443.native.sh),
[compiled inventory](peerreview/evidence/FINAL-2-capture-qualification-20260913/affected443/capture-list.actual.txt),
[native exit](peerreview/evidence/FINAL-2-capture-qualification-20260913/affected443/native.status),
[normal guard results](peerreview/evidence/FINAL-2-capture-qualification-20260913/guards12-normal-p1-source-v2/report.json),
[race guard results](peerreview/evidence/FINAL-2-capture-qualification-20260913/guards12-race-p1-source-v2/report.json).
The active timeout root and all four of its subtests completed three fresh,
sequential successful race executions on the same source and binary SHA-256
`9a027c38dd93146086026039d58856889a0f8b22f40dc1e8dfe11406627c9f17`.
Every outer and test owner exited 0, with unchanged source checks. The last
confirmation completed at **12:23 UTC**.
[Confirmation 1](peerreview/evidence/FINAL-2-capture-qualification-20260913/active-revision1-race-p1-source-v2/report.json),
[confirmation 2](peerreview/evidence/FINAL-2-capture-qualification-20260913/active-revision1-race-p2-source-v2/report.json),
[confirmation 3](peerreview/evidence/FINAL-2-capture-qualification-20260913/active-revision1-race-p3-source-v2/report.json).

The concurrent aggregate on `0dcb5c8` was stopped and joined at **12:14:51 UTC**
to replace the superseded candidate. Ten phases had passed; its simulator race
and complete Connect normal phases were interrupted with exit 143. No actual
aggregate test failure had been observed at the stop, but the incomplete gate
does not establish an aggregate pass. [Raw phase joins](peerreview/evidence/FINAL-2-superseded-aggregate-20260913/outer.stdout),
[actual outer exit](peerreview/evidence/FINAL-2-superseded-aggregate-20260913/outer.exit).

After publication of the qualified `b78b672` correction, its clean native
executable applied the reviewed release lock with exit 0, confirmed at
**12:25 UTC**. The resulting YAML SHA-256 is
`776f6cf9d57d1c8427ac981f3cf2222ddc1441371c90cbded2789d8ea1299767`.
Only `repositories.protocol_source_hash` changed, to
`sha256:83f8fd02ccd0cb8333bade3124aeab1bb3f480a008ceaaa3e74287a3b0d67ceb`;
production Go, runtime, EVM artifact, interface and infrastructure digests
remain unchanged. This was a local file update and sent no chain transaction.
[Actual invocation and result](peerreview/evidence/FINAL-2-capture-lock-20260913/RESULT.json).
The replacement complete gates started at **12:29:35 UTC** on published SN
`90f67b1859368d34b0404870f46819f012712674`. The **producer passed at
14:30:28 UTC**, with all 36 native phase joins exiting 0 and its final source
and release-lock checks passing. This includes the previously failing capture
phase, which completed in 167.019 seconds normally and 579.892 seconds under
race. [Complete producer output](peerreview/evidence/FINAL-2-producer-90f67b1-20260913/capture/outer.stdout),
[actual exit](peerreview/evidence/FINAL-2-producer-90f67b1-20260913/capture/outer.exit),
[finish time](peerreview/evidence/FINAL-2-producer-90f67b1-20260913/capture/outer.finished-at).

The aggregate's complementary simulator race phase **failed its 90-minute
deadline**, with native exit 1 observed at **14:59 UTC**. Its separate two-root
population phase had passed in 1,967.533 seconds. At the alarm, three full
supplement publication roots had been active for 19m25s, 17m20s and 19m22s;
the fourth active root had just entered its transport-bound fixture. Astra's
stack census found 188 roots still queued for parallel execution. The active
publication stacks were traversing local artifact stores, and retained file
timestamps show progressing writes. The alarm does not establish a 90-minute
hang in any one test. The aggregate finished with **exit 1 at 15:40:56 UTC**:
21 native phases passed and only the simulator race phase failed. Its final
source check passed, and the owned processes and private services were joined.
The gate remains failed. [Actual aggregate output](peerreview/evidence/FINAL-2-aggregate-timeout-20260913/capture/outer.stdout),
[actual exit](peerreview/evidence/FINAL-2-aggregate-timeout-20260913/capture/outer.exit),
[original timeout output](peerreview/evidence/FINAL-2-aggregate-timeout-20260913/sn-simulator-race.log),
[failure and active-root record](peerreview/evidence/FINAL-2-aggregate-timeout-20260913/failure.json).

Correction `e99954a`, published at **15:44 UTC**, gives those three whole
publication roots a separate
package clock and retains the complete disjoint race census: **2,181 ordinary,
three publication and two population roots**. It changes scheduling and
existing ownership guards, preserving test bodies, payloads, cryptography,
parallelism, uncached execution and 90-minute deadlines. Qualification and a
successful complete aggregate remain pending for the interrupted roots.
The correction's ten affected scheduling guards passed normally and under
race, with actual outer exits 0 at **15:37:34** and **15:39:58 UTC**, respectively.
Both retained source checks match before and after. The actual compiled
inventory also passed: the 2,186-name list, declared inventory and three-group
union are byte-identical at SHA-256
`6fd7093a8121b06c4cf892df4d524172ba9fe3dca1173d65e4158d5d3ed8f213`.
This establishes complete selection, not execution of all those tests.
[Normal guards](peerreview/evidence/FINAL-2-aggregate-supplement-qualification-20260913/guards10-normal-p1-v2/report.json),
[race guards](peerreview/evidence/FINAL-2-aggregate-supplement-qualification-20260913/guards10-race-p1-v2/report.json),
[compiled inventory result](peerreview/evidence/FINAL-2-aggregate-supplement-qualification-20260913/compiled2186/status.json),
[exact execution plan](peerreview/evidence/FINAL-2-aggregate-supplement-qualification-20260913/QUALIFICATION-PLAN-v2.txt).
The four interrupted roots' three sequential uncached race confirmations
continue on the same isolated source and binary. Preparation omits a duplicate
2,181-root execution; the next full aggregate owns that broad coverage.
Three earlier correction launch attempts were refused during source verification,
before compilation or any test body. Their error matches the helper's internal
30-second Git-command deadline; the specific operation was not recorded.
Staggered persistent-session launches passed the same source checks without
changing source, plans or limits. These attempts executed zero tests and are
preserved as refusals. [Original launch records](peerreview/evidence/FINAL-2-aggregate-supplement-qualification-20260913/prebody-refusals/).

The clean, published `e99954a` bootstrap CLI built successfully at **15:47:04
UTC**, with unchanged observations of all 12 repositories. Its native
release-lock review and apply both exited 0; apply completed at **15:49:37 UTC**
and installed exactly the reviewed YAML, SHA-256
`bd5e492077edc01acfa452d67ce1e437deec6d5e1add7ed8eab41dfd722b254f`.
Only the protocol-script digest changed, to
`sha256:f21b86f2ad38ab2aea7698190ed69cc6ac0fd19ab1e883720ab98a0529eb57c8`.
The runtime code, chain pins, other repository digests and spending limits
remain unchanged. This local update sent no chain transaction. A final stamped
CLI and corrected complete gates will use the resulting publication.
[Bootstrap build result](peerreview/evidence/FINAL-2-supplement-lock-20260913/bootstrap/RESULT-BOOTSTRAP-CLI-BUILD.json),
[reviewed candidate](peerreview/evidence/FINAL-2-supplement-lock-20260913/review/candidate.yml),
[native apply result](peerreview/evidence/FINAL-2-supplement-lock-20260913/apply/RESULT.json).

The preceding `90f67b1` native CLI build completed with actual build and outer exits 0 at
**12:31:52 UTC**. Its SHA-256 is
`9b42372c2ff768a21d7d117d716dcb4dfe00be422c8570880a423708a8a3d6e2`;
its clean VCS stamp names that revision, and all 12 repository observations
match before and after the build. [Build result](peerreview/evidence/FINAL-2-native-admission-20260913/cli/RESULT-FINAL-CLI-BUILD.json),
[actual outer exit](peerreview/evidence/FINAL-2-native-admission-20260913/cli/capture/outer.exit),
[before](peerreview/evidence/FINAL-2-native-admission-20260913/cli/meta/repos.before.tsv)
and [after](peerreview/evidence/FINAL-2-native-admission-20260913/cli/meta/repos.after.tsv).

Two read-only setup reconstructions completed with exit 0 at **12:36:58** and
**12:37:00 UTC**. Both bind plan
`0xd4525b8da2da4f786f4990beb2285ac09b42e033c3c7fdd3c45473a7c9336507`
and contain the same 2,309 action objects, including the approved 3,750-alpha
repair. Their only pair differences are generation time and observations of
adjacent finalized heads; the native plan hash explicitly excludes those
moving fields and apply rechecks them. Their total spend ceilings, including
superseded actions, are 185.748236 TAO, 35,000 alpha, 180 EVM TAO and 262
registrations, with no subnet creation. These are approved ceilings, not new
spending. [Exact comparison and native execution records](peerreview/evidence/FINAL-2-native-admission-20260913/REVIEW-SETUP-PAIR.json).
Setup apply started at **14:32:17 UTC**, after the producer passed. It adopted
this plan locally at **14:36:28 UTC** and began authenticating carried history.
After the aggregate failure, the operator interrupted it at **15:00:54 UTC**;
the process joined with exit 1 and an explicit `context canceled` error.
The post-stop journal still contains 10,258 entries, with **zero entries for
this plan or the additional reserve repair**. No new transaction or repair
credit resulted. Preserve the adopted plan and all prior history for supported
recovery. The original invocation's preparation label is retained; its actual
start, terminal result and cancellation records establish what ran.
[Native result](peerreview/evidence/FINAL-2-setup-canceled-20260913/RESULT.json),
[stop reason](peerreview/evidence/FINAL-2-setup-canceled-20260913/STOP-REQUEST.json),
[stderr](peerreview/evidence/FINAL-2-setup-canceled-20260913/setup.stderr),
[post-stop journal observation](peerreview/evidence/FINAL-2-setup-canceled-20260913/POST-STOP.json).

A fresh complete 256-UID reserve census at **11:25 UTC**, finalized native
block **7,996,371**, found **82,639.777928818 alpha** of registered stake and
**50,180.168141913 alpha** at reserve UID 254: **60.7215670220%**. The block hash
is `0x878ff4aeb7cb63cf858b1287d834cf23c705bdbeee85da1d497ebe90b3e56f15`.
At that snapshot, the approved 3,750-alpha transfer between registered hotkeys,
allowing one alpha-rao of rounding, projects **65.2593333302%**. This is a
projection, not a credited repair or a current target pass; ongoing emissions
change the denominator. A finalized debit/credit and another complete census
remain necessary. [Raw requests and responses](peerreview/evidence/FINAL-2-reserve-before-20260913/rpc.json),
[decoded census and calculation](peerreview/evidence/FINAL-2-reserve-before-20260913/SUMMARY.json).


A later complete census at **12:41:51 UTC**, native block **7,996,753**,
found **82,830.982610680 alpha** of registered stake and
**50,256.924044690 alpha** at the reserve: **60.6740647771%**. At that
snapshot the same approved transfer projects **65.2013562347%** after allowing
one alpha-rao of rounding. It is still unapplied; neither observation proves
the 65% repair target. The observations use the LAN node at one finalized hash
per census and do not include replayed storage proofs.
[Raw later census](peerreview/evidence/FINAL-2-reserve-before-1241-20260913/rpc.json),
[decoded values and projection](peerreview/evidence/FINAL-2-reserve-before-1241-20260913/SUMMARY.json).

A fresh complete census at **15:10:42 UTC**, native block **7,997,497**, hash
`0xa27f1b7924405971de344e295faafdc80c8c0454c1b1a2a659b2e923900a8104`,
found **83,213.547287627 alpha** registered and **50,410.350083881 alpha**
at reserve UID 254: **60.5794990444%**. The still-unapplied approved transfer
projects **65.0859768022%** at this snapshot. It remains a projection, with
the same finalized-credit and complete post-repair census requirements.
[Latest raw census](peerreview/evidence/FINAL-2-reserve-before-1510-20260913/rpc.json),
[values and projection](peerreview/evidence/FINAL-2-reserve-before-1510-20260913/SUMMARY.json).

The next run must retain both complete gate results on its final candidate,
five accelerated epochs, the activated production policy and three consecutive
complete production epochs, both validators' fresh native applications,
required traffic/proof/adversarial and lifecycle evidence, final accounting,
independent replay and actual shutdown results. This section will link their
terminal artifacts as they become available. Pending or failed work stays
visible; this report cannot establish acceptance until that evidence exists.
