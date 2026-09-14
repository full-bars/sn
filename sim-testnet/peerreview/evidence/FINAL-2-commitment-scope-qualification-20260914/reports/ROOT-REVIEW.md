# Production correction review

The defect comes from narrowing a historical executor to the original plan and
then using that plan's ancestry to decide whether a generation was consumed.
The retained audit identifies 17 affected original commitments across 802
relationships in 202 fleets; all exact consumers completed in approved later
plans. The native historical commitment checks and original receipt hashes
already passed before the EVM current-generation refusal.

The reviewed production delta is confined to Executor's private scope field,
consumedFleetCommitmentGeneration's fallback, and the authenticated completed-
renewal historical-source constructor. The fallback is consulted only if the
original plan cannot see its consumer, and only for the constructor's exact
fleet set. It requires approval of the source plan and deployment, the exact
consumer action in both plans, equal original intent, a verified journal entry
in the approved ancestry, and authenticated persisted postcondition bytes.
Accepted prior intent aliases are cleared on a local Action copy. The original
source plan, source receipt, action hash, and native/canonical replay checks
remain intact. Both operational and independent executor copies retain the
same immutable admitted scope.

The new deterministic roots cover original failed consumers later completed
in approved descendants for install batches, refresh batches and challenger
mirrors; absent, unapproved, altered or incomplete consumers; receipt tampering;
intent aliases; and the actual completed-renewal constructor-to-consumer route.
That integration refuses each incomplete renewal successor and verifies exact
source observation/hash preservation. Adjacent family selection covers mirror,
member binding, batches and lifecycle, and excludes the current renewal itself.
Fixtures use synthetic identities and local persisted records, with no sleeps
or external node. Positive case variations are plain loops.

One pre-freeze fixture adjustment was requested: keep the earlier exact
consumer followed by a later aliased consumer in monotonically ordered journal
sequences instead of prepending a sequence-4 record before sequence 1. This is
a test-fixture correction; no further production changes were requested.

This is source review, not executed qualification. Terra owns formatting,
normal/race/causal execution and the required confirmations. The final source
and production digest must be recorded after that handoff; no old full gate
has been relabeled as qualification of this correction.
