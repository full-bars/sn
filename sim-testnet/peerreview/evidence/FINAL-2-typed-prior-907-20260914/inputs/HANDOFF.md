# Typed prior capture ownership handoff

Frozen candidate: `907d18698f3464da8193894a3ad42101184a6a56`; parent `fc9086852125227d4e497f4d54b6590a527032ba`.
Source: `/home/by/urnetwork/temp/sn-prior-carrier-capacity-correction-20260914/sn`. Root reviewed and approved the three-file correction.
Astra ran no tests, builds or formatters. Terra owns all execution. The canonical
and live standalone gate inputs were not edited.

Only `scripts/test-release-1.0-producer-gate.sh`,
`sim-testnet/release_gate_capture_metadata_test.go`, and `sim-testnet/README.md`
changed. `IDENTITY.json` contains exact blobs and SHA-256 values. Formatter scope:
only `sim-testnet/release_gate_capture_metadata_test.go`; reconcile a clean
formatter successor before compiling if it changes bytes. Preserve these frozen
qualification inputs after admission.

## Cause and preserved acceptance

Retained actual failure: `/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/workspace-physical/terra-runtime/producer-final37-standalone-fc908685-r1/tmp/urnetwork-release-gate.4mobFlgK/logs/capture.log`.
SHA-256: `5b4e5e5e4580002bca127fb366bd6beffeab071fa5a498bc08ee2dbe0db816f1`.
Normal package passed in 155.781 seconds. Race package failed at its unchanged
600-second deadline (600.586 seconds including shutdown). The only running root
was `TestFinalCaptureCapacityPriorCarrierDecodeV2KeepsTypedLimitsAndIntentSchema`,
active for 2m34 at the alarm. In installed Go 1.26.6, `testing.go:1768–1816`
explicitly excludes the serial barrier and parallel-slot waits from this active
timer; about 7m26 had elapsed before this root resumed. No per-root profile or
verbose event history exists for that old package, so the evidence does not
attribute that preceding time to one particular sibling or prove an individual
hash took 2m34. The stack shows finite SHA-256 work, not a deadlock.

The root constructs two real 32 MiB + 1 originals, valid and invalid typed intent
controls, at `final_semantic_prior_carrier_decode_v2_test.go:266–281`. Both remain
signed and round-tripped through the independent legacy whole-wire verifier and
the current verifier; caller-byte immutability, size bounds, source hash, signature,
canonical wire and schema checks all remain. The original allocation measurement
and every test body are byte-identical. The root now has its own admitted
normal 5m/race 10m owner, independent of ordinary capture's serial and queued
work. No timeout, fixture size, iteration count or production check changed.

Retained owner environment: GOMAXPROCS=4, RELEASE_GATE_JOBS=3,
RELEASE_GATE_CONCURRENT_GATES=2; source/dependencies frozen, offline Go resolution.
The failing lane ran 20:18:26.807–20:31:24.944Z. Concurrent neighboring owners
capture-evidence, capture-renewal, capture-revision and capture-lifecycle passed.
Those source-scoped passes are retained, not relabeled as final gate success.

## Exact qualification

Use the established qualification runner unchanged, the approved pinned
13-repository projection and package CWD `/home/by/urnetwork/temp/sn-prior-carrier-capacity-correction-20260914/sn/sim-testnet`.
Require the relative policy fixture before a body, exact compiled `-test.list`
membership, `-test.v`, numeric body exit, package-tagged test2json events, source,
dependency and binary pre/post fences, and joined cleanup. Compile one normal
and one race binary, then reuse each within its mode. GOMAXPROCS=4 and
`-test.parallel=4`; `-test.count=1` throughout.

* `BOUNDARY-ROOTS.txt` / `BOUNDARY-SELECTOR.txt`: exactly one root. Normal test
  deadline 300s, outer 360s. Race test deadline 600s, outer 660s. The race root
  needs **three sequential fresh processes on the same source/dependency/binary**:
  p1, then p2 only after p1 passes, then p3 only after p2 passes. No failed normal
  confirmation obligation was opened by the original race-only timeout.
* `GUARDS-ROOTS.txt` / `GUARDS-SELECTOR.txt`: exactly 25 roots in each mode,
  normal 300s/outer360s; race 600s/outer660s. It includes all 12 roots from the
  changed capture-owner file, all 10 roots of the evidence source/command guard,
  compiled capture enumeration, the 3-byte adjacent typed-source boundary, and
  the unchanged process-wide allocation control.
* `QUALIFICATION-ROOTS.txt` / `QUALIFICATION-SELECTOR.txt`: exact disjoint union
  of those 26 roots, for inventory only. Execute the boundary separately so its
  clock never inherits the guard/allocation prefix. No skipped or unresolved
  selected root counts as a pass. Actual compiled/source census is authoritative.

Representative direct-binary arguments (selectors are exact anchored contents
of the files above):

