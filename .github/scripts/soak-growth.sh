#!/bin/sh
# Snapshot and compare a single Linux provider process. A baseline belongs to
# exactly one process: PID and /proc start time are checked on every sample.
set -eu

if [ "$#" -ne 4 ]; then
  echo "usage: $0 baseline|check PID METRICS_URL BASELINE_FILE" >&2
  exit 2
fi
mode=$1 pid=$2 url=$3 file=$4
case "$pid" in ''|*[!0-9]*) echo 'invalid PID' >&2; exit 2;; esac
case "$mode" in baseline|check) ;; *) echo 'invalid mode' >&2; exit 2;; esac

sample() {
  # Strip the parenthesized comm, which may contain spaces and parentheses.
  stat=$(cat "/proc/$pid/stat")
  rest=${stat##*) }
  # Intentional field splitting of the kernel's fixed-format stat suffix.
  # shellcheck disable=SC2086
  set -- $rest
  shift 19
  start=$1
  rss=$(awk '/^VmRSS:/ {print $2}' "/proc/$pid/status")
  [ -r "/proc/$pid/fd" ] || { echo 'cannot read fd directory' >&2; return 1; }
  fds=0
  for fd in /proc/"$pid"/fd/*; do
    [ "$fd" != "/proc/$pid/fd/*" ] || { echo 'empty fd directory' >&2; return 1; }
    fds=$((fds + 1))
  done
  metrics=$(curl -fsS --max-time 10 "$url")
  goroutines=$(printf '%s\n' "$metrics" | awk '$1 == "urnet_goroutines" {n++; v=$2} END {if (n == 1 && v ~ /^[0-9]+$/ && v > 0) print v; else exit 1}')
  for value in "$start" "$rss" "$fds" "$goroutines"; do
    case "$value" in ''|*[!0-9]*) echo 'invalid or missing resource sample' >&2; return 1;; esac
  done
  [ "$rss" -gt 0 ] && [ "$fds" -gt 0 ] || { echo 'empty resource sample' >&2; return 1; }
  # Recheck identity after the scrape to reject a process that restarted mid-sample.
  stat_after=$(cat "/proc/$pid/stat")
  rest_after=${stat_after##*) }
  # shellcheck disable=SC2086
  set -- $rest_after
  shift 19
  [ "$start" = "$1" ] || { echo 'process changed during sampling' >&2; return 1; }
  printf '%s %s %s %s %s\n' "$pid" "$start" "$rss" "$fds" "$goroutines"
}

current=$(sample)
echo "sample pid start rss_kib fds goroutines: $current"
if [ "$mode" = baseline ]; then
  printf '%s\n' "$current" > "$file"
  exit 0
fi
read -r old_pid old_start old_rss old_fds old_goroutines < "$file"
echo "baseline pid start rss_kib fds goroutines: $old_pid $old_start $old_rss $old_fds $old_goroutines"
# shellcheck disable=SC2086
set -- $current
[ "$1" = "$old_pid" ] && [ "$2" = "$old_start" ] || { echo 'FAIL: provider restarted since baseline' >&2; exit 1; }
failed=0
if [ "$(( $3 * 100 ))" -gt "$(( old_rss * 110 ))" ]; then echo 'FAIL: RSS grew more than 10%' >&2; failed=1; fi
if [ "$4" -gt 1000 ]; then echo 'FAIL: FD count exceeds 1000' >&2; failed=1; fi
if [ "$(( $5 * 100 ))" -gt "$(( old_goroutines * 105 ))" ]; then echo 'FAIL: goroutines grew more than 5%' >&2; failed=1; fi
[ "$failed" -eq 0 ] || exit 1
echo 'PASS: resource growth within bounds'
