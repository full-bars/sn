#!/bin/bash
# test_miner_stop.sh: verifies that the miner supervisor forwards SIGTERM and
# exits once the miner has stopped, instead of ignoring `docker stop` until
# Docker SIGKILLs the container (the shell runs as PID 1, where the kernel
# applies no default signal action). The real helpers are extracted from
# start_miner.sh; the miner is a stub that records the signal it received.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

extract_func() {
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\) *\\{" { grab=1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}

src="$REPO/docker/miner/start_miner.sh"

{
    echo '#!/bin/sh'
    echo 'set -eu'
    echo 'APP_DIR=/app'
    echo 'MINER_STOPPING=0'
    echo 'MINER_CHILD_PID=""'
    echo 'log() { echo "LOG: $*" >&2; }'
    for fn in func_forward_stop func_install_stop_trap func_exit_if_stopping func_sleep func_run_miner; do
        extract_func "$src" "$fn"
    done
    cat <<'EOF'
func_install_stop_trap
if func_run_miner "$STUB"; then code=0; else code=$?; fi
if [ "$MINER_STOPPING" -eq 1 ]; then
    echo "stopped code=$code" >>"$OUT/supervisor.log"
    exit 0
fi
echo "exit code=$code" >>"$OUT/supervisor.log"
EOF
} >"$tmp/harness.sh"

cat >"$tmp/stub.sh" <<'EOF'
#!/bin/sh
trap 'echo TERM >"$OUT/stub.signal"; sleep 1; exit 0' TERM
echo $$ >"$OUT/stub.pid"
while :; do sleep 1; done
EOF

cat >"$tmp/crash_stub.sh" <<'EOF'
#!/bin/sh
echo $$ >"$OUT/stub.pid"
exit 5
EOF

chmod +x "$tmp/harness.sh" "$tmp/stub.sh" "$tmp/crash_stub.sh"

wait_file() {
    local f="$1" i=0
    while [ ! -s "$f" ] && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
    [ -s "$f" ]
}

# 1. SIGTERM to the supervisor reaches the miner and the supervisor exits 0.
out="$tmp/local"; mkdir -p "$out"
OUT="$out" STUB="$tmp/stub.sh" sh "$tmp/harness.sh" >"$out/stdout" 2>&1 &
sup=$!
if wait_file "$out/stub.pid"; then
    kill -TERM "$sup"
    i=0
    while kill -0 "$sup" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
    if kill -0 "$sup" 2>/dev/null; then
        bad "supervisor still running 5s after SIGTERM"
        kill -KILL "$sup" 2>/dev/null
    else
        wait "$sup"; rc=$?
        [ "$rc" -eq 0 ] && ok "supervisor exits 0 after SIGTERM" || bad "supervisor exit code $rc, want 0"
    fi
    [ -s "$out/stub.signal" ] && ok "miner received SIGTERM" || bad "miner never received SIGTERM"
    if kill -0 "$(cat "$out/stub.pid")" 2>/dev/null; then
        bad "miner still running after the supervisor exited"
        kill -KILL "$(cat "$out/stub.pid")" 2>/dev/null
    else
        ok "miner stopped"
    fi
else
    bad "stub miner never started"
    kill -KILL "$sup" 2>/dev/null
fi

# 2. set -e parity: a miner crash must not abort the supervisor, and the real
# exit code must survive the background/wait plumbing.
out="$tmp/crash"; mkdir -p "$out"
OUT="$out" STUB="$tmp/crash_stub.sh" sh "$tmp/harness.sh" >"$out/stdout" 2>&1
grep -q "exit code=5" "$out/supervisor.log" \
    && ok "crash exit code survives as 5" \
    || bad "crash exit code not reported as 5"

# 3. The supervisor sleeps are interruptible: a stop during a long wait ends the
# process promptly instead of holding PID 1 for the whole sleep.
{
    echo '#!/bin/sh'
    echo 'set -eu'
    echo 'MINER_STOPPING=0'
    echo 'MINER_CHILD_PID=""'
    echo 'log() { echo "LOG: $*" >&2; }'
    for fn in func_forward_stop func_install_stop_trap func_exit_if_stopping func_sleep; do
        extract_func "$src" "$fn"
    done
    echo 'func_install_stop_trap'
    echo 'func_sleep 30'
    echo 'echo "slept" >>"$OUT/sleep.log"'
} >"$tmp/sleep_harness.sh"
out="$tmp/sleep"; mkdir -p "$out"
t0=$(date +%s)
OUT="$out" sh "$tmp/sleep_harness.sh" >"$out/stdout" 2>&1 &
sup=$!
sleep 0.5
kill -TERM "$sup" 2>/dev/null
i=0
while kill -0 "$sup" 2>/dev/null && [ "$i" -lt 50 ]; do sleep 0.1; i=$((i+1)); done
elapsed=$(( $(date +%s) - t0 ))
if kill -0 "$sup" 2>/dev/null; then
    bad "interruptible sleep did not end on SIGTERM"
    kill -KILL "$sup" 2>/dev/null
else
    [ "$elapsed" -lt 5 ] && ok "stop during a long sleep ends in ${elapsed}s" || bad "stop during sleep took ${elapsed}s"
    [ -s "$out/sleep.log" ] && bad "sleep returned to the caller after a stop" || ok "sleep exits instead of returning after a stop"
fi

echo "pass=$pass fail=$fail"
[ "$fail" -eq 0 ]
