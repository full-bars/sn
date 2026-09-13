# Testnet execution plan

Updated 2026-09-13. The user has requested full finalization and fixes for
previously ignored failures, flakiness and issues exposed by the shortened run.
The full requirements in [FINALIZE.md](FINALIZE.md) govern completion again.
The user explicitly confirmed SN testnet finalization under `sn/FINALIZE.md`;
all qualification and the current report at `sn/sim-testnet/FINAL-2.md` concern
this simulator and its runtime dependencies. Other simulation references were
mistaken and do not add work to this goal.
The earlier shortened-run instructions below are retained as historical scope
for those attempts, whose `final_acceptance=false` results remain unchanged.

Reports are numbered at the user's request: `sim-testnet/FINAL.md` remains
report 1, `sim-testnet/FINAL-2.md` covers this full finalization, and later
finalizations use `FINAL-3.md`, `FINAL-4.md`, and so on. Preserve each earlier
report and its underlying evidence. The [report 2 closure table](sim-testnet/FINAL-2.md)
tracks the first report's peer-review findings. A compatibility exception,
historical replay, pending check or artifact-only assertion is not proof that
the next run met an on-chain target.

The user explicitly directed execution against the real chain limits after
the peer review: retain runtime 455's root-controlled
`max_allowed_validators=64`. Lowering it to 56 is not a prerequisite for this
testnet run. The existing compatibility policy already requires exactly 64;
prove the 200-head topology against actual UID occupancy, permits and native
selection under that value. Report the difference from the whitepaper's ≤56
target explicitly, without treating a permit limit as a fixed UID partition.

Current work:

1. The retained campaign, signer-authority and gate corrections are composed
   and published; their focused qualification and required failure confirmations
   are complete. Preserve the existing deployment, wallets, approvals and journals.
2. Use Terra (`gpt-5.6-terra`, reasoning effort `max`) for all tests and gate
   execution. Use Astra (`gpt-6-astra`, reasoning effort `max`) to diagnose and
   fix failures and flakiness, then return corrected source to Terra for reruns.
3. Complete both full gates on the candidate with the refreshed release lock
   and private test services. The earlier validator, native-receipt and scenario
   failures have completed their scoped confirmations. Keep the original failed
   and interrupted results visible; focused passes do not replace full gates.
4. Complete the required real release campaign and production soak, then
   reconcile on-chain outcomes, public replay, the final report and shutdown.
   Reuse valid completed evidence; unrun, failed and waived checks are not passes.

Retain the approved 6,000-alpha repair allowance, 37,250-alpha lifetime limit,
180 EVM within 200 total TAO, 262 registrations and zero new subnets. Use
`192.168.1.162:9944` without RPC rate limits. Full acceptance remains pending.

Latest checkpoint, 2026-09-13 18:18 UTC: the soak remains stopped. The user
approved one 6,000-alpha replacement for the unsubmitted 3,750-alpha repair,
within 37,250 alpha lifetime. The single-setting vault change is published at
`8b2f481dbe87092d0c1742274712a6f805c1c375` in the independent final candidate
checkout. Preserve the primary vault unchanged until the active race owner
closes. No additional repair transaction has been submitted.

The reserve-succession correction's causal check and 30-root normal/race
qualification are complete. The private-fixture correction passed all seven
selected roots normally and under race; its three timeout roots also completed
all three required normal confirmations on unchanged binary bytes. Two e999
race confirmations have passed, and the third has been running since 17:57:09
UTC. Reuse those closed scopes; do not start another development test campaign.

The matching current upstream dependencies require Warp in source admission.
The reviewed `02ba4c7` integration preserves archived lock formats and extends
the current graph to 13 repositories and 16 live modules. Formatting and module
metadata are clean after the three-line Proxy reconciliation published at
`6204ae7df2a9868bbb3a7b61231917a36e4f5c9f`. Terra owns the exact 24-root
normal/race integration and concurrent preparation of the final executable and
both complete gates. The older `cd036cee` executable and unapplied plan are
historical; fresh native planning must use the qualified current candidate and
approved replacement. Producer success can admit that repair while a clean
aggregate continues; both complete gates remain required for acceptance.
The [report and portable evidence](sim-testnet/FINAL-2.md) distinguish actual
passes, historical failures and remaining work.

