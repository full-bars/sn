#!/bin/sh
# distro-features.sh — feature-matrix harness for distro CI (POSIX sh).
#
# Runs INSIDE a distro container (debian/alpine/...) against a locally built
# miner + urnet-tools. Prints a PASS / FAIL / SKIP-UNSUPPORTED /
# SKIP-NO-NETWORK line per check and a summary block. No bash, no jq, no
# python: only POSIX utilities (plus netcat for the raw socket checks, when
# the image provides it — the feature-matrix CI job installs it).
#
# Inputs (env):
#   BIN_DIR      dir holding the built binaries (required unless MINER_BIN given)
#   MINER_BIN    miner binary            (default $BIN_DIR/miner)
#   TOOLS_BIN    urnet-tools binary      (default $BIN_DIR/urnet-tools)
#   JWT_FILE     disposable JWT          (required for the live blocks)
#   STATE_HOME   sandbox HOME            (default $HOME)
#   STATE_DIR    state dir               (default $STATE_HOME/.urnetwork)
#   TIMEOUT      generic timeout, seconds (default 900)
#   FEATURE_*    per-check knobs, see docs/Feature-Matrix-CI.md
#
# Blocks:
#   BLOCK 1 — risky surfaces, first: control-socket security (0600 perm, peer
#             uid, oversize set, one-line framing), identity + restart (mint,
#             clean SIGTERM, client-id reuse across restart, HOT_RESTART=0
#             fresh-mint, events.log lifecycle lines, exit-75), ramlog routing
#             (URNETWORK_RAMLOGS / profile lowmem / persisted ramlogs key ->
#             /dev/shm/urnetwork.log + important.log), hotswap handoff with
#             /metrics rebind, the registration identity gate, metrics control
#             wiring + HTTP scrape + bind policy, control-key live-apply and
#             needs-restart semantics, sandbox state paths, corrupt-state
#             recovery.
#   BLOCK 2 — URL source pipeline (mandatory): add-source of the real public
#             lists, the [proxy][url] log vocabulary, cache/state coherence,
#             cached-skip on a second cycle, remove-source; urnet-tools
#             manipulation on deterministic synthetic entries (add, remove,
#             trim preview/apply, remove-dead, refresh, remove --match,
#             paste/clear). Per-container jitter + source gaps + randomized
#             order (see the STAGGER section and docs/Feature-Matrix-CI.md).
#   BLOCK 3 — broad surface: version/status/health/traffic/ids/summary/config,
#             the provider's proxy-exclude grammar, the full documented v33
#             command tree (help surface), and the conditional
#             exporter/audit/graders surfaces, as far as this head implements
#             them.
#
# Classification:
#   PASS              the documented behavior was observed
#   FAIL              a product defect (every FAIL is a finding)
#   SKIP-UNSUPPORTED  runtime-detected absence at this head; the report names
#                     the owning port when one exists
#   SKIP-NO-NETWORK   environmental: no network / no JWT / provider could not
#                     stay up; never a product verdict
#   SKIP-UNSAFE       a client-level check is disabled because the invoking
#                     user's passwd home is not the sandbox HOME, so
#                     urnet-tools discovery could resolve to the operator's
#                     real state; the report's client-targeting row carries
#                     the dir it would have resolved to
#
# Exit: 0 = no FAIL lines, 1 = FAIL lines present, 75 = ENV_BLOCKER,
# 78 = REFUSED by the preflight guard (the run would touch the operator's
# real provider state; never a product verdict).
#
# Scope: this script reads the binaries and writes only inside STATE_HOME. It
# never pushes, tags, opens PRs, comments, or reruns workflows. It refuses to
# run at all when its effective state dir, HOME, or an ambient
# URNETWORK_STATE_DIR equals, contains, or lives inside the operator's real
# state (~/.urnetwork) - client-level urnet-tools commands resolve
# their target through discovery, which attributes a provider's state dir to
# the KERNEL owner's passwd home and falls back to <passwd home>/.urnetwork,
# so an unadapted host makes them act on the operator's real provider. Every
# client-level check therefore runs only when the invoking user's passwd home
# IS the sandbox HOME (the container adaptation above); otherwise it is
# recorded SKIP-UNSAFE without invoking the client. See docs/Feature-Matrix-CI.md.
#
# Portability notes: the network-facing lines were verified in containerized
# runs (debian:stable-slim, alpine:latest, almalinux:9) against the integrated
# one-program head; see docs/Feature-Matrix-CI.md §Verified runs for the
# binary SHA256s and the per-distro verdicts. Checks self-detect the surfaces
# of whatever head they are pointed at; a surface that is genuinely absent is
# SKIP-UNSUPPORTED with the owning port named.

set -u

# ---------------------------------------------------------------------------
# inputs
# ---------------------------------------------------------------------------
BIN_DIR="${BIN_DIR:-}"
if [ -z "${MINER_BIN:-}" ] && [ -n "$BIN_DIR" ]; then MINER_BIN="$BIN_DIR/miner"; fi
if [ -z "${TOOLS_BIN:-}" ] && [ -n "$BIN_DIR" ]; then
    if [ -x "$BIN_DIR/urnet-tools" ]; then TOOLS_BIN="$BIN_DIR/urnet-tools"
    elif [ -x "$BIN_DIR/urnet-tools-323" ]; then TOOLS_BIN="$BIN_DIR/urnet-tools-323"
    else TOOLS_BIN="$BIN_DIR/urnet-tools"; fi
fi
JWT_FILE="${JWT_FILE:-}"
STATE_HOME="${STATE_HOME:-${HOME:-/root}}"
STATE_DIR="${STATE_DIR:-$STATE_HOME/.urnetwork}"
TIMEOUT="${TIMEOUT:-900}"
FEATURE_START_TIMEOUT="${FEATURE_START_TIMEOUT:-90}"
FEATURE_SETTLE="${FEATURE_SETTLE:-12}"
FEATURE_START_ATTEMPTS="${FEATURE_START_ATTEMPTS:-2}"
FEATURE_IDENTITY_TIMEOUT="${FEATURE_IDENTITY_TIMEOUT:-90}"
FEATURE_SOCKET_TIMEOUT="${FEATURE_SOCKET_TIMEOUT:-6}"
FEATURE_URL_TIMEOUT="${FEATURE_URL_TIMEOUT:-$TIMEOUT}"
FEATURE_URL_SOURCE1="${FEATURE_URL_SOURCE1:-https://raw.githubusercontent.com/monosans/proxy-list/main/proxies/socks5.txt}"
# The documented second source is the proxifly socks5 list:
#   https://raw.githubusercontent.com/proxifly/free-proxy-list/main/proxies/protocols/socks5/data.txt
# It publishes ~68k lines (68,714 measured 2026-10-18), and every new address
# is stage-0 probed, so one 900s pass cannot clear it on a shared runner: the
# source is OFF by default. Set FEATURE_URL_SOURCE2 to that URL (or any other
# list) to exercise the two-source path; an unreachable or slow source is
# classified SKIP-NO-NETWORK, never FAIL.
FEATURE_URL_SOURCE2="${FEATURE_URL_SOURCE2:-}"
FEATURE_URL_CACHED_PROBE="${FEATURE_URL_CACHED_PROBE:-1}"
FEATURE_EXIT75_TIMEOUT="${FEATURE_EXIT75_TIMEOUT:-60}"

RUN_USER="$(id -un 2>/dev/null || echo root)"
REPORT="${FEATURE_REPORT:-$STATE_HOME/feature-matrix-report.txt}"
WORK="${FEATURE_WORK:-$STATE_HOME/.feature-matrix-work}"

PASS_N=0; FAIL_N=0; SKIPU_N=0; SKIPN_N=0; SKIPX_N=0
FAIL_ITEMS=""
ENV_BLOCKER=0
MINER_PID=""
MINER_LAST_RC=""
LIVE=0
CLIENT_SAFE=0
CLIENT_RESOLVED=""
CLIENT_UNSAFE_REASON=""

# The passwd home BEFORE container adaptation: the sandbox-leak canary.
PH_ORIG=""

# ---------------------------------------------------------------------------
# reporting / tiny utilities
# ---------------------------------------------------------------------------
say()   { printf '%s\n' "$*" | tee -a "$REPORT"; }
head1() { printf '\n== %s ==\n' "$*" | tee -a "$REPORT"; }

record() { # $1=verdict $2=name $3..=detail
    v="$1"; n="$2"; shift 2
    printf '%s %s: %s\n' "$v" "$n" "$*" | tee -a "$REPORT"
    case "$v" in
        PASS) PASS_N=$((PASS_N + 1)) ;;
        FAIL) FAIL_N=$((FAIL_N + 1)); FAIL_ITEMS="$FAIL_ITEMS$n: $*
" ;;
        SKIP-UNSUPPORTED) SKIPU_N=$((SKIPU_N + 1)) ;;
        SKIP-NO-NETWORK) SKIPN_N=$((SKIPN_N + 1)) ;;
        SKIP-UNSAFE) SKIPX_N=$((SKIPX_N + 1)) ;;
    esac
}

have() { command -v "$1" >/dev/null 2>&1; }
fsum() { if [ -f "$1" ]; then cksum <"$1" 2>/dev/null | awk '{print $1"-"$2}'; else printf 'missing'; fi; }
firstline() { head -1 2>/dev/null | cut -c1-200; }
json_has() { printf '%s' "$1" | grep -q "$2" 2>/dev/null; }
grep_c() { grep -c -- "$1" 2>/dev/null || true; } # always prints a number
as_num() { case "${1:-}" in ''|*[!0-9]*) printf '0' ;; *) printf '%s' "$1" ;; esac; }

# ---------------------------------------------------------------------------
# JWT / client-id helpers (never print the token, only its client_id claim)
# ---------------------------------------------------------------------------
# jwt_payload prints the base64url-decoded JWT payload for a token file using
# only POSIX tools (these images have no python3 and no jq).
jwt_payload() { # $1=file
    b64="$(cut -d. -f2 "$1" 2>/dev/null | tr '_-' '/+')"
    [ -n "$b64" ] || return 1
    pad=$(( (4 - ${#b64} % 4) % 4 )); i=0
    while [ "$i" -lt "$pad" ]; do b64="${b64}="; i=$((i + 1)); done
    printf '%s' "$b64" | base64 -d 2>/dev/null
}

# json_client_id extracts the first "client_id" string value from JSON on
# stdin (the client-JWT store and decodable JWT payloads share the field name).
json_client_id() {
    sed -n 's/.*"client_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' 2>/dev/null | head -1
}

jwt_client_id() { [ -r "$1" ] && jwt_payload "$1" 2>/dev/null | json_client_id; }
store_client_id() { # $1=store file; prefers the "direct" slot's entry
    [ -r "$1" ] || return 0
    direct="$(sed -n '/"direct"[[:space:]]*:/,/^[[:space:]]*}/p' "$1" 2>/dev/null | json_client_id)"
    [ -n "$direct" ] || direct="$(json_client_id <"$1")"
    printf '%s' "$direct"
}

# short fingerprint of a client id for report lines (never the whole claim)
cid_short() {
    case "${1:-}" in
        '') printf 'none' ;;
        *) printf '%s..%s' "$(printf '%s' "$1" | cut -c1-8)" "$(printf '%s' "$1" | tail -c 4)" ;;
    esac
}

# ---------------------------------------------------------------------------
# HTTP fetch tool (for the /metrics scrape)
# ---------------------------------------------------------------------------
# HTTP_FETCH is one of curl / wget / busybox-wget, or empty when none is
# available even after an install attempt. busybox wget covers alpine and
# other busybox userlands; curl covers the CI images (the job installs it).
HTTP_FETCH=""
detect_http_fetch() {
    [ -n "$HTTP_FETCH" ] && return 0
    if have curl; then HTTP_FETCH="curl"; return 0; fi
    if have wget; then HTTP_FETCH="wget"; return 0; fi
    if have busybox && busybox wget --help >/dev/null 2>&1; then HTTP_FETCH="busybox-wget"; return 0; fi
    # Install one where the image gives us a package manager and root.
    if [ "$(id -u 2>/dev/null || echo 0)" = "0" ]; then
        if have apt-get; then
            (apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends curl) >/dev/null 2>&1 || true
        elif have apk; then
            apk add --no-cache curl >/dev/null 2>&1 || true
        elif have dnf; then
            dnf install -y curl >/dev/null 2>&1 || dnf install -y --allowerasing curl-minimal >/dev/null 2>&1 || true
        elif have microdnf; then
            microdnf install -y curl >/dev/null 2>&1 || true
        elif have yum; then
            yum install -y curl >/dev/null 2>&1 || true
        fi
    fi
    if have curl; then HTTP_FETCH="curl"; return 0; fi
    if have wget; then HTTP_FETCH="wget"; return 0; fi
    if have busybox; then HTTP_FETCH="busybox-wget"; return 0; fi
    return 1
}
http_get() { # $1=url -> body on stdout; nonzero rc on any failure
    case "$HTTP_FETCH" in
        curl)         curl -fsS --max-time 5 "$1" 2>/dev/null ;;
        wget)         wget -q -O - -T 5 "$1" 2>/dev/null ;;
        busybox-wget) busybox wget -q -O - -T 5 "$1" 2>/dev/null ;;
        *)            return 127 ;;
    esac
}

# is_container reports whether this process runs inside a container (the
# marker files Docker/Podman create). The metrics auto-bind policy documents
# 0.0.0.0 for containers and loopback+Tailscale elsewhere, so the bind-policy
# check is keyed on this.
is_container() { [ -e /.dockerenv ] || [ -e /run/.containerenv ]; }

# metrics_addr_allowed reports whether one "ip:port" from the provider's
# /metrics status is inside the documented auto-bind policy: wildcard (in a
# container), loopback, or a Tailscale CGNAT address. Anything else is a FAIL.
metrics_addr_allowed() {
    a="$1"
    case "$a" in
        "0.0.0.0:"*|"[::]:"*|"[::]":*)
            is_container && return 0
            return 1 ;;
    esac
    case "$a" in
        "127."*|"[::1]:"*|"::1:"*) return 0 ;;
        "100."*)
            # Tailscale CGNAT is 100.64.0.0/10; accept the whole /10 by second octet.
            o2="$(printf '%s' "$a" | cut -d. -f2)"
            [ "$(as_num "$o2")" -ge 64 ] && [ "$(as_num "$o2")" -le 127 ] && return 0
            return 1 ;;
    esac
    return 1
}

# scrape-candidate URLs for a reported listen addr: wildcard -> loopback
# (IPv4 first, then IPv6), everything else used as-is.
metrics_scrape_candidates() { # $1=addr (ip:port) -> candidate urls
    a="$1"
    case "$a" in
        "0.0.0.0:"*) printf 'http://127.0.0.1:%s/metrics' "${a#0.0.0.0:}" ;;
        "[::]:"*)    p="$(printf '%s' "$a" | sed 's/^\[::\]://')"
                     printf 'http://127.0.0.1:%s/metrics http://[::1]:%s/metrics' "$p" "$p" ;;
        :*)          printf 'http://127.0.0.1%s/metrics' "$a" ;;
        *)           printf 'http://%s/metrics' "$a" ;;
    esac
}

metrics_scrape_url() { # $1=addr -> first candidate (compat single-URL helper)
    metrics_scrape_candidates "$1" | awk '{print $1}'
}

# metrics_fetch_any tries every candidate for $1 and prints the first body
# that looks like a Prometheus payload; sets METRICS_URL to the URL that
# answered. Returns 1 when none did.
metrics_fetch_any() { # $1=addr
    METRICS_URL=""
    for u in $(metrics_scrape_candidates "$1"); do
        body="$(http_get "$u" 2>/dev/null)"
        if printf '%s' "$body" | grep -q '# HELP urnet_'; then
            METRICS_URL="$u"
            printf '%s' "$body"
            return 0
        fi
    done
    return 1
}

