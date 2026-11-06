#!/bin/bash
# Regression test for stop-provider.sh: a fake provider keeps writing into its
# state dir for a while after SIGTERM. stop-provider.sh must not return until
# the process is gone, so the teardown's rm -rf cannot race those writes.
set -u
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d)
trap 'kill -KILL "$(cat "$tmp/pid" 2>/dev/null)" 2>/dev/null; rm -rf "$tmp"' EXIT
fail=0 pass=0

# start_fake LINGER: writes files nonstop; after SIGTERM keeps writing for
# LINGER seconds before exiting. LINGER=never ignores SIGTERM entirely.
start_fake() {
  rm -rf "$tmp/state"; mkdir -p "$tmp/state"
  STATE="$tmp/state" LINGER=$1 bash -c '
    stop=0; trap "stop=1" TERM; i=0
    while :; do
      if [ "$stop" = 1 ] && [ "$LINGER" != never ]; then
        end=$((SECONDS + LINGER))
        while [ "$SECONDS" -lt "$end" ]; do
          mkdir -p "$STATE/d$i" && : > "$STATE/d$i/f"; i=$((i + 1)); sleep 0.01
        done
        exit 0
      fi
      mkdir -p "$STATE/d$i" && : > "$STATE/d$i/f"; i=$((i + 1)); sleep 0.01
    done' &
  echo $! > "$tmp/pid"
  # Drop it from the job table so the SIGKILL case logs no job notice.
  disown
  sleep 0.3
}

# reap: make sure the fake is dead before the next case starts.
reap() { kill -KILL "$(cat "$tmp/pid")" 2>/dev/null; sleep 0.1; }

check() { # check NAME EXPECTED ACTUAL
  if [ "$2" = "$3" ]; then echo "ok   $1"; pass=$((pass + 1));
  else echo "FAIL $1 (expected $2, got $3)"; fail=$((fail + 1)); fi
}

alive() { kill -0 "$(cat "$tmp/pid")" 2>/dev/null && echo yes || echo no; }

# Provider that keeps writing for 2s after SIGTERM, then exits cleanly.
start_fake 2
sh "$here/stop-provider.sh" "$tmp/pid" 10; check "lingering exit: stop-provider exit code" 0 "$?"
check "lingering exit: process gone when stop-provider returns" no "$(alive)"
if rm -rf "$tmp/state"; then st=0; else st=1; fi
check "lingering exit: rm -rf of state succeeds" 0 "$st"
reap

# Provider that ignores SIGTERM is SIGKILLed after the timeout.
start_fake never
sh "$here/stop-provider.sh" "$tmp/pid" 1 2>/dev/null; check "SIGTERM-deaf: stop-provider exit code" 0 "$?"
check "SIGTERM-deaf: process gone when stop-provider returns" no "$(alive)"
reap

# Missing, empty, stale and invalid PID files are no-ops, not errors.
sh "$here/stop-provider.sh" "$tmp/nope" 1; check "missing PID file" 0 "$?"
: > "$tmp/empty"; sh "$here/stop-provider.sh" "$tmp/empty" 1; check "empty PID file" 0 "$?"
echo 999999999 > "$tmp/stale"; sh "$here/stop-provider.sh" "$tmp/stale" 1; check "stale PID" 0 "$?"
echo bad > "$tmp/bad"; sh "$here/stop-provider.sh" "$tmp/bad" 1 2>/dev/null; check "invalid PID" 0 "$?"

# PID 0 must never be signalled: kill 0 hits the caller's whole process group.
# setsid gives stop-provider.sh a group of its own, so a regression kills only
# that script (exit 143) and never this test.
for zero in 0 00; do
  echo "$zero" > "$tmp/zero"
  setsid -w sh "$here/stop-provider.sh" "$tmp/zero" 1 2>/dev/null; check "PID $zero is ignored" 0 "$?"
done

echo "passed $pass, failed $fail"
[ "$fail" -eq 0 ]