Previous checkpoint, 2026-09-13 17:32 UTC: the soak remains stopped. The complete
`cd036cee` producer timed out in capture-private normal after 300.104 seconds.
Both superseded gates were stopped and joined: producer outer 143 at 17:19:25
with 23 passed phases, one failed and three interrupted; aggregate outer 143
at 17:19:27 with five passed and two interrupted, without an observed aggregate
product failure. The one-file fixture correction `f6cfd797` preserves all 1,000
miners and actual publication checks. Terra is preparing its exact seven-root
normal/race qualification and three sequential normal confirmations for the
three actual timeout roots. The separate e999 race confirmations continue on
unchanged source and binary; their first pass is complete.

The finalized 17:10 census at block 7,998,093 found 60.4855132935% reserve;
the approved 3,750-alpha repair now projects only 64.9713567471%. Native
read-only setup refused it with exit 1 at 17:14:23 UTC. The approved lifetime
cap remains 35,000 alpha. Correction `6311cb8` permits a provably unsubmitted,
insufficient terminal repair to be replaced through a new plan while preserving
the original history and all started or credited liabilities. Its focused
qualification is being launched independently. Neither correction is deployed,
the original journal is unchanged and no repair transaction has been submitted.

Previous checkpoint, 2026-09-13 16:25 UTC: clean publication `cd036cee` contains
the qualified scheduling correction and reviewed release lock. Its complete
producer and aggregate gates started at 15:57:41 and 16:01:22 UTC with private
test services; both are still running with no observed phase failure. Keep
their complete source and dependency snapshots unchanged. The final native
CLI built with exit 0 and unchanged observations of all 12 repositories;
two read-only setup reconstructions completed with exit 0 and agree on plan
`0x814d362c650dcdb86f1a57e4f266acd789e6be5703c9e65ad753576199bc3358`.
All actions and approved spending ceilings are unchanged. The plan remains
unapplied; the fleet and soak are stopped. The four interrupted roots'
required confirmations continue on their isolated source.

The 16:09:29 UTC complete reserve census at block 7,997,791 found
60.5324340452%; the approved 3,750-alpha repair projected 65.0285723985%.
Fresh native admission must still establish that the repair reaches 65%.
The 60% operating floor does not replace that target. If the approved amount
becomes insufficient, preserve the native refusal and do not exceed the
35,000-alpha lifetime cap. No new transfer has been submitted.

Previous checkpoint, 2026-09-13 15:45 UTC: the producer gate on published
`90f67b1` passed at 14:30:28 UTC with all 36 native phase joins exiting 0.
The aggregate's simulator race complement subsequently timed out after
90 minutes; its separate population race phase passed. The aggregate finished
with exit 1 at 15:40:56 UTC: 21 phases passed, one timed out, and its final source
check passed. All owned processes and private services are joined.
Correction `e99954a`, now pulled and pushed, separates the three full supplement publication roots
from the complement, preserving all 2,186 race roots and existing limits.
The ten affected guards now pass normally and under race; the compiled inventory
confirms all 2,186 roots. Terra continues three sequential race confirmations
for each of the four active timeout roots on unchanged isolated source. Prepare
the refreshed lock, final native CLI and corrected full gates concurrently with
those confirmations. The next full aggregate supplies broad
integration; an extra full development rerun is omitted.

The published `e99954a` bootstrap CLI applied the reviewed release lock with
exit 0 at 15:49:37 UTC. YAML SHA-256 is
`bd5e492077edc01acfa452d67ce1e437deec6d5e1add7ed8eab41dfd722b254f`;
only the protocol-script digest changed. The final CLI and corrected complete
gates follow publication of that lock and the updated evidence. Spending
limits and runtime code are unchanged; this update sent no chain transaction.