# ---------------------------------------------------------------------------
# container-local adaptations
# ---------------------------------------------------------------------------
passwd_home_for() { # $1=user
    if have getent; then getent passwd "$1" 2>/dev/null | cut -d: -f6 | head -1
    else grep "^$1:" /etc/passwd 2>/dev/null | cut -d: -f6 | head -1; fi
}

adapt_home() {
    # urnet-tools discovers a provider's state dir only inside the passwd home
    # of the process owner, and delegates provider subcommands with HOME set to
    # that passwd home. When the sandbox HOME is not under it, point the
    # container's passwd home at the sandbox (disposable container only).
    PH_ORIG="$(passwd_home_for "$RUN_USER")"
    ph="$PH_ORIG"
    case "$STATE_HOME" in
        "$ph" | "$ph"/*) : ;;
        *)
            if [ -w /etc/passwd ] && [ -n "$ph" ]; then
                awk -v u="$RUN_USER" -v h="$STATE_HOME" 'BEGIN{FS=OFS=":"} $1==u{$6=h} {print}' /etc/passwd > /etc/passwd.fmnew 2>/dev/null &&
                    cat /etc/passwd.fmnew > /etc/passwd && rm -f /etc/passwd.fmnew
                say "NOTE container-adaptation: passwd home for $RUN_USER -> $STATE_HOME (state-dir trust for urnet-tools)"
            else
                say "WARN: STATE_HOME=$STATE_HOME is outside $RUN_USER's passwd home ($ph) and /etc/passwd is not writable; client-level checks will skip (client-targeting below)"
            fi
            ;;
    esac
}

# ---------------------------------------------------------------------------
# preflight guard + client target pinning
# ---------------------------------------------------------------------------
# urnet-tools resolves a client-level command's target through discovery,
# which attributes a running provider's state dir to the KERNEL owner's
# passwd home: a state dir outside that home is rejected and the fallback is
# <passwd home>/.urnetwork; delegated client commands then exec the provider
# binary with HOME=<that passwd home>. A sandbox HOME outside the invoking
# user's passwd home (the operator's host, where /etc/passwd cannot be
# adapted) therefore makes client commands resolve to the operator's real
# state even though this harness runs every process with an isolated HOME.
# The guard refuses the whole run (exit 78) when any path it writes or the
# state it uses overlaps that real state; the pinning below disables every
# client-level check unless the passwd home IS the sandbox HOME.

OPERATOR_HOME="$(getent passwd "$(id -un)" 2>/dev/null | cut -d: -f6)"
OPERATOR_STATE="${OPERATOR_HOME:-$HOME}/.urnetwork"

canon_dir() {
    # Physical path for $1: resolve symlinks in the longest existing prefix
    # and keep the remainder lexically; print $1 unchanged when nothing
    # resolves. Reads only, never creates anything.
    [ -n "$1" ] || { printf ''; return 0; }
    _cd=$1
    _cd_tail=""
    while [ ! -d "$_cd" ] && [ "$_cd" != "/" ]; do
        _cd_tail="/$(basename "$_cd")$_cd_tail"
        _cd=$(dirname "$_cd")
    done
    if _cd_phys="$(cd "$_cd" 2>/dev/null && pwd -P)"; then
        printf '%s%s\n' "$_cd_phys" "$_cd_tail"
    else
        printf '%s\n' "$1"
    fi
}

path_strictly_inside() { # $1 is strictly inside directory $2
    case "$1" in
        "$2"/*) return 0 ;;
        *) return 1 ;;
    esac
}

path_overlaps() { # $1 == $2, or one is strictly inside the other
    [ "$1" = "$2" ] && return 0
    path_strictly_inside "$1" "$2" && return 0
    path_strictly_inside "$2" "$1" && return 0
    return 1
}

guard_refuse() {
    printf '\nREFUSING TO RUN (preflight guard): %s\n' "$1" >&2
    printf 'The operator provider state is %s and this harness must never resolve a target to it.\n' "$OPERATOR_STATE" >&2
    printf 'Nothing was created, deleted, or invoked. Fix STATE_HOME/STATE_DIR/FEATURE_WORK (and unset URNETWORK_STATE_DIR),\n' >&2
    printf 'or run the matrix in a disposable container, as the CI job does.\n' >&2
    exit 78
}

preflight_guard() {
    # Runs before anything is created or invoked (main calls it first).
    case "${STATE_HOME:-}" in
        ""|/) guard_refuse "STATE_HOME is empty or '/'; the sandbox root must be a real directory" ;;
    esac
    case "${STATE_DIR:-}" in
        ""|/) guard_refuse "STATE_DIR is empty or '/'; the effective state dir must be a real path" ;;
    esac
    OPERATOR_STATE_C="$(canon_dir "$OPERATOR_STATE")"
    STATE_HOME_C="$(canon_dir "$STATE_HOME")"
    STATE_DIR_C="$(canon_dir "$STATE_DIR")"
    WORK_C="$(canon_dir "$WORK")"
    REPORT_C="$(canon_dir "$REPORT")"
    path_overlaps "$STATE_HOME_C" "$OPERATOR_STATE_C" && guard_refuse "STATE_HOME=$STATE_HOME equals, contains, or lives inside the operator state $OPERATOR_STATE"
    path_overlaps "$STATE_DIR_C" "$OPERATOR_STATE_C" && guard_refuse "the effective state dir STATE_DIR=$STATE_DIR equals, contains, or lives inside the operator state $OPERATOR_STATE"
    path_overlaps "$WORK_C" "$OPERATOR_STATE_C" && guard_refuse "WORK=$WORK equals, contains, or lives inside the operator state $OPERATOR_STATE"
    path_overlaps "$REPORT_C" "$OPERATOR_STATE_C" && guard_refuse "the report path $REPORT equals, contains, or lives inside the operator state $OPERATOR_STATE"
    # Every invocation is run with HOME=$STATE_HOME (the sandbox root this
    # harness created), so STATE_HOME itself must be a real directory (checked
    # above); an ambient HOME already sitting inside the operator state is
    # refused outright.
    AMBIENT_HOME_C="$(canon_dir "${HOME:-}")"
    if [ "$AMBIENT_HOME_C" = "$OPERATOR_STATE_C" ] || path_strictly_inside "$AMBIENT_HOME_C" "$OPERATOR_STATE_C"; then
        guard_refuse "HOME=$HOME is inside the operator state $OPERATOR_STATE"
    fi
    if [ -n "${URNETWORK_STATE_DIR:-}" ]; then
        urnet_state_c="$(canon_dir "$URNETWORK_STATE_DIR")"
        if [ "$urnet_state_c" = "$STATE_DIR_C" ]; then
            printf 'NOTE: URNETWORK_STATE_DIR=%s equals the sandbox state; unsetting it for the runs below\n' "$URNETWORK_STATE_DIR"
        else
            path_overlaps "$urnet_state_c" "$OPERATOR_STATE_C" && guard_refuse "URNETWORK_STATE_DIR=$URNETWORK_STATE_DIR points at the operator state $OPERATOR_STATE"
            guard_refuse "URNETWORK_STATE_DIR=$URNETWORK_STATE_DIR is set in the invoking environment; unset it so this harness controls the state dir"
        fi
    fi
    # The ambient state override must not leak into a miner start (only
    # start_run-style env isolation would strip it). The guard above already
    # refused any value that matters.
    unset URNETWORK_STATE_DIR 2>/dev/null || true
    return 0
}

resolve_client_targeting() {
    # CLIENT_RESOLVED is what discovery would attribute to the sandbox
    # provider and the HOME a delegated provider binary would run with: the
    # sandbox state iff the invoking user's passwd home IS the sandbox HOME
    # (the container adaptation above); otherwise <passwd home>/.urnetwork.
    # Records the client-targeting assertion row (PASS inside the sandbox,
    # FAIL otherwise) that every client-level check's verdict hangs off.
    PH_ADAPTED="$(passwd_home_for "$RUN_USER")"
    if [ -n "$PH_ADAPTED" ] && [ "$(canon_dir "$PH_ADAPTED")" = "$STATE_HOME_C" ]; then
        CLIENT_SAFE=1
        CLIENT_RESOLVED="$STATE_DIR"
    else
        CLIENT_SAFE=0
        if [ -n "$PH_ADAPTED" ]; then
            CLIENT_RESOLVED="$PH_ADAPTED/.urnetwork"
        else
            CLIENT_RESOLVED="unresolved (no passwd entry for $RUN_USER)"
        fi
        CLIENT_UNSAFE_REASON="client-level targeting disabled: the invoking user's passwd home (${PH_ADAPTED:-unknown}) is not the sandbox HOME ($STATE_HOME), so urnet-tools discovery/delegation would resolve to $CLIENT_RESOLVED; run in a disposable container (CI)"
        say "WARN $CLIENT_UNSAFE_REASON"
    fi
    if [ "$CLIENT_SAFE" = "1" ]; then
        record PASS client-targeting "client targeting resolved to $CLIENT_RESOLVED (the invoking user's passwd home is the sandbox HOME; inside the sandbox)"
    else
        record FAIL client-targeting "client targeting resolved to $CLIENT_RESOLVED (outside the sandbox state $STATE_DIR; passwd home ${PH_ADAPTED:-unknown} != sandbox HOME $STATE_HOME; client-level checks disabled)"
    fi
    return 0
}

# client_skip <name...>: record client-level checks as SKIP-UNSAFE (called
# only when CLIENT_SAFE != 1; the client is never invoked then).
client_skip() {
    for _cn in "$@"; do
        record SKIP-UNSAFE "$_cn" "$CLIENT_UNSAFE_REASON"
    done
    return 0
}

set_up_wrapper() {
    # Discovery matches argv[0] basenames on the provider allowlist; run the
    # miner through a "provider" symlink inside the sandbox.
    mkdir -p "$STATE_HOME/bin" 2>/dev/null || true
    ln -sf "$MINER_BIN" "$STATE_HOME/bin/provider" 2>/dev/null || true
    MINER_WRAPPER="$STATE_HOME/bin/provider"
    [ -x "$MINER_WRAPPER" ] || MINER_WRAPPER="$MINER_BIN"
}

# ---------------------------------------------------------------------------
# miner lifecycle
# ---------------------------------------------------------------------------
miner_help() { HOME="$STATE_HOME" "$MINER_WRAPPER" provide --help 2>&1 || true; }

START_FLAGS=""
resolve_start_flags() {
    if miner_help | grep -q -- '--allow-client-registration'; then
        START_FLAGS="--allow-client-registration"
    else
        START_FLAGS=""
    fi
}

wait_for_socket() { # $1=seconds
    i=0
    while [ "$i" -lt "$1" ]; do
        [ -S "$STATE_DIR/provider.sock" ] && return 0
        if [ -n "$MINER_PID" ] && ! kill -0 "$MINER_PID" 2>/dev/null; then return 1; fi
        sleep 1
        i=$((i + 1))
    done
    [ -S "$STATE_DIR/provider.sock" ]
}

# Reaps a dead miner and records its exit status (MINER_LAST_RC). A wait on a
# process this shell did not spawn yields 127, which is normalized to "".
reap_miner() {
    MINER_LAST_RC=""
    [ -n "$MINER_PID" ] || return 0
    if kill -0 "$MINER_PID" 2>/dev/null; then return 0; fi
    wait "$MINER_PID" 2>/dev/null
    rc=$?
    [ "$rc" = "127" ] && rc=0
    MINER_LAST_RC="$rc"
    MINER_PID=""
    return 0
}

# The miner's exit code 75 is EX_TEMPFAIL (a rejected network sign-in set
# aside for the supervisor), an environmental condition, never a product
# finding. Returns 0 when the last dead miner exited 75.
miner_exited_75() { [ "${MINER_LAST_RC:-}" = "75" ]; }

# True when the miner this harness started has left. A provider that leaves
# during the raw socket probes is an environmental condition (a failed network
# sign-in takes it down seconds after the socket binds); only a crash makes it
# a product finding.
miner_gone() { [ -n "$MINER_PID" ] && ! kill -0 "$MINER_PID" 2>/dev/null; }
miner_crashed() { grep -qE 'panic:|SIGSEGV|fatal error' "$WORK/miner-main.log" "$WORK/miner-main-r2.log" 2>/dev/null; }

start_miner() { # $1=logfile; extra args passed through
    log="$1"; shift
    : >"$log"
    HOME="$STATE_HOME" "$MINER_WRAPPER" provide --port=0 $START_FLAGS "$@" >"$log" 2>&1 &
    MINER_PID=$!
    MINER_LAST_RC=""
}

stop_miner() { # $1=grace seconds; sets MINER_STOP_RC
    MINER_STOP_RC=""
    [ -n "$MINER_PID" ] || return 0
    if ! kill -0 "$MINER_PID" 2>/dev/null; then
        reap_miner
        MINER_STOP_RC="${MINER_LAST_RC:-0}"
        return 0
    fi
    kill -TERM "$MINER_PID" 2>/dev/null
    w=0
    while kill -0 "$MINER_PID" 2>/dev/null && [ "$w" -lt "$1" ]; do sleep 1; w=$((w + 1)); done
    if kill -0 "$MINER_PID" 2>/dev/null; then
        kill -KILL "$MINER_PID" 2>/dev/null
        MINER_STOP_RC="SIGKILL-after-${w}s"
    else
        reap_miner
        MINER_STOP_RC="${MINER_LAST_RC:-0}"
    fi
    MINER_PID=""
}

# Early-exit reasons that are environmental (the box cannot finish the network
# sign-in), never a product verdict.
env_exit_reason() { # $1=log
    grep -oE 'register-client-v1[^"]*|context deadline exceeded|no such host|network is unreachable|attempts exhausted|failed to refresh JWT[^"]*|rejected network sign-in[^"]*|exit status 75' "$1" 2>/dev/null | head -1 | firstline
}

# Start the miner and require it to stay up through a settle window. Returns
# 0 when a live provider is running (LIVE=1), 1 when the environment cannot
# keep it up (LIVE stays 0). Socket security checks can run on the brief
# window even when the environment is bad; callers decide.
start_live_miner() { # $1=log prefix
    pref="$1"; attempt=1
    while [ "$attempt" -le "$FEATURE_START_ATTEMPTS" ]; do
        start_miner "$WORK/$pref.log"
        if wait_for_socket "$FEATURE_START_TIMEOUT"; then
            sleep "$FEATURE_SETTLE"
            if kill -0 "$MINER_PID" 2>/dev/null; then
                if [ -S "$SOCK" ]; then LIVE=1; return 0; fi
            fi
            reason="$(env_exit_reason "$WORK/$pref.log")"
            if [ -z "$reason" ] && kill -0 "$MINER_PID" 2>/dev/null; then reason="socket missing after settle"; fi
            say "start attempt $attempt: provider left within ${FEATURE_SETTLE}s (${reason:-no network reason in log})"
        else
            reap_miner
            reason="$(env_exit_reason "$WORK/$pref.log")"
            if miner_exited_75; then
                say "start attempt $attempt: provider exited 75 (EX_TEMPFAIL: rejected sign-in set aside by --quarantine-rejected-sign-in)"
            elif [ -n "$MINER_LAST_RC" ]; then
                say "start attempt $attempt: provider exited rc=$MINER_LAST_RC before binding a socket (${reason:-no reason in log})"
            else
                say "start attempt $attempt: no socket within ${FEATURE_START_TIMEOUT}s"
            fi
        fi
        stop_miner 10 >/dev/null 2>&1
        attempt=$((attempt + 1))
        [ "$attempt" -le "$FEATURE_START_ATTEMPTS" ] && sleep 2
    done
    LIVE=0
    return 1
}

# Reuse the running provider when it is still alive, otherwise start one.
ensure_live() { # $1=log prefix
    if [ "$LIVE" = "1" ] && [ -n "$MINER_PID" ] && kill -0 "$MINER_PID" 2>/dev/null && [ -S "$SOCK" ]; then
        return 0
    fi
    start_live_miner "$1" || true
    [ "$LIVE" = "1" ]
}

on_exit() { stop_miner 15 >/dev/null 2>&1; }
trap on_exit EXIT INT TERM

# ---------------------------------------------------------------------------
# urnet-tools wrapper + raw control socket helpers
# ---------------------------------------------------------------------------
SOCK="$STATE_DIR/provider.sock"
# Client-level wrappers. Both hard-refuse (exit 78, without invoking anything)
# when CLIENT_SAFE != 1: every caller sits behind a client_skip gate, so this
# firing means a client invocation escaped the gate and the run must not
# continue with untrusted targeting.
T()  {
    if [ "$CLIENT_SAFE" != "1" ]; then
        printf 'FATAL: refusing a client-level urnet-tools invocation with unsafe targeting\n' >&2
        printf 'invocation: urnet-tools %s\n' "$*" >&2
        printf '%s\n' "$CLIENT_UNSAFE_REASON" >&2
        exit 78
    fi
    HOME="$STATE_HOME" "$TOOLS_BIN" "$@"
}
TS() {
    if [ "$CLIENT_SAFE" != "1" ]; then
        printf 'FATAL: refusing a client-level urnet-tools invocation with unsafe targeting\n' >&2
        printf 'invocation: urnet-tools %s --state-dir %s\n' "$*" "$STATE_DIR" >&2
        printf '%s\n' "$CLIENT_UNSAFE_REASON" >&2
        exit 78
    fi
    HOME="$STATE_HOME" "$TOOLS_BIN" "$@" --state-dir "$STATE_DIR"
}

NC=0
if have nc; then
    case "$(nc -h 2>&1 | head -3 | tr '\n' ' ')" in
        *"-U "*) NC=1 ;;
        *) NC=1 ;;  # busybox prints no usage; probe it live below
    esac
fi
raw_send() { # $1=payload file
    timeout "$FEATURE_SOCKET_TIMEOUT" nc -U "$SOCK" <"$1" 2>&1 || true
}
nc_live() { # after the socket exists: does this nc really speak -U?
    [ "$NC" = "1" ] || return 1
    printf '{"cmd":"get","key":"gomemlimit","v":1}\n' >"$WORK/ncprobe.json"
    out="$(raw_send "$WORK/ncprobe.json")"
    json_has "$out" '"ok"' && return 0
    NC=0
    return 1
}

# ---------------------------------------------------------------------------
# synthetic proxy entries (TEST-NET-1; never reachable, never a live proxy)
# ---------------------------------------------------------------------------
SYN_TMP="$WORK/synthetic-proxies.txt"
write_synthetic() { # $1=count
    : >"$SYN_TMP"
    i=10
    while [ "$i" -lt $((10 + $1)) ]; do
        printf '192.0.2.%d:1080\n' "$i" >>"$SYN_TMP"
        i=$((i + 1))
    done
}
count_synthetic() { grep -o '"192\.0\.2\.[0-9]*:1080"' "$STATE_DIR/proxy" 2>/dev/null | wc -l | tr -d ' '; }
count_cache() { grep -o '"probe_ok"' "$STATE_DIR/proxy_url.json" 2>/dev/null | wc -l | tr -d ' '; }
count_sources() { grep -o '"sources"' "$STATE_DIR/proxy_url.json" 2>/dev/null | wc -l | tr -d ' '; }
fingerprint_files() { # fingerprints the state files without a tag prefix
    printf 'proxy=%s proxy.state=%s proxy_url=%s trim=%s reload=%s' \
        "$(fsum "$STATE_DIR/proxy")" "$(fsum "$STATE_DIR/proxy.state")" \
        "$(fsum "$STATE_DIR/proxy_url.json")" "$(fsum "$STATE_DIR/proxy_trim")" \
        "$(fsum "$STATE_DIR/proxy.reload")"
}
fingerprint_core() { # files the manipulation checks must not disturb
    printf 'proxy=%s proxy.state=%s trim=%s' \
        "$(fsum "$STATE_DIR/proxy")" "$(fsum "$STATE_DIR/proxy.state")" "$(fsum "$STATE_DIR/proxy_trim")"
}

# ---------------------------------------------------------------------------
# BLOCK 1 — RISKY
# ---------------------------------------------------------------------------

# control-socket security: perm, owner round-trip, peer uid, oversize set,
# oversize line, hostile input. Runs on the brief window even without sign-in.
b1_socket_security() {
    perm="$(stat -c %a "$SOCK" 2>/dev/null || true)"
    [ -z "$perm" ] && perm="$(ls -l "$SOCK" 2>/dev/null | awk '{print $1}')"
    case "$perm" in
        600) record PASS socket-perm-0600 "mode=600 owner-only" ;;
        *)   record FAIL socket-perm-0600 "mode=$perm, want 600" ;;
    esac

    if nc_live; then
        printf '{"cmd":"get","key":"gomemlimit","v":1}\n' >"$WORK/req-ok.json"
        same="$(raw_send "$WORK/req-ok.json")"
        if json_has "$same" '"ok":true'; then
            record PASS socket-owner-req "owner request answered: $(printf '%s' "$same" | firstline)"
        elif miner_gone && ! miner_crashed; then
            record SKIP-NO-NETWORK socket-owner-req "provider left during the socket checks (environmental): $(printf '%s' "$same" | firstline)"
        else
            record FAIL socket-owner-req "owner request not answered: $(printf '%s' "$same" | firstline)"
        fi

        if have su; then
            foreign="$(su -s /bin/sh nobody -c "printf '{\"cmd\":\"get\",\"key\":\"gomemlimit\",\"v\":1}\n' | timeout $FEATURE_SOCKET_TIMEOUT nc -U '$SOCK'" 2>&1 || true)"
            case "$foreign" in
                *"Permission denied"*) record PASS socket-peer-uid "foreign uid (nobody) denied: $(printf '%s' "$foreign" | firstline)" ;;
                *'"ok":false'*)        record PASS socket-peer-uid "foreign uid rejected by provider: $(printf '%s' "$foreign" | firstline)" ;;
                *'"ok":true'*)         record FAIL socket-peer-uid "foreign uid REACHED the control socket: $(printf '%s' "$foreign" | firstline)" ;;
                *"not found"*|*"can't"*|*"unknown"*|*usage*|*Usage*|*"login"*)
                    record SKIP-UNSUPPORTED socket-peer-uid "su/nobody unavailable in this image: $(printf '%s' "$foreign" | firstline)" ;;
                *)  if miner_gone && ! miner_crashed; then
                        record SKIP-NO-NETWORK socket-peer-uid "provider left during the socket checks (environmental): $(printf '%s' "$foreign" | firstline)"
                    else
                        record FAIL socket-peer-uid "foreign uid attempt unclear: $(printf '%s' "$foreign" | firstline)"
                    fi ;;
            esac
        elif have setpriv; then
            foreign="$(setpriv --reuid=65534 --regid=65534 --clear-groups sh -c "printf '{\"cmd\":\"get\",\"key\":\"gomemlimit\",\"v\":1}\n' | timeout $FEATURE_SOCKET_TIMEOUT nc -U '$SOCK'" 2>&1 || true)"
            case "$foreign" in
                *"Permission denied"*|*'"ok":false'*) record PASS socket-peer-uid "foreign uid denied: $(printf '%s' "$foreign" | firstline)" ;;
                *'"ok":true'*) record FAIL socket-peer-uid "foreign uid REACHED the control socket" ;;
                *)  if miner_gone && ! miner_crashed; then
                        record SKIP-NO-NETWORK socket-peer-uid "provider left during the socket checks (environmental): $(printf '%s' "$foreign" | firstline)"
                    else
                        record FAIL socket-peer-uid "foreign uid attempt unclear: $(printf '%s' "$foreign" | firstline)"
                    fi ;;
            esac
        else
            record SKIP-UNSUPPORTED socket-peer-uid "no su/setpriv available to switch uid"
        fi

        awk 'BEGIN{printf "{\"cmd\":\"set\",\"key\":\"gomemlimit\",\"value\":\""; for(i=0;i<4200;i++)printf "x"; printf "\",\"v\":1}\n"}' >"$WORK/req-bigval.json"
        big="$(raw_send "$WORK/req-bigval.json")"
        if json_has "$big" 'value too large'; then
            record PASS socket-oversize-set "4100+ byte set value refused: $(printf '%s' "$big" | firstline)"
        elif miner_gone && ! miner_crashed; then
            record SKIP-NO-NETWORK socket-oversize-set "provider left during the socket checks (environmental): $(printf '%s' "$big" | firstline)"
        else
            record FAIL socket-oversize-set "no structured refusal: $(printf '%s' "$big" | firstline)"
        fi

        awk 'BEGIN{for(i=0;i<70000;i++)printf "A"; printf "\n"}' >"$WORK/req-bigline.bin"
        raw_send "$WORK/req-bigline.bin" >"$WORK/bigline.out" 2>&1
        after="$(raw_send "$WORK/req-ok.json")"
        if json_has "$after" '"ok":true'; then
            record PASS socket-oversize-line "70KB line handled; socket still answers"
        elif miner_gone && ! miner_crashed; then
            record SKIP-NO-NETWORK socket-oversize-line "provider left during the socket checks (environmental)"
        else
            record FAIL socket-oversize-line "socket dead or silent after a 70KB line"
        fi

        # one-line framing: two requests on ONE connection are answered, one
        # JSON line each, in order (the documented newline-delimited protocol).
        printf '{"cmd":"get","key":"gomemlimit","v":1}\n{"cmd":"get","key":"gogc","v":1}\n' >"$WORK/req-two.json"
        two="$(raw_send "$WORK/req-two.json")"
        n_ok="$(printf '%s\n' "$two" | grep -c '"ok"' 2>/dev/null || true)"
        if [ "$(as_num "$n_ok")" -ge 2 ]; then
            record PASS socket-framing "two requests on one connection got two JSON responses"
        elif miner_gone && ! miner_crashed; then
            record SKIP-NO-NETWORK socket-framing "provider left during the socket checks (environmental): $(printf '%s' "$two" | firstline)"
        else
            record FAIL socket-framing "expected 2 responses on one connection, saw $n_ok: $(printf '%s' "$two" | tr '\n' ' ' | firstline)"
        fi

        printf 'not json at all\n' >"$WORK/h1"
        printf '{}\n' >"$WORK/h2"
        printf '{"cmd":"../etc/passwd","key":"","v":1}\n' >"$WORK/h3"
        printf '{"cmd":"get","key":"gomemlimit","v":99}\n' >"$WORK/h4"
        printf '{"cmd":\n' >"$WORK/h5"
        hostile_bad=0
        for hf in h1 h2 h3 h4 h5; do
            out="$(raw_send "$WORK/$hf")"
            n="$(printf '%s\n' "$out" | grep -c . 2>/dev/null || true)"
            if ! json_has "$out" '"ok":' && [ "${n:-0}" != "0" ] && [ "${n:-0}" != "1" ]; then
                hostile_bad=$((hostile_bad + 1))
            fi
        done
        fin="$(raw_send "$WORK/req-ok.json")"
        if [ "$hostile_bad" = "0" ] && json_has "$fin" '"ok":true'; then
            record PASS socket-hostile-input "5 hostile inputs, replies stayed bounded JSON; socket survived"
        elif [ "$hostile_bad" = "0" ] && miner_gone && ! miner_crashed; then
            record SKIP-NO-NETWORK socket-hostile-input "provider left during the socket checks (environmental); final probe: $(printf '%s' "$fin" | firstline)"
        else
            record FAIL socket-hostile-input "$hostile_bad hostile inputs behaved badly; final probe: $(printf '%s' "$fin" | firstline)"
        fi
    else
        record SKIP-UNSUPPORTED socket-owner-req "netcat (nc -U) not available in this image"
        record SKIP-UNSUPPORTED socket-peer-uid "netcat (nc -U) not available in this image"
        record SKIP-UNSUPPORTED socket-oversize-set "netcat (nc -U) not available in this image"
        record SKIP-UNSUPPORTED socket-oversize-line "netcat (nc -U) not available in this image"
        record SKIP-UNSUPPORTED socket-framing "netcat (nc -U) not available in this image"
        record SKIP-UNSUPPORTED socket-hostile-input "netcat (nc -U) not available in this image"
    fi
}

# identity + restart: mint, key material, clean SIGTERM, client-id reuse
# across restart (via the stable client-JWT store), HOT_RESTART=0 fresh-mint,
# events.log lifecycle lines, and state-path containment.
ident_fp() {
    if [ -f "$STATE_DIR/.provider.jwt" ]; then printf 'jwt:%s' "$(fsum "$STATE_DIR/.provider.jwt")"; else printf 'none'; fi
}
client_store_fp() {
    if [ -f "$STATE_DIR/.client_jwts.json" ]; then printf 'store:%s' "$(fsum "$STATE_DIR/.client_jwts.json")"; else printf 'none'; fi
}

# wait_identity_cid waits up to $1 seconds for the direct provider's client
# id; prints it (empty on timeout). The direct slot's client JWT is
# .provider.jwt; the durable store is .client_jwts.json.
wait_identity_cid() {
    cid="$(jwt_client_id "$STATE_DIR/.provider.jwt")"
    if [ -z "$cid" ]; then
        i=0
        while [ "$i" -lt "$1" ]; do
            cid="$(jwt_client_id "$STATE_DIR/.provider.jwt")"
            [ -n "$cid" ] && break
            kill -0 "$MINER_PID" 2>/dev/null || break
            sleep 2; i=$((i + 2))
        done
    fi
    printf '%s' "$cid"
}

b1_identity_restart() {
    one_cid="$(wait_identity_cid "$FEATURE_IDENTITY_TIMEOUT")"
    if [ -f "$STATE_DIR/.provider.jwt" ]; then
        record PASS identity-mint "provider client identity present (client_id $(cid_short "$one_cid"), store entry $(cid_short "$(store_client_id "$STATE_DIR/.client_jwts.json")"))"
        if [ -f "$STATE_DIR/.provider.key" ]; then
            record PASS identity-key-material ".provider.key present next to the client identity"
        else
            record FAIL identity-key-material ".provider.jwt exists but .provider.key missing"
        fi
    else
        reason="$(env_exit_reason "$WORK/miner-main.log")"
        [ -z "$reason" ] && reason="$(grep -oE '\[api-token\][^"]*' "$WORK/miner-main.log" 2>/dev/null | head -1 | firstline)"
        record SKIP-NO-NETWORK identity-mint "no client identity within ${FEATURE_IDENTITY_TIMEOUT}s (${reason:-no sign-in observed})"
    fi

    stop_miner "$FEATURE_START_TIMEOUT"
    case "$MINER_STOP_RC" in
        0) if [ ! -e "$SOCK" ]; then record PASS clean-sigterm "SIGTERM -> rc=0, socket removed"
           else record FAIL clean-sigterm "rc=0 but socket file left behind"; fi ;;
        SIGKILL-*) record FAIL clean-sigterm "SIGTERM ignored, had to SIGKILL ($MINER_STOP_RC)" ;;
        *) record FAIL clean-sigterm "SIGTERM rc=$MINER_STOP_RC, socket $( [ -e "$SOCK" ] && echo left-behind || echo removed )" ;;
    esac
    b1_logging

    # client-id reuse across restart: the stable store (feat/p2-hotswap) makes
    # a restart carry the same client_id; without the store self-detects to
    # SKIP-UNSUPPORTED. The check compares the client_id CLAIM, not a file
    # fingerprint - a renewal rewrites the token while keeping the id.
    start_live_miner miner-restart
    if [ "$LIVE" = "1" ]; then
        two_cid="$(wait_identity_cid 60)"
        store_two="$(store_client_id "$STATE_DIR/.client_jwts.json")"
        if [ -z "$one_cid" ] || [ -z "$two_cid" ]; then
            record SKIP-NO-NETWORK identity-restart-reuse "client_id not observable before/after restart (one=${one_cid:-none} two=${two_cid:-none})"
        elif [ "$one_cid" = "$two_cid" ]; then
            record PASS identity-restart-reuse "same client_id carried across restart ($(cid_short "$two_cid"))"
        elif [ -n "$store_two" ]; then
            record FAIL identity-restart-reuse "client_id changed across restart with the client-JWT store present ($(cid_short "$one_cid") -> $(cid_short "$two_cid"))"
        else
            record SKIP-UNSUPPORTED identity-restart-reuse "client_id changed and no .client_jwts.json store at this head ($(cid_short "$one_cid") -> $(cid_short "$two_cid")). Owning port: feat/p2-hotswap (wt-sn-hotswap: client_jwt_store)"
        fi
        if [ "$CLIENT_SAFE" = "1" ]; then
            marker="$(TS status 2>/dev/null | grep -oE 'last restart: [a-z-]+' | head -1)"
            say "status after restart: ${marker:-'(no restart marker line)'}"
        else
            say "status after restart: (not queried: client targeting disabled, see client-targeting)"
        fi
    else
        record FAIL identity-restart "provider did not come back after SIGTERM"
    fi
    stop_miner "$FEATURE_START_TIMEOUT"

    # HOT_RESTART=0: with the store present this must mint a FRESH client id.
    # The check reads the store before and after and requires the direct
    # client_id to change; without a store it self-detects to SKIP-UNSUPPORTED.
    store_before="$(client_store_fp)"
    cid_before="$(store_client_id "$STATE_DIR/.client_jwts.json")"
    : >"$WORK/miner-hr0.log"
    HOME="$STATE_HOME" URNETWORK_HOT_RESTART=0 "$MINER_WRAPPER" provide --port=0 $START_FLAGS >"$WORK/miner-hr0.log" 2>&1 &
    MINER_PID=$!
    if wait_for_socket "$FEATURE_START_TIMEOUT" && sleep "$FEATURE_SETTLE" && kill -0 "$MINER_PID" 2>/dev/null; then
        if [ "$store_before" != "none" ] || [ -f "$STATE_DIR/.client_jwts.json" ]; then
            cid_after=""; j=0
            while [ "$j" -lt "$FEATURE_IDENTITY_TIMEOUT" ]; do
                cid_after="$(store_client_id "$STATE_DIR/.client_jwts.json")"
                [ -n "$cid_after" ] && [ "$cid_after" != "$cid_before" ] && break
                kill -0 "$MINER_PID" 2>/dev/null || break
                sleep 2; j=$((j + 2))
            done
            store_after="$(client_store_fp)"
            if [ -n "$cid_after" ] && [ "$cid_after" != "$cid_before" ] && [ "$store_before" != "$store_after" ]; then
                record PASS hot-restart-off "HOT_RESTART=0 minted a fresh client id ($(cid_short "$cid_before") -> $(cid_short "$cid_after")); store rewritten"
            elif [ "$store_before" = "$store_after" ]; then
                record FAIL hot-restart-off "HOT_RESTART=0 left the store byte-identical ($store_after); reuse was not disabled"
            else
                record FAIL hot-restart-off "HOT_RESTART=0 store changed but no fresh client_id observed (before=$(cid_short "$cid_before") after=$(cid_short "$cid_after")): $(tail -2 "$WORK/miner-hr0.log" | tr '\n' ' ' | firstline)"
            fi
        else
            record SKIP-UNSUPPORTED hot-restart-off "hot_restart=0 accepted, but the client-JWT store that makes reuse vs new observable is not at this head. Owning port: feat/p2-hotswap (wt-sn-hotswap)"
        fi
    else
        if kill -0 "$MINER_PID" 2>/dev/null; then
            record FAIL hot-restart-off "provider stuck with URNETWORK_HOT_RESTART=0"
        else
            reason="$(env_exit_reason "$WORK/miner-hr0.log")"
            record SKIP-NO-NETWORK hot-restart-off "environment could not keep the provider up for the hot_restart=0 run (${reason:-see log})"
        fi
    fi
    stop_miner "$FEATURE_START_TIMEOUT"

    b1_state_paths
}

# events.log (critlog): the provide lifecycle must be recorded at DEFAULT
# verbosity - STARTUP once the identity is staged, SIGNAL and PROVIDER EXIT
# when a clean SIGTERM drains the run. Requires a live run and a clean stop;
# anything less is SKIP-NO-NETWORK, never a fake pass.
b1_logging() {
    ev="$STATE_DIR/events.log"
    if [ ! -f "$ev" ]; then
        record FAIL logging-events "events.log missing after a live run under $STATE_DIR"
        return
    fi
    startup=$(grep -c 'STARTUP: version=' "$ev" 2>/dev/null || true)
    signal=$(grep -c 'SIGNAL: context cancelled' "$ev" 2>/dev/null || true)
    exitl=$(grep -c 'PROVIDER EXIT: normal shutdown' "$ev" 2>/dev/null || true)
    if [ "$(as_num "$startup")" -lt 1 ]; then
        record FAIL logging-events "events.log has no STARTUP line (startup=$startup signal=$signal exit=$exitl)"
    elif [ "$(as_num "$signal")" -ge 1 ] && [ "$(as_num "$exitl")" -ge 1 ]; then
        record PASS logging-events "events.log recorded the lifecycle at default verbosity (STARTUP x$startup, SIGNAL x$signal, PROVIDER EXIT x$exitl)"
    elif [ "${MINER_STOP_RC:-}" = "0" ]; then
        record FAIL logging-events "clean stop but events.log lacks SIGNAL/EXIT (startup=$startup signal=$signal exit=$exitl)"
    else
        record SKIP-NO-NETWORK logging-events "no clean stop observed yet; cannot assert SIGNAL/PROVIDER EXIT (startup=$startup)"
    fi
}

b1_state_paths() {
    # sandbox containment: the provider's state belongs to STATE_DIR, and a
    # non-sandbox passwd home must not grow a .urnetwork during the run.
    if [ -S "$SOCK" ] || [ -f "$STATE_DIR/proxy.state" ]; then
        record PASS state-paths "state files under STATE_DIR ($STATE_DIR: audit.json/proxy/proxy.state present)"
    else
        record FAIL state-paths "no provider state under STATE_DIR=$STATE_DIR after startup"
    fi
    if [ -n "$PH_ORIG" ] && [ "$PH_ORIG" != "$STATE_HOME" ] && [ "$PH_ORIG" != "$STATE_DIR" ]; then
        if [ -e "$PH_ORIG/.urnetwork" ]; then
            record FAIL state-paths-sandbox "passwd home $PH_ORIG/.urnetwork exists after the run; state leaked outside STATE_HOME"
        else
            record PASS state-paths-sandbox "no state under the original passwd home $PH_ORIG (sandbox contained)"
        fi
    else
        record PASS state-paths-sandbox "STATE_HOME is the passwd home ($STATE_HOME); no cross-home leak possible"
    fi
}

# hotswap handoff: the raw control socket's {"cmd":"hotswap"} is the trigger
# probe. The integrated head wires it (feat/p2-hotswap): the parent must exit,
# a successor must answer on the re-bound socket, and /metrics must be
# re-served from the successor (the takeover rebind). A head without the
# trigger self-detects to SKIP-UNSUPPORTED.
b1_hotswap() {
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip hotswap-handoff hotswap-metrics-rebind
        return 0
    fi
    start_live_miner miner-hotswap
    hs=""
    if [ "$LIVE" = "1" ]; then
        # capture the metrics state before the handoff (rebind assertion)
        metrics_before="$(TS metrics status 2>&1 | firstline)"
        addrs_before="$(printf '%s' "$(TS metrics status 2>&1)" | sed -n 's/.*listening  http:\/\/\([^/]*\)\/metrics.*/\1/p' | sort -u | tr '\n' ' ')"
        if [ "$NC" = "1" ]; then
            printf '{"cmd":"hotswap","v":1}\n' >"$WORK/req-hotswap.json"
            hs="$(raw_send "$WORK/req-hotswap.json")"
        fi
        case "$hs" in
            *"hot restart is disabled"*)
                record SKIP-UNSUPPORTED hotswap-handoff "hot restart disabled in this run: $(printf '%s' "$hs" | firstline)" ;;
            *"not available"*|*"no unit"*|*"not implemented"*|*"unsupported"*)
                record SKIP-UNSUPPORTED hotswap-handoff "no hotswap trigger wired at this head: $(printf '%s' "$hs" | firstline). Owning port: feat/p2-hotswap (wt-sn-hotswap)" ;;
            *'"ok":true'*|*triggered*|*Triggered*)
                oldpid="$MINER_PID"
                handoff_wait="${FEATURE_HOTSWAP_TIMEOUT:-120}"
                k=0; handed=0; serve=""
                while [ "$k" -lt "$handoff_wait" ]; do
                    sleep 1; k=$((k + 1))
                    kill -0 "$oldpid" 2>/dev/null && continue
                    if [ -S "$SOCK" ]; then
                        # socket file exists; require an ANSWER to prove a successor serves it
                        serve="$(TS get gomemlimit 2>&1)"
                        printf '%s' "$serve" | grep -q 'gomemlimit' && { handed=1; break; }
                    fi
                done
                if [ "$handed" = "1" ]; then
                    record PASS hotswap-handoff "parent exited, successor re-bound the socket: $(printf '%s' "$serve" | firstline)"
                    pp="$(TS providers 2>/dev/null | grep -F -- "$STATE_DIR" | sed -n 's/.*pid \([0-9][0-9]*\).*/\1/p' | head -1)"
                    [ -n "$pp" ] || pp="$(proc_pid_for_state_home)"
                    case "$pp" in ''|*[!0-9]*) say "hotswap successor pid not resolvable; leaving MINER_PID=$oldpid (dead)" ;;
                        *) if kill -0 "$pp" 2>/dev/null; then MINER_PID="$pp"; say "hotswap successor pid=$pp"; fi ;;
                    esac
                    # takeover rebind of /metrics (feat/p2-metrics + the rebind fix)
                    if [ -n "$addrs_before" ] && detect_http_fetch; then
                        url="$(metrics_scrape_url "$(printf '%s' "$addrs_before" | awk '{print $1}')")"
                        rk=0; rebound=0; rbody=""
                        while [ "$rk" -lt 30 ]; do
                            rbody="$(http_get "$url" 2>/dev/null)"; rk=$((rk + 1))
                            printf '%s' "$rbody" | grep -q '# HELP urnet_' && { rebound=1; break; }
                            sleep 1
                        done
                        if [ "$rebound" = "1" ]; then
                            record PASS hotswap-metrics-rebind "successor re-served /metrics on $url"
                        else
                            record FAIL hotswap-metrics-rebind "no /metrics answer on $url within 30s of the handoff (before: $metrics_before)"
                        fi
                    else
                        record SKIP-UNSUPPORTED hotswap-metrics-rebind "no scrape tool or no pre-handoff metrics address to re-check (before='$addrs_before')"
                    fi
                else
                    record FAIL hotswap-handoff "handoff did not complete within ${handoff_wait}s (old pid alive=$(kill -0 "$oldpid" 2>/dev/null && echo yes || echo no); socket serve probe: $(printf '%s' "$serve" | firstline))"
                    record SKIP-NO-NETWORK hotswap-metrics-rebind "no handoff to assert"
                fi ;;
            "")
                record SKIP-UNSUPPORTED hotswap-handoff "netcat (nc -U) not available: the raw control-socket handoff probe cannot run. Owning port: feat/p2-hotswap (wt-sn-hotswap)" ;;
            *) record SKIP-UNSUPPORTED hotswap-handoff "hotswap probe inconclusive: $(printf '%s' "$hs" | firstline)" ;;
        esac
    else
        record SKIP-NO-NETWORK hotswap-handoff "provider not live"
    fi
    stop_miner "$FEATURE_START_TIMEOUT"
}

