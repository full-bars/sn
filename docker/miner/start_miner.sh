#!/bin/sh
# Authenticate and run the sn miner for the network.
#
# The miner binary is baked in as /app/urnetwork_<arch>_stable (the same file
# name convention the relay scripts use) and is the program whose usage is
# written as `provider ...`. Environment variables are translated into flags
# that the miner's docopt grammar really declares:
#   ALL_OPERATORS=Y            -> provide --all-operators
#   AUTO_REGISTER=Y            -> provide --auto-register (needs HOTKEY_SEED_FILE)
#   HOTKEY_SEED_FILE=<path>    -> --hotkey_seed_file
#   ALLOW_CLIENT_REGISTRATION  -> Y passes --allow-client-registration, auto and N omit
#   MAX_MEMORY=<mem>           -> --max-memory
#   OPERATORS_URL=<url>        -> --operators-url (provide --all-operators, auth --operator)
#   OPERATOR=<domain>          -> auth --operator=<domain>
#   PROVIDER_ARGS="..."        -> appended to provide verbatim
#   PROXY_FILE=<path>          -> provider proxy add --proxy_file (default /app/proxy.txt)
set -e

APP_DIR="${APP_DIR:-/app}"
STATE_DIR="${URNETWORK_STATE_DIR:-${HOME:-/root}/.urnetwork}"
OPERATOR="${OPERATOR:-}"
HOTKEY_SEED_FILE="${HOTKEY_SEED_FILE:-}"
OPERATORS_URL="${OPERATORS_URL:-}"
MAX_MEMORY="${MAX_MEMORY:-}"
PROVIDER_ARGS="${PROVIDER_ARGS:-}"
PROXY_FILE="${PROXY_FILE:-/app/proxy.txt}"
POSITIONAL_CODE="${1:-}"

log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> sn-miner >>> $*"
}

is_true() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    y|yes|true|1) return 0 ;;
    *) return 1 ;;
  esac
}

func_get_architecture() {
  case "$(uname -m)" in
    x86_64)  A_SYS_ARCH=amd64 ;;
    aarch64) A_SYS_ARCH=arm64 ;;
    *)
      log "[ERROR] Unsupported arch $(uname -m)"
      exit 1
      ;;
  esac
  PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
}

func_check_config() {
  ALLOW_CLIENT_REGISTRATION_LC="$(printf '%s' "${ALLOW_CLIENT_REGISTRATION:-auto}" | tr '[:upper:]' '[:lower:]')"

  if is_true "${AUTO_REGISTER:-}"; then
    if [ -z "$HOTKEY_SEED_FILE" ]; then
      log "[ERROR] AUTO_REGISTER=Y requires HOTKEY_SEED_FILE to be set"
      exit 1
    fi
    if ! is_true "${ALL_OPERATORS:-}"; then
      log "[ERROR] AUTO_REGISTER=Y requires ALL_OPERATORS=Y (the flag only exists on provide --all-operators)"
      exit 1
    fi
  fi

  if [ -n "$HOTKEY_SEED_FILE" ]; then
    if [ ! -s "$HOTKEY_SEED_FILE" ]; then
      log "[ERROR] HOTKEY_SEED_FILE=$HOTKEY_SEED_FILE is missing or empty"
      exit 1
    fi
    if ! is_true "${ALL_OPERATORS:-}"; then
      log "[WARN] HOTKEY_SEED_FILE is only passed to provide when ALL_OPERATORS=Y; it is still used for auth --operator"
    fi
  fi

  if [ -n "$OPERATORS_URL" ] && ! is_true "${ALL_OPERATORS:-}"; then
    log "[WARN] OPERATORS_URL is only passed to provide when ALL_OPERATORS=Y; it is still used for auth --operator"
  fi

  case "$ALLOW_CLIENT_REGISTRATION_LC" in
    y|yes|true|1)
      ALLOW_CLIENT_REGISTRATION_FLAG="--allow-client-registration"
      log "[INFO] ALLOW_CLIENT_REGISTRATION=$ALLOW_CLIENT_REGISTRATION: passing --allow-client-registration"
      ;;
    n|no|false|0)
      ALLOW_CLIENT_REGISTRATION_FLAG=""
      log "[INFO] ALLOW_CLIENT_REGISTRATION=$ALLOW_CLIENT_REGISTRATION: not passing --allow-client-registration"
      ;;
    auto|"")
      ALLOW_CLIENT_REGISTRATION_FLAG=""
      log "[INFO] ALLOW_CLIENT_REGISTRATION=auto: not passing --allow-client-registration (set it to Y to enable client registration)"
      ;;
    *)
      log "[ERROR] ALLOW_CLIENT_REGISTRATION must be auto, Y or N (got '$ALLOW_CLIENT_REGISTRATION')"
      exit 1
      ;;
  esac

  SESSION_JWT="$STATE_DIR/jwt"
  if [ -n "$OPERATOR" ]; then
    SESSION_JWT="$STATE_DIR/operators/$OPERATOR/jwt"
  fi
}

