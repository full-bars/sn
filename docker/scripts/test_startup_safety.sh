#!/bin/bash
# Offline startup tests: extract the actual functions, redirect /app/proxy.txt
# to a fixture, and stub the provider, curl and sleeps. No Docker or network.
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)" || exit 1
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/app" "$tmp/bin" "$tmp/functions"

# Retain the function bodies; only redirect the absolute mounted file path.
python3 - "$HERE" "$tmp/functions" <<'PY'
import pathlib, re, sys
source_dir, dest = map(pathlib.Path, sys.argv[1:])
for name in ('start_jwt', 'start_stable', 'start_nightly', 'pelican_panel'):
    source = (source_dir / (name + '.sh')).read_text()
    functions = ['func_check_proxy', 'func_ip_checker'] if name != 'pelican_panel' else []
    if name != 'start_jwt':
        functions.append('func_do_login')
    result = []
    for func in functions:
        match = re.search(r'^' + func + r'\(\) \{.*?^\}\n', source, re.M | re.S)
        if not match:
            raise SystemExit('missing function ' + func + ' in ' + name)
        result.append(match.group().replace('"/app/proxy.txt"', '"$TEST_APP/proxy.txt"'))
    (dest / (name + '.sh')).write_text('\n'.join(result))
PY
if [ "$?" -ne 0 ]; then echo 'FAIL: extraction'; exit 1; fi

cat > "$tmp/app/provider-stub" <<'SH'
#!/bin/sh
printf '%s\n' "$@" > "$ARGV_LOG"
case "$1" in
  proxy)
    if [ "$PROXY_FAIL" = yes ]; then
      printf '%s\n' partial > "$URNETWORK_STATE_DIR/proxy"
      exit 17
    fi
    jq '.servers = {"new:1080":""}' "$URNETWORK_STATE_DIR/proxy" > "$URNETWORK_STATE_DIR/new" || exit 1
    mv "$URNETWORK_STATE_DIR/new" "$URNETWORK_STATE_DIR/proxy"
    ;;
  auth)
    mkdir -p "${JWT_FILE%/*}"
    printf '%s\n' jwt > "$JWT_FILE"
    printf '%s\n' 'Jwt written to test file' "echoed $USER_AUTH $PASSWORD"
    ;;
  *) exit 1 ;;
esac
SH
chmod +x "$tmp/app/provider-stub"
cp "$tmp/app/provider-stub" "$tmp/app/urnetwork_test_stable"
cp "$tmp/app/provider-stub" "$tmp/app/urnetwork_test_nightly"
cat > "$tmp/bin/curl" <<'SH'
#!/bin/sh
printf '%s\n' called > "$CURL_LOG"
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then output=$2; shift 2; else shift; fi
done
case "$CURL_MODE" in
  fetch-fail) printf '%s\n' 'printf executed > "$CURL_LOG.executed"' > "$output"; exit 22 ;;
  script-fail) printf '%s\n' 'exit 9' > "$output" ;;
  success) printf '%s\n' 'exit 0' > "$output" ;;
  *) exit 1 ;;
esac
SH
chmod +x "$tmp/bin/curl"

cat > "$tmp/driver" <<'SH'
#!/bin/sh
set -e
. "$FUNCTIONS"
log() { printf '%s\n' "$*"; }
func_sleep() { :; }
func_exit_if_stopping() { :; }
APP_DIR="$TEST_APP"
A_SYS_ARCH=test
JWT_FILE="$HOME/.urnetwork/jwt"
export JWT_FILE
ENABLE_IP_CHECKER=true
IP_CHECKER_URL=https://invalid.example/check
case "$ACTION" in
  proxy) func_check_proxy ;;
  ip) func_ip_checker ;;
  login) func_do_login ;;
esac
SH