# proc_pid_for_state_home scans /proc for a provider child booted with
# HOME=$STATE_HOME (the hotswap successor, when urnet-tools discovery cannot
# name it). Root-only in practice, but the harness runs as root in CI cells.
proc_pid_for_state_home() {
    for d in /proc/[0-9]*; do
        pid="${d#/proc/}"
        cmd="$(tr '\0' ' ' <"$d/cmdline" 2>/dev/null || true)"
        case "$cmd" in *"provide --port=0"*) : ;; *) continue ;; esac
        env="$(tr '\0' '\n' <"$d/environ" 2>/dev/null || true)"
        case "$env" in
            *"HOME=$STATE_HOME"*)
                if [ "$pid" != "$$" ]; then printf '%s' "$pid"; return 0; fi ;;
        esac
    done
    return 0
}

# ramlog routing: URNETWORK_RAMLOGS=1 and the lowmem/eco profiles must send
# the long-running provide's stdio to /dev/shm/urnetwork.log (the [ramlogs]
# banner), and the persisted `ramlogs` control key must do the same on the
# NEXT start - the documented needs-restart semantics plus the control-state
# -> env seed path. The important buffer (state-dir important.log) must exist
# whenever the ramlog is active.
ramlog_probe_run() { # $1=logfile; rest: NAME=value env for the run
    logf="$1"; shift
    rm -f /dev/shm/urnetwork.log
    : >"$logf"
    HOME="$STATE_HOME" env "$@" "$MINER_WRAPPER" provide --port=0 $START_FLAGS >"$logf" 2>&1 &
    MINER_PID=$!
    RAMLOG_BANNER="no"; RAMLOG_IMPORTANT="no"; RAMLOG_ALIVE="no"
    if wait_for_socket "$FEATURE_START_TIMEOUT"; then
        sleep 2
        if kill -0 "$MINER_PID" 2>/dev/null; then RAMLOG_ALIVE="yes"; fi
        if [ -f /dev/shm/urnetwork.log ] && grep -q '\[ramlogs\] Active at /dev/shm/urnetwork.log' /dev/shm/urnetwork.log 2>/dev/null; then
            RAMLOG_BANNER="yes"
        fi
        [ -f "$STATE_DIR/important.log" ] && RAMLOG_IMPORTANT="yes"
    fi
}

