#!/bin/bash
# Offline entrypoint tests: redirect the staged copy's /app exec paths to
# stub startup scripts. The state-dir cases also run staged startup functions
# with a stub provider binary and no network, sleeps, or background services.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/app"

# The entrypoint resolves its script directory from APP_DIR, which the image
# sets to /app; staging is a plain copy with APP_DIR pointed at the sandbox.
cp "$HERE/entrypoint.sh" "$tmp/entrypoint.sh"
for name in start_stable start_jwt start_nightly pelican_panel; do
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "${0##*/}" "${URNETWORK_AUTH_CODE:-}" "${URNETWORK_STATE_DIR:-}" "$@" > "$STUB_LOG"' > "$tmp/app/$name.sh"
  chmod +x "$tmp/app/$name.sh"
done

pass=0
fail=0
run_case() {
  local name="$1"
  shift
  mkdir -p "$tmp/$name/home/.urnetwork"
  log="$tmp/$name/out"
  dispatch="$tmp/$name/dispatch"
  env -i PATH="$PATH" HOME="$tmp/$name/home" APP_DIR="$tmp/app" STUB_LOG="$dispatch" \
    "$@" sh "$tmp/entrypoint.sh" > "$log" 2>&1
  rc=$?
}
run_positional() {
  local name="$1" code="$2"
  shift 2
  mkdir -p "$tmp/$name/home/.urnetwork"
  log="$tmp/$name/out"
  dispatch="$tmp/$name/dispatch"
  env -i PATH="$PATH" HOME="$tmp/$name/home" APP_DIR="$tmp/app" STUB_LOG="$dispatch" \
    "$@" sh "$tmp/entrypoint.sh" "$code" > "$log" 2>&1
  rc=$?
}
check() { # status (ok|error), dispatched script (or none), required and forbidden regex
  local status="$1" target="$2" required="$3" forbidden="$4"
  if [ "$status" = ok ]; then [ "$rc" -eq 0 ] || return 1
  else [ "$rc" -ne 0 ] || return 1; fi
  if [ "$target" = none ]; then [ ! -e "$dispatch" ] || return 1
  else [ -f "$dispatch" ] && rg -qx "$target.sh" "$dispatch" || return 1; fi
  [ -z "$required" ] || rg -q "$required" "$log" || return 1
  [ -z "$forbidden" ] || ! rg -q "$forbidden" "$log" || return 1
}
t() {
  local name="$1"
  shift
  if check "$@"; then
    pass=$((pass+1)); printf 'PASS: %s\n' "$name"
  else
    fail=$((fail+1)); printf 'FAIL: %s (exit %s)\n' "$name" "$rc"
    if [ -f "$log" ]; then rg -n '.' "$log" || :; fi
    if [ -f "$dispatch" ]; then rg -n '.' "$dispatch" || :; fi
  fi
}

