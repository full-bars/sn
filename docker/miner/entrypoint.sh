#!/bin/sh
# Entrypoint for the sn miner image.
#
# Normalizes the AUTH onboarding environment, validates it fail-closed, warns
# about the state volume and the payout wallet, then hands over to
# start_miner.sh which authenticates and runs `provider provide`.
#
# AUTH=userpass requires USER_AUTH and PASSWORD.
# AUTH=authcode requires AUTHCODE (alias of URNETWORK_AUTH_CODE), a positional
# code, or a stored session under the state directory.
# OPERATOR=<domain> selects the per-operator sign-in (auth --operator) and the
# per-operator session path <state>/operators/<domain>/jwt.
set -e

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
if [ -f "$SCRIPT_DIR/jwt_verify.sh" ]; then
  . "$SCRIPT_DIR/jwt_verify.sh"
fi

STATE_DIR="${URNETWORK_STATE_DIR:-${HOME:-/root}/.urnetwork}"
[ -d "$STATE_DIR" ] || STATE_DIR="${HOME:-/root}/.urnetwork"
OPERATOR_STATE_DIR="$STATE_DIR/operators/${OPERATOR:-}"

# AUTHCODE is the image-facing alias for the miner's network auth code.
if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ -n "${AUTHCODE:-}" ]; then
  export URNETWORK_AUTH_CODE="$AUTHCODE"
fi

AUTH_RAW="${AUTH:-}"
AUTH_LC="$(printf '%s' "$AUTH_RAW" | tr '[:upper:]' '[:lower:]')"

# BUILD deprecation hint and conflict resolution (the v33 entrypoint
# contract). The 3.23-fix entrypoint derives the build from AUTH and lets an
# explicit BUILD win a conflict; this image selects the auth path with AUTH
# only, so BUILD is accepted, ignored, and AUTH always wins.
if [ -n "${BUILD:-}" ]; then
  case "$AUTH_LC" in
    userpass) AUTH_EXPECTED_BUILD="stable" ;;
    authcode) AUTH_EXPECTED_BUILD="jwt" ;;
    *)        AUTH_EXPECTED_BUILD="" ;;
  esac
  BUILD_LC="$(printf '%s' "$BUILD" | tr '[:upper:]' '[:lower:]')"
  if [ -z "$AUTH_LC" ]; then
    log "WARNING: BUILD is deprecated for auth selection, use AUTH=userpass|authcode (BUILD='$BUILD' is not supported by the miner image)"
  elif [ -n "$AUTH_EXPECTED_BUILD" ] && [ "$BUILD_LC" != "$AUTH_EXPECTED_BUILD" ]; then
    log "WARNING: Conflicting AUTH='$AUTH_RAW' and BUILD='$BUILD' specified; the miner image ignores BUILD and uses AUTH='$AUTH_LC'"
  else
    log "WARNING: BUILD is not supported by the miner image and is ignored (AUTH='$AUTH_LC' applies)"
  fi
fi

if [ -z "$AUTH_LC" ]; then
  if [ -n "${URNETWORK_AUTH_CODE:-}" ] || [ "$#" -gt 0 ]; then
    AUTH_LC="authcode"
  elif [ -n "${OPERATOR:-}" ] && [ -s "$OPERATOR_STATE_DIR/jwt" ]; then
    AUTH_LC="authcode"
  elif [ -s "$STATE_DIR/jwt" ]; then
    AUTH_LC="authcode"
  elif is_true "${ALL_OPERATORS:-}"; then
    # All-operators mode works from the per-operator sessions (and can sign in
    # with the hotkey seed), so no network credential is required here.
    AUTH_LC="authcode"
  elif [ -n "${USER_AUTH:-}" ] || [ -n "${PASSWORD:-}" ]; then
    AUTH_LC="userpass"
  fi
fi