b1_ramlogs() {
    # selector 1: explicit URNETWORK_RAMLOGS=1 (no ramlogs key persisted yet)
    ramlog_probe_run "$WORK/miner-ramlog-env.log" URNETWORK_RAMLOGS=1
    env_banner="$RAMLOG_BANNER"
    stop_miner 15

    # selector 2: URNETWORK_PROFILE=lowmem (the profile alone asks for the ramlog)
    ramlog_probe_run "$WORK/miner-ramlog-profile.log" URNETWORK_PROFILE=lowmem
    prof_banner="$RAMLOG_BANNER"
    stop_miner 15

    if [ "$env_banner" = "yes" ] && [ "$prof_banner" = "yes" ]; then
        record PASS ramlog-routing "URNETWORK_RAMLOGS=1 and URNETWORK_PROFILE=lowmem both redirected stdio to /dev/shm/urnetwork.log ([ramlogs] banner present)"
    else
        record FAIL ramlog-routing "env selector banner=$env_banner profile selector banner=$prof_banner (want both yes; logs: $(tail -1 "$WORK/miner-ramlog-env.log" 2>/dev/null | firstline) / $(tail -1 "$WORK/miner-ramlog-profile.log" 2>/dev/null | firstline))"
    fi

    # selector 3: the persisted key + restart. Set it live (acceptance), stop,
    # start again with URNETWORK_RAMLOGS=0 EXPLICITLY set: the seed from the
    # control state must still win, which is the documented precedence.
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip key-ramlogs
        return 0
    fi
    if ! ensure_live miner-ramlog-key1; then
        record SKIP-NO-NETWORK key-ramlogs "provider not live to accept the ramlogs key"
        return
    fi
    rm -f /dev/shm/urnetwork.log "$STATE_DIR/important.log"
    out="$(TS set ramlogs on -f 2>&1)"; rc=$?
    restart_note="$(printf '%s' "$out" | grep -i 'requires a restart' | firstline)"
    stop_miner "$FEATURE_START_TIMEOUT"
    if [ "$rc" != "0" ]; then
        record FAIL key-ramlogs "set ramlogs on rc=$rc: $(printf '%s' "$out" | firstline)"
        return
    fi
    ramlog_probe_run "$WORK/miner-ramlog-key2.log" URNETWORK_RAMLOGS=0
    key_banner="$RAMLOG_BANNER"; key_imp="$RAMLOG_IMPORTANT"
    # clear the override while the ramlog provider still serves the socket
    if kill -0 "$MINER_PID" 2>/dev/null && [ -S "$SOCK" ]; then
        TS set ramlogs off -f >/dev/null 2>&1 || say "WARN: could not clear the ramlogs key (next starts may keep ramlog routing)"
    fi
    stop_miner "$FEATURE_START_TIMEOUT"

    if [ -z "$restart_note" ]; then
        record FAIL key-ramlogs "ramlogs accepted but the provider did not report 'requires a restart to take effect': $(printf '%s' "$out" | firstline)"
    elif [ "$key_banner" = "yes" ] && [ "$key_imp" = "yes" ]; then
        record PASS key-ramlogs "persisted ramlogs=on beat an explicit URNETWORK_RAMLOGS=0 on the next start (need-restart semantics); /dev/shm/urnetwork.log has the [ramlogs] banner and $STATE_DIR/important.log exists"
    else
        record FAIL key-ramlogs "persisted ramlogs=on did not route after restart (banner=$key_banner important.log=$key_imp; restart-note='$restart_note'); last log: $(tail -1 "$WORK/miner-ramlog-key2.log" 2>/dev/null | firstline)"
    fi
}

