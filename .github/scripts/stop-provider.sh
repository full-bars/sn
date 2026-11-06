#!/bin/sh
# Stop the provider named in a PID file and wait until it has exited, so a
# teardown never deletes state the provider is still writing on shutdown.
# SIGTERM first, poll up to TIMEOUT seconds, then SIGKILL and poll again.
# Exits 0 once the process is gone (or was never running), 1 if it survives.
set -u

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ]; then
  echo "usage: $0 PID_FILE [TIMEOUT_SECONDS]" >&2
  exit 2
fi
pidfile=$1 timeout=${2:-30}
case "$timeout" in ''|*[!0-9]*) echo 'invalid timeout' >&2; exit 2;; esac

[ -s "$pidfile" ] || exit 0
pid=$(cat "$pidfile")
case "$pid" in ''|*[!0-9]*) echo "ignoring invalid PID in $pidfile" >&2; exit 0;; esac
# kill 0 signals the caller's whole process group, so a zero PID is invalid.
[ "$pid" -gt 0 ] 2>/dev/null || { echo "ignoring invalid PID in $pidfile" >&2; exit 0; }

# wait_gone SECONDS: poll ten times a second; 0 once the PID is gone.
wait_gone() {
  n=$(($1 * 10))
  while [ "$n" -gt 0 ]; do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.1
    n=$((n - 1))
  done
  ! kill -0 "$pid" 2>/dev/null
}

kill -TERM "$pid" 2>/dev/null || exit 0
wait_gone "$timeout" && exit 0
echo "provider $pid still running after ${timeout}s; sending SIGKILL" >&2
kill -KILL "$pid" 2>/dev/null || exit 0
wait_gone 5 && exit 0
echo "provider $pid survived SIGKILL" >&2
exit 1
