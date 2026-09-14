#!/usr/bin/env bash
set -euo pipefail
cd /home/by/urnetwork/temp/xops-runtime458-pin-20260914/terra-runtime/runtime458-pin-qualification-20260914T222647Z/causal/old-pin-only/worktree/main/ansible/tests
exec env PYTHONDONTWRITEBYTECODE=1 /home/by/urnetwork/.virtualenv/brien/bin/python3 -m unittest -v test_subtensor_playbook.SubtensorPlaybookTests.test_runtime_convergence_evaluates_current_pin_and_identity_controls test_subtensor_playbook.SubtensorPlaybookTests.test_testnet_pins_network_backend_and_container_dns