Setup began after the producer pass and adopted plan
`0xd4525b8da2da4f786f4990beb2285ac09b42e033c3c7fdd3c45473a7c9336507`
locally. After the aggregate failure, setup was interrupted and joined at
15:00:54 UTC with exit 1 and explicit context cancellation. The post-stop
journal contains no new-plan or reserve-repair entries. Preserve the adopted
plan and prior history for supported recovery; no additional reserve transfer,
renewal, continuation, adoption or fleet launch has occurred.
At the then-current complete census (15:10:42 UTC, block 7,997,497), the reserve was
60.5794990444%; the unapplied approved repair projected 65.0859768022% at that
snapshot. Actual finalized credit and a fresh target census remain required.
See the exact native results and public evidence locators in
[report 2](sim-testnet/FINAL-2.md).

Previous checkpoint, 2026-09-13 12:25 UTC: correction `b78b672` is published.
Its affected 443 capture roots and twelve coverage guards pass normally and
under race. The sole root active at the prior capture timeout, including all
four subtests, completed three fresh sequential race confirmations on identical
binary bytes and unchanged source. The eleven unchanged separately owned
capture roots retain their prior scoped qualification. Both corrected complete
gates still include the full selections.

The `0dcb5c8` producer remains a failed gate (21 passed phases, one failure,
three interrupted); its superseded aggregate was stopped with ten phases passed
and two interrupted, without an observed test failure. Their actual outer exits
are 143 following owned cleanup. All owners are joined and source is released.
Neither incomplete gate is reported as passing. See the portable raw outcomes
and qualification evidence in [report 2](sim-testnet/FINAL-2.md).

The clean, pushed `b78b672` native executable applied exactly the reviewed lock
with exit 0. YAML SHA-256 is
`776f6cf9d57d1c8427ac981f3cf2222ddc1441371c90cbded2789d8ea1299767`;
only the protocol-script digest changed. Final publication is followed by both
complete gates and the final stamped CLI build in parallel, then two fresh
matching setup plans. Producer success can admit the approved reserve repair
while a clean aggregate continues; both gates remain necessary for acceptance.
The historical 11:25 UTC census at block 7,996,371 found 60.7215670220% reserve
share and projected 65.2593333302% after the approved repair. The later
observation above supersedes that moving projection.

Earlier approval, 2026-09-13: the user explicitly approved raising the
lifetime cap from 31,250 to 35,000 alpha for one additional 3,750-alpha reserve
repair. The vault change is committed and pushed at
`d4ea0cbdf49630d8e1afc3e2184858cb58940fd3`. This approval remains valid but
the pending amount is insufficient at the later snapshot above. It does not
authorize exceeding 35,000 alpha lifetime. The per-repair maximum remains
6,000 alpha.

Two actual read-only setup builds on SN `3af4251` completed successfully at
01:48:04 and 01:58:00 UTC, both with plan hash
`0x06116ddc5cdc6945c7d96c8920f2b4cfbaa9a6f04bfd211503cd51803286182a`.
The reviewed revision adds the exact 3,750-alpha repair and authenticated
zero-spend carry of the existing coordinator repair. It retains all prior
positive-alpha actions. Neither plan was applied; the original plan, journal
and signed transaction bytes remain retained. New production corrections
require a refreshed release lock, stamped CLI and fresh bound plans before
application. [Read-only plan review](../temp/sn-approved-alpha-repair-review-20260913/setup-v5-review.json).

The original full producer gate ended with exit 1 at 02:50:47 UTC; the
aggregate ended with exit 1 at 03:23:25 UTC. Their source snapshots remained
clean. Both failed private-service startup, and their actual completed test
failures are being corrected. Jobs joined with exit 143 during cleanup are
recorded as interrupted, not completed test verdicts. [Producer receipt](../temp/sn-final-execution-20260912/runtime/producer-gate/capture/RESULT.json),
[aggregate receipt](../temp/sn-final-execution-20260912/aggregate-gate-prepared-20260912T2254Z/capture/RESULT.json).

