# Moving-head approval causal closure

Status: **CLOSED — expected causal regression reproduced**. This is a
four-root deterministic normal-mode causal on parent source
`0a94b7d0b2362fe6bdff798be49c3f9eb3013bfa`, not a release-gate result.

The disposable causal worktree applied only
`sim-testnet/precompile_probe_successor.go` (one deletion and one addition):
`FinalizedHead: anchor` was restored to `FinalizedHead: head`. The supplied
patch SHA-256 is `14f0747efe4acfc11f07d9b5810264cd84bf6ce45f8cf5f9bd157b2fc06ee394`; the actual applied
diff SHA-256 is `62638ef81940ec87ee2d8dfdd80430aa34226123da8730296675ef13f4c1491b`.
The parent head, source manifest, exact 12-dependency projection, and the
intentional causal diff were unchanged through list and body completion.

Normal compile closed 0 at `2026-09-14T15:06:08Z`, producing
`sim-testnet.causal.normal.test` SHA-256
`3cf7f2a8b7f29eea3b29a6198bcb2ac2b1d253782b40b46e9bcc8e611875a66e`.
The binary list ran from `2026-09-14T15:06:42Z` to
`2026-09-14T15:06:48Z`, closed 0, and exactly matched all four
selector roots. The body ran from `2026-09-14T15:07:08Z` to
`2026-09-14T15:07:19Z`; its body and capture owner both returned
1, as required for this causal.

Exactly one root failed:
`TestPrecompileProbeApprovalSurvivesAdvancingFinality`, with the required
literal `advancing finality changed preview/apply approval` and a real plan
hash mismatch. The remaining three roots passed:
`RejectsChangedSignedAnchor`, `HashBindsAnchor`, and
`RechecksCurrentCustody`. The test2json converter stderr was empty, all 25
events parsed, the binary was unchanged across list/body, and the package CWD
fixture guard covered `testnet.yml` and `../deploy/testnet/policy-v1.yml`.

The raw events, commands, source identities, list admission, outcome fence,
and process closure are retained in this capture. No RPC, native action, source
publication, or product-source modification outside the disposable causal
worktree occurred.
