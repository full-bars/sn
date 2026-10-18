#!/bin/sh
# Local JWT payload validation helper for the sn container scripts.
#
# Validates the exp and nbf claims with a 5 minute clock skew, and never logs
# or prints the token. validate_jwt_file returns 0 when the token looks
# usable, 1 when it is expired, not yet valid, or unparseable.
#
# Sourced by docker/scripts/{entrypoint,start_stable,start_nightly,start_jwt}.sh
# and by docker/miner/{entrypoint,start_miner}.sh. Keep the two copies
# (docker/scripts/jwt_verify.sh and docker/miner/jwt_verify.sh) identical;
# docker/miner/test_miner_entrypoint.sh asserts that.
#
# jwt_expired is the stricter probe the miner image uses to tell "definitely
# expired" apart from "could not validate": it returns 0 only when the token
# is past its exp claim beyond the skew, so a caller never destroys a session
# it cannot read.

validate_jwt_file() {
  jwt_path="$1"
  [ -f "$jwt_path" ] && [ -s "$jwt_path" ] || return 1

  # Read the token (trim whitespace and newlines)
  token_str="$(head -n 1 "$jwt_path" 2>/dev/null | tr -d '\r\n[:space:]')"
  [ -n "$token_str" ] || return 1

  # A JWT must consist of exactly 3 dot-separated segments
  num_parts="$(printf '%s' "$token_str" | awk -F. '{print NF}')"
  [ "$num_parts" -eq 3 ] || return 1

  header_seg="$(printf '%s' "$token_str" | cut -d. -f1)"
  payload_seg="$(printf '%s' "$token_str" | cut -d. -f2)"
  [ -n "$header_seg" ] && [ -n "$payload_seg" ] || return 1

  # Convert base64url to standard base64 and pad with '='
  b64_payload="$(printf '%s' "$payload_seg" | tr -- '-_' '+/')"
  pad_rem=$((${#b64_payload} % 4))
  case "$pad_rem" in
    2) b64_payload="${b64_payload}==" ;;
    3) b64_payload="${b64_payload}=" ;;
    1) return 1 ;;
    0) ;;
  esac

  decoded_json="$(printf '%s' "$b64_payload" | base64 -d 2>/dev/null)" || return 1
  [ -n "$decoded_json" ] || return 1

  if ! printf '%s' "$decoded_json" | jq empty 2>/dev/null; then
    return 1
  fi

  exp_claim="$(printf '%s' "$decoded_json" | jq -r '.exp // empty' 2>/dev/null)"
  nbf_claim="$(printf '%s' "$decoded_json" | jq -r '.nbf // empty' 2>/dev/null)"

  curr_time="$(date +%s 2>/dev/null)" || return 1
  skew_sec=300

  # A missing exp is accepted: the miner reports its own auth error for a token
  # the network rejects, and deleting a token we cannot read would cost the
  # operator a session for nothing.
  if [ -n "$exp_claim" ]; then
    case "$exp_claim" in
      ''|*[!0-9]*) return 1 ;;
    esac
    if [ "$((curr_time - skew_sec))" -gt "$exp_claim" ]; then
      return 1
    fi
  fi

  if [ -n "$nbf_claim" ]; then
    case "$nbf_claim" in
      ''|*[!0-9]*) return 1 ;;
    esac
    if [ "$((curr_time + skew_sec))" -lt "$nbf_claim" ]; then
      return 1
    fi
  fi

  return 0
}

# jwt_expired <file>: 0 only when the token is definitely past its exp claim
# (beyond the skew). Unparseable tokens return 1 so the caller leaves them
# alone rather than destroying a session it cannot read.
jwt_expired() {
  jwt_path="$1"
  [ -f "$jwt_path" ] && [ -s "$jwt_path" ] || return 1
  token_str="$(head -n 1 "$jwt_path" 2>/dev/null | tr -d '\r\n[:space:]')"
  [ -n "$token_str" ] || return 1
  [ "$(printf '%s' "$token_str" | awk -F. '{print NF}')" -eq 3 ] || return 1
  payload_seg="$(printf '%s' "$token_str" | cut -d. -f2)"
  b64_payload="$(printf '%s' "$payload_seg" | tr -- '-_' '+/')"
  case "$((${#b64_payload} % 4))" in
    2) b64_payload="${b64_payload}==" ;;
    3) b64_payload="${b64_payload}=" ;;
    1) return 1 ;;
  esac
  decoded_json="$(printf '%s' "$b64_payload" | base64 -d 2>/dev/null)" || return 1
  [ -n "$decoded_json" ] || return 1
  exp_claim="$(printf '%s' "$decoded_json" | jq -r '.exp // empty' 2>/dev/null)"
  [ -n "$exp_claim" ] || return 1
  case "$exp_claim" in
    ''|*[!0-9]*) return 1 ;;
  esac
  curr_time="$(date +%s 2>/dev/null)" || return 1
  [ "$((curr_time - 300))" -gt "$exp_claim" ] && return 0
  return 1
}