# auth_args <authcode>: fills the global AUTH_ARGS list via `set --` in the
# caller. The auth grammar takes exactly one credential form, so a code wins
# over the hotkey seed and the seed is left to provide/auto-register when both
# are present. The code and password are never echoed.
func_authenticate() {
  auth_code="$1"
  set -- auth
  if [ -n "$OPERATOR" ]; then
    set -- "$@" --operator="$OPERATOR"
  fi
  if [ -n "$OPERATORS_URL" ]; then
    set -- "$@" --operators-url="$OPERATORS_URL"
  fi
  if [ "${AUTH:-}" = "userpass" ]; then
    set -- "$@" --user_auth="$USER_AUTH" --password="$PASSWORD"
  elif [ -n "$auth_code" ]; then
    if [ -n "$HOTKEY_SEED_FILE" ]; then
      log "[INFO] A code and HOTKEY_SEED_FILE are both set; auth uses the code and provide uses the seed"
    fi
    set -- "$@" "$auth_code"
  elif [ -n "$HOTKEY_SEED_FILE" ]; then
    set -- "$@" --hotkey_seed_file="$HOTKEY_SEED_FILE"
  else
    log "[ERROR] no credential form available for auth (need AUTHCODE, HOTKEY_SEED_FILE or USER_AUTH+PASSWORD)"
    return 1
  fi
  set -- "$@" -f

  if [ -n "$OPERATOR" ]; then
    log "[INFO] Authenticating for operator $OPERATOR"
  else
    log "[INFO] Authenticating with the network"
  fi
  "$PROVIDER_BIN" "$@"
}

func_maybe_add_proxies() {
  if [ -s "$PROXY_FILE" ]; then
    log "[INFO] Adding proxies from $PROXY_FILE"
    if ! "$PROVIDER_BIN" proxy add --proxy_file="$PROXY_FILE" -f; then
      log "[WARN] proxy add failed; continuing without the proxy list"
    fi
  fi
}

func_provide() {
  set -- provide
  if is_true "${ALL_OPERATORS:-}"; then
    set -- "$@" --all-operators
  fi
  if is_true "${AUTO_REGISTER:-}"; then
    set -- "$@" --auto-register
  fi
  if is_true "${ALL_OPERATORS:-}" && [ -n "$HOTKEY_SEED_FILE" ]; then
    set -- "$@" --hotkey_seed_file="$HOTKEY_SEED_FILE"
  fi
  if is_true "${ALL_OPERATORS:-}" && [ -n "$OPERATORS_URL" ]; then
    set -- "$@" --operators-url="$OPERATORS_URL"
  fi
  if [ -n "$ALLOW_CLIENT_REGISTRATION_FLAG" ]; then
    set -- "$@" "$ALLOW_CLIENT_REGISTRATION_FLAG"
  fi
  if [ -n "$MAX_MEMORY" ]; then
    set -- "$@" --max-memory="$MAX_MEMORY"
  fi
  if [ -n "$PROVIDER_ARGS" ]; then
    # Intentional word splitting: PROVIDER_ARGS is a raw flag list.
    # shellcheck disable=SC2086
    set -- "$@" $PROVIDER_ARGS
  fi

  log "[INFO] Starting the miner: $PROVIDER_BIN $*"
  if "$PROVIDER_BIN" "$@"; then
    PROVIDE_CODE=0
  else
    PROVIDE_CODE=$?
  fi
}