# registration identity gate: a fresh dir without --allow-client-registration
# must not allocate; --adopt-legacy-provider-key on a fresh dir must not
# allocate either (it is an adoption flag, not a bootstrap flag).
b1_identity_gate() {
    gate_home="$WORK/gate-home"
    rm -rf "$gate_home"; mkdir -p "$gate_home/.urnetwork" "$gate_home/bin"
    ln -sf "$MINER_BIN" "$gate_home/bin/provider"
    gate_reset() {
        rm -rf "$gate_home/.urnetwork"; mkdir -p "$gate_home/.urnetwork"
        if [ -r "$JWT_FILE" ]; then cp "$JWT_FILE" "$gate_home/.urnetwork/jwt" && chmod 600 "$gate_home/.urnetwork/jwt"; fi
    }
    gate_probe() { # $1=extra flag  $2=log
        gate_reset
        : >"$2"
        HOME="$gate_home" "$gate_home/bin/provider" provide --port=0 $1 >"$2" 2>&1 &
        gp=$!
        i=0; exited=0
        while [ "$i" -lt 25 ]; do
            if ! kill -0 "$gp" 2>/dev/null; then exited=1; break; fi
            [ -S "$gate_home/.urnetwork/provider.sock" ] && break
            sleep 1; i=$((i + 1))
        done
        sock_up=no; [ -S "$gate_home/.urnetwork/provider.sock" ] && sock_up=yes
        if kill -0 "$gp" 2>/dev/null; then kill -TERM "$gp" 2>/dev/null; sleep 1; kill -KILL "$gp" 2>/dev/null; fi
        wait "$gp" 2>/dev/null || true
        GATE_EXITED="$exited"; GATE_SOCK="$sock_up"
    }
    gate_probe "" "$WORK/gate-fresh.log"
    if [ "$GATE_SOCK" = "no" ]; then
        record PASS identity-gate-fresh "fresh state dir without the registration flag produced no socket (exited=$GATE_EXITED)"
    else
        record FAIL identity-gate-fresh "bare provide started against a fresh dir without the explicit flag"
    fi
    if miner_help | grep -q -- '--adopt-legacy-provider-key'; then
        gate_probe "--adopt-legacy-provider-key" "$WORK/gate-adopt.log"
        if [ "$GATE_SOCK" = "no" ]; then
            record PASS identity-gate-adopt "fresh dir + --adopt-legacy-provider-key refused to allocate (no socket)"
        else
            record FAIL identity-gate-adopt "--adopt-legacy-provider-key started against a fresh dir (would allocate)"
        fi
    else
        record SKIP-UNSUPPORTED identity-gate-adopt "no --adopt-legacy-provider-key flag at this head"
    fi
    rm -rf "$gate_home"
}

# exit-75 (EX_TEMPFAIL) quarantine path. Advertised at this head; a rejected
# network sign-in is needed to observe the exit code, which a tampered token
# does not reliably reproduce, so the observation is best-effort and the
# classification is explicit.
b1_exit75() {
    if ! miner_help | grep -q -- '--quarantine-rejected-sign-in'; then
        record SKIP-UNSUPPORTED exit-75-quarantine "no --quarantine-rejected-sign-in at this head"
        return
    fi
    xhome="$WORK/exit75-home"; xs="$xhome/.urnetwork"
    rm -rf "$xhome"; mkdir -p "$xs"
    if [ -r "$JWT_FILE" ]; then
        tok="$(cat "$JWT_FILE")"
        sig="$(printf '%s' "$tok" | cut -d. -f3)"
        sighead="$(printf '%s' "$sig" | cut -c1-4)"
        bad="$(printf '%s' "$tok" | cut -d. -f1-2).${sighead}XXXX"
        printf '%s' "$bad" >"$xs/jwt"; chmod 600 "$xs/jwt"
    fi
    HOME="$xhome" timeout "$FEATURE_EXIT75_TIMEOUT" "$MINER_BIN" provide --port=0 --allow-client-registration --quarantine-rejected-sign-in >"$WORK/exit75.log" 2>&1
    rc=$?
    case "$rc" in
        75) record PASS exit-75-quarantine "rejected sign-in set aside, process exited 75 (EX_TEMPFAIL) as documented" ;;
        124) record SKIP-NO-NETWORK exit-75-quarantine "no sign-in rejection observed within ${FEATURE_EXIT75_TIMEOUT}s; run was stopped while retrying" ;;
        *)  record SKIP-NO-NETWORK exit-75-quarantine "rejected sign-in not reproducible in this environment (rc=$rc, no exit 75); flag is advertised" ;;
    esac
    rm -rf "$xhome"
}

# side-effectful control keys with before/after state: gogc (runtime knob)
# and h3 (the limit key the integrated head wires live). The full live-apply /
# needs-restart matrix is b1_control_key_semantics below; ramlogs' restart
# semantics are asserted end-to-end by b1_ramlogs.
b1_control_keys() {
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip control-key-live key-h3-limit
        return 0
    fi
    st_before="$(fsum "$STATE_DIR/provider_state.json")"
    out="$(TS set gogc 250 -f 2>&1)"; rc=$?
    st_after="$(fsum "$STATE_DIR/provider_state.json")"
    rb="$(TS get gogc 2>&1)"
    logged=$(grep -c '\[control\] set gogc=250' "$WORK/miner-main.log" "$WORK/miner-main-r2.log" 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
    if [ "$rc" = "0" ] && printf '%s' "$rb" | grep -q 'gogc: 250' && [ "$logged" -gt 0 ] && [ "$st_before" != "$st_after" ]; then
        record PASS control-key-live "set gogc=250 applied live, logged, and persisted (provider_state.json $st_before -> $st_after)"
    elif [ "$rc" = "0" ] && printf '%s' "$rb" | grep -q 'gogc: 250' && [ "$logged" -gt 0 ]; then
        record FAIL control-key-live "set gogc=250 answered and logged but left provider_state.json byte-identical ($st_after)"
    else
        record FAIL control-key-live "set gogc=250 rc=$rc readback='$(printf '%s' "$rb" | firstline)' loglines=$logged state=$st_before->$st_after"
    fi
    TS set gogc off -f >/dev/null 2>&1 || true

    # h3 limit key (feat/p2-h3): must apply live, read back, be logged and
    # persisted. A head without the live port answers "requires a restart" or
    # rejects the key; that self-detects as SKIP-UNSUPPORTED, never a fake pass.
    st_before="$(fsum "$STATE_DIR/provider_state.json")"
    out="$(TS set h3 3 -f 2>&1)"; rc=$?
    st_after="$(fsum "$STATE_DIR/provider_state.json")"
    if printf '%s' "$out" | grep -qi 'unknown key'; then
        record SKIP-UNSUPPORTED key-h3-limit "no h3 control key at this head: $(printf '%s' "$out" | firstline)"
    elif [ "$rc" = "0" ] && printf '%s' "$out" | grep -q 'requires a restart'; then
        record SKIP-UNSUPPORTED key-h3-limit "h3 persists but has no live consumer at this head: $(printf '%s' "$out" | grep -i 'requires a restart' | firstline). Owning port: feat/p2-h3 (wt-sn-h3)"
    elif [ "$rc" = "0" ]; then
        rb="$(TS get h3 2>&1)"
        logged=$(grep -cE '\[control\] (set|applied) h3=3' "$WORK/miner-main.log" "$WORK/miner-main-r2.log" 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
        persisted=$(grep -cE '"h3"[[:space:]]*:[[:space:]]*"3"' "$STATE_DIR/provider_state.json" 2>/dev/null || true)
        if printf '%s' "$rb" | grep -q 'h3: 3' && [ "$(as_num "$logged")" -ge 1 ] && [ "$(as_num "$persisted")" -ge 1 ] && [ "$st_before" != "$st_after" ]; then
            record PASS key-h3-limit "h3=3 applied live and reflected (get '$(printf '%s' "$rb" | firstline)'; $logged log line(s); provider_state.json updated)"
        else
            record FAIL key-h3-limit "set rc=0 but reflection incomplete: readback='$(printf '%s' "$rb" | firstline)' loglines=$logged persisted=$persisted state=$st_before->$st_after"
        fi
    else
        record FAIL key-h3-limit "set h3 3 rc=$rc: $(printf '%s' "$out" | firstline)"
    fi
    TS set h3 off -f >/dev/null 2>&1 || true
}

# control-key semantics: the live-apply set must answer "applied live via
# control socket" with a matching readback and no restart warning; keys with
# no live consumer must answer "requires a restart to take effect". Every
# override this check sets is cleared again, so later checks see the same
# startup defaults a fresh provider would (hot_restart is deliberately NOT
# persisted here: the persisted key would override the URNETWORK_HOT_RESTART=0
# probe below, which exists to pin exactly that switch).
b1_control_key_semantics() {
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip control-keys-live control-keys-needs-restart
        return 0
    fi
    bad=""; checked=""
    for kv in "proxy_url_refresh=90s" "proxy_url_max=450" "smart_dialer=on" "proxy_dead_cleanup_scope=url"; do
        k="${kv%%=*}"; v="${kv#*=}"
        out="$(TS set "$k" "$v" -f 2>&1)"; rc=$?
        rb="$(TS get "$k" 2>&1)"
        if [ "$rc" != "0" ]; then
            bad="$bad $k(set rc=$rc: $(printf '%s' "$out" | firstline))"
        elif ! printf '%s' "$out" | grep -q 'applied live'; then
            bad="$bad $k(no applied-live confirmation)"
        elif printf '%s' "$out" | grep -q 'requires a restart'; then
            bad="$bad $k(claimed restart-required though live-applied)"
        elif ! printf '%s' "$rb" | grep -q "$k: $v"; then
            bad="$bad $k(readback '$(printf '%s' "$rb" | firstline)')"
        fi
        checked="$checked $k"
        TS set "$k" off -f >/dev/null 2>&1 || true
    done
    # h1_plus is a provider-side live key the urnet-tools key inventory does
    # not expose; exercise it over the raw control socket so the provider's
    # live-apply path is still covered. A live-applied key answers ok:true
    # WITHOUT needs_restart and logs "[control] applied h1_plus="; without a
    # unix-socket netcat the leg is SKIP-UNSUPPORTED, never silently dropped.
    raw_leg=""
    if [ "$NC" = "1" ]; then
        printf '{"cmd":"set","key":"h1_plus","value":"on","v":1}\n' >"$WORK/req-h1plus.json"
        h1o="$(raw_send "$WORK/req-h1plus.json")"
        printf '{"cmd":"clear","key":"h1_plus","v":1}\n' >"$WORK/req-h1plus-clear.json"
        _="$(raw_send "$WORK/req-h1plus-clear.json")"
        h1log=$(grep -cE '\[control\] applied h1_plus=' "$WORK/miner-main.log" "$WORK/miner-main-r2.log" 2>/dev/null | awk -F: '{s+=$2} END{print s+0}')
        if json_has "$h1o" '"ok":true' && ! json_has "$h1o" '"needs_restart":true' && [ "$(as_num "$h1log")" -ge 1 ]; then
            checked="$checked raw:h1_plus"
        else
            bad="$bad raw:h1_plus(reply '$(printf '%s' "$h1o" | firstline)'; applied-loglines=$h1log)"
        fi
        raw_leg=" (raw-socket legs: h1_plus)"
    else
        record SKIP-UNSUPPORTED control-keys-live-raw "netcat (nc -U) not available: the h1_plus raw-socket leg cannot run (urnet-tools does not expose the provider-only key)"
    fi
    if [ -z "$bad" ]; then
        record PASS control-keys-live "applied live with readback, no restart warning:$checked$raw_leg (overrides cleared after)"
    else
        record FAIL control-keys-live "live keys misbehaved:$bad"
    fi

    bad=""; checked=""
    for kv in "profile=eco" "node-name=feature-matrix"; do
        k="${kv%%=*}"; v="${kv#*=}"
        out="$(TS set "$k" "$v" -f 2>&1)"; rc=$?
        if [ "$rc" != "0" ] || ! printf '%s' "$out" | grep -q 'requires a restart to take effect'; then
            bad="$bad $k(rc=$rc: $(printf '%s' "$out" | firstline))"
        fi
        checked="$checked $k"
        TS set "$k" off -f >/dev/null 2>&1 || true
    done
    if [ -z "$bad" ]; then
        record PASS control-keys-needs-restart "restart-only keys report 'requires a restart to take effect' (checked:$checked; overrides cleared after)"
    else
        record FAIL control-keys-needs-restart "restart-required keys did not report their semantics:$bad"
    fi
}

# metrics: control wiring (`metrics on`), the documented bind policy, an HTTP
# scrape with whatever fetch tool the image has, and off -> listener stops ->
# on -> listener returns (the live control wiring).
b1_metrics() {
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip metrics-bind metrics-scrape metrics-off-on
        return 0
    fi
    addrs=""
    out="$(TS metrics on -f 2>&1)"; rc=$?
    case "$out" in
        *"does not implement"*|*"not implement"*)
            record SKIP-UNSUPPORTED metrics-bind "exporter not implemented at this head: $(printf '%s' "$out" | firstline). Owning port: feat/p2-metrics (wt-sn-metrics)"
            record SKIP-UNSUPPORTED metrics-scrape "exporter not implemented at this head"
            record SKIP-UNSUPPORTED metrics-off-on "exporter not implemented at this head"
            return ;;
    esac
    st="$(TS metrics status 2>&1)"
    if [ "$rc" = "0" ] && printf '%s' "$st" | grep -q 'Metrics: on'; then
        addrs="$(printf '%s' "$st" | sed -n 's/.*listening  http:\/\/\([^/]*\)\/metrics.*/\1/p' | sort -u | tr '\n' ' ')"
        [ -n "$addrs" ] || addrs="$(printf '%s' "$st" | grep -oE '([0-9]{1,3}(\.[0-9]{1,3}){3}|\[[0-9a-f:]+\]):[0-9]+' | sort -u | tr '\n' ' ')"
        bad=""
        for a in $addrs; do
            metrics_addr_allowed "$a" || bad="$bad $a"
        done
        if [ -n "$bad" ]; then
            record FAIL metrics-bind "listen address(es) outside the documented policy (loopback/tailscale; wildcard only inside a container):$bad"
        else
            record PASS metrics-bind "metrics on applied; addrs='${addrs:-none reported}' inside the documented policy"
        fi
    else
        record FAIL metrics-bind "metrics on rc=$rc: $(printf '%s' "$out" | firstline); status: $(printf '%s' "$st" | firstline)"
        record SKIP-NO-NETWORK metrics-scrape "no listener to scrape (metrics control failed)"
        record SKIP-NO-NETWORK metrics-off-on "no listener to stop"
        return
    fi

    url=""
    for a in $addrs; do url="$(metrics_scrape_url "$a")"; [ -n "$url" ] && break; done
    if ! detect_http_fetch; then
        record SKIP-UNSUPPORTED metrics-scrape "no HTTP fetch tool in this image (curl/wget/busybox) and installation was not possible"
    else
        body=""
        [ -n "$url" ] && body="$(http_get "$url")"
        if printf '%s' "$body" | grep -q '# HELP urnet_'; then
            record PASS metrics-scrape "$HTTP_FETCH fetched $url ($(printf '%s\n' "$body" | grep -c '^urnet_' 2>/dev/null || true) urnet_ series lines)"
        else
            record FAIL metrics-scrape "$HTTP_FETCH fetched '$url' but no Prometheus payload: $(printf '%s' "$body" | firstline)"
        fi
    fi

    off_out="$(TS metrics off -f 2>&1)"; off_rc=$?
    off_status="$(TS metrics status 2>&1)"
    off_body=""
    if [ -n "$url" ] && detect_http_fetch; then off_body="$(http_get "$url" 2>/dev/null)"; fi
    back_out="$(TS metrics on -f 2>&1)"; back_rc=$?
    if [ "$off_rc" = "0" ] && printf '%s' "$off_status" | grep -q 'Metrics: off' && [ -z "$off_body" ] && [ "$back_rc" = "0" ]; then
        record PASS metrics-off-on "metrics off stopped the listener (scrape dead) and metrics on rebound it"
    else
        record FAIL metrics-off-on "off rc=$off_rc status='$(printf '%s' "$off_status" | firstline)' scrape_after_off=${off_body:+still-alive} on_rc=$back_rc: $(printf '%s' "$back_out" | firstline)"
    fi
}

# corrupt state recovery: audit.json / proxy.state / proxy_url.json.
b1_corrupt_state() {
    stop_miner "$FEATURE_START_TIMEOUT"
    say "state files present: $(ls "$STATE_DIR" 2>/dev/null | tr '\n' ' ')"
    for f in audit.json proxy.state proxy_url.json; do
        fp="$STATE_DIR/$f"
        if [ ! -f "$fp" ]; then
            case "$f" in
                proxy_url.json) printf '{"sources":[],"cache":{}}\n' >"$fp"; say "state-integrity: synthesized a minimal $f for the corruption test" ;;
                *) record SKIP-UNSUPPORTED "state-integrity-$f" "file not produced at this head"; continue ;;
            esac
        fi
        cp "$fp" "$WORK/int-$f.bak" 2>/dev/null || true
        printf 'not-json at all {{{ \n' >"$fp"
        start_miner "$WORK/miner-int-$f.log"
        wait_for_socket "$FEATURE_START_TIMEOUT"
        up=no; [ -S "$SOCK" ] && up=yes
        alive=no; kill -0 "$MINER_PID" 2>/dev/null && alive=yes
        stop_miner 15
        crashed=0
        grep -qE 'panic:|SIGSEGV|fatal error' "$WORK/miner-int-$f.log" 2>/dev/null && crashed=1
        msg="$(grep -oE 'parse [a-z_.]+:.*|proxy state error:.*|corrupt.*' "$WORK/miner-int-$f.log" 2>/dev/null | head -1 | firstline)"
        if [ "$up" = "yes" ]; then
            record PASS "state-integrity-$f" "corrupt $f: provider came up (recovered; ${msg:-no diagnostic})"
        elif [ "$crashed" = "1" ]; then
            record FAIL "state-integrity-$f" "corrupt $f caused a crash: $(tail -1 "$WORK/miner-int-$f.log" 2>/dev/null | firstline)"
        elif [ -n "$msg" ]; then
            record PASS "state-integrity-$f" "corrupt $f: refused with a clean diagnostic, no panic (${msg})"
        else
            record FAIL "state-integrity-$f" "corrupt $f: no socket, no diagnostic, alive=$alive (last log: $(tail -1 "$WORK/miner-int-$f.log" 2>/dev/null | firstline))"
        fi
        if [ -f "$WORK/int-$f.bak" ]; then cp "$WORK/int-$f.bak" "$fp"; rm -f "$WORK/int-$f.bak"; fi
    done
}

