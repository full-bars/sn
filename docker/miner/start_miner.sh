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
#
# The six v33 variables are mapped by provider_env.sh, the ported 3.23-fix
# helper sourced below; OPERATORS_URL, OPERATOR and PROXY_FILE are
# miner-image additions that have no v33 mapping.
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

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
# Local exp/nbf validation for a stored session (sourced by the entrypoint too).
if [ -f "$SCRIPT_DIR/jwt_verify.sh" ]; then
  . "$SCRIPT_DIR/jwt_verify.sh"
elif [ -f "/app/jwt_verify.sh" ]; then
  . "/app/jwt_verify.sh"
fi

# v33 operator env -> provide flags: ALL_OPERATORS, AUTO_REGISTER,
# HOTKEY_SEED_FILE, ALLOW_CLIENT_REGISTRATION, MAX_MEMORY, PROVIDER_ARGS.
if [ -f "$SCRIPT_DIR/provider_env.sh" ]; then
  . "$SCRIPT_DIR/provider_env.sh"
elif [ -f "/app/provider_env.sh" ]; then
  . "/app/provider_env.sh"
fi

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
  # Validate the v33 operator environment once, before anything runs. The
  # same flags are emitted again for the actual provide command.
  if ! provider_env_flags "$STATE_DIR" >/dev/null; then
    log "[ERROR] Invalid provider environment; fix the setting logged above and restart"
    exit 1
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

  case "$(provider_env_allow_client_registration_mode)" in
    y)
      log "[INFO] ALLOW_CLIENT_REGISTRATION=$ALLOW_CLIENT_REGISTRATION: passing --allow-client-registration"
      ;;
    n)
      log "[INFO] ALLOW_CLIENT_REGISTRATION=$ALLOW_CLIENT_REGISTRATION: not passing --allow-client-registration"
      ;;
    auto)
      log "[INFO] ALLOW_CLIENT_REGISTRATION=auto: not passing --allow-client-registration (set it to Y to enable client registration)"
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
  # v33 operator flags from the environment (see provider_env.sh). Empty when
  # none of the variables is set, so the command line stays exactly `provide`.
  if ! PROVIDER_ENV_ARGS="$(provider_env_flags "$STATE_DIR")"; then
    log "[ERROR] Invalid provider environment; fix the setting logged above and restart"
    exit 1
  fi
  # shellcheck disable=SC2086  # deliberate word splitting of the flag list
  set -- "$@" $PROVIDER_ENV_ARGS
  # Miner-image addition with no v33 provider_env.sh mapping.
  if is_true "${ALL_OPERATORS:-}" && [ -n "$OPERATORS_URL" ]; then
    set -- "$@" --operators-url="$OPERATORS_URL"
  fi

  log "[INFO] Starting the miner: $PROVIDER_BIN $*"
  if func_run_miner "$PROVIDER_BIN" "$@"; then
    PROVIDE_CODE=0
  else
    PROVIDE_CODE=$?
  fi
}

# === Stop Forwarding ===
# `docker stop` sends SIGTERM to PID 1, which is this shell. A shell running as
# PID 1 has no default action for SIGTERM, so without a trap the miner never
# hears it and Docker SIGKILLs the container once the grace period expires,
# skipping the miner's own shutdown. The miner runs as a child so the trap can
# forward the signal and the supervisor can wait for the real exit code.
MINER_STOPPING=0
MINER_CHILD_PID=""

func_forward_stop() {
    MINER_STOPPING=1
    if [ -n "$MINER_CHILD_PID" ]; then
        kill -TERM "$MINER_CHILD_PID" 2>/dev/null || true
    fi
}

func_install_stop_trap() {
    trap func_forward_stop TERM INT
}

func_exit_if_stopping() {
    if [ "$MINER_STOPPING" -eq 1 ]; then
        log "[INFO] Stop requested; miner supervisor exiting."
        exit 0
    fi
}

# Interruptible sleep: busybox ash defers a trapped signal until the foreground
# command returns, so a bare `sleep N` swallows `docker stop` for its whole
# duration. Backgrounding the sleep lets the trap run, and both the entry and
# post-spawn checks catch a stop that landed on a command boundary.
func_sleep() {
    func_exit_if_stopping
    sleep "$1" &
    _stop_sleep_pid=$!
    if [ "$MINER_STOPPING" -eq 1 ]; then
        kill "$_stop_sleep_pid" 2>/dev/null || true
    fi
    wait "$_stop_sleep_pid" || true
    func_exit_if_stopping
}

# Run the miner in the background so the trap can fire while it runs, and keep
# its real exit status. A TERM that landed between the loop's stopping check and
# the spawn leaves the flag set with nobody to signal, so forward right after
# the pid exists. wait repeats until the process is really gone, so a graceful
# shutdown is never cut short.
func_run_miner() {
    "$@" &
    MINER_CHILD_PID=$!
    if [ "$MINER_STOPPING" -eq 1 ]; then
        func_forward_stop
    fi
    _stop_code=0
    while :; do
        # a stop interrupts wait and leaves 143 behind; keep that only while
        # the child is still alive, so a clean exit after the stop reports 0
        if wait "$MINER_CHILD_PID"; then
            _stop_code=0
        else
            _stop_code=$?
        fi
        kill -0 "$MINER_CHILD_PID" 2>/dev/null || break
    done
    MINER_CHILD_PID=""
    return "$_stop_code"
}

func_bootstrap() {
  func_get_architecture
  func_check_config
  if [ ! -x "$PROVIDER_BIN" ]; then
    log "[ERROR] $PROVIDER_BIN is missing or not executable"
    exit 1
  fi
  log "[INFO] Running $( "$PROVIDER_BIN" --version 2>/dev/null || echo 'unknown version' )"

  # Reuse a stored session only when it passes the local exp/nbf check. Same
  # helper and policy as the entrypoint: an expired session is not reused (it
  # can be replaced when a credential is available), and an unparseable one is
  # left in place for the miner to report on.
  session_reusable=0
  if [ -s "$SESSION_JWT" ]; then
    if command -v validate_jwt_file >/dev/null 2>&1 && ! validate_jwt_file "$SESSION_JWT"; then
      if command -v jwt_expired >/dev/null 2>&1 && jwt_expired "$SESSION_JWT"; then
        log "[WARN] Stored session at $SESSION_JWT is expired; not reusing it"
      else
        log "[WARN] Stored session at $SESSION_JWT could not be validated locally; leaving it in place for the miner to report on"
        session_reusable=1
      fi
    else
      session_reusable=1
      log "[INFO] Existing session found at $SESSION_JWT; skipping authentication"
    fi
  fi

  if [ "$session_reusable" -ne 1 ]; then
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
    elif [ -s "$SESSION_JWT" ]; then
      # The stored session is expired and no credential is available to
      # replace it; keep it in place and let the miner report the auth state
      # instead of failing the container here.
      log "[WARN] No credential is available to replace the expired session at $SESSION_JWT; the miner will report the auth state"
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
    func_exit_if_stopping
    func_provide
    if [ "$PROVIDE_CODE" -eq 0 ]; then
      log "[INFO] The miner exited cleanly"
      return 0
    fi
    if [ "$PROVIDE_CODE" -eq 75 ]; then
      log "[INFO] The miner asked for a planned restart (exit 75); restarting in 5s"
      failures=0
      func_sleep 5
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
          func_sleep 5
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
    func_sleep 15
  done
}

func_install_stop_trap
func_bootstrap
func_run
