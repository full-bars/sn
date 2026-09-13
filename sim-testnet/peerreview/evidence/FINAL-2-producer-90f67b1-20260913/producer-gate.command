#!/usr/bin/env bash
set -euo pipefail
cd '/home/by/urnetwork/temp/sn-private-services-qualification-20260913/workspace-physical/sn'
exec env TMPDIR='/home/by/urnetwork/temp/sn-final-gates-b78-prepared-20260913T1216Z/producer/tmp' RELEASE_GATE_CONCURRENT_GATES=2 RELEASE_GATE_JOBS=3 RUN_SERVER_DB_TESTS=1 RELEASE_GATE_DIAGNOSTIC=0 bash '/home/by/urnetwork/temp/sn-private-services-qualification-20260913/workspace-physical/sn/scripts/test-release-1.0-producer-gate.sh'
