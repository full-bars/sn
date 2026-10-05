The `runtime-historical-capture` worker builds a complete historical replay job
from a retained read-only parent trie. `capture_historical_on_backend` borrows
an archive node's `state.as_trie_backend()` and uses the pinned SDK's
`prove_execution_on_trie_backend`. It never applies the resulting overlay.
The standalone worker accepts a retained directory of raw Blake2-256-addressed
trie nodes as an interchange format. It is not a RocksDB or ParityDB parser.

The public Go command owns the exact executable and request pins, a single
60–900 second deadline (300 seconds by default), and joined bounded process
pipes:

```
sn-mainnet capture-historical-execution \
  --engine /private/runtime-historical-capture \
  --engine-sha256 sha256:EXACT_EXECUTABLE_DIGEST \
  --request /private/request.json \
  --request-sha256 sha256:EXACT_REQUEST_DIGEST \
  --nodes /private/parent-trie-nodes
```

The request uses schema `urnetwork-historical-execution-capture-v1`. It pins
the SCALE parent and child headers and their hashes, the complete SCALE
extrinsic body, both original runtime-code digests, execution state version,
and an optional original-Wasm observation profile. The runtime code comes
from the parent state. The collector retains code/heap proof paths, executes
the whole original block, materializes write paths, and then invokes the
separate strict proof replay. A partial `state_getReadProof` response is never
accepted merely because its supplied keys were read successfully.

The report retains `job_json` as the exact JSON string whose digest the strict
replay verified. Extract that string without re-encoding its contents to use
it with `verify-historical-execution`. The report also carries the request
digest, actual backend read counts, and the complete replay result. Only a
complete successful capture and replay can emit the envelope. Cancellation,
missing nodes, changed identities, unimplemented hosts and any resource
refusal emit no proof fact.

Raw nodes are read through a retained directory descriptor. Each node must be
a unique regular file named by the lowercase Blake2-256 of its bytes; payload
length and metadata are checked before and after a bounded read. Node count,
individual size, retained proof bytes, cumulative backend I/O, input/output
bytes and Wasm work remain separate finite bounds. A partial or oversized
capture must be retried from the original request with a separately reviewed
resource change; it must not skip state or fabricate zero fees.

This capture does not authorize a runtime, source profile or finalized block.
`runtime_admitted`, `native_fee_withdrawal_refund_observed` and
`production_selection` remain false; the top-level native debit remains null.
Original-callsite observations are candidates, including the distinction
between an absent refund and an actually observed zero. Actual chain use
still requires a retained node backend or authenticated raw-node export,
correspondence to the original deployed code and complete fee callsites,
and independent parent/child finality admission. A prior runtime review,
including v470, grants no authority to observed runtime472. The current
deterministic fixtures are synthetic and do not assert that those external
admissions have happened.
