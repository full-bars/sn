#!/bin/bash
# test_miner_entrypoint.sh: the sn miner image onboarding contract, run against
# a stub provider binary on the dev host. Offline, fake credentials only.
#
# Covers: the AUTH selector fail-closed paths, the environment-to-flag mapping
# for cli/miner, the stored-session policy (reuse / expired / unparseable), and
# that no auth code, password or token ever reaches the logs.
set -u

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="${REPO_DIR:-$(cd "$HERE/../.." && pwd)}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0
t() {
  local name="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    pass=$((pass+1)); echo "PASS: $name"; return 0
  else
    fail=$((fail+1)); echo "FAIL: $name"; return 1
  fi
}

b64url() { printf '%s' "$1" | base64 | tr -d '\r\n' | tr '+/' '-_' | tr -d '='; }
hdr='eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9'
now="$(date +%s)"
mkjwt() { printf '%s.%s.sig' "$hdr" "$(b64url "$1")"; }

APP="$tmp/app"
mkdir -p "$APP"
# The entrypoint execs $APP_DIR/start_miner.sh, so stage the scripts next to
# the stub exactly as the image does.
cp "$REPO"/docker/miner/entrypoint.sh "$REPO"/docker/miner/start_miner.sh "$REPO"/docker/miner/jwt_verify.sh "$REPO"/docker/miner/provider_env.sh "$APP/"
chmod +x "$APP"/*.sh
printf 'fake-hotkey-seed-not-real\n' > "$tmp/seed.bin"

# Stub provider: records argv, writes a session on auth, and exits with the
# code in $STUB_EXIT (default 0) on provide.
cat > "$APP/urnetwork_amd64_stable" <<'STUB'
#!/bin/sh
echo "argv: $*" >> "$STUB_LOG"
case "$1" in
  --version) echo "v0.0.0-stub"; exit 0 ;;
  auth)
    mkdir -p "$(dirname "$STUB_JWT")"
    printf '%s\n' "$STUB_JWT_VALUE" > "$STUB_JWT"
    echo "Jwt written to $STUB_JWT"
    exit 0
    ;;
  proxy) exit 0 ;;
  provide) exit "${STUB_EXIT:-0}" ;;
esac
exit 0
STUB
chmod +x "$APP/urnetwork_amd64_stable"

VALID_JWT="$(mkjwt "{\"sub\":\"1\",\"exp\":$((now + 3600)),\"nbf\":$((now - 3600))}")"
EXPIRED_JWT="$(mkjwt "{\"sub\":\"1\",\"exp\":$((now - 3600))}")"
FAKE_CODE="fake-auth-code-not-real-0001"
FAKE_PW="fake-password-not-real-0002"

run_entry() { # run_entry <home> <jwt_relpath_or_-> <jwt_value> <log> <exit_code> [env...]
  local home="$1" rel="$2" jwt="$3" log="$4" code="$5"
  shift 5
  local jwt_path="$home/.urnetwork/jwt"
  if [ "$rel" != "-" ]; then
    jwt_path="$home/$rel"
  fi
  env -i PATH="$PATH" HOME="$home" APP_DIR="$APP" \
    STUB_LOG="$log" STUB_JWT="$jwt_path" STUB_JWT_VALUE="$jwt" STUB_EXIT="$code" \
    "$@" sh "$APP/entrypoint.sh" > "$log.out" 2>&1
}

echo "--- AUTH selector fail-closed paths ---"
h="$tmp/h1"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l1" 0; rc=$?
t "no AUTH at all fails with a clear message" sh -c "grep -q 'ERROR: set AUTH=userpass|authcode' '$tmp/l1.out' && [ $rc -eq 1 ]"

h="$tmp/h2"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l2" 0 AUTH=userpass; rc=$?
t "AUTH=userpass without credentials names USER_AUTH and PASSWORD" sh -c "grep -q 'ERROR: AUTH=userpass requires USER_AUTH and PASSWORD to be set' '$tmp/l2.out' && [ $rc -eq 1 ]"

h="$tmp/h3"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l3" 0 AUTH=authcode; rc=$?
t "AUTH=authcode without a code or session fails closed" sh -c "grep -q 'ERROR: AUTH=authcode requires AUTHCODE' '$tmp/l3.out' && [ $rc -eq 1 ]"

h="$tmp/h4"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l4" 0 AUTH=INVALID; rc=$?
t "invalid AUTH is rejected by name" sh -c "grep -q \"ERROR: Invalid AUTH 'INVALID'\" '$tmp/l4.out' && [ $rc -eq 1 ]"

h="$tmp/h5"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l5" 0 AUTH=userpass USER_AUTH=u; rc=$?
t "missing PASSWORD alone is named" sh -c "grep -q 'ERROR: AUTH=userpass requires PASSWORD to be set' '$tmp/l5.out' && [ $rc -eq 1 ]"

echo "--- happy path and the env-to-flag mapping ---"
h="$tmp/h6"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l6" 0 \
  AUTH=authcode AUTHCODE="$FAKE_CODE" ALL_OPERATORS=Y AUTO_REGISTER=Y \
  HOTKEY_SEED_FILE="$tmp/seed.bin" ALLOW_CLIENT_REGISTRATION=Y MAX_MEMORY=1500mib \
  OPERATORS_URL=http://127.0.0.1:9/operators.yml PROVIDER_ARGS="--quarantine-rejected-sign-in"
t "AUTHCODE is passed to auth" sh -c "grep -q 'argv: auth .*$FAKE_CODE' '$tmp/l6'"
t "the operator flags are passed to provide" sh -c "grep -q 'argv: provide --all-operators --auto-register --hotkey_seed_file=$tmp/seed.bin --max-memory=1500mib --allow-client-registration --quarantine-rejected-sign-in --operators-url=http://127.0.0.1:9/operators.yml' '$tmp/l6'"
t "the session file is created and reported" sh -c "[ -s '$h/.urnetwork/jwt' ] && grep -q 'Session written to' '$tmp/l6.out'"
t "no auth code or password in the logs" sh -c "! grep -q '$FAKE_CODE' '$tmp/l6.out' && ! grep -q 'fake-password' '$tmp/l6.out'"
t "the wallet reminder is logged" grep -q "A payout wallet and consent must be set" "$tmp/l6.out"

echo "--- ALLOW_CLIENT_REGISTRATION and validation ---"
h="$tmp/h7"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l7" 0 AUTH=authcode AUTHCODE="$FAKE_CODE" ALL_OPERATORS=Y
t "auto omits --allow-client-registration" sh -c "grep -q 'argv: provide --all-operators$' '$tmp/l7'"

h="$tmp/h8"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l8" 0 AUTH=authcode AUTHCODE="$FAKE_CODE" AUTO_REGISTER=Y; rc=$?
t "AUTO_REGISTER without HOTKEY_SEED_FILE fails closed" sh -c "grep -q 'AUTO_REGISTER=Y requires HOTKEY_SEED_FILE' '$tmp/l8.out' && [ $rc -eq 1 ]"

h="$tmp/h9"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l9" 0 AUTH=authcode AUTHCODE="$FAKE_CODE" AUTO_REGISTER=Y \
  HOTKEY_SEED_FILE="$tmp/seed.bin"; rc=$?
t "AUTO_REGISTER without ALL_OPERATORS fails closed" sh -c "grep -q 'AUTO_REGISTER=Y requires ALL_OPERATORS=Y' '$tmp/l9.out' && [ $rc -eq 1 ]"

h="$tmp/h10"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l10" 0 AUTH=authcode AUTHCODE="$FAKE_CODE" \
  HOTKEY_SEED_FILE="$tmp/missing-seed.bin"; rc=$?
t "a missing HOTKEY_SEED_FILE is named" sh -c "grep -q 'HOTKEY_SEED_FILE=.*missing-seed.bin is missing or empty' '$tmp/l10.out' && [ $rc -eq 1 ]"

h="$tmp/h11"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l11" 0 AUTH=authcode AUTHCODE="$FAKE_CODE" ALLOW_CLIENT_REGISTRATION=maybe; rc=$?
t "an unknown ALLOW_CLIENT_REGISTRATION value is rejected" sh -c "grep -q 'ALLOW_CLIENT_REGISTRATION must be auto, Y or N' '$tmp/l11.out' && [ $rc -eq 1 ]"

echo "--- stored session policy ---"
h="$tmp/h12"; mkdir -p "$h/.urnetwork"; printf '%s\n' "$VALID_JWT" > "$h/.urnetwork/jwt"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l12" 0 AUTH=authcode
t "a stored valid session skips auth" sh -c "! grep -q 'argv: auth' '$tmp/l12' && grep -q 'Existing session found' '$tmp/l12.out'"

h="$tmp/h13"; mkdir -p "$h/.urnetwork"; printf '%s\n' "$EXPIRED_JWT" > "$h/.urnetwork/jwt"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l13" 0 AUTH=authcode AUTHCODE="$FAKE_CODE"
t "an expired session is replaced when a code exists" sh -c "grep -q 'stored session is expired; removing it' '$tmp/l13.out' && grep -q 'argv: auth' '$tmp/l13'"

h="$tmp/h14"; mkdir -p "$h/.urnetwork"; printf '%s\n' "$EXPIRED_JWT" > "$h/.urnetwork/jwt"
run_entry "$h" "-" "$EXPIRED_JWT" "$tmp/l14" 0 AUTH=authcode
t "an expired session with no code is left in place" sh -c "[ -s '$h/.urnetwork/jwt' ] && grep -q 'no credentials are provided to re-authenticate' '$tmp/l14.out'"

h="$tmp/h15"; mkdir -p "$h/.urnetwork"; printf 'not-a-jwt\n' > "$h/.urnetwork/jwt"
run_entry "$h" "-" "not-a-jwt" "$tmp/l15" 0 AUTH=authcode
t "an unparseable session is left in place for the miner to report" sh -c "[ -s '$h/.urnetwork/jwt' ] && grep -q 'could not be validated locally' '$tmp/l15.out'"

h="$tmp/h23"; mkdir -p "$h/.urnetwork"; printf '%s\n' "$EXPIRED_JWT" > "$h/.urnetwork/jwt"
run_entry "$h" "-" "" "$tmp/l23" 0 AUTH=authcode
t "an expired session with no code still reaches provide for the miner to report" sh -c "grep -q 'expired; not reusing it' '$tmp/l23.out' && grep -q 'miner will report the auth state' '$tmp/l23.out' && grep -q 'argv: provide' '$tmp/l23' && [ -s '$h/.urnetwork/jwt' ]"

echo "--- operator mode ---"
h="$tmp/h16"
run_entry "$h" ".urnetwork/operators/example.invalid/jwt" "$VALID_JWT" "$tmp/l16" 0 AUTH=authcode AUTHCODE="$FAKE_CODE" \
  OPERATOR=example.invalid HOTKEY_SEED_FILE="$tmp/seed.bin" OPERATORS_URL=http://127.0.0.1:9/operators.yml
t "OPERATOR signs in with --operator and the code (the auth grammar takes one credential)" sh -c "grep -q 'argv: auth --operator=example.invalid --operators-url=http://127.0.0.1:9/operators.yml $FAKE_CODE -f' '$tmp/l16' && [ -s '$h/.urnetwork/operators/example.invalid/jwt' ]"

h="$tmp/h19"
run_entry "$h" ".urnetwork/operators/example.invalid/jwt" "$VALID_JWT" "$tmp/l19" 0 AUTH=authcode \
  OPERATOR=example.invalid HOTKEY_SEED_FILE="$tmp/seed.bin"
t "OPERATOR with only a seed signs in with --hotkey_seed_file" sh -c "grep -q 'argv: auth --operator=example.invalid --hotkey_seed_file=$tmp/seed.bin -f' '$tmp/l19' && [ -s '$h/.urnetwork/operators/example.invalid/jwt' ]"

h="$tmp/h20"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "" "$tmp/l20" 0 AUTH=authcode HOTKEY_SEED_FILE="$tmp/seed.bin"; rc=$?
t "a seed without OPERATOR and without auto-register fails closed" sh -c "grep -q 'no usable credential: set AUTHCODE, or OPERATOR with HOTKEY_SEED_FILE' '$tmp/l20.out' && [ $rc -eq 1 ]"

h="$tmp/h21"
run_entry "$h" ".urnetwork/operators/example.invalid/jwt" "$VALID_JWT" "$tmp/l21" 0 AUTH=authcode \
  ALL_OPERATORS=Y AUTO_REGISTER=Y HOTKEY_SEED_FILE="$tmp/seed.bin"
t "auto-register delegates the sign-in to provide" sh -c "grep -q 'auto-register will sign in operators that have no session' '$tmp/l21.out' && ! grep -q 'argv: auth' '$tmp/l21' && grep -q 'argv: provide --all-operators --auto-register --hotkey_seed_file' '$tmp/l21'"

h="$tmp/h22"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l22" 0 AUTH=authcode ALL_OPERATORS=Y
t "ALL_OPERATORS with no credential runs without a network sign-in" sh -c "grep -q \"uses each operator's stored session\" '$tmp/l22.out' && ! grep -q 'argv: auth' '$tmp/l22' && grep -q 'argv: provide --all-operators' '$tmp/l22'"

echo "--- BUILD deprecation and conflict resolution (AUTH wins) ---"
h="$tmp/h17"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l17" 0 BUILD=jwt AUTH=authcode AUTHCODE="$FAKE_CODE"
t "a consistent BUILD still logs the unsupported warning" grep -q "BUILD is not supported by the miner image" "$tmp/l17.out"

h="$tmp/h24"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l24" 0 BUILD=stable AUTH=authcode AUTHCODE="$FAKE_CODE"
t "a conflicting BUILD and AUTH log the conflict and AUTH is used" sh -c "grep -q \"Conflicting AUTH='authcode' and BUILD='stable' specified\" '$tmp/l24.out' && grep -q 'argv: auth .*$FAKE_CODE' '$tmp/l24' && grep -q 'argv: provide' '$tmp/l24'"

h="$tmp/h25"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l25" 0 BUILD=nightly AUTHCODE="$FAKE_CODE"
t "BUILD alone logs the deprecation hint and AUTH is inferred from the code" sh -c "grep -q 'BUILD is deprecated for auth selection' '$tmp/l25.out' && grep -q 'argv: auth .*$FAKE_CODE' '$tmp/l25'"

h="$tmp/h26"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l26" 0 AUTH=UserPass USER_AUTH=fake-user PASSWORD="$FAKE_PW"
t "AUTH is matched case-insensitively" sh -c "grep -q 'argv: auth --user_auth=fake-user --password=$FAKE_PW -f' '$tmp/l26'"

h="$tmp/h27"; mkdir -p "$h/.urnetwork"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l27" 0 AUTH=authcode URNETWORK_AUTH_CODE="$FAKE_CODE"
t "URNETWORK_AUTH_CODE works without AUTHCODE" sh -c "grep -q 'argv: auth .*$FAKE_CODE' '$tmp/l27'"

echo "--- volume warning ---"
h="$tmp/h18"
run_entry "$h" "-" "$VALID_JWT" "$tmp/l18" 0 AUTH=authcode AUTHCODE="$FAKE_CODE"
t "an unmounted empty state dir logs the new-identity warning" grep -q "WARNING: Without a persistent volume mounted" "$tmp/l18.out"

echo "--- provider_env.sh (the v33 operator mapping port) ---"
PE="$REPO/docker/miner/provider_env.sh"
pe() { # pe <home> [KEY=VALUE...] -> the helper's flags or errors
  local home="$1"
  shift
  env -i PATH="$PATH" HOME="$home" "$@" sh -c '. "$1"; provider_env_flags "$2"' _ "$PE" "$home/.urnetwork" 2>&1
}

h="$tmp/pe1"
out="$(pe "$h")"
t "no variables set emits nothing" [ -z "$out" ]

out="$(pe "$h" ALL_OPERATORS=Y)"
t "ALL_OPERATORS=Y emits --all-operators" [ "$out" = "--all-operators" ]

out="$(pe "$h" ALLOW_CLIENT_REGISTRATION=Y)"
t "ALLOW_CLIENT_REGISTRATION=Y emits --allow-client-registration" [ "$out" = "--allow-client-registration" ]

out="$(pe "$h" ALL_OPERATORS=Y ALLOW_CLIENT_REGISTRATION=auto)"
t "auto never adds the flag (kept miner behavior)" [ "$out" = "--all-operators" ]

out="$(pe "$h" HOTKEY_SEED_FILE="$tmp/seed.bin")"
t "a seed without ALL_OPERATORS is left to the auth path" [ -z "$out" ]

out="$(pe "$h" ALL_OPERATORS=Y AUTO_REGISTER=Y HOTKEY_SEED_FILE="$tmp/seed.bin" \
  MAX_MEMORY=2gib ALLOW_CLIENT_REGISTRATION=Y PROVIDER_ARGS='--skip-audit --dry-run')"
expected="$(printf '%s\n' --all-operators --auto-register "--hotkey_seed_file=$tmp/seed.bin" --max-memory=2gib --allow-client-registration --skip-audit --dry-run)"
t "the v33 operator set maps onto the miner's provide flags" [ "$out" = "$expected" ]

out="$(pe "$h" ALL_OPERATORS=maybe)"; rc=$?
printf '%s' "$out" > "$tmp/pe-bool.out"
t "an invalid ALL_OPERATORS value is refused" sh -c "grep -q 'ALL_OPERATORS must be 1/true/yes/on or 0/false/no/off' '$tmp/pe-bool.out' && [ $rc -ne 0 ]"

out="$(pe "$h" MAX_MEMORY='2 gib')"; rc=$?
printf '%s' "$out" > "$tmp/pe-max.out"
t "whitespace in MAX_MEMORY is refused" sh -c "grep -q 'MAX_MEMORY must not contain whitespace' '$tmp/pe-max.out' && [ $rc -ne 0 ]"

out="$(pe "$h" HOTKEY_SEED_FILE='/tmp/a b')"; rc=$?
printf '%s' "$out" > "$tmp/pe-seed.out"
t "whitespace in HOTKEY_SEED_FILE is refused" sh -c "grep -q 'HOTKEY_SEED_FILE must not contain whitespace' '$tmp/pe-seed.out' && [ $rc -ne 0 ]"

out="$(pe "$h" ALLOW_CLIENT_REGISTRATION=maybe)"; rc=$?
printf '%s' "$out" > "$tmp/pe-allow.out"
t "the helper rejects an unknown ALLOW_CLIENT_REGISTRATION" sh -c "grep -q 'ALLOW_CLIENT_REGISTRATION must be auto, Y or N' '$tmp/pe-allow.out' && [ $rc -ne 0 ]"

out="$(pe "$h" PROVIDER_ARGS='--flag=*')"
t "PROVIDER_ARGS is split on whitespace and not globbed" [ "$out" = "--flag=*" ]

echo "--- urnet-tools: update wrapper, dispatcher and symlink contract ---"
# Stage the wrapper and its digest helper as the image has them.
cp "$REPO"/docker/miner/urnet-tools.sh "$REPO"/docker/scripts/update_verify.sh "$APP/"
chmod +x "$APP"/urnet-tools.sh "$APP"/update_verify.sh
UTS="$APP/urnet-tools.sh"

out="$(sh "$UTS" 2>&1)"; rc=$?
printf '%s' "$out" > "$tmp/ut-usage.out"
t "the wrapper prints usage without a command" sh -c "grep -q 'Usage: urnet-tools <command> \[args\]' '$tmp/ut-usage.out' && [ $rc -ne 0 ]"

out="$(PELICAN=yes sh "$UTS" update 2>&1)"; rc=$?
printf '%s' "$out" > "$tmp/ut-pelican.out"
t "Pelican mode refuses runtime updates" sh -c "grep -q 'runtime updates are disabled under Pelican' '$tmp/ut-pelican.out' && [ $rc -ne 0 ]"

out="$(sh "$UTS" proxy health 2>&1)"; rc=$?
printf '%s' "$out" > "$tmp/ut-proxy.out"
t "proxy health names the missing helper instead of failing silently" sh -c "grep -q 'proxy-health not found' '$tmp/ut-proxy.out' && [ $rc -ne 0 ]"

# Program classifier / install guard, exercised through the source-only seam
# with stubbed binaries (no network, no container).
cat > "$tmp/ut-miner-stub" <<'B'
#!/bin/sh
echo "provider provide --all-operators"
B
cat > "$tmp/ut-relay-stub" <<'B'
#!/bin/sh
echo "provider sn-status"
B
printf 'no marker here\n' > "$tmp/ut-none-stub"
chmod +x "$tmp/ut-miner-stub" "$tmp/ut-relay-stub"
cp "$tmp/ut-miner-stub" "$tmp/ut-miner-stub2"

t "the classifier reads a miner binary" bash -c 'URNET_TOOLS_SOURCE_ONLY=1 . "$1" && [ "$(upd_binary_class "$2")" = miner ]' _ "$REPO/docker/miner/urnet-tools.sh" "$tmp/ut-miner-stub"
t "the classifier reads a relay binary" bash -c 'URNET_TOOLS_SOURCE_ONLY=1 . "$1" && [ "$(upd_binary_class "$2")" = relay ]' _ "$REPO/docker/miner/urnet-tools.sh" "$tmp/ut-relay-stub"
t "the classifier reports unknown for an unmarked file" bash -c 'URNET_TOOLS_SOURCE_ONLY=1 . "$1" && [ "$(upd_binary_class "$2")" = unknown ]' _ "$REPO/docker/miner/urnet-tools.sh" "$tmp/ut-none-stub"
t "the install guard refuses a relay candidate for a miner binary" bash -c 'URNET_TOOLS_SOURCE_ONLY=1 . "$1" && ! upd_install_compatible "$2" "$3"' _ "$REPO/docker/miner/urnet-tools.sh" "$tmp/ut-relay-stub" "$tmp/ut-miner-stub"
t "the install guard accepts a matching miner candidate" bash -c 'URNET_TOOLS_SOURCE_ONLY=1 . "$1" && upd_install_compatible "$2" "$3"' _ "$REPO/docker/miner/urnet-tools.sh" "$tmp/ut-miner-stub2" "$tmp/ut-miner-stub"
t "the install guard refuses an unclassifiable candidate" bash -c 'URNET_TOOLS_SOURCE_ONLY=1 . "$1" && ! upd_install_compatible "$2" "$3"' _ "$REPO/docker/miner/urnet-tools.sh" "$tmp/ut-none-stub" "$tmp/ut-miner-stub"

# The dispatcher keeps the Go client for everything except the update path.
cp "$REPO"/docker/miner/urnet-tools-dispatch.sh "$tmp/urnet-tools-dispatch"
chmod +x "$tmp/urnet-tools-dispatch"
cat > "$tmp/dd-wrapper" <<'B'
#!/bin/sh
echo "wrapper:$*"
B
cat > "$tmp/dd-go" <<'B'
#!/bin/sh
echo "go:$*"
B
chmod +x "$tmp/dd-wrapper" "$tmp/dd-go"

out="$(env URNET_TOOLS_WRAPPER="$tmp/dd-wrapper" URNET_TOOLS_GO_BINARY="$tmp/dd-go" "$tmp/urnet-tools-dispatch" update --tag v1)"
t "the dispatcher routes update to the shell wrapper" [ "$out" = "wrapper:update --tag v1" ]
out="$(env URNET_TOOLS_WRAPPER="$tmp/dd-wrapper" URNET_TOOLS_GO_BINARY="$tmp/dd-go" "$tmp/urnet-tools-dispatch" idle-update --threshold 1)"
t "the dispatcher routes idle-update to the shell wrapper" [ "$out" = "wrapper:idle-update --threshold 1" ]
out="$(env URNET_TOOLS_WRAPPER="$tmp/dd-wrapper" URNET_TOOLS_GO_BINARY="$tmp/dd-go" "$tmp/urnet-tools-dispatch" status --json)"
t "every other command keeps the Go client" [ "$out" = "go:status --json" ]
out="$(env URNET_TOOLS_WRAPPER="$tmp/dd-wrapper" URNET_TOOLS_GO_BINARY="$tmp/dd-go" "$tmp/urnet-tools-dispatch")"
t "a bare dispatcher call runs the Go client" [ "$out" = "go:" ]

echo "--- Dockerfile.miner ships the image contract ---"
t "the wrapper is copied to /app/urnet-tools.sh" grep -q 'COPY docker/miner/urnet-tools.sh /app/urnet-tools.sh' "$REPO/Dockerfile.miner"
t "the dispatcher is installed as /usr/local/bin/urnet-tools" grep -q 'COPY docker/miner/urnet-tools-dispatch.sh /usr/local/bin/urnet-tools' "$REPO/Dockerfile.miner"
t "the Go client is kept as urnet-tools-go" grep -q 'urnet_tools_bin /usr/local/bin/urnet-tools-go' "$REPO/Dockerfile.miner"
t "proxy-health is symlinked onto PATH" grep -q 'ln -sf /app/proxy-health.sh /usr/local/bin/proxy-health' "$REPO/Dockerfile.miner"
t "proxy-traffic is symlinked onto PATH" grep -q 'ln -sf /app/proxy-traffic.sh /usr/local/bin/proxy-traffic' "$REPO/Dockerfile.miner"
t "logs is symlinked onto PATH" grep -q 'ln -sf /app/logs.sh /usr/local/bin/logs' "$REPO/Dockerfile.miner"
t "urnet-tools-sh points at the wrapper" grep -q 'ln -sf /app/urnet-tools.sh /usr/local/bin/urnet-tools-sh' "$REPO/Dockerfile.miner"
t "update_verify.sh is symlinked onto PATH" grep -q 'ln -sf /app/update_verify.sh /usr/local/bin/update_verify.sh' "$REPO/Dockerfile.miner"
t "jwt_verify.sh is symlinked onto PATH" grep -q 'ln -sf /app/jwt_verify.sh /usr/local/bin/jwt_verify.sh' "$REPO/Dockerfile.miner"

echo "--- jwt helpers ---"
# shellcheck source=docker/miner/jwt_verify.sh
. "$REPO/docker/miner/jwt_verify.sh"
printf '%s\n' "$VALID_JWT" > "$tmp/v.jwt"
t "jwt_verify accepts a valid token" validate_jwt_file "$tmp/v.jwt"
printf '%s\n' "$EXPIRED_JWT" > "$tmp/e.jwt"
t "jwt_verify rejects an expired token" sh -c "! validate_jwt_file '$tmp/e.jwt'"
t "jwt_expired detects an expired token" jwt_expired "$tmp/e.jwt"
t "jwt_expired does not fire on a valid token" sh -c "! jwt_expired '$tmp/v.jwt'"
printf 'not-a-jwt\n' > "$tmp/n.jwt"
t "jwt_expired does not fire on an unparseable token" sh -c "! jwt_expired '$tmp/n.jwt'"
t "jwt_verify rejects an nbf in the future" sh -c "printf '%s\n' '$hdr.$(b64url "{\"exp\":$((now + 3600)),\"nbf\":$((now + 3600))}").sig' > '$tmp/nbf.jwt' && ! validate_jwt_file '$tmp/nbf.jwt'"

echo "--- jwt_verify.sh is shared with docker/scripts ---"
t "the two jwt_verify.sh copies are identical" cmp -s "$REPO/docker/scripts/jwt_verify.sh" "$REPO/docker/miner/jwt_verify.sh"
# shellcheck source=docker/scripts/jwt_verify.sh
. "$REPO/docker/scripts/jwt_verify.sh"
t "the shared helper accepts a valid token" validate_jwt_file "$tmp/v.jwt"
t "the shared helper rejects an expired token" sh -c "! validate_jwt_file '$tmp/e.jwt'"
t "the shared helper does not treat an unparseable token as expired" sh -c "! jwt_expired '$tmp/n.jwt'"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
