#!/bin/sh
# provider_env.sh: map the v33 operator environment variables onto
# `provide` flags for the sn miner image.
#
# Ported from the 3.23-fix image helper of the same name
# (docker/scripts/provider_env.sh in the v33 tree) and sourced by
# start_miner.sh. Every variable is optional: with none of them set,
# provider_env_flags prints nothing and the provide command line is exactly
# the legacy `provide`.
#
# Flags are spelled as the miner's usage declares them:
#   --all-operators, --auto-register, --hotkey_seed_file=<file>,
#   --allow-client-registration, --max-memory=<mem>.
#
#   ALL_OPERATORS=1|true|yes|on|Y
#       provide --all-operators: one child miner per listed operator.
#   AUTO_REGISTER=1|true|yes|on|Y
#       With ALL_OPERATORS only: sign in to each operator with the hotkey.
#       Needs HOTKEY_SEED_FILE.
#   HOTKEY_SEED_FILE=<path>
#       The hotkey seed for `auth --operator` sign-in and for
#       --auto-register. Must not contain whitespace. Without
#       ALL_OPERATORS it is left to the auth path and not passed to provide.
#   ALLOW_CLIENT_REGISTRATION=auto|Y|N
#       Y adds --allow-client-registration, N never does, and auto never does
#       either (set Y to enable client registration).
#   MAX_MEMORY=<mem>
#       --max-memory=<mem>, for example 2gib. Must not contain whitespace.
#   PROVIDER_ARGS="<args>"
#       Extra arguments appended to `provide`, split on whitespace. Use this
#       for flags that have no dedicated variable.
#
# Documented differences from the 3.23-fix helper:
#   * ALLOW_CLIENT_REGISTRATION=auto never adds the flag. The 3.23-fix helper
#     added it for a fresh identity (no .provider.key and no operators/); the
#     miner keeps its own documented behavior instead.
#   * HOTKEY_SEED_FILE without ALL_OPERATORS is not refused: the miner uses the
#     seed for `auth --operator`, while the 3.23-fix helper only feeds provide.
#   * PROVIDER_PORT and V are deliberately absent: neither exists in the v33
#     scripts (verified in wt-rc6 @ 9bdb0909a), so neither is mapped here.

# Print 1 for a true value, 0 for a false or empty value, and return 2 for
# anything else so callers can fail on a typo instead of silently changing
# the command line. Accepts the 3.23-fix spellings plus the miner image's
# Y/N convention.
provider_env_bool() {
  case "$(printf '%s' "${1:-}" | tr '[:upper:]' '[:lower:]')" in
    ""|0|false|no|off|n) printf '0\n' ;;
    1|true|yes|on|y)     printf '1\n' ;;
    *) return 2 ;;
  esac
}

# True when ALL_OPERATORS asks for provide --all-operators.
provider_env_all_operators() {
  [ "$(provider_env_bool "${ALL_OPERATORS:-}")" = "1" ]
}

# Print the ALLOW_CLIENT_REGISTRATION mode: y, n or auto. Returns 2 for an
# unknown value so the caller can fail closed.
provider_env_allow_client_registration_mode() {
  case "$(printf '%s' "${ALLOW_CLIENT_REGISTRATION:-}" | tr '[:upper:]' '[:lower:]')" in
    ""|n|no|false|0) printf '%s\n' n ;;
    y|yes|true|1)    printf '%s\n' y ;;
    auto)            printf '%s\n' auto ;;
    *) return 2 ;;
  esac
}

_provider_env_error() {
  if command -v log >/dev/null 2>&1; then
    log "[ERROR] $*" >&2
  else
    printf 'ERROR: %s\n' "$*" >&2
  fi
}

# Print the extra `provide` arguments, one per line. Returns non-zero with an
# ERROR line on stderr when the environment is inconsistent.
#
# The optional state-directory argument is accepted for compatibility with the
# 3.23-fix helper; the miner's `auto` mode does not inspect it.
provider_env_flags() {
  _pe_state_dir="${1:-${HOME:-/root}/.urnetwork}"

  if ! _pe_all="$(provider_env_bool "${ALL_OPERATORS:-}")"; then
    _provider_env_error "ALL_OPERATORS must be 1/true/yes/on or 0/false/no/off (got '${ALL_OPERATORS:-}')"
    return 1
  fi
  if ! _pe_auto="$(provider_env_bool "${AUTO_REGISTER:-}")"; then
    _provider_env_error "AUTO_REGISTER must be 1/true/yes/on or 0/false/no/off (got '${AUTO_REGISTER:-}')"
    return 1
  fi

  _pe_hotkey="${HOTKEY_SEED_FILE:-}"
  case "$_pe_hotkey" in
    *[[:space:]]*)
      _provider_env_error "HOTKEY_SEED_FILE must not contain whitespace (got '$_pe_hotkey')"
      return 1
      ;;
  esac

  # --auto-register only exists in the `provide --all-operators` usage. The
  # missing-seed error is checked first, matching the miner image's original
  # validation order.
  if [ "$_pe_auto" = "1" ] && [ -z "$_pe_hotkey" ]; then
    _provider_env_error "AUTO_REGISTER=Y requires HOTKEY_SEED_FILE to be set"
    return 1
  fi
  if [ "$_pe_all" != "1" ] && [ "$_pe_auto" = "1" ]; then
    _provider_env_error "AUTO_REGISTER=Y requires ALL_OPERATORS=Y (the flag only exists on provide --all-operators)"
    return 1
  fi

  if [ "$_pe_all" = "1" ]; then
    printf '%s\n' --all-operators
    [ "$_pe_auto" = "1" ] && printf '%s\n' --auto-register
    [ -z "$_pe_hotkey" ] || printf '%s\n' "--hotkey_seed_file=$_pe_hotkey"
  fi

  _pe_max_memory="${MAX_MEMORY:-}"
  case "$_pe_max_memory" in
    *[[:space:]]*)
      _provider_env_error "MAX_MEMORY must not contain whitespace (got '$_pe_max_memory')"
      return 1
      ;;
  esac
  [ -z "$_pe_max_memory" ] || printf '%s\n' "--max-memory=$_pe_max_memory"

  if ! _pe_allow="$(provider_env_allow_client_registration_mode)"; then
    _provider_env_error "ALLOW_CLIENT_REGISTRATION must be auto, Y or N (got '${ALLOW_CLIENT_REGISTRATION:-}')"
    return 1
  fi
  [ "$_pe_allow" = "y" ] && printf '%s\n' --allow-client-registration

  if [ -n "${PROVIDER_ARGS:-}" ]; then
    case $- in *f*) _pe_glob_off=1 ;; *) _pe_glob_off=0 ;; esac
    set -f
    # shellcheck disable=SC2086  # PROVIDER_ARGS is deliberately split on whitespace
    printf '%s\n' ${PROVIDER_ARGS}
    [ "$_pe_glob_off" = "1" ] || set +f
  fi

  unset _pe_state_dir _pe_all _pe_auto _pe_hotkey _pe_max_memory _pe_allow _pe_glob_off
  return 0
}
