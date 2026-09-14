# Historical commitment-consumption scope refusal

Native setup on clean SN `abae9a6` ran from 10:15:46 to 10:32:15 UTC on
2026-09-14 and exited 1 before repair submission. It completed the batched
1,000 historical checks and reached the 950/3,456 carried-action progress
marker. `fleet.commitment.34` then failed because generation 1 was required
to remain the exact current finalized commitment after renewal.

The native historical commitment checks succeeded. The source-plan replay
executor narrowed its consumer search to the original plan's ancestry. Its
batch consumer failed in that original plan but the same intent subsequently
finalized and verified in an approved descendant. The narrowed lookup hid that
valid completion. The adjacent retained-journal/source review covers 802
relationships across all 202 fleets; 17 original commitments (fleets 34–40 and
91–100) have this same scope mismatch. Every one has the exact consumer intent
verified in the active plan's approved ancestry. This is artifact/source
analysis, not a fresh independent chain verification of all those consumers.

`native/RESULT.json` retains the exact before/after state hashes. Native setup
adopted the reviewed F634 approval and changed plan/config inputs. Every action
and the approved plan hash match the saved preview; generated time and live
observations differ. The transaction journal, both supervisor files and public
identities are unchanged. No repair transaction was submitted. The native
process joined; no replacement writer is implied by these records.

This bundle records a real failure and its diagnosed scope. It does not claim
that the subsequent production fix, deterministic regressions, full gates or
live campaign have passed. Original private plan bytes, secrets and signed
transactions are excluded. `MANIFEST.json` maps every raw retained record to
its source and SHA-256; `SHA256SUMS` seals the complete bundle.
