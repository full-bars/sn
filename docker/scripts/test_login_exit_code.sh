#!/bin/bash
# test_login_exit_code.sh: verifies the login retry loop reports the auth
# command's REAL exit code. `AUTH_OUTPUT=$(...) || true` followed by `$?`
# captured the status of `true`, so every failure logged
# "Authentication failed (exit code: 0)" and the panic line was the only real
# diagnostic left. The fix keeps the exit status of the auth command itself in
# AUTH_EXIT_CODE. The loop is extracted from pelican_panel.sh; start_stable.sh
# and start_nightly.sh carry the identical snippet (port of the same-named
# test in full-bars/urnetwork-3.23-fix).
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() { pass=$((pass+1)); echo "PASS: $1"; }
bad() { fail=$((fail+1)); echo "FAIL: $1"; }

# the fixed snippet must be present in every script that carries a login loop
for f in pelican_panel.sh start_stable.sh start_nightly.sh; do
    if grep -q '|| true$' "$REPO/docker/scripts/$f" 2>/dev/null \
       && grep -q 'AUTH_OUTPUT=.* || true' "$REPO/docker/scripts/$f"; then
        bad "$f still pins the auth exit code to 0 (|| true)"
    else
        ok "$f captures the auth exit status"
    fi
done

extract_func() {
    awk -v fn="$2" '
        $0 ~ "^"fn"\\(\\) *\\{" { grab=1 }
        grab { print }
        grab && /^}/ { exit }
    ' "$1"
}

common="$REPO/docker/scripts/pelican_panel.sh"

# make_stub <exit-code> <message> <also-write-jwt: yes|no>
# A stub binary whose auth subcommand behaves like the real one: fails with a
# nonzero status (bad credentials, or a binary that cannot even exec on this
# platform) or succeeds and writes the JWT. Behavior rides on STUB_* files
# exported to the stub because the real invocation is
# `auth --user_auth=... --password=... -f`, so the subcommand is never a
# plain positional argument; it must be found by scanning the args.
make_stub() {
    printf '%s\n' "$1" > "$tmp/stub_exit"
    printf '%s\n' "$2" > "$tmp/stub_msg"
    printf '%s\n' "$3" > "$tmp/stub_jwt"
    cat > "$tmp/urnetwork_amd64_stable" <<STUB_EOF
#!/bin/sh
for a in "\$@"; do
    case "\$a" in
        auth) STUB_AUTH=1 ;;
    esac
done
[ "\${STUB_AUTH:-}" ] || exit 0
cat "\$STUB_MSG_FILE"
[ "\$(cat "\$STUB_WRITE_JWT")" = "yes" ] && printf 'jwt-token' > "\$JWT_FILE"
exit "\$(cat "\$STUB_EXIT_FILE")"
STUB_EOF
    chmod +x "$tmp/urnetwork_amd64_stable"
}

# run_login <out-dir> [timeout-seconds]
# Builds the runner around the REAL func_do_login with the same stubs the
# other docker harnesses use (log -> file, sleep -> no-op) and prints the
# runner's exit status.
run_login() {
    local out="$1" tmo="${2:-10}"
    mkdir -p "$out"
    {
        echo 'set -u'
        echo "APP_DIR='$tmp'"
        echo "A_SYS_ARCH='amd64'"
        echo "USER_AUTH='test-user'"
        echo "PASSWORD='test-pass'"
        echo "JWT_FILE='$out/jwt'"
        echo "export JWT_FILE"
        echo "STUB_MSG_FILE='$tmp/stub_msg'"
        echo "STUB_EXIT_FILE='$tmp/stub_exit'"
        echo "STUB_WRITE_JWT='$tmp/stub_jwt'"
        echo "export STUB_MSG_FILE STUB_EXIT_FILE STUB_WRITE_JWT"
        echo "DEBUG=false"
        echo "log() { echo \"\$*\" >> '$out/login.log'; }"
        echo "sleep() { :; }"
        # func_do_login now starts its retry loop with the stopping check and
        # sleeps through func_sleep; the runner needs the stop-flag state and
        # both helpers before the login-loop extraction.
        echo "PROVIDER_STOPPING=0"
        echo "PROVIDER_CHILD_PID=\"\""
        extract_func "$common" func_exit_if_stopping
        extract_func "$common" func_sleep
        extract_func "$common" func_do_login
        echo 'func_do_login'
        echo 'echo "rc=$?"'
    } > "$tmp/runner.sh"
    timeout "$tmo" bash "$tmp/runner.sh" > "$out/runner.out" 2>&1
}

# 1. Failure path: the logged exit code is the auth command's own status.
make_stub 3 "panic: runtime error: bad credentials" no
out="$tmp/fail"
run_login "$out" 3
rc=$?
[ "$rc" -eq 124 ] && ok "failure loop keeps retrying (killed by timeout)" \
    || bad "failure loop ended early (rc=$rc); it must retry until stopped"
grep -q "Authentication failed (exit code: 3)" "$out/login.log" \
    && ok "failure logs the real exit code (3)" \
    || bad "failure does not log the real exit code"
if grep -q "exit code: 0" "$out/login.log"; then
    bad "failure still logs exit code 0"
else
    ok "failure never logs a fake exit code 0"
fi
grep -qi "PANIC: RUNTIME ERROR" "$out/login.log" \
    && ok "panic line is uppercased into the failure log" \
    || bad "panic line missing from the failure log"

# 2. Success path: JWT written and verified, loop returns 0.
make_stub 0 "Jwt written to $tmp/ok/jwt" yes
out="$tmp/ok"
run_login "$out" 10
rc=$?
[ "$rc" -eq 0 ] && ok "success path returns 0" || bad "success path returned $rc"
grep -q "rc=0" "$out/runner.out" && ok "func_do_login exits 0 on success" \
    || bad "func_do_login did not exit 0: $(cat "$out/runner.out")"
grep -q "Authentication successful" "$out/login.log" \
    && ok "success logs the JWT confirmation" \
    || bad "success confirmation missing"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
