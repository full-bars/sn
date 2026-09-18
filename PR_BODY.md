## What

Syncs `.github/scripts/vt-scan.py` with the fixed version (mirrors meso-miner `#95`): adds the `VT_JSON_FILE` machine-readable export — one JSON object per scanned file — and fails the scan when the JSON export cannot be written.

## Why

`release.yml` sets `VT_JSON_FILE` and the "Stage WDSI submissions" step consumes `vt-scan-results.json`, but the script never wrote it, so WDSI staging always skipped ("No VT scan results — skipping WDSI staging") and flagged files never got a staged Defender-submission bundle.
