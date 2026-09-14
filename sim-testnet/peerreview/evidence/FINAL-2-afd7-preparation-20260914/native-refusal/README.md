# Closed native refusal — separate from the preparation-success chain

This directory retains the authorized public-safe closed refusal record. It is deliberately separate from the successful bootstrap, lock, publication, final-CLI, and setup-preview records.

- Closed exit: `1`
- Started: `2026-09-14T11:20:02.123874+00:00`
- Finished: `2026-09-14T11:41:35.465689+00:00`
- Failure layer: the historical preflight rejected a restored generation-2 commitment because it was not the current finalized state.
- The closure records no reserve transfer or new campaign acceptance.
- It records zero repair-journal entries, a non-live native PID at closure, unchanged journal/supervisor/public-identity state, and local plan/config admission changes only.

Included source-derived public records: `RESULT.json`, `CLOSURE.json`, and `stderr`. Excluded: the request, all private plan/config contents, keys, signed receipt material, journals, raw identity data, and live captures.
