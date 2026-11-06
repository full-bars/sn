#!/bin/sh
# Run targeted changes and compare all three complete proxy sets after each.
set -eu
prefix=${1:-urfix}
fixtures=${2:-/tmp/multi}
tool=${URNET_DOCKER:-/tmp/urnet-docker}
check=$(dirname "$0")/assert-docker-proxy-state.sh
expected=$(mktemp -d)
trap 'rm -rf "$expected"' EXIT HUP INT TERM
for mode in turbo auto lowmem; do : > "$expected/$mode"; done
check_all() {
  for mode in turbo auto lowmem; do
    sh "$check" "$prefix-$mode" "$expected/$mode"
  done
}
check_all
for mode in turbo auto lowmem; do
  "$tool" proxy add --unit "$prefix-$mode" "$fixtures/$mode.txt"
  cp "$fixtures/$mode.txt" "$expected/$mode"
  check_all
done
for mode in turbo lowmem; do
  "$tool" proxy clear --unit "$prefix-$mode" -y
  : > "$expected/$mode"
  check_all
done
