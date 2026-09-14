# FC908 producer gate — failed complete run

The complete producer ran from 2026-09-14 19:22:34 to 21:28:46 UTC and exited 1.
All 37 phases joined: 36 passed, and only `capture` failed. Its normal body
passed in 155.781 seconds; its race body reached the 10-minute package timeout.
The only named running test was
`TestFinalCaptureCapacityPriorCarrierDecodeV2KeepsTypedLimitsAndIntentSchema`.
The exact failure, passing phases, preflight results and source fences are
retained. This is not final release acceptance.

SN source was `fc9086852125227d4e497f4d54b6590a527032ba`; release-lock YAML
SHA-256 was `76cd7fa7031ee5566301173a94e1a4369ccbc871542de05119b46bbcb1461f35`.
The 13-repository source pair, launcher, command and terminal identity are
included. Initial and final source checks passed. The terminal process census
contains its own awk observer, which is not a gate worker.

The runtime-metadata addendum preserves an actual qualification limitation:
the old checker used a configured public manifest endpoint without retaining a
destination trace. Its route is configured-public/source-inferred. It is not
proof of fresh LAN-only verification and covers only runtimes 451–455.

This portable copy preserves all original bytes. The nested
`capture/SHA256SUMS` is the original manifest and retains absolute source paths;
the top-level `SHA256SUMS` verifies the portable copy using relative paths:

```sh
sha256sum --check --strict SHA256SUMS
```

Root verified all 76 entries in the original manifest before copying. Original
RESULT SHA-256 is
`ca819876867e338dbb831bc5e82295e8ff2686e47b1187ae408362da91227ca4`;
original manifest SHA-256 is
`84aed5429f8dd2afb93f93400d2496543ae8c8e6fb953e2ef83cd1d830d21186`.
The two raw transcript hashes are recorded in RESULT.json.

The successor typed-boundary correction and its focused qualification are
separate evidence. The new live runtime 458 also requires a reviewed source and
artifact update. Neither changes this failed run's verdict or authorizes
restarting its obsolete runtime-455 setup request.
