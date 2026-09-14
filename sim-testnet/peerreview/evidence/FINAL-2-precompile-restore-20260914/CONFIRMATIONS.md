# Affected confirmation sequence

- Source: `52def6334e72f77a0b2e3b655a37ec2d0df8d3fc`
- Normal binary SHA-256: `3f9e4f49a2c59593a0e73ecb5f20299678385d4c4fee25a745531dc49560f05b`
- Race binary SHA-256: `258268e06724e0bf4641c163f86058c42bc53bb3be83e84a36adb629aaf6d89d`
- Original command-failure event union: 24 parents plus 4 explicit descendants in each mode.
- Confirmation selector: exactly the 24 affected parents; all 28 inherited event identities were checked.

| Pass | Normal | Race |
| --- | --- | --- |
| p1 | 44 parents / 48 events PASS | 44 parents / 48 events PASS |
| p2 | 24 parents / 28 events PASS | 24 parents / 28 events PASS |
| p3 | 24 parents / 28 events PASS | 24 parents / 28 events PASS |

The full verbose matrix is p1. Each p2/p3 entry is a fresh package-CWD process using the same fenced binary/source/dependency inputs. The constructor root `TestPrecompileRestoreHistoryRequiresCompletedRenewal` appears in normal p1, p2, and p3. The sequence closed successfully.

- Normal p2: exit 0; `2026-09-14T12:25:12Z`–`12:25:20Z`; converter stderr empty; binary identity unchanged.
- Normal p3: exit 0; `2026-09-14T12:26:49Z`–`12:26:56Z`; converter stderr empty; binary identity unchanged.
- Race p2: exit 0; `2026-09-14T12:25:12Z`–`12:26:03Z`; converter stderr empty; binary identity unchanged.
- Race p3: exit 0; `2026-09-14T12:26:49Z`–`12:27:37Z`; converter stderr empty; binary identity unchanged.
