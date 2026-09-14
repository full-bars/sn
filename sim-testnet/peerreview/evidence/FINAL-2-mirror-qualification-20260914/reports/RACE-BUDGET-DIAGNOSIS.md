# Expanded race selector exhausted its shared package budget

The 402 race body failed at the unchanged ten-minute package alarm. The
evidence supports a qualification-budget composition error, not a newly
introduced production or metadata-test defect. No source change, formatter,
build, test, RPC, or event replay was performed during this diagnosis.

## Original evidence

Current capture:
`<restricted-capture>/sn-mirror-replay-correction-20260914/terra-runtime/capture-mirror-fixture-20260914T095212Z/race`.
Its body ran 09:58:15–10:08:18 UTC and exited 2; converter, tee, source and binary
fences exited 0. The later cleanup check records no test binary or timeout
wrapper remaining. The earlier progress snapshot was incomplete and must not
be used as the final census.

The final raw log contains 87 root starts and 86 raw PASS lines. The converted
events contain 85 terminal root PASS events, no assertion FAIL events and two
unresolved roots:

- `TestProducerGateCaptureSelectionRejectsMetadataPartitionDrift` was actually
  still running; the alarm reports only 1m29s of execution for this root.
- `TestProducerGateCaptureSelectionRejectsPopulationPartitionDrift` has a raw
  PASS at 61.39s, but its terminal event was not flushed before the panic.

Use **85 event-terminal passes and two unresolved roots** as the conservative
scope. The body remains a timeout failure. All nine historical alias/codec
roots and both renewal-oracle roots are among those 85 explicit passes.

## Why raw and events differ

The installed Go converter's `cmd/internal/test2json/test2json.go:325–330`
buffers a PASS report in `c.report`, flushing the previous report when it sees
the next framing directive. With `-test.v=test2json`, unmarked panic text is
plain output (`:208–210`) and stays attributed to the previous `testName`.
`Converter.Close` (`:381`) flushes input and output but does not flush pending
reports. PopulationPartitionDrift was the last raw PASS before the unframed
timeout. Its raw line is retained in events as output, followed by panic output
under that name, but there is no terminal pass event. The panic itself names
MetadataPartitionDrift as the running root. This explains the mismatch without
inventing a terminal result, changing the converter, or editing old events.

## Measured comparison

Both runs used GOMAXPROCS=4, parallel=4, count=1, ten-minute test limits and a
660-second outer timeout. The ff62 comparison is
`<restricted-capture>/sn-capture-census-correction-20260914/terra-runtime/capture-enumeration-20260914T090512Z/race`.
It passed all 24 selected roots in 161 seconds (09:13:25–09:16:06 UTC).

| Root or phase | ff62 race24 | 402 race87 |
| --- | ---: | ---: |
| Serial passes before first parallel continuation | 7.01s, 4 roots | 511.36s, 71 roots |
| MetadataPartitionDrift | PASS 120.10s | interrupted after 89s |
| PopulationPartitionDrift | PASS 76.55s | raw PASS 61.39s; event unresolved |
| SourceCensusDrift | PASS 79.57s | PASS 68.10s |
| RequiresIndependentPopulation | PASS 10.70s | PASS 10.09s |
| RejectsMetadataRaceBudgetLeak | PASS 14.95s | PASS 13.67s |

The preceding 43cc race21 run also passed MetadataPartitionDrift in 115.89s.
Normal c50 and 402 executions of that root passed in 6.78s and 6.19s. No
peer metadata root shows a new slowdown. The expanded selector's heavy serial
renewal/revision roots include 166.80s, 82.56s, 81.81s and 74.33s executions.
They are deliberately separate owners in the full producer script, but were
combined into this focused 87-root process.

After 511.36s of serial work, a 600s package deadline cannot provide the
previously measured 115.89–120.10s for the unchanged final metadata root. The
timeout stack is runnable regexp evaluation inside the bounded mutation loop,
not a blocked channel, mutex, child process or network request.

The exact metadata test file and script it reads are byte-identical ff62→402:

- `sim-testnet/release_gate_capture_metadata_test.go` sha256
  `5ec278097a43387a548fdbe2fbdecd93929af0ca3813456322b29c7a3108b991`.
- `scripts/test-release-1.0-producer-gate.sh` sha256
  `50fc81a5ed6d866ad146e8c7a739a193308dd9cde974a4e9d6e1c70ce5c17034`.

MetadataPartitionDrift reads only that script and uses unchanged constants and
helpers in that test file. Its workload does not enumerate the newly added
fixture source. The repeated regexp work is expensive under race but already
measured and unchanged. There is no present evidence requiring a code rewrite.

## Minimal completion and limits

Retain the current source and already-built race binary. After parent admission,
Terra should run three sequential fresh processes with the exact selector:

```text
^TestProducerGateCaptureSelectionRejects(MetadataPartitionDrift|PopulationPartitionDrift)$
```

Keep GOMAXPROCS=4, count=1, parallel=4, the ten-minute test deadline and
660-second outer limit. Bind every process to the original source, dependencies
and binary and retain raw output, actual exits and cleanup. Do not increase a
timeout or rerun the full slow serial prefix. Any failure or missing result
returns to diagnosis and resets that root's race streak. The two roots need
race confirmation; the already-completed six alias normal streaks remain
their scoped evidence.

`FINALIZE.md:11–12` explicitly permits reuse within verified source/evidence
scope. `sim-testnet/README.md:395–400` allows development qualification in
measured exact-membership shards with unchanged limits. These support retaining
the 85 attributable completed results and finishing the two unresolved roots.
That is a record of scoped results; it does not turn the old interrupted body
into a successful whole-package command or a release-gate certificate.

If a complete successful selected-union certificate is required, run disjoint
72-root runtime and 15-root capture partitions on the same existing binary,
with the same limits and a separately proved compiled union of all 87 names.
Runtime is the original selector minus `ProducerGateCaptureSelection`; capture
is exactly `^TestProducerGateCaptureSelection`. Measured runtime serial work
was 504.60s; capture has its own full ten minutes. No assertion or root is
dropped. The missing-root three-pass obligation remains separate, although a
successful partition execution can count as its first pass.

Both complete release gates and final acceptance remain outstanding. Decisions
about the already-approved native repair belong to the parent using the
completed runtime scope and its actual source/lock/executable admission.