case "$AUTH_LC" in
  userpass)
    if [ -z "${USER_AUTH:-}" ] || [ -z "${PASSWORD:-}" ]; then
      missing=""
      [ -z "${USER_AUTH:-}" ] && missing="USER_AUTH"
      [ -z "${PASSWORD:-}" ] && missing="${missing:+$missing and }PASSWORD"
      log "ERROR: AUTH=userpass requires $missing to be set"
      exit 1
    fi
    ;;
  authcode)
    if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ "$#" -eq 0 ] && [ -z "${HOTKEY_SEED_FILE:-}" ]; then
      stored=""
      [ -s "$STATE_DIR/jwt" ] && stored="1"
      if [ -n "${OPERATOR:-}" ] && [ -s "$OPERATOR_STATE_DIR/jwt" ]; then
        stored="1"
      fi
      if [ -z "$stored" ]; then
        if is_true "${ALL_OPERATORS:-}"; then
          # provide --all-operators works from each operator's own session under
          # <state>/operators/<domain>/jwt, and --auto-register can sign in with
          # the hotkey seed. Missing sessions are reported as awaiting auth.
          log "INFO: ALL_OPERATORS is set without an auth code; operators without a stored session will be reported as awaiting auth"
        else
          log "ERROR: AUTH=authcode requires AUTHCODE, HOTKEY_SEED_FILE (or a stored session under $STATE_DIR)"
          exit 1
        fi
      fi
    fi
    ;;
  "")
    log "ERROR: set AUTH=userpass|authcode (with AUTHCODE, or USER_AUTH and PASSWORD)"
    exit 1
    ;;
  *)
    log "ERROR: Invalid AUTH '$AUTH_RAW'. Valid options are: userpass, authcode"
    exit 1
    ;;
esac
export AUTH="$AUTH_LC"

# Local session check. A stored session is dropped ONLY when it is definitely
# expired (beyond the 5 minute skew) and a way to re-authenticate exists;
# otherwise it is left in place for the miner to report on. Deleting a token
# this script cannot parse would cost the operator a single-use auth code.
SESSION_JWT="$STATE_DIR/jwt"
if [ -n "${OPERATOR:-}" ] && [ -s "$OPERATOR_STATE_DIR/jwt" ]; then
  SESSION_JWT="$OPERATOR_STATE_DIR/jwt"
fi

if [ -s "$SESSION_JWT" ]; then
  if command -v validate_jwt_file >/dev/null 2>&1; then
    if validate_jwt_file "$SESSION_JWT"; then
      log "INFO: stored session accepted by the local exp/nbf check"
    else
      if command -v jwt_expired >/dev/null 2>&1 && jwt_expired "$SESSION_JWT"; then
        if [ -n "${URNETWORK_AUTH_CODE:-}" ] || [ "$#" -gt 0 ] || { [ "$AUTH_LC" = "userpass" ]; }; then
          log "WARNING: stored session is expired; removing it to re-authenticate"
          rm -f "$SESSION_JWT"
        else
          log "WARNING: stored session is expired and no credentials are provided to re-authenticate; leaving it in place"
        fi
      else
        log "WARNING: stored session could not be validated locally; leaving it in place for the miner to report on"
      fi
    fi
  fi
fi

# Startup safety messages.
is_mounted() {
  target="$1"
  if command -v mountpoint >/dev/null 2>&1; then
    mountpoint -q "$target" 2>/dev/null && return 0
  fi
  if [ -r /proc/self/mountinfo ]; then
    awk -v d="$target" '$5 == d { found=1 } END { exit !found }' /proc/self/mountinfo 2>/dev/null && return 0
  fi
  return 1
}

if ! is_mounted "$STATE_DIR" && ! { [ -s "$STATE_DIR/jwt" ] || [ -d "$STATE_DIR/operators" ]; }; then
  log "WARNING: Without a persistent volume mounted at $STATE_DIR every container restart creates a NEW miner identity."
fi

log "INFO: A payout wallet and consent must be set in the URnetwork app or website (or with the provider wallet commands) for the miner to be paid."

exec "${APP_DIR:-/app}/start_miner.sh" "$@"