func_bootstrap() {
  func_get_architecture
  func_check_config
  if [ ! -x "$PROVIDER_BIN" ]; then
    log "[ERROR] $PROVIDER_BIN is missing or not executable"
    exit 1
  fi
  log "[INFO] Running $( "$PROVIDER_BIN" --version 2>/dev/null || echo 'unknown version' )"

  if [ -s "$SESSION_JWT" ]; then
    log "[INFO] Existing session found at $SESSION_JWT; skipping authentication"
  else
    auth_code="$POSITIONAL_CODE"
    if [ -z "$auth_code" ]; then
      auth_code="${URNETWORK_AUTH_CODE:-}"
    fi
    if [ -n "$auth_code" ] || [ "${AUTH:-}" = "userpass" ]; then
      if ! func_authenticate "$auth_code"; then
        log "[ERROR] Authentication failed"
        exit 1
      fi
    elif [ -n "$HOTKEY_SEED_FILE" ] && [ -n "$OPERATOR" ]; then
      # A seed sign-in names the operator domain it signs in to.
      if ! func_authenticate ""; then
        log "[ERROR] Authentication failed"
        exit 1
      fi
    elif is_true "${ALL_OPERATORS:-}"; then
      # No network sign-in is needed here: provide --all-operators works from
      # each operator's own session under <state>/operators/<domain>/jwt, and
      # --auto-register signs in with the hotkey seed where one is missing.
      log "[INFO] ALL_OPERATORS without an explicit credential: provide uses each operator's stored session under $STATE_DIR/operators/<domain>/jwt"
      if is_true "${AUTO_REGISTER:-}" && [ -n "$HOTKEY_SEED_FILE" ]; then
        log "[INFO] --auto-register will sign in operators that have no session"
      fi
    else
      log "[ERROR] No stored session and no usable credential: set AUTHCODE, or OPERATOR with HOTKEY_SEED_FILE, or USER_AUTH and PASSWORD"
      exit 1
    fi
    if [ -n "$auth_code" ] || [ "${AUTH:-}" = "userpass" ] || { [ -n "$HOTKEY_SEED_FILE" ] && [ -n "$OPERATOR" ]; }; then
      if [ ! -s "$SESSION_JWT" ]; then
        log "[ERROR] Authentication reported success but no session was written to $SESSION_JWT"
        exit 1
      fi
      log "[INFO] Session written to $SESSION_JWT"
    fi
  fi

  func_maybe_add_proxies
}

func_run() {
  failures=0
  reauth_attempts=0
  while :; do
    func_provide
    if [ "$PROVIDE_CODE" -eq 0 ]; then
      log "[INFO] The miner exited cleanly"
      return 0
    fi
    if [ "$PROVIDE_CODE" -eq 75 ]; then
      log "[INFO] The miner asked for a planned restart (exit 75); restarting in 5s"
      failures=0
      sleep 5
      continue
    fi
    if [ "$PROVIDE_CODE" -eq 78 ]; then
      log "[WARN] The miner reported an auth error (exit 78)"
      rm -f "$SESSION_JWT"
      reauth_attempts=$((reauth_attempts + 1))
      if [ "$reauth_attempts" -ge 3 ]; then
        log "[CRITICAL] Re-auth attempted $reauth_attempts times without recovery; exiting"
        return 78
      fi
      auth_code="$POSITIONAL_CODE"
      if [ -z "$auth_code" ]; then
        auth_code="${URNETWORK_AUTH_CODE:-}"
      fi
      if [ -n "$auth_code" ] || [ "${AUTH:-}" = "userpass" ] || [ -n "$HOTKEY_SEED_FILE" ]; then
        log "[INFO] Re-authenticating (attempt $reauth_attempts)"
        if func_authenticate "$auth_code" && [ -s "$SESSION_JWT" ]; then
          failures=0
          reauth_attempts=0
          sleep 5
          continue
        fi
        log "[ERROR] Re-authentication failed"
      fi
      log "[CRITICAL] Session rejected and re-auth unavailable; exiting 78"
      return 78
    fi
    failures=$((failures + 1))
    log "[WARN] The miner crashed (#$failures, code=$PROVIDE_CODE)"
    if [ "$failures" -ge 3 ]; then
      log "[ERROR] Too many consecutive crashes; exiting for the supervisor to restart"
      return 1
    fi
    sleep 15
  done
}

func_bootstrap
func_run
