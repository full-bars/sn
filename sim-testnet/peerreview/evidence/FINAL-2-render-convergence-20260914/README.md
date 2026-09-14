# Report 2: strict render convergence and superseded gate attempts

This bundle contains closed preparation evidence from 14 September 2026.
It establishes the affected render qualification and preserves the failed,
refused and canceled attempts. It does not establish successful reserve repair,
rendering of the real deployment, renewal, traffic or full final acceptance.

The native setup attempt at 05:22 UTC found that both retained operator staging
files still enabled provisional discovery and retained contexts. Their original
render receipt could not establish the currently approved strict outputs.
`source/CAUSAL-DIFF.json` records the exact field and hash differences.

Commit `cf4eae3d70a83277494048e8d7cc44ea099b5c2d` advances the existing render
format to `strict-reserved-owned-rpc-v2`, binds its intent to the resolved-input
hash, and routes reserved native discovery through the existing loopback proxy
`ws://127.0.0.10:19946`. That proxy forwards to `192.168.1.162:9944` without
pacing or public fallback. This preserves the server's TLS-or-literal-loopback
requirement. The first candidate, `1469b14`, incorrectly used direct plain
WebSocket access to the LAN address and failed that requirement; the original
normal output and patch remain in `focused/` and `source/`.

Astra authored the correction and regressions. Terra executed the same seven
affected roots normally and with race detection on the clean corrected source.
Both passed, with actual test-binary durations of 290.949 and 473.434 seconds.
The scope and source/binary identities are recorded in
`focused/affected-qualification.result`, with complete terminal outputs.
The separately invoked named normal root first failed immediately because its
test binary was started from the module root instead of the package directory.
On the same retained binary, it then passed three consecutive executions from
the correct directory, in 268.11, 174.35 and 175.96 seconds. The original failure
is retained and remains a failure. These focused results are not full gates.

The published release-lock commit was
`e31236859c1012c21aed08034cf5074d85bdb9a6`. Its final executable build and exact
13-repository observations are in `final-cli-e312/`. The capture retained the
older `RESULT-BOOTSTRAP-CLI-BUILD.json` filename; the actual revision and
artifact inside identify the final e312 executable. Native setup preview
passed at 06:19:08 UTC, preserving the four observed state files and changing
only the local render action among all 2,309 actions. The approved 6,000-alpha
repair and financial limits stayed unchanged.

An initial apply refused its source check after an operational cache probe
added 138 lines to the pinned Connect `go.sum`. No action ran. Root retained
the changed bytes and diff, restored only that file to its exact pinned bytes,
and confirmed all 13 repository heads and clean states. The restoration receipt,
accidental and pinned files, and exact diff are retained in
`cache-probe-source-restoration/`. Two producer attempts also refused before
any test body because the private cache lacked pinned modules; their actual
outer captures are in `prebody-refusals/`. Subsequent commands used the populated
global module cache with module downloads disabled. The actual apply retry
adopted the reviewed plan, then exited 1 at 06:41:20 UTC on the old fleet refresh
postcondition, before the pending repair or fresh render ran. That separate
failure led to the renewal recovery documented in the oracle preparation bundle.

The complete e312 producer and aggregate attempts were deliberately canceled
when the oracle correction superseded their source. Their actual terminal
results at 07:00:33 UTC are **CANCELED, not PASS**:

| Gate | Admitted and joined owners | Exit 0 before cancellation | Exit 143 during cleanup | Outer exit |
| --- | ---: | ---: | ---: | ---: |
| Producer | 13 | 10 | 3 | 143 |
| Aggregate | 5 | 3 | 2 | 143 |

`canceled-producer/` and `canceled-aggregate/` contain the commands, source
observations, complete outer captures, individual phase logs, preflight output
and original cancellation records. Both passed their five initial preflights.
No recorded job leader remained at the 07:02:05 UTC cleanup observation, and no
private service directory had been created. No later pass changes these
partial attempts into complete gate passes. Final source `a59294e9` has its own
separate producer and aggregate executions.

Native plan JSON, executables, caches, mutable test roots and custody material
are omitted. Raw included files are copied unchanged; paths inside them identify
the original capture. Verify the portable bytes with:

```sh
sha256sum -c SHA256SUMS
```
