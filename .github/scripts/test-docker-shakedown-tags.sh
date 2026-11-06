#!/bin/sh
set -eu
script=$(dirname "$0")/docker-shakedown.sh
check() {
  expected=$1
  tag=$2
  rc=0
  output=$(bash "$script" --validate-version "$tag") || rc=$?
  case "$expected:$rc" in
    accept:0|reject:75) printf '%s (exit %s)\n' "$output" "$rc" ;;
    *) printf 'FAIL: expected %s for %s, got %s (exit %s)\n' "$expected" "$tag" "$output" "$rc" >&2; exit 1 ;;
  esac
}
check accept v2026.11.1-1087168030-sn
check accept v2026.10.30-1086010350
check accept v3.23.0-fix.33.0-rc4
check accept v3.23.0-fix.32.10
check reject v2026.10.6-1064742750-meso
check reject xxv2026.11.1-1087168030-sn
check reject v2026.11.1-1087168030-sn-extra
check reject v3.23.0-fix.33bad
check reject latest