The previously pending focused qualifications are now complete on their
recorded immutable sources. The Go 1.26 qualification launcher correction passed its four
regression roots normally and under race. Corrected CRV4 and server artifact
test binaries subsequently exited 0 in both modes, but their captures failed
because expected-outcome files omitted legitimate subtests. Existing offline
replay has now checked both retained streams against corrected exact declarations;
the original failed captures remain unchanged. The isolated PostgreSQL control
reproduced `Permission denied` on the copied mode-0700 initialization directory.
After restoring public fixture permissions, the actual PostgreSQL 18, Redis
and fixture preflight passed with successful owned cleanup at 03:43:02 UTC.
[Corrected service preflight exit](../temp/sn-private-services-qualification-20260913/runtime/preflight-0755/capture/outer.exit).
Service18, monitor14 and cache/provisional23 integration checks passed normally
and under race on SN `ebe70a3` and server `e2358826`. Their previously failed
normal roots have three fresh passing confirmations on the recorded immutable
binaries. [Sealed scoped qualification](../temp/sn-private-services-qualification-20260913/runtime/RESULT-service-monitor-cache-corrected.json)
(`sha256:c98dbe1e98943cf2eb7087e362b4c0b17e08ec08006007d87c21ef2b39d493cf`).
Connect's actual plain-WebSocket resolver bypass is corrected at `3d29e1f`.
Its 15-root integration passed normally and under race; all eight previously
failed roots completed three sequential passes in each mode on the same
recorded binaries. The remaining 18 simulator normal confirmations, both
CRV4 roots in both modes, three private-capture normal confirmations and the
seven-root private-capture race integration also passed. Earlier simulator,
validator, stabi, service, monitor and cache results retain their original
source scope. See the [current qualification evidence](sim-testnet/FINAL.md).

The integrated source includes server `bbfe4296`, Connect `3d29e1f`, SDK
`169d4c2c`, operator-proxy `714f10f0`, proxy `c11c7eb4` and xops `ec84346`.
All 15 live modules passed dependency validation. The xops update leaves all
15 Subtensor infrastructure lock inputs unchanged; the normal infrastructure
gate still covers its affected inputs. The new release-lock YAML hash is
`sha256:ddcd0ec9f44f11c86c22b6b9ff73a31a09c0d0b910e07a1d8b966a8c7af40923`.
The native stamped `5a79b62` renderer applied exactly the reviewed bytes with
exit 0. Runtime, EVM, interface and infrastructure lock fields are unchanged.
The final stamped CLI build and both full gates follow this publication in
parallel, using the existing physical workspace and separate private services.
Completed historical qualification remains reusable within its recorded scope;
the complete current candidate still needs both full-gate results.

The fleet and soak remain stopped. There has been no new reserve transfer,
renewal, relay continuation or live campaign during this preparation. The
critical path is to close the actual failures, publish the composed source and
lock, obtain launch admission, apply the approved reserve repair, then perform
renewal, relay continuation and retained-history adoption before the full run.
Producer success can admit the live campaign while a clean aggregate is still
running, as specified by the complete plan; any actual aggregate failure stops
new mutations. Both full gates are required for final acceptance.

Historical checkpoint, 2026-09-12 23:13 UTC: the full fleet is stopped. Both
operator APIs and temporary payout-recovery proxies were also stopped after
all 16 funded epoch309 claims finalized, paying 103.320655346 alpha with eight
alpha-rao of accounted rounding residue. The [final peer-review report](sim-testnet/FINAL.md)
contains the receipts and pinned state; the historical 15/17 scenario remains
`final_acceptance=false`.

Actual read-only doctor on local source `02dfe50` passed 63 of 64 checks. Its
sole failure was systemd's degraded state from 43 stopped simulator units.
Their metadata and all 621 available journal entries were preserved before
resetting only those historical failure flags; the manager now reports running,
without restarting any process. Actual read-only setup first refused the full
retained repair audit budget's `observed_at` field. Its corrected reader preserves
the signed projection and complete document hash. The next attempt exposed the
missing recovery case for the original finalized repair transactions. Candidate
`9c444e4` passed that gate, then refused the original companion's predecessor
CREATE while binding the later repaired coordinator. Source `cf3ccd6` corrected
the combined carry path and reached reserve-majority planning. Its fourth actual
attempt stopped at 21:48:11 UTC: the target needs another 2,855.249565922 alpha,
but signed transfers already consume the 31,250-alpha lifetime allowance.
The prior 5,999.806443325-alpha repair is already credited. The configured
6,000-alpha allowance applies per repair and does not replenish the lifetime
budget. User approval is pending to raise that lifetime cap to 35,000 alpha for
one additional 3,750-alpha tranche. At pinned native block 7,992,355, that would
raise the observed 61.449% reserve share to at least 66.113%; fresh planning
must recheck stake and transferable source capacity. No revised plan or
transaction was emitted; the original plan and journal remain byte-identical.
[Latest actual admission error](../temp/sn-full-finalization-20260912/readonly-admission-20260912/setup-v4.stderr).

