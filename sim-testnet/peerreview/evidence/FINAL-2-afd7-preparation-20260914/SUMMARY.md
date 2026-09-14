# FINAL-2 AFD7 preparation evidence

This external, public-safe bundle records the closed preparation chain from the `84b6` bootstrap through release-lock preview/apply, `afd7` publication, the final stamped CLI build, and the read-only native setup preview.

| Stage | Closed result | Bound identity |
| --- | --- | --- |
| Bootstrap CLI | outer/build exit `0` | source `84b6ca681eaffb5b103a8600e0fd056d26ea92ba`; CLI `30c5205a57fe1d22580acc6ee2dd42cab2932e962f7684a72bddc6f47cb8094a` |
| Release-lock preview | exit `0` | reviewed source-hash field only; candidate `ddcd53ff09be38290c88f06a658abcad3b7d4fc660cdd3f6392d5786a0a8fed9` |
| Release-lock apply | exit `0` | installed exact reviewed candidate; only lock dirty |
| Publication | exit `0` | source `afd7b26c9c1b2847e8f73648e6a6eac13928453e`; parent `84b6ca681eaffb5b103a8600e0fd056d26ea92ba` |
| Final CLI | outer/build exit `0` | CLI `dc516d630ace555889e7c0705381ff0ed1d305824f45ad18ddf8d825ed5b46d4`; pair `47b08d8d42383a9b2a4d3def6f21ba22b4fef6a8d431de54e13d158e7e751c2e` |
| Native setup preview | exit `0` | source `afd7b26c9c1b2847e8f73648e6a6eac13928453e`; observed state unchanged |
| Closed native refusal (separate) | exit `1` | historical preflight refusal; no reserve transfer or new campaign acceptance claimed |

See [closed exits](checks/closed-exits.tsv), [source/CLI identities](identity/cli-and-source.tsv), [chain join](JOIN.md), the two sanitized reviews, and the separate [closed refusal record](native-refusal/README.md).

## Proof limits

Except for the expressly authorized `native-refusal/stderr` record, this bundle contains no raw commands, logs, stdout/stderr, private plans, configuration, keys, signed receipt material, journals, live-state captures, binary artifacts, caches, or raw identity files. The closed refusal is retained only in its separately labeled, public-safe directory; its request and private plan material remain excluded. It does not claim final full-gate acceptance or native repair completion.
