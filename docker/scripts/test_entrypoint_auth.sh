#!/bin/bash
# Offline entrypoint tests: redirect the staged copy's /app exec paths to
# stub startup scripts, so neither the startup scripts nor provider run.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/app"

# Keep all entrypoint logic intact; only replace the absolute exec targets.
python3 - "$HERE/entrypoint.sh" "$tmp/entrypoint.sh" <<'PY'
import pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text()
assert source.count('exec /app/') == 6
pathlib.Path(sys.argv[2]).write_text(source.replace('exec /app/', 'exec "$STUB_APP"/'))
PY
if [ "$?" -ne 0 ]; then
  echo 'FAIL: could not stage entrypoint'
  exit 1
fi
for name in start_stable start_jwt start_nightly pelican_panel; do
  printf '%s\n' '#!/bin/sh' 'printf "%s\n" "${0##*/}" "${URNETWORK_AUTH_CODE:-}" "$@" > "$STUB_LOG"' > "$tmp/app/$name.sh"
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
  env -i PATH="$PATH" HOME="$tmp/$name/home" STUB_APP="$tmp/app" STUB_LOG="$dispatch" \
    "$@" sh "$tmp/entrypoint.sh" > "$log" 2>&1
  rc=$?
}
run_positional() {
  local name="$1" code="$2"
  shift 2
  mkdir -p "$tmp/$name/home/.urnetwork"
  log="$tmp/$name/out"
  dispatch="$tmp/$name/dispatch"
  env -i PATH="$PATH" HOME="$tmp/$name/home" STUB_APP="$tmp/app" STUB_LOG="$dispatch" \
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

echo "Results: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
