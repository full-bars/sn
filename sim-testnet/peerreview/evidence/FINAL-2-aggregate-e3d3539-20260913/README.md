# Original aggregate gate: failed

This bundle preserves the completed 2026-09-13 aggregate on SN `e3d3539`.
The actual outer exit is 1, from 19:37:21 to 23:14:29 UTC. All 23 native
phases joined: 20 exited 0 and three exited 1. This is not acceptance of
that release or evidence that the next candidate passed.

The failed phases are `sn-simulator-race` (cumulative 90-minute package
timeout), `server-unit` (missing migration-monitor entries), and `server-db`
(the full 1,000-client registration cohort deadline). Their original logs
are retained without editing. Later fixes and their scoped confirmations
must be reported separately; they do not turn this failed gate into a pass.

All initial preflights exited 0. The final source check separately refused
because canonical SN origin/main had advanced independently from `e3d3539`
to `928b7d5`. The empty final source inventory is an incomplete final
attestation, not proof that all local worktrees changed. The original
refusal and outer diff remain in the raw capture.

`MANIFEST.json` records copied byte hashes, original locators, sizes and
capture modes. `phase-joins.tsv` and `source/identity.json` summarize the
retained native records. Hash checks establish integrity, not a test rerun.
The bundle excludes binaries, caches, source contents, vault contents,
service environments and runtime data. Original absolute paths are evidence
locators from this host, not instructions to rerun the failed command.