The final execution workspace and pinned Solidity libraries are prepared at
`temp/sn-final-execution-20260912/workspace`, with real Git directories for
authentic executable VCS stamping. Every tracked source file was compared to
the prior workspace for byte and mode equivalence. The existing unlocked vault
is reused. No key or plaintext secret was copied.
Candidate `9c444e4` is committed and pushed on its review branch, with a reviewed
source lock and an independently built read-only CLI. Five repair-carry and four
relay-continuation roots, the offline authority root and six archive roots each
completed three fresh normal passes and a race pass. Public checkpoint, native
capture and CRV4 checkpoint checks also passed in both modes.
[Completed affected qualification](../temp/sn-final-release-20260912/runtime-9c444e4-20260912T2118Z/RESULT.md).
Successor `eca9e19` passed native coverage, companion carry, adjacent evidence,
archive/payout and native consumers in both modes. Its reward-reader cancellation
failure is corrected: all three reward roots passed three fresh normal runs and
one race run on `94cb3dd`. Simulator and validator V2 observation also pass both
modes. Automatic native readiness and preparation share one absolute deadline;
the full remaining-work bound is 7,570 blocks, preserving both acceptance phases.
The retained-ledger capacity checks pass, including the exact 8,065-block limit
and refusal at 8,066. The 43-root matrix's outdated horizon fixture was its sole
normal failure, with that three-root race partition initially unrun.
[Exact completed matrix and retained failure](../temp/sn-final-release-20260912/runtime-94cb3dd-20260912T221700Z/RESULT-94-GO-MATRIX.md).

On `da27b85`, the corrected horizon root has three fresh normal passes, and its
original three-root partition passes under race. The exact final semantic census
contains 310 roots. Two of 27 gate guards failed in both modes: a stale direct-call
assumption across the real startup delegation chain and three renewal consumers
without the required parallel marker. Astra corrected those three test files;
on `e8bceaaa62e6d1c3ad2f5a30535f7a7a3806661d`, Terra completed three fresh normal
and three fresh race passes of both failed guards. The three affected renewal
consumers also pass together normally and under race. Production source and
the release lock remain unchanged. These confirmations are complete; historical
streaks and horizon checks will not restart.
[Exact partial qualification and stamped CLI](../temp/sn-final-execution-20260912/runtime-da27b85-20260912T2245Z/RESULT-PARTIAL.md).
[Completed guard corrections and adjacent integration](../temp/sn-final-execution-20260912/runtime-e8bceaaa-20260912T230158Z/RESULT-E8-GUARD-CORRECTION-CORRECTED.md).

Both full-gate launch commands are prepared with separate private mutable
resources. The twelve-repository snapshot includes the vault budget. Therefore
the pending spending decision and any approved vault edit, commit and push must
precede the final source freeze and concurrent producer/aggregate launch. A
mid-gate budget change would invalidate the final snapshot. Focused correction
and qualification are complete; final publication and CLI preparation proceed
while approval is pending. After both gates,
proceed through fresh setup-plan admission, renewed fleet authorizations, bounded
relay continuation, retained-history adoption and the actual full campaign.

Earlier component checkpoints, superseded by the completed results above:

