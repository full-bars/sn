#!/bin/sh
set -e
# Entrypoint script for selecting and starting the build of UrNetwork.
#
# Usage:
#   AUTH=<userpass|authcode> ./entrypoint.sh [AUTH_CODE]
#
# Environment Variables:
#   AUTH  - Select userpass (USER_AUTH and PASSWORD) or authcode
#           (URNETWORK_AUTH_CODE, AUTHCODE, a positional code or stored session).
#   BUILD - Legacy selector: stable, jwt or nightly. Explicit BUILD wins over
#           AUTH; nightly is available only through BUILD=nightly.
#
# With neither selector, an environment or positional auth code selects jwt;
# otherwise stable is used. Selectors are case-insensitive.

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
}

ORIG_BUILD="${BUILD:-}"
AUTH_RAW="${AUTH:-}"
AUTH_LC="$(printf '%s' "$AUTH_RAW" | tr '[:upper:]' '[:lower:]')"
AUTH_DERIVED_BUILD=""
case "$AUTH_LC" in
  "") ;;
  userpass) AUTH_DERIVED_BUILD="stable" ;;
  authcode) AUTH_DERIVED_BUILD="jwt" ;;
  *)
    log "ERROR: Invalid AUTH '$AUTH_RAW'. Valid options are: userpass, authcode; nightly is available only via BUILD=nightly"
    exit 1
    ;;
esac

if [ -n "$ORIG_BUILD" ]; then
  BUILD="$(printf '%s' "$ORIG_BUILD" | tr '[:upper:]' '[:lower:]')"
  if [ -z "$AUTH_LC" ]; then
    log "BUILD is deprecated for auth selection; use AUTH=userpass or AUTH=authcode (nightly still requires BUILD=nightly)"
  elif [ "$BUILD" != "$AUTH_DERIVED_BUILD" ]; then
    log "WARNING: Conflicting AUTH='$AUTH_RAW' and BUILD='$ORIG_BUILD'; BUILD takes precedence"
  fi
elif [ -n "$AUTH_LC" ]; then
  BUILD="$AUTH_DERIVED_BUILD"
elif [ "$#" -gt 0 ] || [ -n "${URNETWORK_AUTH_CODE:-}" ]; then
  BUILD="jwt"
else
  BUILD="stable"
fi

# Validate only when AUTH selects the effective build; a conflicting BUILD
# must retain its existing behavior. Legacy BUILD and auto-selection are unchanged.
if [ -n "$AUTH_LC" ] && [ "$BUILD" = "$AUTH_DERIVED_BUILD" ]; then
  if [ "$AUTH_LC" = "userpass" ]; then
    missing=""
    [ -z "${USER_AUTH:-}" ] && missing="USER_AUTH"
    [ -z "${PASSWORD:-}" ] && missing="${missing:+$missing and }PASSWORD"
    if [ -n "$missing" ]; then
      log "ERROR: AUTH=userpass requires $missing to be set"
      exit 1
    fi
  else
    # Match the miner image's AUTHCODE alias without changing legacy defaults.
    if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ -n "${AUTHCODE:-}" ]; then
      export URNETWORK_AUTH_CODE="$AUTHCODE"
    fi
    if [ -z "${URNETWORK_AUTH_CODE:-}" ] && [ "$#" -eq 0 ] && [ ! -s "$HOME/.urnetwork/jwt" ]; then
      log "ERROR: AUTH=authcode requires AUTHCODE or URNETWORK_AUTH_CODE, a positional auth code or a stored session at $HOME/.urnetwork/jwt"
      exit 1
    fi
  fi
fi

# Translate TURBO=v4|v8 into URNETWORK_PROFILE so the binary picks it up.
# GOGC is handled internally by the binary when turbo is active.
TURBO="$(echo "${TURBO:-}" | tr '[:upper:]' '[:lower:]')"
case "$TURBO" in
  v4|v8)
    export URNETWORK_PROFILE="turbo-${TURBO}"
    log "Turbo mode: ${TURBO} (window=$([ "$TURBO" = "v4" ] && echo 4 || echo 8)MiB)"
    ;;
  "")
    ;;
  *)
    log "WARNING: unknown TURBO value '$TURBO' — ignoring (valid: v4, v8)"
    ;;
esac

log "Script version: v2026"
#log "Starting with"
#log "*** *** *** *** *** *** *** *** *** ***"
#log "USER_AUTH = $USER_AUTH"
#log "PASSWORD  = $PASSWORD"
#log "AUTH-CODE = $AUTHCODE $JWT_TOKEN"
#log "BUILD     = $BUILD"
#log "PELICAN   = $PELICAN"
#log "*** *** *** *** *** *** *** *** *** ***"

# === Helper to run as pelican if requested ===
run_as_pelican() {
  log "Running Pelican Panel mode..."
  exec /app/pelican_panel.sh
}

run_normal() {
  case "$BUILD" in
    stable)
      exec /app/start_stable.sh
      ;;
    nightly)
      exec /app/start_nightly.sh
      ;;
    jwt)
      if [ "$#" -eq 0 ] && [ -s "$HOME/.urnetwork/jwt" ]; then
        log "Existing session found — skipping auth"
        exec /app/start_jwt.sh
      elif [ "$#" -eq 0 ] && [ -n "$URNETWORK_AUTH_CODE" ]; then
        log "Authentication requested via environment variable"
        exec /app/start_jwt.sh
      elif [ "$#" -ne 1 ]; then
        log "ERROR: jwt mode requires a JWT token on first run"
        log "ERROR: Usage: docker run ... IMAGE <JWT_TOKEN>"
        log "ERROR: On subsequent starts the session is read from the $HOME/.urnetwork volume automatically."
        exit 1
      fi
      log "Entrypoint received $# arguments"
      JWT_TOKEN="$1"
      exec /app/start_jwt.sh "$JWT_TOKEN"
      ;;
    *)
      log "Invalid build: $BUILD"
      log "Valid options are: stable, nightly, jwt"
      exit 1
      ;;
  esac
}

# Route based on PELICAN setting
if [ "$PELICAN" = "yes" ]; then
  run_as_pelican
else
  run_normal "$@"
fi