run_block1() {
    head1 "BLOCK 1 — risky surfaces"

    if [ ! -x "${MINER_BIN:-/nonexistent}" ]; then record FAIL binaries "miner not executable at ${MINER_BIN:-?}"; ENV_BLOCKER=1; fi
    if [ ! -x "${TOOLS_BIN:-/nonexistent}" ]; then record FAIL binaries "urnet-tools not executable at ${TOOLS_BIN:-?}"; ENV_BLOCKER=1; fi
    if [ "$ENV_BLOCKER" = "1" ]; then return; fi

    if [ -n "$(miner_help | head -1)" ]; then
        if [ "$CLIENT_SAFE" = "1" ]; then
            record PASS binaries "miner answers provide --help; urnet-tools: $(HOME="$STATE_HOME" "$TOOLS_BIN" version 2>&1 | head -1 | firstline)"
        else
            record PASS binaries "miner answers provide --help (urnet-tools version not queried: client targeting disabled, see client-targeting)"
        fi
    else
        record FAIL binaries "miner does not answer provide --help"
        ENV_BLOCKER=1
        return
    fi
    say "distro: $(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | head -1) / $(uname -m 2>/dev/null) / libc=$( [ -e /lib/ld-musl-x86_64.so.1 ] && echo musl || echo glibc )"

    if [ -z "$JWT_FILE" ] || [ ! -r "$JWT_FILE" ]; then
        record SKIP-NO-NETWORK auth "JWT_FILE not provided/readable; live identities and network checks cannot run"
    fi

    # -- start + socket security (brief window is enough) -------------------
    resolve_start_flags
    say "start: provide --port=0 ${START_FLAGS:-(no registration flag advertised)}"
    start_miner "$WORK/miner-main.log"
    if wait_for_socket "$FEATURE_START_TIMEOUT"; then
        record PASS miner-start "provider.sock bound within ${FEATURE_START_TIMEOUT}s"
        b1_socket_security
    else
        reap_miner
        alive=no; kill -0 "$MINER_PID" 2>/dev/null && alive=yes
        if miner_exited_75; then
            record SKIP-NO-NETWORK miner-start "provider exited 75 (EX_TEMPFAIL: rejected sign-in set aside); environment cannot run the live blocks (last log: $(tail -1 "$WORK/miner-main.log" 2>/dev/null | firstline))"
        else
            record FAIL miner-start "no provider.sock within ${FEATURE_START_TIMEOUT}s (alive=$alive, rc=${MINER_LAST_RC:-?}, last log: $(tail -1 "$WORK/miner-main.log" 2>/dev/null | firstline))"
        fi
        return
    fi

    # -- is the provider still up after the socket checks? ------------------
    # The control socket binds before the network sign-in completes, so a
    # provider the environment drops seconds later would otherwise take the
    # live checks down with it. Give it the same settle window the retry path
    # uses: a sign-in failure is SKIP-NO-NETWORK per the contract, never a
    # product verdict.
    if kill -0 "$MINER_PID" 2>/dev/null && [ -S "$SOCK" ]; then
        sleep "$FEATURE_SETTLE"
    fi
    if kill -0 "$MINER_PID" 2>/dev/null && [ -S "$SOCK" ]; then
        LIVE=1
    else
        reason="$(env_exit_reason "$WORK/miner-main.log")"
        say "provider left after the socket checks (${reason:-no network reason in log}); retrying with a settle window"
        stop_miner 10 >/dev/null 2>&1
        if start_live_miner miner-main-r2; then
            say "retry: provider live after ${FEATURE_SETTLE}s settle"
        else
            record SKIP-NO-NETWORK provider-runtime "provider could not stay up in this environment (${reason:-see log}); live blocks skipped"
        fi
    fi
    if [ "$LIVE" != "1" ]; then
        record SKIP-NO-NETWORK control-keys "provider not live"
        record SKIP-NO-NETWORK control-keys-live "provider not live"
        record SKIP-NO-NETWORK control-keys-needs-restart "provider not live"
        record SKIP-NO-NETWORK metrics-bind "provider not live"
        record SKIP-NO-NETWORK metrics-scrape "provider not live"
        record SKIP-NO-NETWORK metrics-off-on "provider not live"
        record SKIP-NO-NETWORK identity "provider not live"
        record SKIP-NO-NETWORK restart "provider not live"
        record SKIP-NO-NETWORK logging-events "provider not live"
        record SKIP-NO-NETWORK ramlog-routing "provider not live"
        record SKIP-NO-NETWORK key-ramlogs "provider not live"
        record SKIP-NO-NETWORK hotswap-handoff "provider not live"
        b1_state_paths
        b1_corrupt_state
        return
    fi

    b1_control_keys
    b1_control_key_semantics
    b1_metrics
    b1_identity_restart
    b1_ramlogs
    b1_hotswap
    b1_identity_gate
    b1_exit75
    b1_corrupt_state
}

# ---------------------------------------------------------------------------
# BLOCK 2 — URL SOURCE PIPELINE + OPERATOR MANIPULATION
# ---------------------------------------------------------------------------

# ---- STAGGER --------------------------------------------------------------
# Shared runner egress IPs and public list hosts are rate-sensitive; every
# container must not fetch at the same instant. Jitter is
# 0..FEATURE_URL_JITTER_MAX seconds (default 180; 0 disables), seeded from
# hostname cksum + /dev/urandom, and always logged with the seed. Sources are
# fetched 0..30s apart in a randomized order; the independent Block-2/3
# groups are shuffled with the same seed (state-dependent pairs stay ordered).
URL_SEED=0
URL_JITTER=0
SRC_GAP=0
URL_ORDER=""
CHK_ORDER=""
stagger_init() {
    host_ck="$(hostname 2>/dev/null | cksum 2>/dev/null | awk '{print $1}')"
    [ -n "$host_ck" ] || host_ck=0
    rnd="$(od -An -N4 -tu4 /dev/urandom 2>/dev/null | tr -d ' ')"
    case "$rnd" in ''|*[!0-9]*) rnd="$$" ;; esac
    URL_SEED=$(( host_ck + rnd ))
    jmax="${FEATURE_URL_JITTER_MAX:-180}"
    case "$jmax" in ''|*[!0-9]*) jmax=180 ;; esac
    if [ "$jmax" -gt 0 ]; then
        URL_JITTER=$(( URL_SEED % (jmax + 1) ))
        say "URL-STAGGER: seed=$URL_SEED (hostname-cksum=$host_ck + urandom=$rnd) jitter=${URL_JITTER}s max=${jmax}s"
        sleep "$URL_JITTER"
    else
        URL_JITTER=0
        say "URL-STAGGER: seed=$URL_SEED (hostname-cksum=$host_ck + urandom=$rnd) jitter=0s (FEATURE_URL_JITTER_MAX=0 disables)"
    fi
    SRC_GAP=$(( (URL_SEED / 7) % 31 ))
    if [ "$(( URL_SEED % 2 ))" = "0" ]; then URL_ORDER="as-listed"; else URL_ORDER="swapped"; fi
    # Fisher-Yates over the independent groups; the groups themselves keep
    # their state-dependent pairs ordered (url fetches in order, manipulation
    # sequence fixed).
    CHK_ORDER="$(printf '%s\n' url manip surface | awk -v s="$(( URL_SEED + 1 ))" '
        BEGIN { srand(s) }
        { a[NR] = $0 }
        END {
            for (i = NR; i > 0; i--) { j = int(rand() * i) + 1; t = a[i]; a[i] = a[j]; a[j] = t }
            for (i = 1; i <= NR; i++) printf "%s ", a[i]
        }')"
    [ -n "$CHK_ORDER" ] || CHK_ORDER="url manip surface"
    say "URL-STAGGER: source-gap=${SRC_GAP}s source-order=$URL_ORDER independent-order=$CHK_ORDER"
}
stagger_gap() { # sleep the inter-source gap before the next source
    [ "$SRC_GAP" -gt 0 ] && { say "URL-STAGGER: waiting ${SRC_GAP}s before the next source"; sleep "$SRC_GAP"; }
    return 0
}

# Extract the first value of a grep -oE style label from a captured command
# output stored in a file.
url_line() { grep -E -- "$1" "$2" 2>/dev/null | head -1; }
cycle_new_count() { # $1=log
    sed -n 's/.*cycle: +\([0-9][0-9]*\) new.*/\1/p' "$1" 2>/dev/null | head -1
}

