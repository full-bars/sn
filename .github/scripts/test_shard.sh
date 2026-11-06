#!/usr/bin/env bash
# Print the test units in one shard, verify and print the partition, or run
# one shard: its whole packages in one go test, then each name split.
set -euo pipefail
census=.github/scripts/test_census.py
run_shard() {
  local shard="$1" unit package split selector skip status=0
  local -a units=() whole=() splits=() skip_args=()
  mapfile -t units < <(python3 "$census" --shard "$shard")
  ((${#units[@]} > 0))
  skip="$(python3 "$census" --skip "$shard")"
  if [[ -n "$skip" ]]; then
    echo "shard $shard skips quarantined tests: $skip"
    skip_args=(-skip "$skip")
  fi
  for unit in "${units[@]}"; do
    if [[ "$unit" == *" "* ]]; then splits+=("$unit"); else whole+=("$unit"); fi
  done
  if ((${#whole[@]} > 0)); then
    go test -short -count=1 -timeout 600s "${skip_args[@]}" "${whole[@]}" || status=1
  fi
  for unit in "${splits[@]}"; do
    package="${unit% *}" split="${unit#* }"
    echo "== $package split $split"
    if ! selector="$(python3 "$census" --select "$package" "$split")"; then
      status=1
      continue
    fi
    go test -short -count=1 -timeout 600s "${skip_args[@]}" -run "$selector" "$package" || status=1
  done
  return "$status"
}
case "${1:-}" in
  --verify) exec python3 "$census" --verify ;;
  0|1|2) exec python3 "$census" --shard "$1" ;;
  run)
    case "${2:-}" in
      0|1|2) run_shard "$2" ;;
      *) echo "usage: test_shard.sh run <0|1|2>" >&2; exit 1 ;;
    esac
    ;;
  *) echo "usage: test_shard.sh <0|1|2|--verify|run <0|1|2>>" >&2; exit 1 ;;
esac
