#!/usr/bin/env bash
set -euo pipefail
umask 077
runtime='/home/by/urnetwork/temp/sn-warp-admission-final-gates-prepared-20260913T1837Z/producer'
: "${FINAL_WORKSPACE:?FINAL_WORKSPACE is required}" "${SOURCE_PAIR:?SOURCE_PAIR is required}" "${EXPECTED_SN:?EXPECTED_SN is required}"
[[ "$EXPECTED_SN" =~ ^[0-9a-f]{40}$ && -d "$FINAL_WORKSPACE/sn/.git" && -f "$SOURCE_PAIR" && ! -L "$SOURCE_PAIR" && "$(stat -c '%a' "$SOURCE_PAIR")" == 600 ]] || exit 125
[[ "$(wc -l < "$SOURCE_PAIR")" == 13 && "$(git -C "$FINAL_WORKSPACE/sn" rev-parse HEAD)" == "$EXPECTED_SN" ]] || exit 125
cd "$FINAL_WORKSPACE/sn"
exec env TMPDIR="$runtime/tmp" RELEASE_GATE_CONCURRENT_GATES=2 RELEASE_GATE_JOBS=3 RUN_SERVER_DB_TESTS=1 RELEASE_GATE_DIAGNOSTIC=0 bash "$FINAL_WORKSPACE/sn/scripts/test-release-1.0-producer-gate.sh"
