#!/usr/bin/env bash
set -euo pipefail
runner='/home/by/urnetwork/temp/sn-initial-six-qualification-20260913/helper-go-tools-ebe70a/runtime/bin/qualification-ebe70a'; receipt='/home/by/urnetwork/temp/sn-initial-six-qualification-20260913/helper-go-tools-ebe70a/runtime/bin/qualification-ebe70a.sha256'
[[ -x "$runner" && -f "$receipt" ]] || exit 125
sha256sum --check --strict --quiet "$receipt"
exec "$runner" run '/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/plans/guards7-race-p1-v2.json' '/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/captures/guards7-race-p1-v2'