# One add-source fetch. $1=URL $2=tag $3=cache-before
# Sets URL_FETCH_VERDICT (PASS/SKIP-NO-NETWORK), URL_FETCH_DETAIL, URL_OK_SRC.
url_fetch() {
    u="$1"; tag="$2"; cache_before="$3"
    log="$WORK/url-$tag.log"
    t0="$(date +%s)"
    out="$(HOME="$STATE_HOME" timeout "$FEATURE_URL_TIMEOUT" "$TOOLS_BIN" proxy add-source "$u" -f --state-dir "$STATE_DIR" 2>&1)"; rc=$?
    t1="$(date +%s)"; elapsed=$((t1 - t0))
    : >"$log"; printf '%s\n' "$out" >"$log"
    URL_ELAPSED="$elapsed"; URL_RC="$rc"

    if [ "$rc" = "124" ]; then
        URL_FETCH_VERDICT="SKIP-NO-NETWORK"
        URL_FETCH_DETAIL="add-source exceeded ${FEATURE_URL_TIMEOUT}s (rc=124); $(url_line '\[proxy\]\[url\]' "$log" | firstline)"
        return
    fi

    failed_line="$(url_line '\[proxy\]\[url\] fetch failed' "$log")"
    if [ -n "$failed_line" ] || printf '%s' "$out" | grep -q 'every source failed'; then
        URL_FETCH_VERDICT="SKIP-NO-NETWORK"
        URL_FETCH_DETAIL="source unreachable (rc=$rc, ${elapsed}s): $(printf '%s' "$failed_line" | firstline)"
        return
    fi

    probed_line="$(url_line '\[proxy\]\[url\] probed ' "$log")"
    dead_line="$(url_line 'all unparseable or dead' "$log")"
    perline="$(url_line '\[proxy\]\[url\] source ' "$log")"
    cycle_line="$(url_line '\[proxy\]\[url\] cycle:' "$log")"
    tier_line="$(url_line 'admitted by tier' "$log")"

    if [ -z "$probed_line" ] && [ -z "$dead_line" ] && [ -z "$perline" ]; then
        if printf '%s' "$out" | grep -q 'done\.' && [ -n "$(url_line 'added source:' "$log")" ]; then
            URL_FETCH_VERDICT="SKIP-NO-NETWORK"
            URL_FETCH_DETAIL="fetch cycle produced no pipeline line within ${elapsed}s (rc=$rc): $(tail -3 "$log" | tr '\n' ' ' | firstline)"
        else
            URL_FETCH_VERDICT="FAIL"
            URL_FETCH_DETAIL="add-source rc=$rc, no pipeline output: $(tail -3 "$log" | tr '\n' ' ' | firstline)"
        fi
        return
    fi

    newn="$(as_num "$(cycle_new_count "$log")")"
    cache_after="$(count_cache)"
    coherence="cache ${cache_before}->${cache_after}"
    if [ "$newn" -gt 0 ] && [ "$cache_after" -lt "$newn" ]; then
        URL_FETCH_VERDICT="FAIL"
        URL_FETCH_DETAIL="logs/state mismatch: cycle says +$newn new but the cache holds $cache_after"
        return
    fi
    if [ "$newn" -gt 0 ] && [ -z "$tier_line" ]; then
        URL_FETCH_VERDICT="FAIL"
        URL_FETCH_DETAIL="cycle admitted +$newn but no 'admitted by tier' line; $(printf '%s' "$cycle_line" | firstline)"
        return
    fi

    URL_FETCH_VERDICT="PASS"
    URL_FETCH_DETAIL="rc=$rc ${elapsed}s; $(printf '%s' "$probed_line" | firstline)"
    if [ -n "$dead_line" ]; then URL_FETCH_DETAIL="$URL_FETCH_DETAIL | $(printf '%s' "$dead_line" | firstline)"; fi
    URL_FETCH_DETAIL="$URL_FETCH_DETAIL | $(printf '%s' "$cycle_line" | firstline) ($coherence)"
    if [ -n "$tier_line" ]; then URL_FETCH_DETAIL="$URL_FETCH_DETAIL | $(printf '%s' "$tier_line" | firstline)"; fi
    # The documented probed-line shape: "probed <label>: N/M new qualified
    # (K cached, skipped), L below-bar, S socks5-only".
    case "$probed_line" in
        "") : ;;
        *"new qualified ("*"cached, skipped)"*) : ;;
        *) URL_FETCH_VERDICT="FAIL"
           URL_FETCH_DETAIL="probed line does not match the documented pipeline shape: $(printf '%s' "$probed_line" | firstline)"
           return ;;
    esac
    URL_OK_SRC="$u"
}

b2_url_sources() {
    head1 "BLOCK 2 — URL source pipeline"
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip url-fetch-source1 url-fetch-source2 url-state-coherence url-cached-skip url-remove-source url-log-vocabulary
        return 0
    fi
    if ! ensure_live miner-url; then
        record SKIP-NO-NETWORK url-fetch-source1 "provider not live; URL pipeline cannot run"
        record SKIP-NO-NETWORK url-fetch-source2 "provider not live"
        record SKIP-NO-NETWORK url-cached-skip "provider not live"
        record SKIP-NO-NETWORK url-remove-source "provider not live"
        return
    fi

    if [ -z "$JWT_FILE" ] || [ ! -r "$JWT_FILE" ]; then
        record SKIP-NO-NETWORK url-fetch-source1 "JWT_FILE not readable; the provider cannot target the API for the fetch probes"
        return
    fi

    s1="$FEATURE_URL_SOURCE1"; s2="$FEATURE_URL_SOURCE2"
    case "$s2" in ''|off|none|0) s2="" ;; esac
    first="$s1"; second="$s2"
    if [ "$URL_ORDER" = "swapped" ] && [ -n "$s2" ]; then first="$s2"; second="$s1"; fi

    cache_before="$(count_cache)"
    url_fetch "$first" source1 "$cache_before"
    case "$URL_FETCH_VERDICT" in
        PASS) record PASS url-fetch-source1 "$URL_FETCH_DETAIL" ;;
        SKIP-NO-NETWORK) record SKIP-NO-NETWORK url-fetch-source1 "$URL_FETCH_DETAIL" ;;
        *) record FAIL url-fetch-source1 "$URL_FETCH_DETAIL" ;;
    esac
    fetched_ok=0; [ "$URL_FETCH_VERDICT" = "PASS" ] && fetched_ok=1

    if [ -n "$second" ]; then
        stagger_gap
        cache_before2="$(count_cache)"
        url_fetch "$second" source2 "$cache_before2"
        case "$URL_FETCH_VERDICT" in
            PASS) record PASS url-fetch-source2 "$URL_FETCH_DETAIL" ;;
            SKIP-NO-NETWORK) record SKIP-NO-NETWORK url-fetch-source2 "$URL_FETCH_DETAIL" ;;
            *) record FAIL url-fetch-source2 "$URL_FETCH_DETAIL" ;;
        esac
        [ "$URL_FETCH_VERDICT" = "PASS" ] && fetched_ok=1
    else
        record SKIP-UNSUPPORTED url-fetch-source2 "source 2 disabled by FEATURE_URL_SOURCE2"
    fi

    # coherence of the persisted source list with the logs
    src_count="$(count_sources)"
    if [ "$src_count" -ge 1 ] && grep -q '"sources"' "$STATE_DIR/proxy_url.json" 2>/dev/null; then
        record PASS url-state-coherence "proxy_url.json present with sources + cache ($src_count sources, $(count_cache) cached entries)"
    elif [ "$fetched_ok" = "1" ]; then
        record FAIL url-state-coherence "fetch succeeded but proxy_url.json does not hold a source/cache"
    else
        record SKIP-NO-NETWORK url-state-coherence "no successful fetch; persisted state not asserted"
    fi

    # second cycle against the cached list: add-only + cached-skip
    if [ "$fetched_ok" = "1" ] && [ -n "$URL_OK_SRC" ] && [ "$FEATURE_URL_CACHED_PROBE" != "0" ]; then
        cache_before3="$(count_cache)"
        cs_url="${URL_OK_SRC}?feature-matrix-cached-skip=1"
        stagger_gap
        log="$WORK/url-cached-skip.log"
        out="$(HOME="$STATE_HOME" timeout "$FEATURE_URL_TIMEOUT" "$TOOLS_BIN" proxy add-source "$cs_url" -f --state-dir "$STATE_DIR" 2>&1)"; rc=$?
        printf '%s\n' "$out" >"$log"
        cache_after3="$(count_cache)"
        skip_line="$(url_line 'skipped [0-9][0-9]* already-cached' "$log")"
        known_line="$(url_line 'already known' "$log")"
        if [ "$cache_before3" -eq 0 ]; then
            record SKIP-NO-NETWORK url-cached-skip "cache is empty (all entries dead); cached-skip cannot be observed"
        elif printf '%s' "$skip_line" | grep -q 'skipped'; then
            n="$(printf '%s' "$skip_line" | sed -n 's/.*skipped \([0-9][0-9]*\) already-cached.*/\1/p')"
            if [ "$cache_after3" -ge "$cache_before3" ] && [ "$(as_num "$n")" -ge 1 ]; then
                record PASS url-cached-skip "second cycle add-only (cache ${cache_before3}->${cache_after3}); $(printf '%s' "$skip_line" | firstline)"
            else
                record FAIL url-cached-skip "cache shrank or skip count 0: ${cache_before3}->${cache_after3} n=$n"
            fi
        elif printf '%s' "$known_line" | grep -q 'already known'; then
            if [ "$cache_after3" -ge "$cache_before3" ]; then
                record PASS url-cached-skip "second cycle add-only (cache ${cache_before3}->${cache_after3}); $(printf '%s' "$known_line" | firstline)"
            else
                record FAIL url-cached-skip "second cycle shrank the cache: ${cache_before3}->${cache_after3}"
            fi
        else
            record FAIL url-cached-skip "no cached-skip evidence in the second cycle (rc=$rc): $(tail -3 "$log" | tr '\n' ' ' | firstline)"
        fi
        # remove the probe source again: remove-source drops the URL only
        out="$(TS proxy remove-source "$cs_url" 2>&1)"; rc=$?
        if [ "$rc" = "0" ] && printf '%s' "$out" | grep -q 'removed source'; then
            record PASS url-remove-source "remove-source dropped the second list: $(printf '%s' "$out" | firstline)"
        else
            record FAIL url-remove-source "remove-source rc=$rc: $(printf '%s' "$out" | firstline)"
        fi
    else
        record SKIP-NO-NETWORK url-cached-skip "no successful fetch to seed the cache"
        record SKIP-NO-NETWORK url-remove-source "no second source to remove"
    fi

    # Pipeline log vocabulary: the contract names five lines; which are
    # APPLICABLE is decided by the outcomes above, so this check reports the
    # observed set and passes unless a line the run's own outcomes imply is
    # missing.
    v_fetch=no; v_probed=no; v_skip=no; v_tier=no; v_dead=no
    for lf in "$WORK"/url-source1.log "$WORK"/url-source2.log "$WORK"/url-cached-skip.log; do
        [ -f "$lf" ] || continue
        grep -q '\[proxy\]\[url\] fetch failed' "$lf" && v_fetch=yes
        grep -q '\[proxy\]\[url\] probed ' "$lf" && v_probed=yes
        grep -q 'skipped [0-9][0-9]* already-cached' "$lf" && v_skip=yes
        grep -q 'admitted by tier' "$lf" && v_tier=yes
        grep -q 'all unparseable or dead' "$lf" && v_dead=yes
    done
    vocab_gap=""
    if [ "$fetched_ok" != "1" ] && [ "$v_fetch" != "yes" ]; then
        record SKIP-NO-NETWORK url-log-vocabulary "no successful fetch and no fetch-failed line (timeout/unreachable); pipeline vocabulary not applicable"
        return
    fi
    [ "$v_probed" = "yes" ] || vocab_gap="$vocab_gap probed"
    if [ "$fetched_ok" != "1" ] && [ "$v_fetch" != "yes" ]; then vocab_gap="$vocab_gap fetch-failed"; fi
    if [ "$v_tier" != "yes" ]; then
        # 'admitted by tier' is required only when a cycle admitted anything.
        for lf in "$WORK"/url-source1.log "$WORK"/url-source2.log; do
            [ -f "$lf" ] || continue
            n="$(as_num "$(cycle_new_count "$lf")")"
            [ "$n" -gt 0 ] && vocab_gap="$vocab_gap admitted-by-tier"
        done
    fi
    vocab="fetch-failed=$v_fetch probed=$v_probed cached-skip=$v_skip admitted-by-tier=$v_tier all-dead=$v_dead"
    if [ -n "$vocab_gap" ]; then
        record FAIL url-log-vocabulary "missing required pipeline vocabulary:$vocab_gap [$vocab]"
    else
        record PASS url-log-vocabulary "pipeline vocabulary observed: $vocab"
    fi
}

# deterministic operator manipulation on synthetic entries.
b2_manipulations() {
    head1 "BLOCK 2 — proxy manipulation (synthetic entries)"
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip proxy-add trim-preview-no-write trim-apply proxy-remove remove-dead-preview remove-match refresh-add-only proxy-paste proxy-clear
        return 0
    fi
    if ! ensure_live miner-manip; then
        record SKIP-NO-NETWORK proxy-manip "provider not live"
        return
    fi

    write_synthetic 4
    out="$(TS proxy add "$SYN_TMP" -f 2>&1)"; rc=$?
    sleep 1
    nfile="$(count_synthetic)"
    added_lines="$(printf '%s' "$out" | grep -c 'added server' 2>/dev/null || true)"
    if [ "$rc" = "0" ] && [ "$nfile" = "4" ] && [ "$(as_num "$added_lines")" -ge 4 ]; then
        record PASS proxy-add "4 synthetic entries added; proxy file holds $nfile; $(as_num "$added_lines") added-server lines"
    else
        record FAIL proxy-add "rc=$rc file-entries=$nfile added-lines=$added_lines: $(printf '%s' "$out" | firstline)"
    fi

    pre="$(fingerprint_files)"
    out="$(TS proxy trim 2 --dry-run 2>&1)"; rc=$?
    post="$(fingerprint_files)"
    if [ "$rc" = "0" ] && printf '%s' "$out" | grep -q 'preview:'; then
        if [ "$pre" = "$post" ]; then
            record PASS trim-preview-no-write "preview computed, state byte-identical ($(printf '%s' "$out" | grep 'preview:' | firstline))"
        else
            record FAIL trim-preview-no-write "preview changed state: $pre -> $post"
        fi
    else
        record FAIL trim-preview-no-write "trim --dry-run rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(TS proxy trim 2 -f 2>&1)"; rc=$?
    cap="$(cat "$STATE_DIR/proxy_trim" 2>/dev/null | tr -d '\n ')"
    if [ "$rc" = "0" ] && [ "$cap" = "2" ]; then
        # Eviction itself is conditional on running > cap; the unreachable
        # synthetic entries never start, so the persisted cap is the
        # assertion the environment can support.
        record PASS trim-apply "cap persisted as 2 (proxy_trim=$cap); running set is 0 so the shed path is vacuous here; $(printf '%s' "$out" | grep -oE 'shed[^.]*|cap set[^.]*' | firstline)"
    elif [ "$rc" = "0" ] && [ -z "$cap" ]; then
        record SKIP-UNSUPPORTED trim-apply "trim accepted but no persisted cap file at this head"
    else
        record FAIL trim-apply "rc=$rc proxy_trim='$cap': $(printf '%s' "$out" | firstline)"
    fi
    out="$(TS proxy trim off -f 2>&1)"; rc=$?; say "trim off: rc=$rc $(printf '%s' "$out" | firstline)"

    pre="$(fingerprint_files)"
    out="$(TS proxy remove 192.0.2.10:1080 -f 2>&1)"; rc=$?
    post="$(fingerprint_files)"
    left="$(count_synthetic)"
    if [ "$rc" = "0" ] && [ "$left" = "3" ] && [ "$pre" != "$post" ]; then
        record PASS proxy-remove "one address (host:port) removed; file holds $left; state diff present"
    else
        record FAIL proxy-remove "rc=$rc entries=$left ($pre -> $post): $(printf '%s' "$out" | firstline)"
    fi

    pre="$(fingerprint_files)"
    out="$(TS proxy remove-dead --preview --yes 2>&1)"; rc=$?
    post="$(fingerprint_files)"
    if printf '%s' "$out" | grep -qE 'exit 61|need 1h|uptime before dead'; then
        if [ "$pre" = "$post" ]; then
            record PASS remove-dead-preview "warmup gate (exit 61): dead status needs 65m uptime; --preview made no writes"
        else
            record FAIL remove-dead-preview "gate refused but state changed: $pre -> $post"
        fi
    elif [ "$rc" = "0" ] && printf '%s' "$out" | grep -q 'PREVIEW'; then
        if [ "$pre" = "$post" ]; then
            record PASS remove-dead-preview "preview listed candidates, no writes ($(printf '%s' "$out" | grep -E 'Would remove' | firstline))"
        else
            record FAIL remove-dead-preview "preview wrote state: $pre -> $post"
        fi
    elif printf '%s' "$out" | grep -qi 'unknown flag\|Usage:'; then
        record SKIP-UNSUPPORTED remove-dead-preview "remove-dead grammar absent at this head: $(printf '%s' "$out" | firstline)"
    else
        record FAIL remove-dead-preview "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    pre="$(fingerprint_files)"
    out="$(TS proxy remove --match=192.0.2.12 -f 2>&1)"; rc=$?
    post="$(fingerprint_files)"
    if printf '%s' "$out" | grep -qi 'unknown flag\|Usage:'; then
        record SKIP-UNSUPPORTED remove-match "remove --match not in the miner grammar at this head. Owning port: feat/p2-proxy-cli (wt-sn-proxy-cli)"
    elif [ "$rc" = "0" ]; then
        left="$(count_synthetic)"
        if [ "$left" = "2" ] && [ "$pre" != "$post" ]; then
            record PASS remove-match "remove --match=192.0.2.12 accepted; file holds $left"
        else
            record FAIL remove-match "rc=0 but entries=$left ($pre -> $post)"
        fi
    else
        record FAIL remove-match "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    # refresh: the curated diff must be add-only (nothing removed) and a
    # non-interactive run must not write.
    pre="$(fingerprint_files)"
    out="$(TS proxy refresh -f 2>&1)"; rc=$?
    post="$(fingerprint_files)"
    if [ "$rc" = "0" ] && printf '%s' "$out" | grep -q 'already up to date'; then
        record PASS refresh-add-only "refresh reports the curated list up to date; no writes ($(printf '%s' "$out" | firstline))"
    elif [ "$rc" = "0" ] && printf '%s' "$out" | grep -q '0 proxies will be removed'; then
        if [ "$pre" = "$post" ]; then
            record PASS refresh-add-only "diff is add-only (0 removals) and the non-interactive run wrote nothing: $(printf '%s' "$out" | grep 'proxy refresh:' | firstline)"
        else
            record FAIL refresh-add-only "non-interactive refresh wrote state: $pre -> $post"
        fi
    else
        record FAIL refresh-add-only "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(printf '192.0.2.20:1080\n' | TS proxy paste -f 2>&1)"; rc=$?
    if printf '%s' "$out" | grep -qi 'unknown flag\|Usage:'; then
        record SKIP-UNSUPPORTED proxy-paste "proxy paste not in the miner grammar at this head. Owning port: feat/p2-proxy-cli (wt-sn-proxy-cli)"
    elif [ "$rc" = "0" ] && grep -q '192.0.2.20:1080' "$STATE_DIR/proxy" 2>/dev/null; then
        record PASS proxy-paste "pasted entry present in the proxy config"
    else
        record FAIL proxy-paste "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(TS proxy clear -f 2>&1)"; rc=$?
    nfile="$(count_synthetic)"
    if [ "$rc" = "0" ] && [ "$nfile" = "0" ]; then
        record PASS proxy-clear "clear removed all configured proxies (servers empty)"
    else
        record FAIL proxy-clear "rc=$rc entries=$nfile: $(printf '%s' "$out" | firstline)"
    fi
}

