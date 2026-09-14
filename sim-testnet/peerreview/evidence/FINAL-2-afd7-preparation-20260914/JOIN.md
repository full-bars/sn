# Closed preparation chain

1. The temporary bootstrap CLI closed successfully on source `84b6ca681eaffb5b103a8600e0fd056d26ea92ba`.
2. The closed release-lock preview and apply records bind the reviewed source-hash change to candidate `ddcd53ff09be38290c88f06a658abcad3b7d4fc660cdd3f6392d5786a0a8fed9`.
3. Publication closed successfully at `afd7b26c9c1b2847e8f73648e6a6eac13928453e`, whose parent is `84b6ca681eaffb5b103a8600e0fd056d26ea92ba`, with the reviewed source digest `sha256:834f494db190a1f05bda556752c26dc17062217696088382a8b5386b2c3f7875`.
4. The final stamped CLI closed successfully on `afd7b26c9c1b2847e8f73648e6a6eac13928453e` with SHA-256 `dc516d630ace555889e7c0705381ff0ed1d305824f45ad18ddf8d825ed5b46d4` and source-pair digest `47b08d8d42383a9b2a4d3def6f21ba22b4fef6a8d431de54e13d158e7e751c2e`.
5. The reviewed setup preview closed successfully on `afd7b26c9c1b2847e8f73648e6a6eac13928453e` with observed state unchanged.

The chain is limited to closed preparation evidence. A later closed native refusal is retained separately in `native-refusal/`; it is not a successful-chain stage. Neither record set certifies a live repair, submission, native transaction, or final full-gate outcome.