run_case authcode AUTH=authcode URNETWORK_AUTH_CODE=fake-code
t 'AUTH=authcode selects jwt with environment code' ok start_jwt 'Authentication requested via environment variable' ''
run_case alias AUTH=authcode AUTHCODE=fake-alias
t 'AUTHCODE alias reaches jwt startup' ok start_jwt 'Authentication requested via environment variable' ''
if [ -f "$dispatch" ] && rg -qx 'fake-alias' "$dispatch"; then pass=$((pass+1)); echo 'PASS: AUTHCODE exported to started script'; else fail=$((fail+1)); echo 'FAIL: AUTHCODE not exported'; fi
run_case both_missing AUTH=userpass
t 'missing both userpass variables are named' error none 'requires USER_AUTH and PASSWORD' ''
run_case password_missing AUTH=userpass USER_AUTH=fake-user
t 'missing PASSWORD is named' error none 'requires PASSWORD to be set' ''
run_case user_missing AUTH=userpass PASSWORD=fake-password
t 'missing USER_AUTH is named' error none 'requires USER_AUTH to be set' ''
run_case userpass AUTH=UsErPaSs USER_AUTH=fake-user PASSWORD=fake-password
t 'case-insensitive AUTH=userpass selects stable' ok start_stable '' 'deprecated|Conflicting'
run_case bogus AUTH=bogus
t 'invalid AUTH lists valid values' error none 'Valid options are: userpass, authcode' ''
run_case invalid_nightly AUTH=nightly
t 'AUTH=nightly explains BUILD=nightly' error none 'nightly is available only via BUILD=nightly' ''
run_case build_jwt BUILD=jwt URNETWORK_AUTH_CODE=fake-code
t 'legacy BUILD=jwt works and warns' ok start_jwt 'BUILD is deprecated.*AUTH=userpass' ''
run_case build_stable BUILD=stable URNETWORK_AUTH_CODE=fake-code
t 'legacy BUILD=stable overrides an environment code without new credential checks' ok start_stable 'BUILD is deprecated' ''
run_case invalid_build BUILD=bogus
t 'invalid legacy BUILD is still rejected' error none 'Invalid build: bogus' ''
run_case invalid_auth_with_build AUTH=bogus BUILD=stable
t 'invalid AUTH is rejected even with BUILD set' error none 'Valid options are: userpass, authcode' ''
run_case conflict AUTH=authcode BUILD=stable URNETWORK_AUTH_CODE=fake-code
t 'BUILD=stable wins conflict without deprecation nag' ok start_stable "Conflicting AUTH='authcode' and BUILD='stable'; BUILD takes precedence" 'deprecated'
run_case reverse_conflict AUTH=userpass BUILD=jwt URNETWORK_AUTH_CODE=fake-code
t 'BUILD=jwt wins conflict without userpass credentials' ok start_jwt "Conflicting AUTH='userpass' and BUILD='jwt'; BUILD takes precedence" 'deprecated'
run_case agree AUTH=AuthCode BUILD=JWT URNETWORK_AUTH_CODE=fake-code
t 'agreeing case-insensitive selectors are quiet' ok start_jwt 'Authentication requested' 'deprecated|Conflicting'
run_case nightly AUTH=userpass BUILD=nightly
t 'BUILD=nightly wins conflict' ok start_nightly 'BUILD takes precedence' 'deprecated'
run_case legacy_nightly BUILD=nightly
t 'legacy nightly stays selectable' ok start_nightly 'BUILD is deprecated' ''
run_case auto URNETWORK_AUTH_CODE=fake-code
t 'unset selectors auto-select jwt from environment' ok start_jwt 'Authentication requested' 'deprecated|Conflicting'
run_positional auto_pos fake-positional
t 'unset selectors auto-select jwt from positional code' ok start_jwt 'Entrypoint received 1 arguments' 'deprecated|Conflicting'
run_positional auth_pos fake-positional AUTH=AUTHCODE
t 'AUTH=authcode accepts positional code case-insensitively' ok start_jwt 'Entrypoint received 1 arguments' ''
run_case no_code AUTH=authcode
t 'AUTH=authcode without code or session fails clearly' error none 'requires AUTHCODE.*stored session' ''
mkdir -p "$tmp/session/home/.urnetwork"
printf '%s\n' stored-fake-session > "$tmp/session/home/.urnetwork/jwt"
run_case session AUTH=authcode
t 'AUTH=authcode accepts a stored session' ok start_jwt 'Existing session found' ''
run_case default
t 'unset selectors without code still select stable' ok start_stable '' 'deprecated|Conflicting'
run_case empty AUTH= BUILD= URNETWORK_AUTH_CODE=fake-code
t 'empty selectors retain jwt auto-selection' ok start_jwt 'Authentication requested' 'deprecated|Conflicting'
run_case pelican PELICAN=yes
t 'legacy Pelican routing remains intact' ok pelican_panel 'Running Pelican' ''


echo '--- state directory routing and provider startup ---'
mkdir -p "$tmp/override-session/state"
printf '%s\n' fake-session > "$tmp/override-session/state/jwt"
run_case override-session AUTH=authcode URNETWORK_STATE_DIR="$tmp/override-session/state"
t 'entrypoint finds an override session' ok start_jwt 'Existing session found' 'JWT file not written'
if rg -Fxq "$tmp/override-session/state" "$dispatch" && rg -Fq "State directory: $tmp/override-session/state" "$log"; then
  pass=$((pass+1)); echo 'PASS: entrypoint exports and logs override'
else fail=$((fail+1)); echo 'FAIL: entrypoint override export or log'; fi
run_case default-state AUTH=authcode URNETWORK_AUTH_CODE=fake-code
if rg -Fxq "$tmp/default-state/home/.urnetwork" "$dispatch" && rg -Fq "State directory: $tmp/default-state/home/.urnetwork" "$log"; then
  pass=$((pass+1)); echo 'PASS: entrypoint exports and logs default'
else fail=$((fail+1)); echo 'FAIL: entrypoint default export or log'; fi

