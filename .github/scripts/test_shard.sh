#!/usr/bin/env bash
# Print the test packages in one shard, or verify and print the partition.
set -euo pipefail
case "${1:-}" in
  --verify) exec python3 .github/scripts/test_census.py --verify ;;
  0|1|2) exec python3 .github/scripts/test_census.py --shard "$1" ;;
  *) echo "usage: test_shard.sh <0|1|2|--verify>" >&2; exit 1 ;;
esac