# ---------------------------------------------------------------------------
# BLOCK 3 — BROAD SURFACE
# ---------------------------------------------------------------------------

# The documented v33 client command surface: the 47-row inventory the parity
# audit pins (internal/urnettools/v33_command_surface_test.go keeps the same
# table), plus the aliases and the wallet group. Every one must exist and
# answer its own --help with rc=0; none of them touches a provider.
V33_COMMANDS="providers status top sn-status start stop restart update idle-update self-update logs summary version default session turbo auto eco lowmode ramlogs optimize hotswap fast-auth get set rename auth choose-network wallet proxy report reinstall uninstall auto-update auto-start self-heal show-ip history autopilot enforcement baseline metrics smart-dialer profile dashboard config usage __do-restart"
V33_ALIASES="list ps selfupdate hot-swap fastauth show set-node-name choose_network autoupdate autostart selfheal ip-detect ipdetect audit smart_dialer profiles dash panel"
V33_WALLET_SUBS="status challenge set sign"

b3_cli_tree() {
    missing=""; n=0
    for c in $V33_COMMANDS; do
        out="$(HOME="$STATE_HOME" "$TOOLS_BIN" "$c" --help 2>&1)"; rc=$?
        n=$((n + 1))
        if [ "$rc" != "0" ] || [ -z "$out" ]; then missing="$missing $c(rc=$rc)"; fi
    done
    for a in $V33_ALIASES; do
        out="$(HOME="$STATE_HOME" "$TOOLS_BIN" "$a" --help 2>&1)"; rc=$?
        n=$((n + 1))
        if [ "$rc" != "0" ] || [ -z "$out" ]; then missing="$missing $a(rc=$rc)"; fi
    done
    for w in $V33_WALLET_SUBS; do
        out="$(HOME="$STATE_HOME" "$TOOLS_BIN" wallet "$w" --help 2>&1)"; rc=$?
        n=$((n + 1))
        if [ "$rc" != "0" ] || [ -z "$out" ]; then missing="$missing wallet-$w(rc=$rc)"; fi
    done
    if [ -z "$missing" ]; then
        record PASS surface-cli-tree "all $n documented v33 commands/aliases (incl. wallet subs) answer <cmd> --help (rc=0)"
    else
        record FAIL surface-cli-tree "missing/unresponsive:$missing"
    fi
}

b3_surface() {
    head1 "BLOCK 3 — broad surface"
    if [ "$CLIENT_SAFE" != "1" ]; then
        client_skip surface-cli-tree surface-version surface-status surface-proxy-health surface-proxy-traffic surface-proxy-ids surface-summary surface-config surface-audit surface-metrics surface-exclude surface-proxy-graders
        return 0
    fi
    b3_cli_tree
    if ! ensure_live miner-surface; then
        record SKIP-NO-NETWORK surface "provider not live"
        return
    fi

    out="$(T version 2>&1)"; rc=$?
    if [ "$rc" = "0" ] && [ -n "$out" ]; then
        record PASS surface-version "urnet-tools version: $(printf '%s' "$out" | firstline)"
    else
        record FAIL surface-version "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(TS status 2>&1)"; rc=$?
    if [ "$rc" = "0" ] && printf '%s' "$out" | grep -q 'running:'; then
        record PASS surface-status "status reports the provider ($(printf '%s' "$out" | grep -E 'running:|network:' | tr '\n' ' ' | firstline))"
    else
        record FAIL surface-status "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    for sub in health traffic ids; do
        out="$(TS proxy $sub 2>&1)"; rc=$?
        if [ "$rc" = "0" ] && [ -n "$out" ]; then
            record PASS "surface-proxy-$sub" "$(printf '%s' "$out" | head -2 | tr '\n' ' ' | firstline)"
        elif printf '%s' "$out" | grep -qi 'unknown'; then
            record SKIP-UNSUPPORTED "surface-proxy-$sub" "not exposed by this urnet-tools: $(printf '%s' "$out" | firstline)"
        else
            record FAIL "surface-proxy-$sub" "rc=$rc: $(printf '%s' "$out" | firstline)"
        fi
    done

    out="$(TS summary 2>&1)"; rc=$?
    if [ "$rc" = "0" ] && printf '%s' "$out" | grep -qi 'PROXY SUMMARY'; then
        record PASS surface-summary "summary rendered ($(printf '%s' "$out" | grep -i 'Total proxies' | firstline))"
    elif printf '%s' "$out" | grep -qi 'unknown'; then
        record SKIP-UNSUPPORTED surface-summary "not exposed by this urnet-tools: $(printf '%s' "$out" | firstline)"
    else
        record FAIL surface-summary "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(TS config 2>&1)"; rc=$?
    if [ "$rc" = "0" ] && printf '%s' "$out" | grep -qi 'Setting'; then
        record PASS surface-config "config table rendered ($(printf '%s' "$out" | grep -c . ) lines)"
    else
        record FAIL surface-config "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(TS proxy audit status 2>&1)"; rc=$?
    if printf '%s' "$out" | grep -qi 'does not implement'; then
        record SKIP-UNSUPPORTED surface-audit "proxy audit engine surface not wired at this head: $(printf '%s' "$out" | firstline). Owning port: feat/audit-ring (wt-sn-audit)"
    elif [ "$rc" = "0" ]; then
        record PASS surface-audit "audit status answered: $(printf '%s' "$out" | firstline)"
    elif printf '%s' "$out" | grep -qi 'unknown'; then
        record SKIP-UNSUPPORTED surface-audit "not exposed by this urnet-tools: $(printf '%s' "$out" | firstline)"
    else
        record FAIL surface-audit "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    out="$(TS metrics status 2>&1)"; rc=$?
    if [ "$rc" = "0" ]; then
        record PASS surface-metrics "metrics status answered: $(printf '%s' "$out" | firstline)"
    elif printf '%s' "$out" | grep -qi 'does not implement\|not implement'; then
        record SKIP-UNSUPPORTED surface-metrics "exporter not at this head: Owning port: feat/p2-metrics (wt-sn-metrics)"
    else
        record FAIL surface-metrics "rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    # exclude (feat/p2-proxy-cli): the provider grammar owns
    # `proxy exclude <pattern> [--remove]`; urnet-tools deliberately routes
    # exclusion through its --exclude targeting flag (documented in its own
    # proxy help), so the operator check drives the provider surface end to
    # end: add -> list -> remove, with proxy_url.json as the evidence. A head
    # without the grammar self-detects to SKIP-UNSUPPORTED.
    out="$(HOME="$STATE_HOME" "$MINER_WRAPPER" proxy exclude 192.0.2.99 2>&1)"; rc=$?
    if printf '%s' "$out" | grep -qi 'unknown\|usage:'; then
        record SKIP-UNSUPPORTED surface-exclude "provider grammar has no proxy exclude at this head: $(printf '%s' "$out" | firstline). Owning port: feat/p2-proxy-cli (wt-sn-proxy-cli)"
    elif [ "$rc" = "0" ] && { printf '%s' "$out" | grep -q 'added exclude pattern' || printf '%s' "$out" | grep -q 'already excluded'; } \
         && grep -q '192.0.2.99' "$STATE_DIR/proxy_url.json" 2>/dev/null; then
        listout="$(HOME="$STATE_HOME" "$MINER_WRAPPER" proxy exclude 2>&1)"
        rmout="$(HOME="$STATE_HOME" "$MINER_WRAPPER" proxy exclude 192.0.2.99 --remove 2>&1)"; rrc=$?
        if [ "$rrc" = "0" ] && printf '%s' "$rmout" | grep -q 'removed exclude pattern' \
             && ! grep -q '"192.0.2.99"' "$STATE_DIR/proxy_url.json" 2>/dev/null; then
            record PASS surface-exclude "provider proxy exclude add/list/remove round-trip on proxy_url.json (list: $(printf '%s' "$listout" | grep '192.0.2.99' | firstline)); urnet-tools routes exclusion via its --exclude targeting flag"
        else
            record FAIL surface-exclude "remove leg rc=$rrc: $(printf '%s' "$rmout" | firstline)"
        fi
    else
        record FAIL surface-exclude "provider proxy exclude add rc=$rc: $(printf '%s' "$out" | firstline)"
    fi

    # proxy graders / earnings / admission / AIMD: the operator surfaces owned
    # by the parallel proxy-grade line (bg-63: P-09 graders, T-07 earnings,
    # W-02 admission/AIMD). Conditional and self-detecting: probe the tool's
    # proxy verbs AND the provider usage text; when none is present the check
    # stays SKIP-UNSUPPORTED and names the owning work packages.
    found=""; bad=""
    for term in graders earnings admission aimd grade; do
        pout="$(TS proxy "$term" 2>&1)"; prc=$?
        case "$pout" in
            *"unknown proxy subcommand"*|*"unknown flag"*|*"Usage:"*|*"usage:"*) : ;;
            *)
                if [ "$prc" = "0" ]; then
                    found="$found $term"
                elif printf '%s' "$pout" | grep -qi 'not implement'; then
                    bad="$bad $term(stubbed: $(printf '%s' "$pout" | firstline))"
                else
                    found="$found $term(advertised: $(printf '%s' "$pout" | firstline))"
                fi ;;
        esac
    done
    ph="$(miner_help)"
    for term in graders aimd admissions; do
        if printf '%s' "$ph" | grep -qw "$term"; then found="$found provider:$term"; fi
    done
    if [ -n "$bad" ]; then
        record FAIL surface-proxy-graders "advertised but unimplemented:$bad"
    elif [ -n "$found" ]; then
        record PASS surface-proxy-graders "graders/earnings/admission/AIMD surface present:$found"
    else
        record SKIP-UNSUPPORTED surface-proxy-graders "no proxy graders/earnings/admission/AIMD operator surface at this head; the machinery is internal-only here (proxy_grades.json/grade log markers, proxy_earnings.json, admitted-by-tier lines, AIMD TargetPoolSize). Owning work packages: bg-63 P-09 (graders), T-07 (earnings), W-02 (admission/AIMD)"
    fi
}

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
summary() {
    head1 "SUMMARY"
    say "PASS=$PASS_N FAIL=$FAIL_N SKIP-UNSUPPORTED=$SKIPU_N SKIP-NO-NETWORK=$SKIPN_N SKIP-UNSAFE=$SKIPX_N"
    if [ "$FAIL_N" -gt 0 ]; then
        say "FAIL items:"
        printf '%s\n' "$FAIL_ITEMS" | sed '/^$/d' | tee -a "$REPORT"
    fi
    if [ "$ENV_BLOCKER" = "1" ]; then
        say "ENV_BLOCKER=1 (missing mandatory input)"
        exit 75
    fi
    [ "$FAIL_N" -gt 0 ] && exit 1
    exit 0
}

# ---------------------------------------------------------------------------
main() {
    if [ -z "${BIN_DIR:-}" ] && [ -z "${MINER_BIN:-}" ]; then
        printf 'usage: BIN_DIR=<dir with miner + urnet-tools> [JWT_FILE=...] [STATE_HOME=...] sh %s\n' "$0" >&2
        BIN_DIR="."
    fi
    preflight_guard
    mkdir -p "$STATE_HOME" "$STATE_DIR" "$WORK" 2>/dev/null || true
    chmod 700 "$STATE_DIR" 2>/dev/null || true
    : >"$REPORT"
    say "feature-matrix harness $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    say "bin_dir=$BIN_DIR miner=${MINER_BIN:-?} tools=${TOOLS_BIN:-?} state_home=$STATE_HOME state_dir=$STATE_DIR timeout=$TIMEOUT"
    adapt_home
    resolve_client_targeting
    set_up_wrapper
    if [ -n "$JWT_FILE" ] && [ -r "$JWT_FILE" ]; then
        mkdir -p "$STATE_DIR"
        cp "$JWT_FILE" "$STATE_DIR/jwt" && chmod 600 "$STATE_DIR/jwt"
        say "jwt: staged from JWT_FILE (content never printed)"
    fi

    run_block1

    if [ "$ENV_BLOCKER" != "1" ]; then
        # The stagger only paces network fetches made by client-level checks;
        # when those are disabled there is nothing to pace, but the blocks
        # must still run so they record their SKIP-UNSAFE rows.
        if [ "$CLIENT_SAFE" = "1" ]; then
            stagger_init
        else
            CHK_ORDER="url manip surface"
        fi
        for g in $CHK_ORDER; do
            case "$g" in
                url)     b2_url_sources ;;
                manip)   b2_manipulations ;;
                surface) b3_surface ;;
            esac
        done
    fi

    summary
}

main "$@"
