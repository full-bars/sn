#!/bin/sh
# URNetwork Provider Entrypoint Script
# ------------------------------------
# This script bootstraps the URNetwork provider inside a container.
# Responsibilities:
#   - Validate environment and credentials
#   - Configure proxy if provided
#   - Detect system architecture
#   - Optionally check public IP
#   - Start vnStat monitoring and lightweight HTTP server
#   - Authenticate and obtain JWT
#   - Manage provider lifecycle (restart on crash)

# Exit immediately if any command fails
set -e

# === Configuration Variables ===
export TZ="America/Tijuana"
APP_DIR="/app"

ENABLE_VNSTAT="${ENABLE_VNSTAT:-true}"
ENABLE_IP_CHECKER="${ENABLE_IP_CHECKER:-false}"
IP_CHECKER_URL="https://raw.githubusercontent.com/techroy23/IP-Checker/refs/heads/main/app.sh"

# === Logging Helper ===
log() {
  echo "$(date '+%Y-%m-%d %H:%M:%S') >>> UrNetwork >>> $*"
}

# Resolve once before any state paths are used, including by child binaries.
resolve_state_dir() {
    STATE_DIR=$(printf '%s' "${URNETWORK_STATE_DIR:-}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//')
    STATE_DIR=${STATE_DIR:-${HOME:-/root}/.urnetwork}
    case "$STATE_DIR" in /*) ;; *) STATE_DIR="$PWD/$STATE_DIR" ;; esac
    if ! mkdir -p "$STATE_DIR"; then
        log "ERROR: Cannot create state directory '$STATE_DIR' (see mkdir error above)" >&2
        exit 1
    fi
    if ! resolved_state_dir=$(cd "$STATE_DIR" && pwd -P); then
        log "ERROR: Cannot access state directory '$STATE_DIR'" >&2
        exit 1
    fi
    STATE_DIR=$resolved_state_dir
    export URNETWORK_STATE_DIR="$STATE_DIR"
    if [ "${URNETWORK_STATE_DIR_LOGGED:-}" != 1 ]; then
        log "State directory: $STATE_DIR"
        export URNETWORK_STATE_DIR_LOGGED=1
    fi
}

resolve_state_dir
JWT_FILE="$STATE_DIR/jwt"

# === Stop Forwarding ===
# Same helpers as full-bars/urnetwork-3.23-fix docker/scripts/provider_common.sh,
# kept inline because this tree has no shared provider_common.sh (each start_*.sh
# is standalone). `docker stop` signals PID 1, which is this shell; with no
# default action for SIGTERM the provider never hears it and Docker SIGKILLs
# the container after the timeout, skipping the provider's clean shutdown.
PROVIDER_CHILD_PID=""
PROVIDER_STOPPING=0

func_forward_stop() {
    PROVIDER_STOPPING=1
    if [ -n "$PROVIDER_CHILD_PID" ]; then
        kill -TERM "$PROVIDER_CHILD_PID" 2>/dev/null || true
    fi
    # a hotswap successor is not this shell's child; skip the tracked child
    # (already signalled above) so the provider sees exactly one TERM
    for _successor in $(pgrep -f "^$APP_DIR/urnetwork_[^ ]* (auth-)?provide" 2>/dev/null); do
        [ "$_successor" = "$PROVIDER_CHILD_PID" ] || kill -TERM "$_successor" 2>/dev/null || true
    done
}

func_install_stop_trap() {
    trap func_forward_stop TERM INT
}

func_exit_if_stopping() {
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        # a hotswap successor is not our child, so wait cannot reap it; poll
        # (bounded below Docker's 10s stop timeout) so PID 1 does not exit
        # and SIGKILL the successor in the middle of its graceful shutdown
        _stop_i=0
        while [ "$_stop_i" -lt 8 ] \
            && pgrep -f "^$APP_DIR/urnetwork_[^ ]* (auth-)?provide" >/dev/null 2>&1; do
            sleep 1
            _stop_i=$((_stop_i+1))
        done
        log "[INFO] Stop requested; supervisor exiting."
        exit 0
    fi
}

# func_sleep is the interruptible supervisor sleep. busybox ash defers a
# trapped signal until the foreground command returns, so a bare foreground
# `sleep N` swallows a `docker stop` for its whole duration and Docker
# SIGKILLs the container; backgrounding the sleep lets the trap run. The
# wait keeps the elapsed time, and every sleep doubles as a stopping
# checkpoint. Both the entry and the post-spawn flag checks matter: a trap
# consumed at a command boundary between them leaves the flag set with no
# pending signal, and the sleep would otherwise run to its full duration.
func_sleep() {
    func_exit_if_stopping
    sleep "$1" &
    _sleep_pid=$!
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        kill "$_sleep_pid" 2>/dev/null || true
    fi
    wait "$_sleep_pid" || true
    func_exit_if_stopping
}

# func_run_provider runs the provider in the background so the stop trap can
# fire while it runs, and sets code to its exit status. `|| code=$?` keeps
# the run alive under set -e for a crashed provider or a wait interrupted by
# the TERM trap (status 143): a bare failing wait would abort the supervisor
# instead of handling the exit. wait repeats until the provider has really
# exited, so a graceful shutdown is never cut short.
func_run_provider() {
    "$@" &
    PROVIDER_CHILD_PID=$!
    # a TERM that landed between the loop's stopping check and this spawn
    # left the stop flag set with nobody to signal; forward now, otherwise
    # wait would outlive the stop request while the fresh provider runs on
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        func_forward_stop
    fi
    code=0
    while :; do
        wait "$PROVIDER_CHILD_PID" || code=$?
        kill -0 "$PROVIDER_CHILD_PID" 2>/dev/null || break
    done
    PROVIDER_CHILD_PID=""
    if [ "$PROVIDER_STOPPING" -eq 1 ]; then
        log "[INFO] Stop requested; UrNetwork exited (code=$code)."
        func_exit_if_stopping
    fi
}

# === Directory Validation ===
func_check_dir() {
    [ -d "$APP_DIR" ] || {
        log "[ERROR] APP_DIR '$APP_DIR' does not exist." >&2
        exit 1
    }
    cd "$APP_DIR" || {
        log "[ERROR] Cannot cd to '$APP_DIR'." >&2
        exit 1
    }
}

# === Credential Validation ===
func_check_credentials() {
    if [ -z "$USER_AUTH" ] || [ -z "$PASSWORD" ]; then
        log "[ERROR] USER_AUTH or PASSWORD not set"
        log "[ERROR] Please provide both -e USER_AUTH and -e PASSWORD"
        exit 1
    else
        log "[INFO] Credentials found"
    fi
}

# === Proxy Setup ===
func_check_proxy() {
    log "[INFO] Checking proxy configuration"
    proxy_dir="${STATE_DIR:-${URNETWORK_STATE_DIR:-$HOME/.urnetwork}}"
    proxy_config="$proxy_dir/proxy"
    if [ ! -f "/app/proxy.txt" ]; then
        if [ -f "$proxy_config" ]; then
            log "[INFO] No proxy.txt found; kept existing proxy configuration"
        else
            log "[INFO] No proxy.txt found; none configured"
        fi
        return 0
    fi

    # Build the replacement in the same filesystem, away from live state.
    # Preserve named auth entries while replacing the server list.
    if ! mkdir -p "$proxy_dir" || ! proxy_stage=$(mktemp -d "$proxy_dir/.proxy-import.XXXXXX"); then
        if [ -f "$proxy_config" ]; then
            log "[WARN] Proxy staging failed; kept existing configuration"
        else
            log "[WARN] Proxy staging failed; none configured"
        fi
        return 1
    fi
    if [ -f "$proxy_config" ]; then
        if ! jq -c 'select(type == "object") | .servers = {}' "$proxy_config" > "$proxy_stage/proxy" || [ ! -s "$proxy_stage/proxy" ]; then
            log "[WARN] Cannot read existing proxy configuration; kept it unchanged"
            rm -rf "$proxy_stage"
            return 1
        fi
    else
        if ! printf '%s\n' '{"servers":{}}' > "$proxy_stage/proxy"; then
            log "[WARN] Proxy staging failed; none configured"
            rm -rf "$proxy_stage"
            return 1
        fi
    fi
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
    if URNETWORK_STATE_DIR="$proxy_stage" "$PROVIDER_BIN" proxy add --proxy_file="/app/proxy.txt" -f &&
       [ -f "$proxy_stage/proxy" ] && chmod 600 "$proxy_stage/proxy" &&
       mv -f "$proxy_stage/proxy" "$proxy_config"; then
        log "[INFO] Proxy configuration replaced from proxy.txt"
    else
        if [ -f "$proxy_config" ]; then
            log "[WARN] Proxy replacement failed; kept existing configuration"
        else
            log "[WARN] Proxy replacement failed; none configured"
        fi
        rm -rf "$proxy_stage"
        return 1
    fi
    rm -rf "$proxy_stage"
}

# === Architecture Detection ===
func_get_architecture() {
    case "$(uname -m)" in
      x86_64)  A_SYS_ARCH=amd64  ;;
      aarch64) A_SYS_ARCH=arm64  ;;
      *)
        log "[ERROR] Unsupported arch $(uname -m)" >&2
        exit 1
        ;;
    esac
}

# === Custom Network Selection ===
# UR_API_URL / UR_CONNECT_URL let operators point the provider at a
# custom API + connect backend without a custom-built binary, using
# the `provider choose_network` command. Both must be set together
# (the binary's choose_network requires both positional args). Saved
# to the state directory as network.json, so — like the JWT — it survives
# restarts if that directory is on a mounted volume.
func_choose_network() {
    if [ -n "$UR_API_URL" ] && [ -n "$UR_CONNECT_URL" ]; then
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
        log "[INFO] Custom network requested: api_url=$UR_API_URL connect_url=$UR_CONNECT_URL"
        if ! "$PROVIDER_BIN" choose_network "$UR_API_URL" "$UR_CONNECT_URL"; then
            log "[ERROR] choose_network rejected UR_API_URL/UR_CONNECT_URL — check scheme (http(s):// for UR_API_URL, ws(s):// for UR_CONNECT_URL)"
            exit 1
        fi
    elif [ -n "$UR_API_URL" ] || [ -n "$UR_CONNECT_URL" ]; then
        log "[ERROR] UR_API_URL and UR_CONNECT_URL must both be set together"
        exit 1
    fi
}

# === Client Identity Reporting ===
# Fetches the public IP used to build this node's dashboard identity label
# (node name @ redacted-IP [version]). Always on; no configuration required.
# Distinct from func_ip_checker below, which is an opt-in diagnostic.
func_report_identity() {
  # Use curl ip.me -4 with a 5s timeout to avoid hanging startup
  export URNETWORK_PUBLIC_IP="$(curl -s --max-time 5 --retry 0 ip.me -4 || echo "")"
  if [ -n "$URNETWORK_PUBLIC_IP" ]; then
    log "[INFO] Public IP detected: $URNETWORK_PUBLIC_IP"
  else
    log "[WARN] Could not detect public IP (timeout or service unreachable)"
  fi
}

# === Public IP Checker (diagnostic) ===
# Opt-in (ENABLE_IP_CHECKER=true). Runs the external techroy23 IP-Checker
# script to log the full public IP to the console. Distinct from the dashboard
# reporter above (func_report_identity), which only sends a redacted IP to the backend.
func_ip_checker() {
  if [ "$ENABLE_IP_CHECKER" = "true" ]; then
    log "[INFO] Checking current public IP..."
    if ! ip_checker_file=$(mktemp "${TMPDIR:-/tmp}/urnetwork-ip-checker.XXXXXX"); then
      log "[WARN] IP checker fetch failed: could not create temporary file"
    elif ! curl -fsSL "$IP_CHECKER_URL" -o "$ip_checker_file"; then
      log "[WARN] IP checker fetch failed"
      rm -f "$ip_checker_file"
    else
      if sh "$ip_checker_file"; then
        log "[INFO] IP checker script ran successfully"
      else
        log "[WARN] IP checker script failed"
      fi
      rm -f "$ip_checker_file"
    fi
  else
    log "[INFO] IP checker disabled"
  fi
}

# === vnStat Monitoring Setup ===
func_start_vnstat() {
    VNSTAT_LC="$(printf '%s' "$ENABLE_VNSTAT" | tr '[:upper:]' '[:lower:]')"
    if [ "$VNSTAT_LC" = "true" ]; then
        if [ -f /var/lib/vnstat/vnstat.db ]; then
            log "[INFO] vnStat DB already exists (SQLite backend)"
        elif [ -f /var/lib/vnstat/.config ]; then
            log "[INFO] vnStat DB already exists (binary backend)"
        else
            log "[INFO] Initializing vnStat database"
            vnstatd --initdb
        fi
        vnstatd -d --alwaysadd >/dev/null 2>&1
        log "[INFO] vnstatd started"
        mkdir -p /app/www && ln -sf /app/cgi-bin /app/www/cgi-bin && httpd -f -p 8080 -h /app/www &
        log "[INFO] HTTP server started on container port 8080"
    else
        log "[INFO] VNSTAT disabled ..."
    fi
}

# === Authentication (JWT) ===
func_do_login() {
    PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"

    # If a JWT already exists (e.g. persisted via a Docker volume), skip auth entirely.
    # This is the Watchtower-safe path: container restarts after an image update reuse
    # the existing token rather than hitting the API again with a spent auth code.
    if [ -s "$JWT_FILE" ]; then
        log "[INFO] Existing JWT found at $JWT_FILE — skipping authentication"
        return 0
    fi

    log "[INFO] No JWT found, starting authentication flow..."

    # Retry loop for authentication
    while true; do
        func_exit_if_stopping
        log "[INFO] Sleeping 15s before obtaining new JWT..."
        func_sleep 15
        
        log "[INFO] Attempting authentication..."
        
        # Capture the auth command's real exit status: `|| true` followed by
        # $? always recorded 0, hiding every failure code (same fix as the
        # nightly/stable supervisor loops in full-bars/urnetwork-3.23-fix).
        AUTH_EXIT_CODE=0
        AUTH_OUTPUT=$("$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f 2>&1) || AUTH_EXIT_CODE=$?
        PANIC_LINE=$(echo "$AUTH_OUTPUT" | grep -E '^panic:' || true)
        
        if [ "${DEBUG:-false}" = "true" ]; then
            if [ -n "$USER_AUTH" ]; then user_auth_set=yes; else user_auth_set=no; fi
            if [ -n "$PASSWORD" ]; then password_set=yes; else password_set=no; fi
            echo "DEBUG: USER_AUTH set=$user_auth_set length=${#USER_AUTH}"
            echo "DEBUG: PASSWORD set=$password_set length=${#PASSWORD}"
            echo "DEBUG: AUTH_EXIT_CODE=$AUTH_EXIT_CODE"
            # Preserve the output line without exposing echoed credentials.
            case "$AUTH_OUTPUT" in
                *"$USER_AUTH"*|*"$PASSWORD"*) echo "DEBUG: AUTH_OUTPUT=[redacted]" ;;
                *) echo "DEBUG: AUTH_OUTPUT=$AUTH_OUTPUT" ;;
            esac
        fi
        
        # Check for success message in output
        if echo "$AUTH_OUTPUT" | grep -q "Jwt written to"; then
            log "[INFO] Authentication successful - JWT written"
            func_sleep 5
            
            # Verify JWT file exists as backup check
            if [ -s "$JWT_FILE" ]; then
                log "[INFO] JWT file verified at $JWT_FILE"
                func_sleep 5
                return 0
            else
                log "[WARN] Success message found but JWT file missing - retrying"
                log "[INFO] Will retry authentication in 1 minutes (60 seconds)..."
                func_sleep 60
            fi
        else
            # Authentication failed - output exit code and auth output
            log "[ERROR] Authentication failed (exit code: $AUTH_EXIT_CODE)" >&2
			log "[ERROR] $(echo "$PANIC_LINE" | tr '[:lower:]' '[:upper:]')" >&2
            log "[INFO] Will retry authentication in 5 minutes (300 seconds)..."
            func_sleep 300
        fi
    done
}

# === Provider Lifecycle Management ===
func_start_provider(){
    failures=0
    while :; do
        func_exit_if_stopping
        log "[INFO] Starting UrNetwork (attempt #$((failures+1)))"
        PROVIDER_BIN="$APP_DIR/urnetwork_${A_SYS_ARCH}_stable"
		BIN_VER="$($PROVIDER_BIN --version)"
		log "[INFO] Running UrNetwork build v${BIN_VER}"
        # --allow-client-registration is required on a first run: without it the
        # provider custody gate refuses to create .provider.key and the process
        # exits 1, leaving a container that is Up with a valid JWT but has no
        # provider in it (the smoke job's first in-container command then fails
        # with "no providers found on this box"). The gate is still the safety
        # net: with AllowCreate set it refuses anyway when any retained .provider*
        # history exists (provider_key_missing_with_retained_history), so this
        # cannot silently replace a lost identity. Every other lane already passes
        # the flag (tool-functional-smoke.yml, systemd-smoke.sh, distro-smoke.sh,
        # freebsd-smoke.sh, lsm-smoke.sh).
        func_run_provider "$PROVIDER_BIN" provide --allow-client-registration
        if [ "$code" -eq 0 ]; then
            if [ -f "${JWT_FILE%/jwt}/update-pending" ]; then
                rm -f "${JWT_FILE%/jwt}/update-pending"
                log "[INFO] Binary updated — restarting provider."
                failures=0
                continue
            fi
            if [ -f "${JWT_FILE%/jwt}/hotswap-pending" ] || pgrep -f "urnetwork.*provide" >/dev/null 2>&1; then
                rm -f "${JWT_FILE%/jwt}/hotswap-pending"
                log "[INFO] UrNetwork hotswap successor active; continuing supervision."
                failures=0
                while pgrep -f "urnetwork.*provide" >/dev/null 2>&1; do
                    func_sleep 2
                done
                func_exit_if_stopping
                continue
            fi
            log " [INFO] UrNetwork exited cleanly."
            break
        fi
        if [ "$code" -eq 78 ]; then
            log "[WARN] Provider exited with auth error (code 78) — token expired or revoked."
            rm -f "$JWT_FILE"
            if [ -n "$USER_AUTH" ] && [ -n "$PASSWORD" ]; then
                log "[INFO] Re-authenticating with USER_AUTH/PASSWORD..."
                if "$PROVIDER_BIN" auth --user_auth="$USER_AUTH" --password="$PASSWORD" -f && [ -s "$JWT_FILE" ]; then
                    log "[INFO] Re-authentication successful. Restarting provider..."
                    func_sleep 5
                    continue
                else
                    log "[ERROR] Re-authentication failed."
                fi
            fi
            log "[CRITICAL] Token expired/revoked and credentials unavailable. Check USER_AUTH/PASSWORD and restart."
            func_sleep 30
            exit 78
        fi
        if [ "$code" -eq 75 ]; then
            log "[INFO] UrNetwork requested planned restart (exit 75); restarting in 5s."
            failures=0
            func_sleep 5
            continue
        fi
        failures=$((failures+1))
        log "[WARN] UrNetwork crashed (#$failures; code=$code)"
        if [ "$failures" -ge 3 ]; then
            log "[ERROR] Too many crashes; clearing JWT and reauthenticating"
            rm -f "$JWT_FILE" || true
            func_do_login
            failures=0
        fi
        log "[INFO] Waiting 60s before retry"
        func_sleep 60
    done
}

# === Bootstrap Sequence ===
func_bootstrap() {
    # sh /app/urnetwork_ipinfo.sh
	func_get_architecture
	func_choose_network
	func_check_dir
	func_check_credentials
    func_check_proxy
    func_report_identity
    func_ip_checker
    func_start_vnstat
    # Pass host's actual hostname (if provided) so provider reports correctly on dashboard
    export HOST_HOSTNAME="${HOST_HOSTNAME:-}"
    func_do_login
    func_start_provider
}

# === Main Entrypoint ===
main() {
    func_install_stop_trap
    func_bootstrap
}

main "$@"
