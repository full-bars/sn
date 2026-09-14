#!/usr/bin/env bash
set -euo pipefail
cd /home/by/urnetwork/temp/xops-runtime458-pin-20260914/xops/main/ansible/tests
exec env PYTHONDONTWRITEBYTECODE=1 /home/by/urnetwork/.virtualenv/brien/bin/python3 -m unittest -v test_subtensor_playbook