Terra passed the strict V2 history-adoption core and corrected EMA bridge
normally and under race, including three fresh confirmations in each failed
mode. Those fixes are integrated. The ten focused renewal roots also passed
normally and under race on source `006c0c0`, including three fresh confirmations
in each failed mode for the two repaired roots. [Renewal validation](../temp/sn-renewal-006c-validation-20260912T1820Z/runtime/renewal-006c-20260912T1828Z/RESULT.md).
The combined strict CLI, owned-LAN routing, repair carry, plan revision and
renewed lifecycle code is assembled. On source `2984c9b`, all 71 selected
validator roots pass normally and under race; the corrected cadence root also
has three fresh passes per failed mode. The 98-root simulator matrix exposed
renewal-evidence, lifecycle and history-adoption fixture failures. Its race
process exhausted the shared ten-minute budget; the terminal lifecycle root
had run for 53 seconds. Preserve that timeout and qualify the complete selected
population in bounded partitions with unchanged deadlines. A mode-775 TMPDIR
also caused one invalid launcher refusal, which is not a product diagnosis.
[Combined validation and original failures](../temp/sn-strict-composed-fixes-validation-20260912/runtime/preflight-20260912T191953Z/RESULT.md).

Contract generation is complete at `5c4c546`. The revised full Forge build,
18 binding-policy tests, generator consistency checks, and generator/stabi
tests normally and under race pass. The coordinator creation and runtime bytes
exactly match the retained deployed repair; runtime size is 24,564 bytes.
The original oversized test harness and first overflow-fixture failure remain
retained. [Exact artifact comparison](../temp/sn-contract-generation-20260912T1841Z/runtime/generation-20260912T1845Z/full-build-revised-coordinator-compare.stdout).
The generated payload is integrated. Both corrected renewal-evidence roots
have three fresh normal and three fresh race passes on `73ad855`.
[Renewal evidence confirmations](../temp/sn-semantic-renewal-generation-validation-20260912/runtime/RESULT.md).
Composed `bcb1ce0` passes the selected native/EVM checkpoint, capture, history-read
and startup populations in both modes. Its three simulator fixture failures and
two archive fixture failures are preserved; their corrections and the frozen
public publication/consumer/recorder code are composed in successor `e06f055`
for Terra qualification. [Composed results](../temp/sn-finalization-integration-20260912/runtime/RESULT.md).
The later native interval and reward/application coverage results are above.
Neither full gate nor either complete live acceptance phase has passed.
The unrelated calibration prerequisites were removed from SN qualification;
Terra passed all 12 affected guard roots normally and under race. The existing
runtime dependency census and full SN gates remain required. [Scope validation](../temp/sn-scope-validation-20260912T1830Z/runtime/scope-guard-20260912T1831Z/RESULT.md).

Read-only copies of all four retained source ledgers were inspected without
changing their file metadata. The largest source contains 134,673 records,
16,958 trails and 698,568,804 raw record bytes, within the original limits of
655,360 records, 81,920 trails and 10 GiB. This observation does not replay record
signatures or certify the remaining campaign. All four activation contexts bind
block 7,975,563; the existing 10,080-block relay allowance formula therefore ends
at 7,985,643, before the observed finalized block 7,991,348. No `evidence.relay.*`
entry exists in the retained journal. The retained locators contain 182 pending
members; the corrected 7,570-block full-work forecast requires another 200.
Locator counts still require complete signature and immutable-slot authentication. Astra has
frozen an explicit plan-bound continuation at `a52758b` with up to 512 relay slots at
50 gwei per 1,000,000-gas action, within the existing 25.6-TAO relay reserve.
It must preserve original activation and liabilities, bind the actual pending
census and finite remaining run window, and retain every original source-storage
and lifetime monetary limit. The continuation and capacity code has passed its
affected checks above; no continuation plan has been applied. Choose its fixed
end only after gates and renewal so preparation does not consume the remaining
source-capacity margin.

## Historical shortened execution — 2026-09-11 17:33 UTC

The user directed us to stop preparation tests, run the actual simulation on the
real testnet, and fix issues found by that run. The former producer and aggregate
gate prerequisites, repeated failed-test confirmation sequences, duplicate plan
comparisons, separate pre-launch smoke rehearsals, and repeated audit work are
removed from the launch path. Do not start another preparation test cycle.
Previously completed evidence remains reusable within its recorded scope;
waived or unrun checks must never be described as passing. Full preparation
gates are no longer conditions of completing this testnet exercise.