```
-test.v -test.run <selector> -test.count=1 -test.parallel=4 -test.timeout=5m
-test.v -test.run <selector> -test.count=1 -test.parallel=4 -test.timeout=10m
```

The first is normal, the second race. The existing `ExcludesOfflineSemanticAnalysis`
root independently compares all compiled capture declarations with current host
source. `RequiresIndependentPopulation` proves every selected source has exactly
one owner per mode. These are not replaced by the focused 26-root census.

## Deterministic old-scheduling causal

`CAUSAL-corrected-to-fc908-script.patch` is **forward**, corrected candidate to
original fc908 scheduling, SHA-256 `b4bf44c4f61082cc87340fe19eb31ceb65317b69b70a959b7a141a3dadf31c38`.
Use a separate disposable candidate checkout, never the qualification source.
The same corrected normal binary can read the changed script from that checkout's
package CWD; no new compilation or original ten-minute package rerun is needed.

1. Verify clean candidate source and the corrected script SHA-256
   `32236597d320ec1c92c285c5e5200b6e1cb469ae40fd4375a07711264798c7b8`.
2. `git apply --check <patch>`, then `git apply <patch>` (no `--reverse`).
3. Prove the only changed path is the producer script, `git diff fc9086852125227d4e497f4d54b6590a527032ba -- scripts/test-release-1.0-producer-gate.sh`
   is empty, and script SHA-256 equals
   `606c3ffa34d5594977aac3d3442629441c2bcfd422808611cf7f49222af65af3`.
4. `git apply --reverse --check <patch>` must succeed **after** this mutation.
5. List and run exactly `CAUSAL-ROOTS.txt` / `CAUSAL-SELECTOR.txt` under a normal
   5m/outer360s owner. Expect body exit1 with exactly one FAIL:
   `TestProducerGateCaptureSelectionRequiresIndependentTypedPrior`, containing
   `typed prior boundary lacks an independent unchanged budget` and
   `capture metadata changed capture execution or its scoped budgets`.
   Exactly two PASS controls:
   `TestProducerGateCaptureSelectionRequiresPrivateFixtureScheduling` and
   `TestFinalCaptureCapacityPriorCarrierCanonicalV2KeepsTypedSourceBoundary`.
6. Preserve the expected failure and all fences. Do not mutate/revert the active
   qualification checkout, relabel this causal as a pass, or mix its source
   identity with qualification.

Existing mutation guards now additionally reject missing/duplicate/conditional
new-owner registration; missing, broadened or masked normal/race commands;
changed deadlines/counts; sharing the ordinary budget; and missing/duplicated
actual typed-boundary declarations. The new positive root checks all real codec
siblings remain ordinary except the one exact full-size root. This is a
source/script ownership proof, not a timing assertion.

## Adjacent review and release implications

Reviewed `final_semantic_prior_carrier_decode_v2_test.go`,
`final_semantic_prior_carrier_canonical_v2_test.go`,
`final_semantic_prior_carrier_v2.go`, `final_semantic_prior_carrier_decode_v2.go`,
`evidence_metadata_v2.go`, `evidence_metadata_v2_test.go`,
`evidence_metadata_row_size_v2_test.go`, `evidence_custody_test.go`,
`evidence_public_file_v2_test.go`, and the capture/source-command/scheduling guard
callers. Searched all `final_semantic*test.go` and `evidence*test.go` references to
`maximumCampaignEvidenceRawFileBytes` and `rawFileBytes(...)+1`. Other full-size
metadata/custody controls are in their existing evidence owner; manifest-only
size refusals do not construct the signed 32 MiB originals. The adjacent canonical
typed-path test uses 3 bytes and stays ordinary. The six process-global allocation
controls and two existing stress owners retain their serial guards.

Reviewed actual producer registry/selectors plus `release_gate_jobs_test.go`,
`release_gate_evidence_v2_test.go`, `final_semantic_producer_test.go`,
`scripts/release-gate-jobs.sh`, and `scripts/check-qualification-events.py`.
There is no hard-coded current 37-phase total in those source consumers.
`PRODUCER-PHASES.txt` records 38 release phase jobs (formerly37), with only
`capture-typed-prior` added. Historical 37-phase captures/reports remain unchanged.
The existing evidence own-source guard includes the changed test file and selects
its new root. Aggregate complete normal/race ownership and 90m bounds are unchanged.

No production Go, contract or fixture bytes changed. The producer script is
explicitly hashed by `release_lock.go:680–705`, so final repository integration
must regenerate `repositories.protocol_source_hash`, review the resulting final
lock/plan identity, and use a correctly stamped final CLI. This gate-only source
change does not authorize modifying native history, renewed commitments, custody,
action limits, or any receipt. Root owns that integration once live consumers
close. Completed source-scoped evidence remains reusable; complete producer and
aggregate gates are still required on the final accepted source.