pass=0
fail=0
report() {
  if "$@"; then
    pass=$((pass + 1)); printf 'PASS: %s\n' "$case_name"
  else
    fail=$((fail + 1)); printf 'FAIL: %s (exit %s)\n' "$case_name" "$rc"
    if [ -f "$out" ]; then rg -n '.' "$out" || :; fi
  fi
}
run() {
  env PATH="$tmp/bin:$PATH" HOME="$home" TEST_APP="$tmp/app" FUNCTIONS="$tmp/functions/$mode.sh" \
    ACTION="$action" PROXY_FAIL="${proxy_fail:-no}" ARGV_LOG="$argv" CURL_MODE="${curl_mode:-fetch-fail}" \
    CURL_LOG="$curl_log" DEBUG=true USER_AUTH='fake-user-123' PASSWORD='fake-password-456' \
    URNETWORK_STATE_DIR="${state_override:-}" sh "$tmp/driver" > "$out" 2>&1
  rc=$?
}
proxy_kept() { [ "$rc" -eq 0 ] && cmp -s "$state/proxy" "$original" && rg -q 'kept existing proxy configuration' "$out" && [ ! -e "$argv" ]; }
proxy_none() { [ "$rc" -eq 0 ] && [ ! -e "$state/proxy" ] && rg -q 'none configured' "$out" && [ ! -e "$argv" ]; }
proxy_failure() { [ "$rc" -ne 0 ] && cmp -s "$state/proxy" "$original" && rg -q 'kept existing configuration' "$out" && rg -qx 'proxy' "$argv" && rg -qx 'add' "$argv"; }
proxy_success() { [ "$rc" -eq 0 ] && rg -q 'configuration replaced' "$out" && jq -e '.servers == {"new:1080":""} and .auths.keep.user == "alice"' "$state/proxy" >/dev/null && [ "$(stat -c %a "$state/proxy")" = 600 ]; }
fetch_failure() { [ "$rc" -eq 0 ] && [ -f "$curl_log" ] && [ ! -e "$curl_log.executed" ] && rg -q 'IP checker fetch failed' "$out" && ! rg -q 'ran successfully|script failed' "$out"; }
script_failure() { [ "$rc" -eq 0 ] && rg -q 'IP checker script failed' "$out" && ! rg -q 'ran successfully|fetch failed' "$out"; }
script_success() { [ "$rc" -eq 0 ] && rg -q 'IP checker script ran successfully' "$out" && ! rg -q 'fetch failed|script failed' "$out"; }
login_safe() { [ "$rc" -eq 0 ] && ! rg -Fq 'fake-user-123' "$out" && ! rg -Fq 'fake-password-456' "$out" && rg -qx 'DEBUG: USER_AUTH set=yes length=13' "$out" && rg -qx 'DEBUG: PASSWORD set=yes length=17' "$out" && rg -qx 'DEBUG: AUTH_EXIT_CODE=0' "$out" && rg -qx 'DEBUG: AUTH_OUTPUT=\[redacted\]' "$out"; }

for mode in start_jwt start_stable start_nightly; do
  home="$tmp/$mode/home"; state="$home/.urnetwork"; state_override=; mkdir -p "$state"
  original="$tmp/$mode/original"; out="$tmp/$mode/out"; argv="$tmp/$mode/argv"; curl_log="$tmp/$mode/curl-log"
  printf '%s\n' '{"auths":{"keep":{"user":"alice","password":"secret"}},"servers":{"old:1080":"keep"}}' > "$original"
  cp "$original" "$state/proxy"
  rm -f "$tmp/app/proxy.txt" "$argv"
  action=proxy; proxy_fail=no; run; case_name="$mode keeps config with no mount"; report proxy_kept
  rm -f "$state/proxy" "$argv"
  run; case_name="$mode reports no config with no mount"; report proxy_none
  cp "$original" "$state/proxy"; printf '%s\n' 'new:1080' > "$tmp/app/proxy.txt"
  proxy_fail=yes; run; case_name="$mode keeps config when add partially writes then fails"; report proxy_failure
  proxy_fail=no; run; case_name="$mode replaces config and retains named auth"; report proxy_success
  state="$tmp/$mode/override"; state_override="$state"; mkdir -p "$state"; cp "$original" "$state/proxy"
  proxy_fail=yes; run; case_name="$mode keeps overridden state on add failure"; report proxy_failure
  action=ip; curl_mode=fetch-fail; rm -f "$curl_log"; run; case_name="$mode reports fetch failure"; report fetch_failure
  curl_mode=script-fail; run; case_name="$mode reports script failure"; report script_failure
  curl_mode=success; run; case_name="$mode reports script success"; report script_success
done
for mode in start_stable start_nightly pelican_panel; do
  home="$tmp/login-$mode/home"; state="$tmp/login-$mode/state"; mkdir -p "$home/.urnetwork" "$state"
  out="$tmp/login-$mode/out"; argv="$tmp/login-$mode/argv"; curl_log="$tmp/login-$mode/curl-log"
  action=login; run; case_name="$mode DEBUG hides credentials even in auth output"; report login_safe
done
printf 'Results: %s passed, %s failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
