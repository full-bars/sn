# Runtime-458 xops qualification

Candidate 446cbdb56e0dc5004b66d7e0cbf05a4d9c49224c remained clean, with the three reviewed xops paths byte-identical before and after every candidate run. The managed interpreter was /home/by/urnetwork/.virtualenv/brien/bin/python3, with PYTHONDONTWRITEBYTECODE=1 and package cwd main/ansible/tests.

full ran the complete existing test_subtensor_playbook module once: 30/30 passed. That includes test_testnet_convergence_pins_deployed_runtime_458 and test_runtime_convergence_evaluates_current_pin_and_identity_controls. Fresh confirmations/p2 and confirmations/p3 each ran those two named methods and passed 2/2, producing three successful processes for both corrected runtime-pin controls.

The isolated causal/old-pin-only/worktree began from the same candidate and changed exactly one assignment in vars.yml: subtensor_expected_spec_version: 458 to 455. Its two-method body exited 1 as expected: test_runtime_convergence_evaluates_current_pin_and_identity_controls failed at the exact current-runtime spec-version assertion, while unchanged test_testnet_pins_network_backend_and_container_dns passed. The candidate checkout stayed clean; the disposable mutant stayed limited to that vars assignment.

No node, RPC, deployment, or full-module causal execution occurred.
