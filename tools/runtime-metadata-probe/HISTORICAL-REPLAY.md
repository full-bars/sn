# Historical proof replay backend

`runtime-historical-proof --historical-proof-replay-v1` reads one bounded JSON
job from stdin and writes a report only after complete execution and post-state
comparison. The process owner must impose a finite total deadline (300 seconds
by default), bounded stdout/stderr capture, and kill/join on cancellation. This
worker does not yet have a production monitor caller.

The job binds canonical SCALE parent/child headers, their exact hashes, the
complete extrinsic vector, exact parent runtime code, and raw `StorageProof`
nodes. Parent linkage, child number, extrinsics root, proved `:code`, proved
`:heappages`, executing state version, and reproduced child state root are
separate checks. Consensus seals are retained in the original header identity
and removed only from the runtime execution input. No seal/finality verifier or
source-to-code admission is supplied by this backend.

The executor and trie implementation are pinned to SDK
`cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a`. A strict adapter runs fallible
`delta_trie_root` and `child_delta_trie_root` before the SDK root writer. This is
necessary because the pinned `trie_backend_essence.rs` writer logs an incomplete
trie error and can return the old root. Root equality alone would therefore not
prove that all required write paths were present. Missing reads, child roots,
child nodes and write paths refuse; authenticated absence remains distinct.

Current resource limits are 96 MiB JSON, 8192 distinct proof nodes / 24 MiB total
decoded proof, 8 MiB runtime code before / 32 MiB after decompression, 8 MiB block
body, 16384 extrinsics, 64 KiB headers, 64 digest items, 64 MiB Wasm memory,
65536 metered host/argument/iterator steps and 64 MiB cumulative metered I/O.
The v2 host profile retains the original `storage_calls` and `storage_io_bytes`
wire fields for these stricter cumulative totals. Rolled-back work
still counts. A bounded original job is never retried with a skipped state or a
replacement root when a limit is reached.

The `substrate-proof-bounded-hosts-v2` profile supports bounded top/child
reads, writes, individual and prefix deletion, iteration, append, roots and
balanced transactions, plus allocator, logging, hash and trie helpers. Prefix
and child-kill calls delegate the exact SDK overlay/backend limit semantics:
zero limits still clear overlay entries and repeated limited calls do not
invent cursor progress. Every backend iterator step shares the job's meter;
incomplete iterator creation or traversal traps instead of inheriting the
SDK bulk-delete helper's logged-error/partial-success result.

A filtered host registry reuses exact pinned SDK ed25519 verification,
sr25519 verification v1/v2, and secp256k1 recovery/compressed recovery v1/v2.
Its static and dynamic dispatch both charge work and bound memory reads before
argument allocation. No key generation, signing or keystore functions are
registered. Proof-size observation follows the exact SDK no-recorder sentinel
`u64::MAX`; serialized proof length is never substituted for dynamic usage.
Other imports trap if invoked. BLS host calls, offchain I/O, runtime spawning
and runtime-version extension semantics remain outside this host profile.
The synthetic controls are not evidence that a production runtime is admitted
or that its complete invoked host surface is supported.

The retained official v470 artifact's read-only structural census found
original named fee-handler functions and imports for this host increment.
Names alone do not establish callsite meanings, a source build match or
authority for currently observed spec472. The original compressed code and
every function body remain unchanged by the host registry.

Every successful report states `anchor_authority=caller-supplied-unapproved`,
`runtime_admitted=false`, `production_selection=false`,
`native_fee_withdrawal_refund_observed=false`, and `native_fee_debit=null`.
Historical fee verification still requires independently admitted original
code and a fee-specific execution observation mechanism proving actual payer
withdrawal and actual refund, including zero, partial and failed refunds.
Receipt gas, generic same-phase balance events and balance deltas do not supply
that witness. Full historical replay and MG03 remain open beyond this milestone.

The deterministic tests construct complete parent state using SDK
`TestExternalities`, retain every raw top/child/value node, and independently
construct the expected child map/root. They exercise the public JSON decoder,
real Wasm storage hosts, missing read/write/child paths, unchanged-root fallback,
body/header/code substitution, unsupported hosts, transactions and bounds.
They do not use an actual admitted Subtensor runtime or claim fee attribution.

An optional `observation_profile` now binds the exact original code digest and
function-body digests/ranges. The registry wrapper reads the real Wasmtime
function-relative call stack at host entry; it inserts no runtime instructions.
Before executing, it proves that every code body remains byte-identical through
the pinned SDK's memory normalization. The job owns its trace, transaction
stack and work limits. Rollback discards effects while retaining cumulative
work; incomplete execution or changed post-state emits no trace.

The trace permits at most 32 nonoverlapping rules, 64 stack frames, 4096 retained
records and 2 MiB cumulative encoded records. Labels such as `fee-withdraw` are
supplied review references, not proof that a function implements that role.
`hook_observations.authority` therefore remains
`caller-supplied-unapproved-callsite-profile`; all native fee fields stay
unknown. Complete refund branch coverage, an admitted original-code callsite
map and finality still need to be joined before this can establish an actual fee.
The new controls exercise original-code frames, rollback, ambiguous nested
labels, range/body substitution, cumulative limits and post-state refusal.

When `observation_profile.metadata_sha256` is present, the executor calls
`Metadata_metadata` on the same original Wasm using stateless hosts. It requires
that exact digest, canonical metadata v14/v15, unique pallet/event indices,
32-byte payer and native u64 amount layouts. A caller cannot replace metadata
with a supplied decoder table. The bounded decoder retains exact event bytes,
`ApplyExtrinsic` placement and `Ethereum.Executed` transaction identity from the
selected original callsite trace. Its current payer mapping is the reviewed
Subtensor `blake2_256("evm:" || H160)` form; admitting another runtime or mapping
requires its own explicit implementation and review.

`hook_observations.fee_events` reports candidate withdrawal/refund pairs with
authority `original-runtime-metadata-and-unapproved-callsite-profile`. An actual
zero Deposit differs from an absent refund; missing withdrawal/refund or
initialization/finalization placement leaves debit unknown. Late unmatched
events remain counted and retained, and conflicting payer, amount width,
duplicate transaction/effect, ordering, topic grammar or refund-over-withdrawal
refuses attribution. Unlabelled same-phase balance events are not selected as
gas. These facts still do not prove a complete failed-refund branch or establish
cryptographic source review/finality. The top-level native fee authority remains
false/null, including when a candidate pair is present.