# Run the real path, login, proxy and network functions with a stub binary;
# replace only main and app location to avoid services and network access.
# The start scripts source their helpers (update_verify.sh and friends) by
# relative path, so the sandbox needs the whole scripts directory, not just the
# one script under test.
for f in "$HERE"/*.sh; do cp "$f" "$tmp/"; done
mkdir -p "$tmp/provider-app"
cat > "$tmp/provider-app/urnetwork_amd64_stable" <<'STUB'
#!/bin/sh
printf '%s:%s\n' "$1" "$URNETWORK_STATE_DIR" >> "$STUB_LOG"
case "$1" in
  --version) echo stub ;;
  auth) printf 'fake-jwt\n' > "$URNETWORK_STATE_DIR/jwt"; echo 'Jwt written to state directory' ;;
  choose_network) printf 'fake-network\n' > "$URNETWORK_STATE_DIR/network.json" ;;
  provide) exit 0 ;;
esac
STUB
chmod +x "$tmp/provider-app/urnetwork_amd64_stable"
cp "$tmp/provider-app/urnetwork_amd64_stable" "$tmp/provider-app/urnetwork_amd64_nightly"
for kind in jwt stable nightly; do
  python3 - "$HERE/start_$kind.sh" "$tmp/start-$kind.sh" "$tmp/provider-app" "$kind" <<'PY'
import pathlib, sys
src = pathlib.Path(sys.argv[1]).read_text()
body = src.rstrip()
assert body.count('APP_DIR="/app"') == 1, 'expected one APP_DIR literal'
assert body.endswith('main "$@"'), 'script must end with its main dispatch'
src = body.replace('APP_DIR="/app"', 'APP_DIR="' + sys.argv[3] + '"', 1)
tail = """func_sleep() { :; }
func_exit_if_stopping() { :; }
pgrep() { return 1; }
A_SYS_ARCH=amd64
func_check_proxy
func_choose_network
"""
tail += 'func_start_provider\n' if sys.argv[4] == 'jwt' else 'func_do_login\n'
head = src[:-len('main "$@"')].rstrip('\n')
pathlib.Path(sys.argv[2]).write_text(head + '\n' + tail)
PY
  for mode in override default; do
    case_dir="$tmp/provider-$kind-$mode"
    mkdir -p "$case_dir/home/.urnetwork"
    if [ "$mode" = override ]; then
      state="$case_dir/custom state"
      mkdir -p "$state"
      state_env=("URNETWORK_STATE_DIR=$state")
      printf 'default-proxy\n' > "$case_dir/home/.urnetwork/proxy"
    else
      state="$case_dir/home/.urnetwork"
      state_env=()
    fi
    printf 'old-proxy\n' > "$state/proxy"
    env -i PATH="$PATH" HOME="$case_dir/home" "${state_env[@]}" \
      URNETWORK_AUTH_CODE=fake-code USER_AUTH=fake-user PASSWORD=fake-password \
      UR_API_URL=https://example.invalid UR_CONNECT_URL=wss://example.invalid \
      STUB_LOG="$case_dir/binary.log" sh "$tmp/start-$kind.sh" > "$case_dir/out" 2>&1
    rc=$?
    if [ "$rc" -eq 0 ] && [ -s "$state/jwt" ] && [ -s "$state/network.json" ] && [ ! -e "$state/proxy" ] \
      && rg -Fxq "auth:$state" "$case_dir/binary.log" \
      && rg -Fxq "choose_network:$state" "$case_dir/binary.log" \
      && { [ "$mode" = default ] || [ -s "$case_dir/home/.urnetwork/proxy" ]; } \
      && rg -Fq "State directory: $state" "$case_dir/out" \
      && ! rg -q 'JWT file not written|JWT file missing' "$case_dir/out"; then
      pass=$((pass+1)); echo "PASS: $kind $mode auth, proxy and network use state dir"
    else
      fail=$((fail+1)); echo "FAIL: $kind $mode state dir (exit $rc)"
      rg -n '.' "$case_dir/out" "$case_dir/binary.log" || :
    fi
  done
done

# Exercise the actual entrypoint-to-startup handoff with a stored JWT.
cp "$tmp/start-jwt.sh" "$tmp/provider-app/start_jwt.sh"
chmod +x "$tmp/provider-app/start_jwt.sh"
case_dir="$tmp/provider-handoff"
mkdir -p "$case_dir/home" "$case_dir/state"
printf 'existing-jwt\n' > "$case_dir/state/jwt"
env -i PATH="$PATH" HOME="$case_dir/home" AUTH=authcode \
  URNETWORK_STATE_DIR="$case_dir/state" APP_DIR="$tmp/provider-app" \
  STUB_LOG="$case_dir/binary.log" UR_API_URL= UR_CONNECT_URL= \
  sh "$tmp/entrypoint.sh" > "$case_dir/out" 2>&1
rc=$?
if [ "$rc" -eq 0 ] && rg -Fq "Existing session found at $case_dir/state/jwt" "$case_dir/out" \
  && ! rg -q '^auth:' "$case_dir/binary.log" \
  && [ "$(rg -c 'State directory:' "$case_dir/out")" -eq 1 ]; then
  pass=$((pass+1)); echo 'PASS: entrypoint handoff reuses override session and logs once'
else
  fail=$((fail+1)); echo "FAIL: entrypoint handoff (exit $rc)"
  rg -n '.' "$case_dir/out" "$case_dir/binary.log" || :
fi

echo "Results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
