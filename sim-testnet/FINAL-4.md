# Sim-testnet finalization report 4 — R48

**R48 is terminal and did not pass final acceptance.** The signed release attempt
`20260926T202718.915754659Z-release-1.0` reached its acceptance boundary at
2026-09-26 22:43:42 UTC, but stopped at 22:48:54 UTC when its process-log
heartbeat classified three findings as release-blocking. It recorded no fully
observed acceptance epoch. The owner exited with status 1; no R49 has been
started. This is the final testnet attempt as directed. The exact
[signed attempt](peerreview/evidence/FINAL-4-R48/attempt.evidence.json),
[terminal result](peerreview/evidence/FINAL-4-R48/result.json), and
[local readback](peerreview/evidence/FINAL-4-R48/terminal_local.json) are
preserved for review.

The attempt ran on testnet chain **945**, genesis
`0x8f9cf856bf558a14440e75569c9e58594757048d7b3a84b5d25f6bd978263105`,
netuid **521**, using the owned LAN RPC `192.168.1.162:9944`. The chain
evidence below was queried from that endpoint after block 8,093,380 was
finalized; it does not rely on the simulator's own contract-state reader.
The independent [capture script](peerreview/evidence/FINAL-4-R48/capture_chain.py)
and its [raw RPC responses, decoded events, block headers and successful
receipts](peerreview/evidence/FINAL-4-R48/onchain.json) are committed together.
The capture file's SHA-256 is
`bc63597fa21eb8a45409a8b5a2a4c0685b44220a15045e5fc2dc2eb8195b09d4`.

| Stage | Result | Evidence and limit |
| --- | --- | --- |
| R48 preparation | Completed under the same signed generation 48 and run ID | [Attempt](peerreview/evidence/FINAL-4-R48/attempt.evidence.json); prior generation 47 is linked by hash. |
| Historical operator usage repair | 4,183 exact epoch-658 contracts quarantined with zero credit; 3,082,728 reported bytes retained as debt | [Manifests](peerreview/evidence/FINAL-4-R48/operator-1-manifest.json), [operator 2](peerreview/evidence/FINAL-4-R48/operator-2-manifest.json), [guarded transaction receipts](peerreview/evidence/FINAL-4-R48/live-repair-receipt.json). This is an explicit `final_acceptance=false` exception. |
| Epoch 658 close | Both taskworkers closed locally, 43 and 27 leaves; both on-chain roots missed their commit window | `RootMissed` and `OperatorEpochFinalized(rootPresent=false)` for both NOs at finalized block **8,093,315**, with transaction hashes in [onchain.json](peerreview/evidence/FINAL-4-R48/onchain.json). No value was carried: both event values are zero alpha-rao. |
| Readiness and boundary | New signed source-658 artifacts observed; the low-usage exception allowed the provisional boundary | [Baseline observation](peerreview/evidence/FINAL-4-R48/baseline-observation.json), pinned at block **8,093,328**, hash `0x9dc99854fbdd929b3df3f27d8fbb42c52f7aaf031af17f48ade76c40d687b730`. The rate check remained `ready=false`. |
| Release execution | Interrupted before the first complete epoch | [Result](peerreview/evidence/FINAL-4-R48/result.json): first required epoch 660, five 300-block epochs, terminal block **8,095,024**; terminal observation remained at epoch 659/block 8,093,328. |
| Final acceptance | **Failed** | Six assertions were evaluated: one passed, five failed. Forty faults remained pending; two pre-armed filters were recorded active at the stop. The post-exit readback found both filter files and the active-fault ledger absent, with all 33 supervised processes healthy; that physical cleanup does not turn the failed assertions into passes. |

The decisive failure was the process-log gate. Its
[cursor and finding ledger](peerreview/evidence/FINAL-4-R48/process-logs.json)
shows `operator-1-api/stderr` and `operator-2-api/stderr` `log-overrun`, plus
`validator-1/stdout` `release-steering-attempt-failure`. The API overruns were
already recorded at 20:44 UTC, before acceptance, and were observed again at
22:47 UTC. Both API stderr cursors remained at their old offsets
(1,825,550,978 and 1,373,126,166 bytes); the scanner's 64 MiB per-scan cap
rejects a larger unread delta without advancing either cursor. That makes a
large but readable backlog a terminal release error. Validator 1 also kept
reporting that strict native-history adoption missed its approved first native
epoch **1696**; the sampled line says `subnet epoch 1698 attempt 1`. That is a
separate known history exception, not evidence of a chain reorganization.
The terminal `scenario_context` assertion states the immediate stop condition:
`heartbeat process log gate: process log gate found 3 release-blocking
class(es)`. The [anomaly ledger](peerreview/evidence/FINAL-4-R48/anomalies.json)
contains 54 open entries and 42 derived consequences of the interrupted fault
schedule. Its derived “not exercised” entries must not be counted as 42
independent defects.