## Execute now

1. Reuse the existing attempt-4 plan and completed receipts. The carry repair,
   database migration and configuration rendering are complete. Do not repeat
   them or regenerate the plan for a provisional driver correction.
2. Keep the existing `epoch` scenario running with explicit --provisional-resume,
   using the retained configuration and corrected driver. The first timed run
   started at 15:56:51 UTC. Current ownership and the latest run ID are recorded
   in the external finalization directory's CURRENT.json. This scenario observes
   an epoch transition on the working fleet; it does not
   certify the full release or production acceptance window. Fix failures
   observed by this run and report its actual outcome.
   The full release attempt completed all 16 lifecycle preparation actions,
   then stopped before acceptance because its planned pruning target was UID 7
   while the computed and recorded target was UID 1. Preserve that attempt and
   failure; defer the pruning/fault campaign while the epoch scenario runs.
   When an actual runtime correction requires a new worker image, preserve the
   interrupted result and completed state, join its owner and fleet, deploy the
   corrected image to both simulator and dedicated Connect, and resume the
   existing epoch scenario. Never describe an interrupted interval as passing.
   Keep native custody,
   spending limits, journal serialization, process ownership and live health.
   Process log classifications are observations in this mode: preserve every
   finding and its original classification without stopping the fleet for it.
   Provisional startup may use ready providers while other live swarms catch up;
   retain actual health values. A new controller may adopt the existing fleet
   without replacing its binary or manifest, or restarting its processes.
   Bound actual process readiness to 30 seconds; retain exact generation and
   process identities, live PIDs, and non-provider health probes.
   Provisional scenario startup uses the same authenticated live-fleet
   preparation as resume to omit the full doctor when no spend is pending.
   It reuses the completed topology handoff through the existing process log
   gate; it must not manufacture completion or new verified setup receipts.
   The nested provisional campaign executor reuses its exact parent's already
   authenticated deployment payloads. Omit the duplicate historical deployment
   preflight while retaining current scenario actions and their postconditions.
   Provisional relay startup and preparation require only the next block to
   fit the original paid horizon; log the full requested forecast as waived.
   Skip the duplicate pending-public-census preview, while authenticating each
   actual publication before its relay admission and send. Keep the original
   activation/native anchors, 256-slot ceiling, debits and all spending caps.
   Full phase coverage is not established by this provisional admission.
   Provisional launch omits precompile conformance and the pre-launch
   governance drill. Preserve their actual failed/unrun evidence and report
   both prerequisites as waived. Do not repeat probe funding or commitments
   to enter the traffic run; keep actual takeover binding actions and their
   spending/transaction postconditions. Full conformance remains unproven.
   The owned LAN RPC at 192.168.1.162:9944 has been verified against testnet
   chain 945 and the original native genesis. Native and EVM traffic now use
   that route through an invocation-only provisional transport override and
   the existing workload fault proxies, with all RPC rate limits removed.
   Record the actual endpoints and zero RPC rate limits. Retain the approved
   plan, signed inputs, receipts and spending limits; omit independent public
   RPC comparison in this mode and keep final_acceptance=false.
   Fresh signed proof coverage is an observation during the run, not a
   provisional campaign startup prerequisite. Record
   fresh_proof_startup_waived=true, the original proof baseline, and observed
   counts with observed_proof_counts_verified=false. Do not fill verified proof
   counts or describe old coverage as a fresh-proof pass. Continue actual proof
   validation during scenario observations and completion reporting.
   Provisional adoption also waives strict public deployment evidence
   publication, which revalidates superseded historical manifests. Record
   deployment_evidence_publication_waived=true and final_acceptance=false in the
   provisional handoff. Preserve existing public files and publication errors;
   do not create a substitute published manifest or describe publication as
   passing. Keep approved topology actions, journal entries and spending caps.
   The four exact private activation contexts may grant testnet staging directly under
   the explicit retained-context allowance. Upload signatures, session/object
   binding, finite intent expiry and quotas remain enforced. Historical and
   current-chain admission checks are waived/unrun, never reported as passing.
3. Observe real transactions, provider traffic, validator proofs and accepted
   epochs. Fix concrete runtime failures and resume supported completed work.
   Keep original errors and actual completion markers; never invent a pass.
4. Report the achieved coverage, transactions, epochs, failures and remaining
   gaps from the actual run. Public replay and release certification work must
   not delay launch; describe any omitted validation honestly.

## Limits and current state

Use public Bittensor testnet, netuid 521, with the existing 1,000 providers,
20 swarms, two operators and two validators. Retain attempt-4, its keys,
used activations, signed setup, journal, approvals and deployed contracts.
Caps remain: 6,000 alpha reserve-repair allowance, 31,250 alpha lifetime,
180 EVM within 200 total TAO, 262 registrations, and no new subnets.
Do not reset state or repeat funding/registration transactions.

The provisional fleet has run real testnet work. All twenty swarms passed live
startup on September 11 before a process-log gate stopped that generation for
onboarding-metric SQL warnings. The provisional driver now records those
classifications without making them launch conditions. The SQL correction is
prepared separately and must not delay launch. No preparation tests are running.
This mode records final_acceptance=false; do not claim strict certification.

Deployed in CLI25: after the hash-pinned testnet handoff validates,
enable existing closed-native-input deferral in memory. Preserve signed subnet
1391 inputs from settlement 290; report deferral without native submission and
continue at the next native epoch. This does not establish successful trail
proofs.

Also deployed in CLI25: validated provisional shared boundary preparation receives a
120-second canonical-read budget within its existing producer deadline (240
seconds in this run). Trail/packet deadlines and ordinary reads remain 30
seconds. The owned LAN route now has no RPC request quota; canonical checks
remain in place. CLI27 also corrected the stale dedicated Connect binary,
which had disabled subnet egress attribution. This produced 660 additional
proof rows across all four validator/operator paths before the epoch scenario.
That scenario exposed a settlement rollover failure. CLI29 excluded local
signature verification and scratch writes from the HTTP I/O deadline; both
settlement 292 closures completed. Public evidence replay then encountered a
truncated stream; its interaction with server deadlines is the inferred cause.
CLI32 gives that bounded route a ten-minute
request/write allowance and records abort causes. It also uses existing
same-nonce cancellation for an expired close intent, and schedules ST sync and
close retries every five seconds to reach the five-block close window. The
owned-LAN deployment restores the original taskworker count 8 / batch size 4.
Both operators finalized their epoch293 closes before the cutoff; captured
emission and payout remained zero. Both validators subsequently published
epoch293 evidence. CLI33 normalized validated in-memory contract address text,
allowing native1394 measurements to seal without changing retained files.
CLI34 added a bounded observation of the actual retained V2 intent records;
it leaves strict authenticated counters and acceptance claims unchanged.
The next actual native submission failure exposed GSRPC's handling of JSON
null storage results. Decode those results as nullable strings so an absent
slot remains distinguishable from malformed responses. Deploy this correction
through the same retained-state resume procedure; keep all original failures.

The bounded consumer run completed once and joined successfully. All eight
escrow contracts settled, producing 1,052,426 and 1,052,424 provider usage bytes
for the two operators from existing credits. No new account, credit or chain
funding was created. These byte sweeps establish actual usage, not a chain
payout; their fiat revenue is zero. Do not repeat this traffic as preparation.
Observe epoch294 provider eligibility and a nonempty payout commitment, then
the existing epoch295 deposit and subsequent pool scoring. A nonempty usage
root can be committed even when captured emission is zero. The sealed294
measurements correctly gave zero pool weight because source293 had no root;
preserve those measurements. Native application, positive capture and payout
remain actual-run outcomes to establish. Record their results in CURRENT.json
and the report while the fleet continues working.

Historical evidence remains in [FINALIZE-COMPLETE.md](FINALIZE-COMPLETE.md),
[FINAL.md](FINAL.md), and the external finalization directory. The native
campaign's full epoch windows remain real elapsed time; there is no renewed
14-hour completion promise before an actual campaign start.