The bounded LAN `eth_call` reader also logged a 30-second timeout during the
pre-boundary snapshot and retried with a smaller batch. The owner reached the
boundary afterward, so that timeout was transient and **not** the terminal
cause. The parallel public-evidence census completed successfully. The owner
and supervisor retained their process identities through the operator repair;
only the two taskworkers were restarted. The isolated server fix is commit
`74893863e9df0af3abaaa19f1db26742cf61092c`, compiled into clean
Git-stamped binary SHA-256
`3e032e95695972af1dfae2306f5d8e4ab0a89cc4b4a0cb42daa97da8e5623154`.
Its deterministic pre-fix regression failed as intended; five focused tests
passed normally and with race detection, and the adjacent contract-usage
suite passed. [Qualification evidence](peerreview/evidence/FINAL-4-R48/qualification-final.json)
records the commands and hashes. The two live serializable repairs then
committed 3,151 and 1,032 exact rows; both post-write readbacks found zero
remaining epoch-658 null usage snapshots, zero credited repair bytes, and
unchanged bilateral report minima. The [review record](peerreview/evidence/FINAL-4-R48/repair-review.json)
distinguishes these local database facts from on-chain facts.

The source-658 artifacts contained **1,705,655** and **1,448,032** credited
usage bytes for NOs 1 and 2, with content hashes
`sha256:23e5d2ce1c40be2ef7837c33e7f0a5a06cfa43046d3c37fc5f67197a71f935a0`
and
`sha256:0c122251d7f7bcc782c2b9b65cf8b45c96aadd57649efeab32133852c803100c`.
These are signed operator-source observations, **not** the 3,082,728 legacy
bytes quarantined by the repair. All six rate-tier equivalents remained below
the 200,000 TAO-rao twice-native-minimum target. The provisional record names
the unmatched older on-chain payout source (epoch 654) and explicitly sets
`final_acceptance=false`. The boundary therefore establishes that the actual
campaign launched under a disclosed exception; it does not establish economic
readiness.

The independent LAN event capture gives a narrow on-chain chronology:

| Finalized block | Chain event | Transaction evidence |
| --- | --- | --- |
| 8,093,077 | `EmissionCaptured(658, NO 1/2, 0)` | `0xa7a5bbbc1bed1769b1e15f51283f891e97055fc2f5480b4cec494681441d6a99`, `0x0b0d76cabc599097ae0d3c4d93a2f89aeee241783d6c24751ac22ec668db2c4d` |
| 8,093,315 | `RootMissed(658, NO 1/2, carried=0)` and `OperatorEpochFinalized(rootPresent=false)` | `0xd0721a441721f61005653b79ff868f44c6c59c8e14f7c8c60e21b92e60640905`, `0x11fa97e53d8a6f5a147c71bb6595f2a9685ca6941091c0af0b4baac5352b1a4f` |
| 8,093,328 | Owner's pinned acceptance baseline | Block hash above, independently reproduced by `eth_getBlockByNumber`. |
| 8,093,377 | `EmissionCaptured(659, NO 1/2, 0)` | `0x01dc644426826e964a13ba1e3b64262629011d4cbeef9ba5599c3c005e48aa22`, `0xfc5ba55ee08ee53e85a891d2a733983247fd2372b1ee63202620fbe712402b9e` |
| 8,093,380 | Both epoch-659 `OperatorRootCommitted` events | `0xc2595cd4fcc396a27d2b4c52cc805e5e109594b9353924b4f024806297eae31c`, `0xbf3d7cf27943cd8edd880ae9d6a3edc72bd98c30672c074740714d22fe8c2908` |

Those later root transactions show the operators continued making real testnet
transactions after the repair. They do **not** substitute for five observed
release epochs or resolve the native-history and process-log failures. All 12
captured transaction receipts succeeded on-chain; the raw logs and receipt
block hashes are in `onchain.json`. No claim is made about later blocks beyond
this fixed capture window. The signed result reports cumulative historical
capture of 2,022,131,361,988 alpha-rao, paid claims of 106,058,636,825
alpha-rao, and accounted escrow liability of 1,916,072,725,163 alpha-rao;
these are owner-reported cumulative figures at its pinned baseline, not new
R48 acceptance-window payments.

For reproduction from an archive-capable LAN RPC, run
`python3 sim-testnet/peerreview/evidence/FINAL-4-R48/capture_chain.py` from the
SN repository root. It derives event topics from the checked-in ABI with the
peer reviewer's independent Keccak implementation, checks chain ID/genesis,
requires finalized block 8,093,380, verifies the boundary block hash, and
fetches the fixed contract logs and their receipts. `SN_RPC_URL` may override
the LAN endpoint for a second archive-node comparison. The JSON capture is
regenerated with a new `captured_at` and current finalized head, so those
two metadata values can change while the fixed historical blocks, events and
receipts should reproduce exactly. The copied local files are byte-identical
to their runtime originals; [terminal_local.json](peerreview/evidence/FINAL-4-R48/terminal_local.json)
records their hashes. Operator usage, process logs, fault timing and the signed
attempt are local/off-chain evidence and cannot be independently established
from EVM receipts alone.

R48 leaves three production hardening gates before mainnet: stream or
incrementally checkpoint noisy process logs without turning a bounded scan
into a permanent unreadable backlog; resolve the missed native-1696 adoption
with an explicit signed forward plan; and keep quarantined historical usage
debt visible and non-crediting while the normal immutable close path remains
complete. These are tracked in [PRELAUNCH-FIXES.md](../mainnet/PRELAUNCH-FIXES.md).
This report records the final testnet result; it does not claim mainnet launch
readiness or silently turn a partial run into a pass.
