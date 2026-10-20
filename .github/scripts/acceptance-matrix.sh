#!/bin/sh
# acceptance-matrix.sh — the committed, repeatable acceptance matrix for the
# one-program line (miner + urnet-tools + urnet-docker).
#
# POSIX sh on purpose: it runs INSIDE a distro container (CI: debian:stable-slim)
# where there is no bash assumption, no jq and no python3. It is the repeatable
# form of the ad-hoc P2 acceptance workbench (scratch/p2b/acc1, scratch/p2d):
# the same checks, committed, isolated, and self-reporting.
#
# What it proves:
#   (a) hot-restart identity reuse — a fresh state mints an identity, a restart
#       reuses the same client_id (no re-mint), URNETWORK_HOT_RESTART=0 mints a
#       new one, the default afterwards reuses again, the renew path keeps the
#       id across an expired stored token, and .client_jwts.json keeps the v33
#       snapshot shape (plus the timestamped .bak snapshot on restart).
#   (b) urnet-tools command surface against the running daemon — version,
#       status, the raw snapshot (status --json), traffic, internals,
#       goroutines, history, the restored v33 commands (wallet, enforcement,
#       completion), control keys (set/get roundtrip, metrics on + /metrics
#       scrape), logs, and the proxy commands with synthetic TEST-NET-1 entries.
#       traffic/internals/goroutines have no urnet-tools subcommand of their
#       own (top is their only consumer); they are exercised with the same raw
#       control-socket request urnet-tools sends, via `nc -U`. Without a unix-
#       socket netcat those checks emit SKIP-NO-TOOL with that reason.
#   (c) urnet-docker runtime checks that apply to the miner line — a miner
#       container is created from the payload, named urnet-* so discovery finds
#       it, and providers/status/exec/logs/top/proxy are exercised through
#       urnet-docker. Container jobs (including CI) have no docker daemon, so
#       without one these checks emit SKIP-NO-DOCKER with that reason; run the
#       script with the host's /var/run/docker.sock and a docker CLI mounted to
#       exercise them.
#   (d) command-tree parity — every documented v33 command (the inventory
#       pinned by internal/urnettools/v33_command_surface_test.go
#       TestV33CommandSurfaceParity; the same rows are embedded here), its
#       aliases, the wallet subcommands and the proxy dispatcher verbs exist
#       and respond; the urnet-docker inventory gets the same check when the
#       binary is present.
#
# Every check prints exactly one line:
#   PASS <name>: <detail>
#   FAIL <name>: <detail>
#   SKIP-<kind> <name>: <reason>
# Kinds: NO-NETWORK (the live platform was unreachable or refused this run),
# UNSUPPORTED (runtime-detected absence at this head; the reason names the
# next step), NO-DOCKER (no docker CLI/daemon in the sandbox), NO-TOOL (a
# sandbox tool the check needs is missing), UPSTREAM (an earlier check did not
# produce the value this check asserts against), UNSAFE (a client-level check
# is disabled because the invoking user's passwd home is not the sandbox HOME,
# so urnet-tools discovery could attribute the operator's real state; see the
# preflight guard and the client-targeting row).
#
# Exit status: 0 = no FAIL lines; 1 = FAIL lines present; 75 = ENV_BLOCKER
# (a missing mandatory input: BIN_DIR/miner/urnet-tools or no JWT source);
# 78 = REFUSED by the preflight guard (the run would touch the operator's
# real provider state; never a product verdict). SKIP lines never fail a run.
# ENV_BLOCKER is not a product verdict.
#
# JWT contract: exactly like the release matrix. In CI the job mints a
# disposable token from GAUNTLET_USER/GAUNTLET_PW once (shakedown.yml's
# invocation) and passes it as JWT_FILE; the script also accepts JWT_FILE
# locally (e.g. /home/klets/ur/scratch/p2-auth/.urnetwork/jwt). When JWT_FILE
# is unset the script mints once itself from GAUNTLET_USER/GAUNTLET_PW into its
# isolated HOME. Every path applies the production-account refusal guard (a
# mesocyclone token fails the run), copies the token mode 600 into the state
# dir, and never prints it or writes it to the evidence directory.
#
# Isolation: everything runs with HOME set to HOME_DIR (a throwaway created by
# this script), the miner is started through a `provider`-named symlink and the
# invoking user's passwd home is pointed at HOME_DIR (the same container-local
# discovery adaptations distro-smoke.sh uses), and every process this script
# starts is stopped on exit (EXIT/INT/TERM trap). Nothing touches the ambient
# HOME, no production account, no pushes/tags/PRs/comments/workflow reruns.
#
# Preflight guard and target pinning (post-incident hardening). urnet-tools
# resolves a client-level command's target through discovery, which attributes
# a provider's state dir to the KERNEL owner's passwd home: a state dir outside
# that home is rejected and the fallback is <passwd home>/.urnetwork; delegated
# client commands then exec the provider binary with HOME=<that passwd home>.
# So a sandbox HOME outside the invoking user's passwd home (the operator's
# host, where /etc/passwd cannot be adapted) makes client commands resolve to
# the operator's real state (/home/klets/.urnetwork) even though every process
# runs with an isolated HOME. This harness therefore:
#   1. refuses to run at all (exit 78) when its effective state dir, HOME, or
#      an ambient URNETWORK_STATE_DIR equals, contains, or lives inside the
#      operator's real state, or when HOME is not inside the sandbox root it
#      creates; and
#   2. runs every client-level check only when the invoking user's passwd home
#      IS the sandbox HOME (the container adaptation above), otherwise those
#      checks record SKIP-UNSAFE and the report's client-targeting assertion
#      row records the dir discovery would have targeted.
# Provider-level runs always use an explicit isolated HOME and are never
# affected. See docs/Feature-Matrix-CI.md for the incident and the contract.
#
# Inputs (env):
#   BIN_DIR                    dir with `miner` (required) and optionally
#                              `urnet-tools` / `urnet-docker`
#   TOOLS_BIN                  urnet-tools path (default $BIN_DIR/urnet-tools)
#   DOCKER_BIN                 urnet-docker path (default $BIN_DIR/urnet-docker)
#   JWT_FILE                   disposable JWT to use (copied mode 600)
#   GAUNTLET_USER / GAUNTLET_PW  disposable CI account; used only when JWT_FILE
#                              is unset/empty (the shakedown.yml mint)
#   MINT_API_URL               API for the mint (default https://api.bringyour.com)
#   HOME_DIR                   isolated HOME (default /tmp/acceptance-matrix/home)
#   WORK_DIR                   scratch dir (default /tmp/acceptance-matrix/work)
#   OUT_DIR                    evidence dir; when set, logs and the report land
#                              there (never the raw store or a token)
#   TIMEOUT_S                  per-run socket wait (default 90)
#   IDENTITY_TIMEOUT_S         per-run wait for an identity/renewal marker
#                              (default 240)
#   STOP_TIMEOUT_S             SIGTERM budget per run (default 20)
#   ACCEPTANCE_DOCKER_IMAGE    base image for the miner container
#                              (default debian:stable-slim; must be present)
#   ACCEPTANCE_DOCKER_NAME     container name (default urnet-acceptance-miner;
#                              must contain "urnet" for urnet-docker discovery)
#   ACCEPTANCE_SKIP_DOCKER=1   mark every docker runtime check SKIP on purpose
#
# See docs/Acceptance-Matrix-CI.md for what runs where and the pass criteria.
set -u

BIN_DIR="${BIN_DIR:-}"
TOOLS_BIN="${TOOLS_BIN:-}"
DOCKER_BIN="${DOCKER_BIN:-}"
JWT_FILE="${JWT_FILE:-}"
GAUNTLET_USER="${GAUNTLET_USER:-}"
GAUNTLET_PW="${GAUNTLET_PW:-}"
MINT_API_URL="${MINT_API_URL:-https://api.bringyour.com}"
HOME_DIR="${HOME_DIR:-/tmp/acceptance-matrix/home}"
WORK_DIR="${WORK_DIR:-/tmp/acceptance-matrix/work}"
OUT_DIR="${OUT_DIR:-}"
TIMEOUT_S="${TIMEOUT_S:-90}"
IDENTITY_TIMEOUT_S="${IDENTITY_TIMEOUT_S:-240}"
STOP_TIMEOUT_S="${STOP_TIMEOUT_S:-20}"
ACCEPTANCE_DOCKER_IMAGE="${ACCEPTANCE_DOCKER_IMAGE:-debian:stable-slim}"
ACCEPTANCE_DOCKER_NAME="${ACCEPTANCE_DOCKER_NAME:-urnet-acceptance-miner}"
ACCEPTANCE_SKIP_DOCKER="${ACCEPTANCE_SKIP_DOCKER:-0}"

SAFE_PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

PASSES=0
FAILS=0
SKIPS=0
CURRENT_PID=""
DOCKER_CREATED=0
REPORT_FILE=""

say()  { printf '\n== %s\n' "$*"; }
note() { printf '  note: %s\n' "$*"; }
record() {
    [ -n "$REPORT_FILE" ] && printf '%s\n' "$*" >>"$REPORT_FILE"
    return 0
}
pass() {
    PASSES=$((PASSES + 1))
    printf 'PASS %s: %s\n' "$1" "$2"
    record "PASS $1: $2"
}
fail() {
    FAILS=$((FAILS + 1))
    printf 'FAIL %s: %s\n' "$1" "$2"
    record "FAIL $1: $2"
}
# skip <check> <kind> <reason>
skip() {
    SKIPS=$((SKIPS + 1))
    printf 'SKIP-%s %s: %s\n' "$2" "$1" "$3"
    record "SKIP-$2 $1: $3"
}

# --------------------------------------------------------------- cleanup --

stop_current() {
    # SIGTERM the run in CURRENT_PID (if any) and wait up to STOP_TIMEOUT_S.
    # Returns 1 when a SIGKILL was needed, 0 otherwise.
    _pid="$CURRENT_PID"
    [ -n "$_pid" ] || return 0
    CURRENT_PID=""
    kill -TERM "$_pid" 2>/dev/null || return 0
    _i=0
    while [ "$_i" -lt "$STOP_TIMEOUT_S" ]; do
        kill -0 "$_pid" 2>/dev/null || return 0
        sleep 1
        _i=$((_i + 1))
    done
    kill -KILL "$_pid" 2>/dev/null || true
    return 1
}

cleanup() {
    stop_current >/dev/null 2>&1 || true
    if [ "$DOCKER_CREATED" = "1" ] && command -v docker >/dev/null 2>&1; then
        docker rm -f "$ACCEPTANCE_DOCKER_NAME" >/dev/null 2>&1 || true
        DOCKER_CREATED=0
    fi
    return 0
}
trap cleanup EXIT
trap 'cleanup; exit 130' INT
trap 'cleanup; exit 143' TERM

# ----------------------------------------------------------------- inputs --

if [ -z "$BIN_DIR" ]; then
    printf 'FATAL: BIN_DIR is required (the directory holding the miner binary)\n' >&2
    exit 2
fi
[ -n "$TOOLS_BIN" ] || TOOLS_BIN="$BIN_DIR/urnet-tools"
[ -n "$DOCKER_BIN" ] || DOCKER_BIN="$BIN_DIR/urnet-docker"

# ------------------------------------------------------ preflight guard -----
# Runs BEFORE anything is created, deleted, or invoked. The operator's real
# provider state is a hard no-touch constant: when any path this harness uses
# for state, HOME, scratch, or evidence equals, contains, or lives inside it,
# the run is refused here (exit 78) instead of being attempted at all. This
# is the guard for the P2 acceptance incident where client-level urnet-tools
# commands resolved through discovery to the operator's real state while
# every process ran with an isolated HOME.

OPERATOR_STATE="/home/klets/.urnetwork"

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
    printf 'Nothing was created, deleted, or invoked. Fix HOME_DIR/WORK_DIR/OUT_DIR (and unset URNETWORK_STATE_DIR),\n' >&2
    printf 'or run the matrix in a disposable container, as the CI job does.\n' >&2
    exit 78
}

say "preflight guard"
case "${HOME_DIR:-}" in
    ""|/) guard_refuse "HOME_DIR is empty or '/'; the sandbox root must be a real directory" ;;
esac
case "${WORK_DIR:-}" in
    ""|/) guard_refuse "WORK_DIR is empty or '/'; the sandbox root must be a real directory" ;;
esac
OPERATOR_STATE_C="$(canon_dir "$OPERATOR_STATE")"
HOME_DIR_C="$(canon_dir "$HOME_DIR")"
WORK_DIR_C="$(canon_dir "$WORK_DIR")"
STATE_C="$(canon_dir "$HOME_DIR/.urnetwork")"
path_overlaps "$HOME_DIR_C" "$OPERATOR_STATE_C" && guard_refuse "HOME_DIR=$HOME_DIR equals, contains, or lives inside the operator state $OPERATOR_STATE"
path_overlaps "$WORK_DIR_C" "$OPERATOR_STATE_C" && guard_refuse "WORK_DIR=$WORK_DIR equals, contains, or lives inside the operator state $OPERATOR_STATE"
path_overlaps "$STATE_C" "$OPERATOR_STATE_C" && guard_refuse "the effective state dir $HOME_DIR/.urnetwork equals, contains, or lives inside the operator state $OPERATOR_STATE"
if [ -n "$OUT_DIR" ]; then
    OUT_DIR_C="$(canon_dir "$OUT_DIR")"
    path_overlaps "$OUT_DIR_C" "$OPERATOR_STATE_C" && guard_refuse "OUT_DIR=$OUT_DIR equals, contains, or lives inside the operator state $OPERATOR_STATE"
fi
if [ -n "${URNETWORK_STATE_DIR:-}" ]; then
    urnet_state_c="$(canon_dir "$URNETWORK_STATE_DIR")"
    if [ "$urnet_state_c" = "$STATE_C" ]; then
        note "URNETWORK_STATE_DIR=$URNETWORK_STATE_DIR equals the sandbox state; unsetting it for the runs below"
    else
        path_overlaps "$urnet_state_c" "$OPERATOR_STATE_C" && guard_refuse "URNETWORK_STATE_DIR=$URNETWORK_STATE_DIR points at the operator state $OPERATOR_STATE"
        guard_refuse "URNETWORK_STATE_DIR=$URNETWORK_STATE_DIR is set in the invoking environment; unset it so this harness controls the state dir"
    fi
fi
# The ambient state override must not leak into the JWT mint or a client
# invocation (the miner honours URNETWORK_STATE_DIR; only provider-level runs
# get env -i). The guard above already refused any value that mattered.
unset URNETWORK_STATE_DIR 2>/dev/null || true

if [ -n "$OUT_DIR" ]; then
    mkdir -p "$OUT_DIR" || { printf 'FATAL: cannot create OUT_DIR=%s\n' "$OUT_DIR" >&2; exit 2; }
    REPORT_FILE="$OUT_DIR/acceptance-report.txt"
    : >"$REPORT_FILE"
fi

say "acceptance matrix — inputs"
printf 'bin_dir=%s\ntools_bin=%s\ndocker_bin=%s\nhome_dir=%s\nwork_dir=%s\n' \
    "$BIN_DIR" "$TOOLS_BIN" "$DOCKER_BIN" "$HOME_DIR" "$WORK_DIR"

ENV_BLOCKER=0
if [ -x "$BIN_DIR/miner" ]; then
    pass "binary-miner" "miner present and executable: $BIN_DIR/miner"
else
    fail "binary-miner" "miner missing or not executable: $BIN_DIR/miner"
    ENV_BLOCKER=1
fi
if [ -x "$TOOLS_BIN" ]; then
    pass "binary-tools" "urnet-tools present and executable: $TOOLS_BIN"
else
    fail "binary-tools" "urnet-tools missing or not executable: $TOOLS_BIN"
    ENV_BLOCKER=1
fi
if [ -x "$DOCKER_BIN" ]; then
    pass "binary-docker" "urnet-docker present and executable: $DOCKER_BIN"
else
    skip "binary-docker" "UNSUPPORTED" "urnet-docker not supplied at $DOCKER_BIN (section (c) and the docker parity rows will skip)"
fi
if [ "$ENV_BLOCKER" = "1" ]; then
    printf '\nENV_BLOCKER: mandatory binaries are missing; nothing was run.\n' >&2
    exit 75
fi

# ------------------------------------------------------------- sandboxing --
say "sandbox"

rm -rf "$HOME_DIR" "$WORK_DIR" 2>/dev/null || true
mkdir -p "$HOME_DIR/.urnetwork" "$WORK_DIR/tmp" "$WORK_DIR/bin" "$WORK_DIR/logs" "$WORK_DIR/tools" || {
    printf 'FATAL: cannot create the sandbox under %s / %s\n' "$HOME_DIR" "$WORK_DIR" >&2
    exit 2
}
chmod 700 "$HOME_DIR" 2>/dev/null || true
STATE="$HOME_DIR/.urnetwork"
export HOME="$HOME_DIR"
PROVIDER_LINK="$WORK_DIR/bin/provider"
ln -sf "$BIN_DIR/miner" "$PROVIDER_LINK" || { printf 'FATAL: cannot create %s\n' "$PROVIDER_LINK" >&2; exit 2; }

OS_PRETTY="$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | head -n 1 | tr -d '"')"
if [ -e /lib/ld-musl-x86_64.so.1 ] || [ -e /lib/ld-musl-aarch64.so.1 ]; then
    LIBC="musl"
elif command -v ldd >/dev/null 2>&1; then
    LIBC="$(ldd --version 2>&1 | head -n 1)"
else
    LIBC="unknown"
fi
printf 'os=%s\nlibc=%s\nhome=%s\nstate=%s\n' "${OS_PRETTY:-unknown}" "$LIBC" "$HOME_DIR" "$STATE"
record "os=${OS_PRETTY:-unknown} libc=$LIBC home=$HOME_DIR state=$STATE"

# The discovery contract (same adaptations distro-smoke.sh makes): urnet-tools
# matches process basenames against its provider allowlist and attributes a
# state dir that lives under the invoking user's passwd home. Both are
# container-local adaptations, not product changes.
PEME="$(id -un 2>/dev/null || echo root)"
passwd_home_for() { # $1=user -> passwd home (empty when unknown)
    if command -v getent >/dev/null 2>&1; then
        getent passwd "$1" 2>/dev/null | head -n 1 | cut -d: -f6
    else
        grep "^$1:" /etc/passwd 2>/dev/null | head -n 1 | cut -d: -f6
    fi
}
if [ -w /etc/passwd ]; then
    CUR_HOME="$(grep "^$PEME:" /etc/passwd 2>/dev/null | head -n 1 | cut -d: -f6)"
    if [ -z "$CUR_HOME" ]; then
        note "no /etc/passwd entry for $PEME; discovery attribution not adapted"
    elif [ "$CUR_HOME" != "$HOME_DIR" ]; then
        sed "s|^\($PEME:[^:]*:[^:]*:[^:]*:[^:]*:\)[^:]*:|\1$HOME_DIR:|" \
            /etc/passwd >"$WORK_DIR/passwd.new" 2>/dev/null &&
            cp "$WORK_DIR/passwd.new" /etc/passwd 2>/dev/null &&
            note "passwd home for $PEME pointed at $HOME_DIR (container-local discovery adaptation)"
    fi
else
    note "cannot adapt /etc/passwd home (not writable); client-level checks will skip (client-targeting below)"
fi
# The HOME every invocation runs with must be inside the sandbox root this
# script created; HOME is exported to HOME_DIR above, so this asserts the link.
HOME_C="$(canon_dir "$HOME")"
if [ "$HOME_C" != "$HOME_DIR_C" ] && ! path_strictly_inside "$HOME_C" "$HOME_DIR_C"; then
    guard_refuse "HOME=$HOME is not inside the sandbox root $HOME_DIR"
fi

# Client targeting resolution and pinning. CLIENT_RESOLVED is what discovery
# would attribute to the sandbox provider and the HOME a delegated provider
# binary would run with: the sandbox state iff the invoking user's passwd home
# IS the sandbox HOME (the container adaptation above); otherwise client-level
# commands resolve to <passwd home>/.urnetwork and are disabled with
# SKIP-UNSAFE, and the assertion row below fails naming that dir.
PASSWD_HOME="$(passwd_home_for "$PEME")"
CLIENT_SAFE=0
CLIENT_UNSAFE_REASON=""
if [ -n "$PASSWD_HOME" ] && [ "$(canon_dir "$PASSWD_HOME")" = "$HOME_DIR_C" ]; then
    CLIENT_SAFE=1
    CLIENT_RESOLVED="$STATE"
else
    if [ -n "$PASSWD_HOME" ]; then
        CLIENT_RESOLVED="$PASSWD_HOME/.urnetwork"
    else
        CLIENT_RESOLVED="unresolved (no passwd entry for $PEME)"
    fi
    CLIENT_UNSAFE_REASON="client-level targeting disabled: the invoking user's passwd home (${PASSWD_HOME:-unknown}) is not the sandbox HOME ($HOME_DIR), so urnet-tools discovery/delegation would resolve to $CLIENT_RESOLVED; run in a disposable container (CI)"
    note "$CLIENT_UNSAFE_REASON"
fi
if [ "$CLIENT_SAFE" = "1" ]; then
    pass "client-targeting" "client targeting resolved to $CLIENT_RESOLVED (the invoking user's passwd home is the sandbox HOME; inside the sandbox)"
else
    fail "client-targeting" "client targeting resolved to $CLIENT_RESOLVED (outside the sandbox state $STATE; passwd home ${PASSWD_HOME:-unknown} != sandbox HOME $HOME_DIR; client-level checks disabled)"
fi
pass "sandbox" "isolated HOME=$HOME_DIR, state=$STATE, miner reachable as $PROVIDER_LINK"

# --------------------------------------------------------- disposable JWT --
say "disposable JWT"

# guard_token <file>: refuse the production account, the same guard
# shakedown.yml and distro-smoke.sh apply. Decoded with POSIX tools on
# purpose. The token value is only ever held in a shell variable; it is never
# printed and never written to OUT_DIR.
guard_token() {
    _raw="$(cat "$1" 2>/dev/null)"
    _payload="$(printf '%s' "$_raw" | cut -d. -f2 | tr '_-' '/+')"
    _pad=$(( (4 - ${#_payload} % 4) % 4 ))
    while [ "$_pad" -gt 0 ]; do
        _payload="$_payload="
        _pad=$((_pad - 1))
    done
    if printf '%s' "$_payload" | base64 -d >"$WORK_DIR/jwt-payload.json" 2>/dev/null &&
       [ -s "$WORK_DIR/jwt-payload.json" ]; then
        _network="$(sed -n 's/.*"network_name":"\([^"]*\)".*/\1/p' "$WORK_DIR/jwt-payload.json" | head -n 1)"
        if grep -q 'mesocyclone' "$WORK_DIR/jwt-payload.json"; then
            rm -f "$WORK_DIR/jwt-payload.json"
            fail "jwt-guard" "REFUSING: the token is the production account (mesocyclone)"
            return 1
        fi
        pass "jwt-guard" "token account is not the production account (network_name=${_network:-unknown}; token never printed)"
        [ -n "$OUT_DIR" ] && printf 'network_name=%s\n' "${_network:-unknown}" >"$OUT_DIR/jwt-account.txt"
        rm -f "$WORK_DIR/jwt-payload.json"
        return 0
    fi
    rm -f "$WORK_DIR/jwt-payload.json"
    skip "jwt-guard" "NO-TOOL" "could not decode the token payload with base64; the production-account refusal guard was not applied"
    return 0
}

JWT_READY=0
if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
    case "$(cat "$JWT_FILE")" in
        *.*.*) : ;;
        *) fail "jwt-source" "JWT_FILE=$JWT_FILE is not shaped like a JWT"; JWT_READY=-1 ;;
    esac
    if [ "$JWT_READY" = "0" ]; then
        mkdir -p "$STATE"
        cp "$JWT_FILE" "$STATE/jwt"
        chmod 600 "$STATE/jwt" 2>/dev/null || true
        pass "jwt-source" "supplied JWT copied into the state dir mode 600 (contents never printed)"
        guard_token "$STATE/jwt" || JWT_READY=-1
        [ "$JWT_READY" = "0" ] && JWT_READY=1
    fi
elif [ -n "$GAUNTLET_USER" ] && [ -n "$GAUNTLET_PW" ]; then
    MINT_LOG="$WORK_DIR/mint.log"
    if HOME="$HOME_DIR" "$BIN_DIR/miner" auth \
        --user_auth="$GAUNTLET_USER" --password="$GAUNTLET_PW" \
        --api_url="$MINT_API_URL" -f >"$MINT_LOG" 2>&1; then
        pass "jwt-mint" "miner auth exited 0 for the disposable-account mint"
    fi
    if [ -s "$HOME_DIR/.urnetwork/jwt" ] && [ "$HOME_DIR/.urnetwork" != "$STATE" ]; then
        cp "$HOME_DIR/.urnetwork/jwt" "$STATE/jwt" 2>/dev/null || true
    fi
    if [ -s "$STATE/jwt" ]; then
        chmod 600 "$STATE/jwt" 2>/dev/null || true
        pass "jwt-source" "minted a disposable JWT into the state dir (never printed)"
        guard_token "$STATE/jwt" || JWT_READY=-1
        [ "$JWT_READY" = "0" ] && JWT_READY=1
    else
        fail "jwt-source" "JWT mint failed (verification required?); tail of $MINT_LOG follows"
        tail -n 5 "$MINT_LOG" 2>/dev/null | sed 's/^/    | /'
        JWT_READY=-1
    fi
else
    fail "jwt-source" "no disposable JWT: set JWT_FILE or GAUNTLET_USER/GAUNTLET_PW (local: JWT_FILE=/home/klets/ur/scratch/p2-auth/.urnetwork/jwt)"
    JWT_READY=-1
fi

if [ "$JWT_READY" != "1" ]; then
    printf '\nENV_BLOCKER: no usable disposable JWT; nothing was run.\n' >&2
    exit 75
fi

# The network fixture every harness uses: the production API and connect URLs.
if [ ! -f "$STATE/network.json" ]; then
    printf '%s\n' '{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}' >"$STATE/network.json"
fi
pass "network-fixture" "present: $STATE/network.json"

# ------------------------------------------------------------ run helpers --

LOGDIR="$WORK_DIR/logs"
TOOLS_RUNS="$WORK_DIR/tools"
STORE="$STATE/.client_jwts.json"

# start_run <name> [VAR=VAL ...] — start the miner provide with an env-only,
# isolated HOME and record the exact pid. URNETWORK_HOT_RESTART=0 rides in
# through the extra VAR=VAL pairs.
start_run() {
    _name=$1
    shift
    _base="$LOGDIR/$_name"
    : >"$_base.out"
    : >"$_base.err"
    ( cd "$WORK_DIR" && exec env -i PATH="$SAFE_PATH" HOME="$HOME_DIR" TMPDIR="$WORK_DIR/tmp" "$@" "$PROVIDER_LINK" provide --port=0 --allow-client-registration ) \
        >"$_base.out" 2>"$_base.err" &
    CURRENT_PID=$!
    printf '%s\n' "$CURRENT_PID" >"$_base.pid"
    _i=0
    while [ "$_i" -lt "$TIMEOUT_S" ]; do
        [ -S "$STATE/provider.sock" ] && break
        kill -0 "$CURRENT_PID" 2>/dev/null || break
        sleep 1
        _i=$((_i + 1))
    done
    printf 'run=%s pid=%s socket_after=%ss\n' "$_name" "$CURRENT_PID" "$_i" | tee -a "$WORK_DIR/runs.log"
    record "run=$_name pid=$CURRENT_PID socket_after=${_i}s"
}

# wait_marker <name> <ere-pattern> <timeout_s>: wait for a marker line in the
# run's stderr (tlog). Returns 1 on timeout or if the miner exits first.
wait_marker() {
    _name=$1
    _pat=$2
    _tmo=$3
    _i=0
    while [ "$_i" -lt "$_tmo" ]; do
        grep -qE "$_pat" "$LOGDIR/$_name.err" 2>/dev/null && return 0
        kill -0 "$CURRENT_PID" 2>/dev/null || return 1
        sleep 1
        _i=$((_i + 1))
    done
    return 1
}

run_id_new()    { sed -n 's/.*\[new\] client_id: *\([0-9a-fA-F][0-9a-fA-F-]*\).*/\1/p' "$LOGDIR/$1.err" 2>/dev/null | head -n 1; }
run_id_reuse()  { sed -n 's/.*\[reuse\] client_id: *\([0-9a-fA-F][0-9a-fA-F-]*\).*/\1/p' "$LOGDIR/$1.err" 2>/dev/null | head -n 1; }
run_id_renew()  { sed -n 's/.*renewed identity *\([0-9a-fA-F][0-9a-fA-F-]*\).*/\1/p' "$LOGDIR/$1.err" 2>/dev/null | head -n 1; }

# identity_failure_kind <name>: classify a run that produced no identity
# marker. Echoes "network" (live-platform reachability), "refused" (the
# platform refused the credential) or "fail".
identity_failure_kind() {
    _f="$LOGDIR/$1.err"
    if [ -f "$STATE/.provider.jwt.rejected" ] ||
       grep -qE 'renewal rejected|sign-in rejected|sign in rejected|network_sign_in_rejected' "$_f" 2>/dev/null; then
        printf 'refused\n'
    elif ! grep -qE '\[new\] client_id|\[reuse\] client_id|renewed identity' "$_f" 2>/dev/null &&
         tail -n 60 "$_f" 2>/dev/null | grep -qE 'no such host|SERVFAIL|network is unreachable|i/o timeout|connection refused'; then
        printf 'network\n'
    else
        printf 'fail\n'
    fi
}

# store_id <file> / store_token_sha <file>: read the v33 store's direct entry.
store_id() {
    sed -n 's/.*"client_id": *"\([^"]*\)".*/\1/p' "$1" 2>/dev/null | head -n 1
}
store_token_sha() {
    _t="$(sed -n 's/.*"by_client_jwt": *"\([^"]*\)".*/\1/p' "$1" 2>/dev/null | head -n 1)"
    if [ -n "$_t" ]; then
        printf '%s' "$_t" | sha256sum | cut -d' ' -f1
    else
        printf 'MISSING\n'
    fi
}
# redacted_store_copy <dest>: never let a token into the evidence directory.
redacted_store_copy() {
    [ -n "$OUT_DIR" ] || return 0
    [ -f "$STORE" ] || return 0
    sed 's/\("by_client_jwt": *"\)[^"]*/\1REDACTED/' "$STORE" >"$1" 2>/dev/null || true
}

MINT_OK=0
ID1=""
ID3=""
TOK_CRAFTED=""
RENEW_OK=0
RENEWED_SHA=""

# -------------------------------------------- (a) hot-restart identity matrix --
say "hot-restart identity matrix (a)"

# h1: a fresh state mints an identity.
start_run h1
if wait_marker h1 '\[new\] client_id: [0-9a-fA-F-]+' "$IDENTITY_TIMEOUT_S"; then
    ID1="$(run_id_new h1)"
    if [ -f "$STORE" ] && [ "$(store_id "$STORE")" = "$ID1" ]; then
        MINT_OK=1
        pass "hr-fresh-mint" "fresh state minted client_id=$ID1; store written with the same id"
    else
        fail "hr-fresh-mint" "minted $ID1 but the store does not carry it (store_id=$(store_id "$STORE"))"
    fi
else
    kind="$(identity_failure_kind h1)"
    case "$kind" in
        network|refused)
            skip "hr-fresh-mint" "NO-NETWORK" "the live platform did not hand out an identity this run ($kind)"
            ;;
        *)
            fail "hr-fresh-mint" "no [new] client_id within ${IDENTITY_TIMEOUT_S}s"
            ;;
    esac
fi
if stop_current; then
    pass "hr-sigterm" "h1 stopped cleanly on SIGTERM"
else
    fail "hr-sigterm" "h1 needed SIGKILL after ${STOP_TIMEOUT_S}s of SIGTERM"
fi
redacted_store_copy "$OUT_DIR/h1.client_jwts.redacted.json"

# h2: a restart reuses the same identity and re-mints nothing.
if [ "$MINT_OK" = "1" ]; then
    TOK1="$(store_token_sha "$STORE")"
    start_run h2
    if wait_marker h2 '\[reuse\] client_id: [0-9a-fA-F-]+' "$IDENTITY_TIMEOUT_S"; then
        ID2="$(run_id_reuse h2)"
        TOK2="$(store_token_sha "$STORE")"
        PROBLEM=""
        [ "$ID2" = "$ID1" ] || PROBLEM="reuse id $ID2 != minted id $ID1"
        [ "$TOK2" = "$TOK1" ] || PROBLEM="$PROBLEM; stored token changed (no re-mint expected)"
        if grep -qE '\[new\] client_id' "$LOGDIR/h2.err" 2>/dev/null; then
            PROBLEM="$PROBLEM; [new] line present (a fresh mint happened despite reuse)"
        fi
        if [ -z "$PROBLEM" ]; then
            pass "hr-restart-reuse" "restart reused client_id=$ID2 with the byte-identical stored token; no [new] line (no re-mint)"
        else
            fail "hr-restart-reuse" "$PROBLEM"
        fi
    else
        kind="$(identity_failure_kind h2)"
        case "$kind" in
            refused|network)
                skip "hr-restart-reuse" "NO-NETWORK" "the platform did not accept the stored identity this run ($kind); a rejection here is platform-side, not a reuse verdict (see ${STATE}/.provider.jwt.rejected)"
                ;;
            *)
                fail "hr-restart-reuse" "no [reuse] client_id within ${IDENTITY_TIMEOUT_S}s"
                ;;
        esac
    fi
    if stop_current; then :; else fail "hr-sigterm" "h2 needed SIGKILL after ${STOP_TIMEOUT_S}s of SIGTERM"; fi
    redacted_store_copy "$OUT_DIR/h2.client_jwts.redacted.json"
else
    skip "hr-restart-reuse" "UPSTREAM" "hr-fresh-mint did not produce an identity to reuse"
fi

# h3: URNETWORK_HOT_RESTART=0 mints a new identity.
if [ "$MINT_OK" = "1" ]; then
    start_run h3 URNETWORK_HOT_RESTART=0
    if wait_marker h3 '\[new\] client_id: [0-9a-fA-F-]+' "$IDENTITY_TIMEOUT_S"; then
        ID3="$(run_id_new h3)"
        PROBLEM=""
        [ "$ID3" != "$ID1" ] || PROBLEM="id $ID3 equals the reused identity (hot restart did not disable reuse)"
        if grep -qE '\[reuse\] client_id' "$LOGDIR/h3.err" 2>/dev/null; then
            PROBLEM="$PROBLEM; [reuse] line present with URNETWORK_HOT_RESTART=0"
        fi
        if [ "$(store_id "$STORE")" != "$ID3" ]; then
            PROBLEM="$PROBLEM; store holds $(store_id "$STORE") not $ID3"
        fi
        if [ -z "$PROBLEM" ]; then
            pass "hr-hotrestart-off-new-id" "URNETWORK_HOT_RESTART=0 minted a new client_id=$ID3 (differs from $ID1); store carries it; no [reuse] line"
        else
            fail "hr-hotrestart-off-new-id" "$PROBLEM"
        fi
    else
        kind="$(identity_failure_kind h3)"
        case "$kind" in
            network|refused) skip "hr-hotrestart-off-new-id" "NO-NETWORK" "the live platform did not hand out an identity with HOT_RESTART=0 this run ($kind)" ;;
            *) fail "hr-hotrestart-off-new-id" "no [new] client_id within ${IDENTITY_TIMEOUT_S}s with URNETWORK_HOT_RESTART=0" ;;
        esac
    fi
    if stop_current; then :; else fail "hr-sigterm" "h3 needed SIGKILL after ${STOP_TIMEOUT_S}s of SIGTERM"; fi
    redacted_store_copy "$OUT_DIR/h3.client_jwts.redacted.json"
else
    skip "hr-hotrestart-off-new-id" "UPSTREAM" "hr-fresh-mint did not produce the baseline identity"
fi

# h4: back on the default, the h3 identity is reused again.
if [ -n "$ID3" ]; then
    start_run h4
    if wait_marker h4 '\[reuse\] client_id: [0-9a-fA-F-]+' "$IDENTITY_TIMEOUT_S"; then
        ID4="$(run_id_reuse h4)"
        if [ "$ID4" = "$ID3" ]; then
            pass "hr-default-reuse-again" "default run reused the HOT_RESTART=0 identity ($ID4)"
        else
            fail "hr-default-reuse-again" "reuse returned $ID4, expected the h3 identity $ID3"
        fi
    else
        kind="$(identity_failure_kind h4)"
        case "$kind" in
            refused|network) skip "hr-default-reuse-again" "NO-NETWORK" "the platform did not accept the stored identity this run ($kind)" ;;
            *) fail "hr-default-reuse-again" "no [reuse] client_id within ${IDENTITY_TIMEOUT_S}s" ;;
        esac
    fi
    if stop_current; then :; else fail "hr-sigterm" "h4 needed SIGKILL after ${STOP_TIMEOUT_S}s of SIGTERM"; fi
    redacted_store_copy "$OUT_DIR/h3.client_jwts.after-h4.redacted.json"
else
    skip "hr-default-reuse-again" "UPSTREAM" "hr-hotrestart-off-new-id did not record an identity"
fi

# h5: a stored token crafted expired is renewed in place, keeping the id.
if [ -n "$ID3" ] && [ -f "$STORE" ] && command -v base64 >/dev/null 2>&1; then
    CRAFT_OK=0
    _tok="$(sed -n 's/.*"by_client_jwt": *"\([^"]*\)".*/\1/p' "$STORE" | head -n 1)"
    _h="$(printf '%s' "$_tok" | cut -d. -f1)"
    _p="$(printf '%s' "$_tok" | cut -d. -f2)"
    _s="$(printf '%s' "$_tok" | cut -d. -f3)"
    _dec="$(printf '%s' "$_p" | tr '_-' '/+')"
    _pad=$(( (4 - ${#_dec} % 4) % 4 ))
    while [ "$_pad" -gt 0 ]; do
        _dec="$_dec="
        _pad=$((_pad - 1))
    done
    _claims="$(printf '%s' "$_dec" | base64 -d 2>/dev/null)"
    if [ -n "$_claims" ]; then
        _newclaims="$(printf '%s' "$_claims" | sed 's/"exp":[0-9][0-9]*/"exp":1000000000/')"
        if [ "$_newclaims" != "$_claims" ]; then
            _np="$(printf '%s' "$_newclaims" | base64 | tr -d '\n' | tr '+/' '-_' | tr -d '=')"
            _newtok="$_h.$_np.$_s"
            sed 's|"by_client_jwt": *"[^"]*"|"by_client_jwt": "'"$_newtok"'"|' "$STORE" >"$WORK_DIR/store.new" &&
                cp "$WORK_DIR/store.new" "$STORE" && rm -f "$WORK_DIR/store.new"
            TOK_CRAFTED="$(store_token_sha "$STORE")"
            [ "$TOK_CRAFTED" != "$(printf '%s' "$_tok" | sha256sum | cut -d' ' -f1)" ] && CRAFT_OK=1
        fi
    fi
    if [ "$CRAFT_OK" = "1" ]; then
        note "crafted the stored token's exp into the past for the renew path (id unchanged in the store)"
        start_run h5
        if wait_marker h5 'renewed identity [0-9a-fA-F-]+' "$IDENTITY_TIMEOUT_S"; then
            ID5="$(run_id_renew h5)"
            TOK5="$(store_token_sha "$STORE")"
            PROBLEM=""
            [ "$ID5" = "$ID3" ] || PROBLEM="renewal changed the id: $ID3 -> $ID5"
            [ "$TOK5" != "$TOK_CRAFTED" ] || PROBLEM="$PROBLEM; store still holds the crafted token (no renewed token persisted)"
            if grep -qE '\[new\] client_id' "$LOGDIR/h5.err" 2>/dev/null; then
                PROBLEM="$PROBLEM; a fresh mint happened instead of a renewal"
            fi
            if [ "$(store_id "$STORE")" != "$ID3" ]; then
                PROBLEM="$PROBLEM; store id is $(store_id "$STORE") not $ID3"
            fi
            if [ -z "$PROBLEM" ]; then
                RENEW_OK=1
                RENEWED_SHA="$TOK5"
                pass "hr-renew-keeps-id" "expired stored token renewed in place; client_id kept ($ID5); store now holds the renewed token"
            else
                fail "hr-renew-keeps-id" "$PROBLEM"
            fi
        else
            kind="$(identity_failure_kind h5)"
            case "$kind" in
                network|refused) skip "hr-renew-keeps-id" "NO-NETWORK" "the platform did not complete the renewal this run ($kind); the renewal needs a live platform response" ;;
                *) fail "hr-renew-keeps-id" "no 'renewed identity' line within ${IDENTITY_TIMEOUT_S}s for the expired stored token" ;;
            esac
        fi
        if stop_current; then :; else fail "hr-sigterm" "h5 needed SIGKILL after ${STOP_TIMEOUT_S}s of SIGTERM"; fi
        redacted_store_copy "$OUT_DIR/h5.client_jwts.redacted.json"
    else
        skip "hr-renew-keeps-id" "NO-TOOL" "could not craft an expired token (base64/sed path unavailable in the sandbox)"
    fi
elif [ -n "$ID3" ]; then
    skip "hr-renew-keeps-id" "NO-TOOL" "base64 not available in the sandbox to craft the expired stored token"
else
    skip "hr-renew-keeps-id" "UPSTREAM" "hr-hotrestart-off-new-id did not record an identity"
fi

# h6: the renewed identity is reused; the surface checks then run against this
# still-running daemon.
SURFACE_RUN="surface"
if [ "$MINT_OK" = "1" ]; then
    SURFACE_RUN="h6"
    start_run h6
    if wait_marker h6 '\[reuse\] client_id: [0-9a-fA-F-]+' "$IDENTITY_TIMEOUT_S"; then
        ID6="$(run_id_reuse h6)"
        PROBLEM=""
        [ "$ID6" = "$ID3" ] || PROBLEM="reused $ID6, expected the renewed identity $ID3"
        if [ "$RENEW_OK" = "1" ] && [ "$(store_token_sha "$STORE")" != "$RENEWED_SHA" ]; then
            PROBLEM="$PROBLEM; token changed again (unexpected extra renew)"
        fi
        if [ -z "$PROBLEM" ]; then
            pass "hr-renewed-reuse" "renewed identity reused by the next default run ($ID6); token unchanged since the renewal"
        else
            fail "hr-renewed-reuse" "$PROBLEM"
        fi
    else
        kind="$(identity_failure_kind h6)"
        case "$kind" in
            refused|network) skip "hr-renewed-reuse" "NO-NETWORK" "the platform did not accept the renewed identity this run ($kind)" ;;
            *) fail "hr-renewed-reuse" "no [reuse] client_id within ${IDENTITY_TIMEOUT_S}s for the renewed identity" ;;
        esac
    fi
    redacted_store_copy "$OUT_DIR/h6.client_jwts.redacted.json"
else
    skip "hr-renewed-reuse" "UPSTREAM" "no renewed identity was recorded"
    # The surface checks still need a live daemon; give them one.
    start_run surface
fi

# v33 snapshot shape: top-level "direct" entry with exactly the v33 fields and
# well-formed values, plus the timestamped .bak snapshot the restart path
# writes before touching the store.
if [ -f "$STORE" ]; then
    PROBLEM=""
    for _field in by_client_jwt client_id network_id minted_at; do
        grep -q "\"$_field\": *\"" "$STORE" 2>/dev/null || PROBLEM="$PROBLEM; missing field $_field"
    done
    grep -q '"direct": *{' "$STORE" 2>/dev/null || PROBLEM="$PROBLEM; missing the top-level \"direct\" entry"
    _top="$(grep -c '^  "' "$STORE" 2>/dev/null)"
    _fields="$(grep -c '^    "' "$STORE" 2>/dev/null)"
    [ "$_top" = "1" ] || PROBLEM="$PROBLEM; expected exactly one top-level entry, found $_top"
    [ "$_fields" = "4" ] || PROBLEM="$PROBLEM; expected exactly 4 entry fields, found $_fields"
    _sid="$(store_id "$STORE")"
    case "$_sid" in
        [0-9a-f]*-*-*) : ;;
        *) PROBLEM="$PROBLEM; client_id is not uuid-shaped ($_sid)" ;;
    esac
    _jwt="$(sed -n 's/.*"by_client_jwt": *"\([^"]*\)".*/\1/p' "$STORE" | head -n 1)"
    case "$_jwt" in
        *.*.*) : ;;
        *) PROBLEM="$PROBLEM; by_client_jwt is not a 3-part JWT" ;;
    esac
    _nid="$(sed -n 's/.*"network_id": *"\([^"]*\)".*/\1/p' "$STORE" | head -n 1)"
    case "$_nid" in
        [0-9a-f]*-*-*) : ;;
        *) PROBLEM="$PROBLEM; network_id is not uuid-shaped ($_nid)" ;;
    esac
    _minted="$(sed -n 's/.*"minted_at": *"\([^"]*\)".*/\1/p' "$STORE" | head -n 1)"
    case "$_minted" in
        [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T*) : ;;
        *) PROBLEM="$PROBLEM; minted_at is not RFC3339-shaped ($_minted)" ;;
    esac
    _bak_count="$(ls "$STATE"/.client_jwts.json-*.bak 2>/dev/null | wc -l | tr -d ' ')"
    if [ "$_bak_count" -lt 1 ] 2>/dev/null; then
        PROBLEM="$PROBLEM; no timestamped .client_jwts.json-*.bak snapshot on disk"
    fi
    if [ -z "$PROBLEM" ]; then
        pass "hr-store-shape-v33" ".client_jwts.json matches the v33 shape (one \"direct\" entry with by_client_jwt/client_id/network_id/minted_at) and ${_bak_count} timestamped .bak snapshot(s) exist"
    else
        fail "hr-store-shape-v33" "store shape problem(s):$PROBLEM"
    fi
else
    if [ "$MINT_OK" = "1" ]; then
        fail "hr-store-shape-v33" "no store at $STORE although an identity was minted"
    else
        skip "hr-store-shape-v33" "UPSTREAM" "no store was written (no identity minted this run)"
    fi
fi

# ------------------------------------------- (b) urnet-tools against daemon --

# t_run <name> <args...>: run urnet-tools, capture out/err, expose T_RC.
# Hard-refuses (exit 78, without invoking anything) when client targeting is
# not pinned to the sandbox: every caller must sit inside the CLIENT_SAFE
# gates below, so this firing means a client invocation escaped a gate.
t_run() {
    if [ "$CLIENT_SAFE" != "1" ]; then
        printf 'FATAL: refusing a client-level urnet-tools invocation with unsafe targeting\n' >&2
        printf 'invocation: urnet-tools %s\n' "$*" >&2
        printf '%s\n' "$CLIENT_UNSAFE_REASON" >&2
        exit 78
    fi
    _n=$1
    shift
    "$TOOLS_BIN" "$@" >"$TOOLS_RUNS/$_n.out" 2>"$TOOLS_RUNS/$_n.err"
    T_RC=$?
    if [ -n "$OUT_DIR" ]; then
        cp "$TOOLS_RUNS/$_n.out" "$OUT_DIR/tools-$_n.out" 2>/dev/null || true
        cp "$TOOLS_RUNS/$_n.err" "$OUT_DIR/tools-$_n.err" 2>/dev/null || true
    fi
    return 0
}
# d_run <name> <args...>: run urnet-docker the same way.
d_run() {
    _n=$1
    shift
    "$DOCKER_BIN" "$@" >"$TOOLS_RUNS/$_n.out" 2>"$TOOLS_RUNS/$_n.err"
    T_RC=$?
    if [ -n "$OUT_DIR" ]; then
        cp "$TOOLS_RUNS/$_n.out" "$OUT_DIR/docker-$_n.out" 2>/dev/null || true
        cp "$TOOLS_RUNS/$_n.err" "$OUT_DIR/docker-$_n.err" 2>/dev/null || true
    fi
    return 0
}
t_out()  { cat "$TOOLS_RUNS/$1.out" 2>/dev/null; }
t_text() { cat "$TOOLS_RUNS/$1.out" "$TOOLS_RUNS/$1.err" 2>/dev/null; }

# The two helpers below record the client-level checks of section (b) as
# SKIP-UNSAFE when CLIENT_SAFE != 1; the client is never invoked in that case.
# The raw nc control probes (daemon-traffic/internals/goroutines) stay
# unaffected because they talk to the socket directly.
client_skip_status_rows() {
    skip "tools-version" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "tools-status" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "daemon-snapshot" "UNSAFE" "$CLIENT_UNSAFE_REASON"
}
client_skip_surface_rows() {
    skip "daemon-history" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "control-metrics-on" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "control-metrics-scrape" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "control-key-set-get" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "tools-logs" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "restored-enforcement" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "restored-completion" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "restored-wallet" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "restored-wallet-status" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "proxy-synthetic-add" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "proxy-trim-preview" "UNSAFE" "$CLIENT_UNSAFE_REASON"
    skip "proxy-remove-clear" "UNSAFE" "$CLIENT_UNSAFE_REASON"
}

# nc_ok: a netcat that supports unix sockets (busybox nc does not).
nc_ok() {
    command -v nc >/dev/null 2>&1 || return 1
    _e="$(nc -U -w 1 "$WORK_DIR/no-such.sock" </dev/null 2>&1)"
    case "$_e" in
        *"invalid option"*|*"unknown option"*|*"unrecognized option"*|*"illegal option"*|*"usage:"*|*"Usage:"*) return 1 ;;
        *) return 0 ;;
    esac
}

# probe_control <cmd>: send one raw control request and echo the reply line.
probe_control() {
    printf '{"cmd":"%s"}\n' "$1" | nc -U -w 4 "$STATE/provider.sock" 2>/dev/null | head -n 1
}

# http_get <url>: curl, else wget; non-zero when neither exists.
http_get() {
    if command -v curl >/dev/null 2>&1; then
        curl -fsS --max-time 10 "$1"
    elif command -v wget >/dev/null 2>&1; then
        wget -q -O - --timeout=10 "$1"
    else
        return 127
    fi
}

say "urnet-tools command surface against the running daemon (b) [$SURFACE_RUN]"

# Client-level checks run only when targeting is pinned (CLIENT_SAFE); the
# body indentation is deliberately left at the old level to keep this gate a
# small, reviewable diff over the committed harness.
if [ "$CLIENT_SAFE" = "1" ]; then

t_run version version
if [ "$T_RC" = "0" ] && head -n 1 "$TOOLS_RUNS/version.out" 2>/dev/null | grep -q '^urnet-tools '; then
    if grep -q 'running' "$TOOLS_RUNS/version.out" 2>/dev/null; then
        pass "tools-version" "$(head -n 1 "$TOOLS_RUNS/version.out") — discovered provider reported running"
    else
        fail "tools-version" "version exited 0 but no discovered provider is reported running: $(head -n 2 "$TOOLS_RUNS/version.out" | tr '\n' ' ')"
    fi
else
    fail "tools-version" "urnet-tools version exited $T_RC or lacks the 'urnet-tools <version>' marker: $(head -n 2 "$TOOLS_RUNS/version.out" 2>/dev/null | tr '\n' ' ')"
fi

t_run status status --state-dir "$STATE"
if [ "$T_RC" = "0" ]; then
    PROBLEM=""
    grep -q "state-dir:.*$STATE" "$TOOLS_RUNS/status.out" 2>/dev/null || PROBLEM="$PROBLEM; no state-dir line for $STATE"
    grep -q 'running:.*true' "$TOOLS_RUNS/status.out" 2>/dev/null || PROBLEM="$PROBLEM; running: true missing"
    grep -q 'control-socket:.*true' "$TOOLS_RUNS/status.out" 2>/dev/null || PROBLEM="$PROBLEM; control-socket: true missing"
    _spid="$(sed -n 's/^pid: *//p' "$TOOLS_RUNS/status.out" 2>/dev/null | head -n 1)"
    [ "$_spid" = "$CURRENT_PID" ] || PROBLEM="$PROBLEM; status pid '$_spid' != started pid '$CURRENT_PID'"
    if [ -z "$PROBLEM" ]; then
        pass "tools-status" "status --state-dir reports the live daemon (pid $CURRENT_PID, running, control-socket)"
    else
        fail "tools-status" "status exited 0 but:$PROBLEM"
    fi
else
    fail "tools-status" "urnet-tools status --state-dir exited $T_RC: $(head -n 2 "$TOOLS_RUNS/status.err" 2>/dev/null | tr '\n' ' ')"
fi

t_run status-json status --state-dir "$STATE" --json
if [ "$T_RC" = "0" ] && grep -q '"v": *1' "$TOOLS_RUNS/status-json.out" 2>/dev/null &&
   grep -q '"state"' "$TOOLS_RUNS/status-json.out" 2>/dev/null &&
   grep -q '"resources"' "$TOOLS_RUNS/status-json.out" 2>/dev/null; then
    pass "daemon-snapshot" "raw live snapshot over the control socket (status --json: v=1, state, resources present)"
else
    fail "daemon-snapshot" "status --json exited $T_RC or the snapshot lacks v/state/resources: $(head -c 200 "$TOOLS_RUNS/status-json.out" 2>/dev/null)"
fi

else
    # Client-level targeting is not pinned to the sandbox: record the checks
    # this block would have run and skip them; nothing was invoked.
    client_skip_status_rows
fi

# traffic / internals / goroutines: the same raw requests urnet-tools sends for
# top's light counters and its Internals panel.
if nc_ok; then
    _t="$(probe_control traffic)"
    case "$_t" in
        *'"ok":true'*'"traffic"'*) pass "daemon-traffic" "raw traffic counter reply: $(printf '%s' "$_t" | head -c 140)" ;;
        *) fail "daemon-traffic" "traffic probe reply not understood: $(printf '%s' "$_t" | head -c 200)" ;;
    esac
    _i="$(probe_control internals)"
    case "$_i" in
        *'"ok":true'*'"internals"'*'"goroutines"'*) pass "daemon-internals" "raw internals reply: $(printf '%s' "$_i" | head -c 140)" ;;
        *) fail "daemon-internals" "internals probe reply not understood: $(printf '%s' "$_i" | head -c 200)" ;;
    esac
    _g="$(probe_control goroutines)"
    case "$_g" in
        *'"ok":true'*'"goroutines"'*'"total"'*) pass "daemon-goroutines" "raw goroutine-profile reply: $(printf '%s' "$_g" | head -c 140)" ;;
        *) fail "daemon-goroutines" "goroutines probe reply not understood: $(printf '%s' "$_g" | head -c 200)" ;;
    esac
else
    skip "daemon-traffic" "NO-TOOL" "no unix-socket netcat (nc -U) in the sandbox; the CI job installs netcat-openbsd"
    skip "daemon-internals" "NO-TOOL" "no unix-socket netcat (nc -U) in the sandbox; the CI job installs netcat-openbsd"
    skip "daemon-goroutines" "NO-TOOL" "no unix-socket netcat (nc -U) in the sandbox; the CI job installs netcat-openbsd"
fi

# Client-level checks run only when targeting is pinned (CLIENT_SAFE); the
# body indentation is deliberately left at the old level to keep this gate a
# small, reviewable diff over the committed harness. The raw nc probes above
# already ran, so the daemon rows they produced are unaffected.
if [ "$CLIENT_SAFE" = "1" ]; then

t_run history history --state-dir "$STATE" 20
if [ "$T_RC" = "0" ] && grep -q 'audit entries' "$TOOLS_RUNS/history.out" 2>/dev/null &&
   grep -qE '^\[[0-9]{4}-' "$TOOLS_RUNS/history.out" 2>/dev/null; then
    pass "daemon-history" "history --state-dir rendered the audit trail: $(grep -m1 'audit entries' "$TOOLS_RUNS/history.out")"
else
    fail "daemon-history" "history exited $T_RC or no audit entry rendered: $(head -n 2 "$TOOLS_RUNS/history.out" 2>/dev/null | tr '\n' ' ')"
fi

# control key: metrics on, then the /metrics scrape.
t_run metrics-on metrics on --state-dir "$STATE"
t_run metrics-status metrics status --state-dir "$STATE"
MPORT=9100
if [ "$T_RC" = "0" ] && grep -q 'Metrics: on' "$TOOLS_RUNS/metrics-status.out" 2>/dev/null &&
   grep -q 'listening' "$TOOLS_RUNS/metrics-status.out" 2>/dev/null; then
    MADDR="$(sed -n 's|.*listening  *http://\([^/]*\)/metrics.*|\1|p' "$TOOLS_RUNS/metrics-status.out" 2>/dev/null | head -n 1)"
    MPORT="$(printf '%s' "$MADDR" | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p')"
    [ -n "$MPORT" ] || MPORT=9100
    pass "control-metrics-on" "metrics on applied live; metrics status reports 'Metrics: on' listening on ${MADDR:-unknown} (port ${MPORT})"
else
    fail "control-metrics-on" "metrics on/status did not report a live listening address (status rc=$T_RC): $(head -n 3 "$TOOLS_RUNS/metrics-status.out" 2>/dev/null | tr '\n' ' ')"
fi

if command -v curl >/dev/null 2>&1 || command -v wget >/dev/null 2>&1; then
    _ok=0
    _i=0
    while [ "$_i" -lt 5 ]; do
        _body="$(http_get "http://127.0.0.1:${MPORT}/metrics" 2>/dev/null)" && case "$_body" in
            *urnet_info*|*urnet_startup*) _ok=1 ;;
        esac
        [ "$_ok" = "1" ] && break
        sleep 2
        _i=$((_i + 1))
    done
    if [ "$_ok" = "1" ]; then
        pass "control-metrics-scrape" "http://127.0.0.1:${MPORT}/metrics served Prometheus text (urnet_* series present)"
    else
        fail "control-metrics-scrape" "no urnet_* series from http://127.0.0.1:${MPORT}/metrics after 5 tries"
    fi
else
    skip "control-metrics-scrape" "NO-TOOL" "no curl or wget in the sandbox; the CI job installs curl"
fi

# control key: set/get roundtrip (applied live, no restart).
t_run set-gogc set gogc 250 -f --state-dir "$STATE"
t_run get-gogc get gogc --state-dir "$STATE"
t_run set-gogc-off set gogc off -f --state-dir "$STATE"
t_run get-gogc-off get gogc --state-dir "$STATE"
if grep -q 'applied live' "$TOOLS_RUNS/set-gogc.out" 2>/dev/null &&
   grep -q 'gogc: 250' "$TOOLS_RUNS/get-gogc.out" 2>/dev/null &&
   grep -q 'not set' "$TOOLS_RUNS/get-gogc-off.out" 2>/dev/null; then
    pass "control-key-set-get" "set gogc 250 applied live, read back 250, cleared again (no restart)"
else
    fail "control-key-set-get" "set/get roundtrip failed: set='$(head -n 1 "$TOOLS_RUNS/set-gogc.out" 2>/dev/null)' get='$(head -n 1 "$TOOLS_RUNS/get-gogc.out" 2>/dev/null)' get-after-off='$(head -n 1 "$TOOLS_RUNS/get-gogc-off.out" 2>/dev/null)'"
fi

# logs: the tool streams the unitless provider's stdout (documented follow
# behavior). The runner is put in its own session/process group so the follow
# and its tail child are both cleaned up.
LOGS_RC=1
if command -v setsid >/dev/null 2>&1; then
    setsid "$TOOLS_BIN" logs --state-dir "$STATE" 5 >"$TOOLS_RUNS/logs.out" 2>"$TOOLS_RUNS/logs.err" &
    _lpid=$!
    sleep 6
    kill -TERM "-$_lpid" 2>/dev/null || kill -TERM "$_lpid" 2>/dev/null || true
    wait "$_lpid" 2>/dev/null
    LOGS_RC=$?
    if [ "$LOGS_RC" = "0" ]; then LOGS_RC=143; fi
else
    timeout 6 "$TOOLS_BIN" logs --state-dir "$STATE" 5 >"$TOOLS_RUNS/logs.out" 2>"$TOOLS_RUNS/logs.err"
    LOGS_RC=$?
fi
if { [ "$LOGS_RC" = "0" ] || [ "$LOGS_RC" = "124" ] || [ "$LOGS_RC" = "143" ] || [ "$LOGS_RC" = "130" ]; } &&
   grep -q 'Streaming stdout of pid' "$TOOLS_RUNS/logs.out" 2>/dev/null; then
    pass "tools-logs" "logs followed the provider's stdout: $(head -n 1 "$TOOLS_RUNS/logs.out")"
else
    fail "tools-logs" "logs did not stream (rc=$LOGS_RC): $(head -n 2 "$TOOLS_RUNS/logs.out" "$TOOLS_RUNS/logs.err" 2>/dev/null | tr '\n' ' ')"
fi
[ -n "$OUT_DIR" ] && cp "$TOOLS_RUNS/logs.out" "$OUT_DIR/tools-logs.out" 2>/dev/null

# restored v33 commands: enforcement, completion, wallet.
t_run enforcement enforcement history 5 --state-dir "$STATE"
if [ "$T_RC" = "0" ] && grep -q 'Governor enforcement for' "$TOOLS_RUNS/enforcement.out" 2>/dev/null; then
    pass "restored-enforcement" "enforcement history responds for the targeted state dir ($(head -n 1 "$TOOLS_RUNS/enforcement.out"))"
else
    fail "restored-enforcement" "enforcement exited $T_RC or lacks the governor marker: $(head -n 2 "$TOOLS_RUNS/enforcement.out" 2>/dev/null | tr '\n' ' ')"
fi

t_run completion completion bash
if [ "$T_RC" = "0" ] && t_text completion | grep -q 'bash completion V2 for urnet-tools'; then
    pass "restored-completion" "completion bash emits the v33 shell-completion script"
else
    fail "restored-completion" "completion bash exited $T_RC without the completion script marker"
fi

t_run wallet-help wallet status -h
if [ "$T_RC" = "0" ] && t_text wallet-help | grep -q 'wallet status'; then
    pass "restored-wallet" "wallet group responds (wallet status -h exits 0 with its help page)"
else
    fail "restored-wallet" "wallet status -h exited $T_RC without its help page"
fi
t_run wallet-status wallet status --state-dir "$STATE"
if [ "$T_RC" = "0" ]; then
    pass "restored-wallet-status" "wallet status answered for the running provider: $(head -n 1 "$TOOLS_RUNS/wallet-status.out")"
elif t_text wallet-status | grep -q 'provider provide --'; then
    skip "restored-wallet-status" "UNSUPPORTED" "the urnet-tools v33 wallet surface is restored and delegates, but the miner CLI does not implement 'wallet status' at this head (it refuses with its usage); flips to PASS when the provider-side command lands"
else
    fail "restored-wallet-status" "wallet status exited $T_RC: $(head -n 2 "$TOOLS_RUNS/wallet-status.err" 2>/dev/null | tr '\n' ' ')"
fi

# proxy commands with synthetic entries (TEST-NET-1, can never be live).
SYNTH="$WORK_DIR/synth-proxies.txt"
printf '192.0.2.10:1080\n192.0.2.11:1080\n192.0.2.12:1080\n' >"$SYNTH"
t_run proxy-add proxy add "$SYNTH" --state-dir "$STATE"
_proxy_file="$STATE/proxy"
if [ "$T_RC" = "0" ] && [ "$(grep -c 'added server' "$TOOLS_RUNS/proxy-add.out" 2>/dev/null)" = "3" ] &&
   grep -q '192.0.2.10:1080' "$_proxy_file" 2>/dev/null &&
   grep -q '192.0.2.12:1080' "$_proxy_file" 2>/dev/null; then
    pass "proxy-synthetic-add" "proxy add admitted all 3 synthetic TEST-NET-1 entries; the proxy file carries them"
else
    fail "proxy-synthetic-add" "proxy add exited $T_RC or the synthetic entries are not all in $_proxy_file"
fi
t_run proxy-trim proxy trim 1 --dry-run --state-dir "$STATE"
if [ "$T_RC" = "0" ] && t_text proxy-trim | grep -q 'preview:'; then
    pass "proxy-trim-preview" "proxy trim 1 --dry-run printed a preview and changed nothing: $(head -n 1 "$TOOLS_RUNS/proxy-trim.out")"
else
    fail "proxy-trim-preview" "proxy trim --dry-run exited $T_RC without a preview line"
fi
t_run proxy-remove proxy remove 192.0.2.10:1080 -f --state-dir "$STATE"
t_run proxy-clear proxy clear -f --state-dir "$STATE"
if [ "$T_RC" = "0" ] && ! grep -q '192\.0\.2\.' "$_proxy_file" 2>/dev/null; then
    pass "proxy-remove-clear" "proxy remove <addr> -f then proxy clear -f removed every synthetic entry"
else
    fail "proxy-remove-clear" "proxy remove/clear left synthetic entries behind (rc=$T_RC)"
fi

else
    # Client-level targeting is not pinned to the sandbox: record every check
    # this block would have run and skip them; nothing was invoked.
    client_skip_surface_rows
fi

# ------------------------------------------------------- (c) urnet-docker --
say "urnet-docker runtime checks, miner line (c)"

docker_skip_all() {
    skip "docker-providers" "NO-DOCKER" "$1"
    skip "docker-status" "NO-DOCKER" "$1"
    skip "docker-exec-control" "NO-DOCKER" "$1"
    skip "docker-exec-metrics" "NO-DOCKER" "$1"
    skip "docker-logs" "NO-DOCKER" "$1"
    skip "docker-top" "NO-DOCKER" "$1"
    skip "docker-proxy-summary" "NO-DOCKER" "$1"
}

DOCKER_SECTION=0
if [ ! -x "$DOCKER_BIN" ]; then
    docker_skip_all "urnet-docker binary not supplied at $DOCKER_BIN"
elif [ "$ACCEPTANCE_SKIP_DOCKER" = "1" ]; then
    docker_skip_all "ACCEPTANCE_SKIP_DOCKER=1"
elif ! command -v docker >/dev/null 2>&1; then
    docker_skip_all "no docker CLI in the sandbox: container jobs (including CI) have no docker daemon; locally mount the host /var/run/docker.sock and install docker onto PATH"
elif ! docker ps >/dev/null 2>&1; then
    docker_skip_all "docker CLI present but no reachable docker daemon"
elif ! docker image inspect "$ACCEPTANCE_DOCKER_IMAGE" >/dev/null 2>&1; then
    docker_skip_all "base image $ACCEPTANCE_DOCKER_IMAGE not present locally (docker pull it or set ACCEPTANCE_DOCKER_IMAGE)"
else
    DOCKER_SECTION=1
fi

if [ "$DOCKER_SECTION" = "1" ]; then
    say "urnet-docker runtime against a miner container"
    DOCKER_NAME="$ACCEPTANCE_DOCKER_NAME"
    docker rm -f "$DOCKER_NAME" >/dev/null 2>&1 || true
    DOCKER_OK=1
    docker create --name "$DOCKER_NAME" -e HOME=/root --entrypoint /bin/sh "$ACCEPTANCE_DOCKER_IMAGE" \
        -c 'exec /opt/provider provide --port=0 --allow-client-registration' >/dev/null 2>&1 || DOCKER_OK=0
    if [ "$DOCKER_OK" = "1" ]; then
        DOCKER_CREATED=1
        # The miner runs as /opt/provider so the in-container discovery matches
        # its provider allowlist; urnet-tools is on PATH for docker exec.
        docker cp "$BIN_DIR/miner" "$DOCKER_NAME:/opt/provider" >/dev/null 2>&1 || DOCKER_OK=0
        docker cp "$TOOLS_BIN" "$DOCKER_NAME:/usr/local/bin/urnet-tools" >/dev/null 2>&1 || DOCKER_OK=0
        mkdir -p "$WORK_DIR/docker-stage/.urnetwork"
        cp "$STATE/jwt" "$WORK_DIR/docker-stage/.urnetwork/jwt" 2>/dev/null || DOCKER_OK=0
        chmod 600 "$WORK_DIR/docker-stage/.urnetwork/jwt" 2>/dev/null || true
        printf '%s\n' '{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}' >"$WORK_DIR/docker-stage/.urnetwork/network.json"
        docker cp "$WORK_DIR/docker-stage/.urnetwork" "$DOCKER_NAME:/root/" >/dev/null 2>&1 || DOCKER_OK=0
        docker start "$DOCKER_NAME" >/dev/null 2>&1 || DOCKER_OK=0
    fi
    if [ "$DOCKER_OK" != "1" ]; then
        docker_skip_all "could not stage the miner container ($DOCKER_NAME) from $ACCEPTANCE_DOCKER_IMAGE"
    else
        _i=0
        while [ "$_i" -lt "$TIMEOUT_S" ]; do
            docker exec "$DOCKER_NAME" test -S /root/.urnetwork/provider.sock >/dev/null 2>&1 && break
            sleep 1
            _i=$((_i + 1))
        done
        printf 'miner container=%s socket_after=%ss\n' "$DOCKER_NAME" "$_i"
        record "docker container=$DOCKER_NAME socket_after=${_i}s"

        d_run dk-providers providers
        if [ "$T_RC" = "0" ] && grep -q "$DOCKER_NAME" "$TOOLS_RUNS/dk-providers.out" 2>/dev/null &&
           grep -q 'true' "$TOOLS_RUNS/dk-providers.out" 2>/dev/null; then
            pass "docker-providers" "urnet-docker providers lists $DOCKER_NAME as RUNNING"
        else
            fail "docker-providers" "providers did not list $DOCKER_NAME as running: $(t_out dk-providers | head -n 3 | tr '\n' ' ')"
        fi

        d_run dk-status status --unit "$DOCKER_NAME"
        if [ "$T_RC" = "0" ] && grep -q "container:.*$DOCKER_NAME" "$TOOLS_RUNS/dk-status.out" 2>/dev/null &&
           grep -q 'running:.*true' "$TOOLS_RUNS/dk-status.out" 2>/dev/null &&
           grep -q 'state-dir (in container):.*urnetwork' "$TOOLS_RUNS/dk-status.out" 2>/dev/null; then
            pass "docker-status" "status --unit names the container, image, running state and in-container state dir"
        else
            fail "docker-status" "status exited $T_RC without the expected fields: $(t_out dk-status | head -n 4 | tr '\n' ' ')"
        fi

        d_run dk-exec-control exec --unit "$DOCKER_NAME" -- urnet-tools status --state-dir /root/.urnetwork
        if [ "$T_RC" = "0" ] && grep -q 'running:.*true' "$TOOLS_RUNS/dk-exec-control.out" 2>/dev/null; then
            pass "docker-exec-control" "urnet-docker exec delegated status into the container; the in-container daemon answers"
        else
            fail "docker-exec-control" "exec delegation failed (rc=$T_RC): $(t_text dk-exec-control | head -n 2 | tr '\n' ' ')"
        fi

        d_run dk-exec-metrics exec --unit "$DOCKER_NAME" -- urnet-tools metrics status
        if [ "$T_RC" = "0" ] && grep -q 'Metrics:' "$TOOLS_RUNS/dk-exec-metrics.out" 2>/dev/null; then
            pass "docker-exec-metrics" "the miner-only metrics surface answers through exec: $(head -n 1 "$TOOLS_RUNS/dk-exec-metrics.out")"
        else
            fail "docker-exec-metrics" "exec metrics status failed (rc=$T_RC): $(t_text dk-exec-metrics | head -n 2 | tr '\n' ' ')"
        fi

        timeout 6 "$DOCKER_BIN" logs --unit "$DOCKER_NAME" 10 >"$TOOLS_RUNS/dk-logs.out" 2>"$TOOLS_RUNS/dk-logs.err"
        _rc=$?
        if { [ "$_rc" = "0" ] || [ "$_rc" = "124" ]; } && [ -s "$TOOLS_RUNS/dk-logs.out" ]; then
            pass "docker-logs" "urnet-docker logs streamed the container log ($(wc -l <"$TOOLS_RUNS/dk-logs.out" | tr -d ' ') lines before the follow was killed, rc=$_rc)"
        else
            fail "docker-logs" "logs did not stream (rc=$_rc)"
        fi

        if command -v script >/dev/null 2>&1; then
            TERM=xterm timeout 10 script -qec "$DOCKER_BIN top --unit $DOCKER_NAME" /dev/null >"$TOOLS_RUNS/dk-top.out" 2>&1
            _rc=$?
            _bytes="$(wc -c <"$TOOLS_RUNS/dk-top.out" | tr -d ' ')"
            if [ "$_rc" = "124" ] && [ "$_bytes" -gt 1024 ] 2>/dev/null &&
               ! grep -qE 'not a TTY|needs an interactive terminal|cannot open the terminal' "$TOOLS_RUNS/dk-top.out" 2>/dev/null; then
                pass "docker-top" "urnet-docker top rendered inside the pty for the full 10s window (${_bytes} bytes, killed by the timeout)"
            else
                fail "docker-top" "top did not render (rc=$_rc, ${_bytes} bytes): $(head -c 160 "$TOOLS_RUNS/dk-top.out" | tr '\n' ' ')"
            fi
        else
            skip "docker-top" "NO-TOOL" "no 'script' in the sandbox to give docker exec -it a pty"
        fi

        d_run dk-proxy-summary proxy summary --unit "$DOCKER_NAME"
        if [ "$T_RC" = "0" ]; then
            pass "docker-proxy-summary" "urnet-docker proxy summary answered inside the miner container"
        elif t_text dk-proxy-summary | grep -q 'unknown proxy subcommand "summary"'; then
            skip "docker-proxy-summary" "UNSUPPORTED" "the miner urnet-tools proxy surface lacks 'summary' (the relay-only summary path; documented P2(d) finding); flips to PASS when the surface lands"
        else
            fail "docker-proxy-summary" "proxy summary failed (rc=$T_RC): $(t_text dk-proxy-summary | head -n 2 | tr '\n' ' ')"
        fi

        docker rm -f "$DOCKER_NAME" >/dev/null 2>&1 || true
        DOCKER_CREATED=0
    fi
fi

# urnet-docker version needs no daemon: run it whenever the binary is present.
if [ -x "$DOCKER_BIN" ]; then
    d_run dk-version version
    if [ "$T_RC" = "0" ] && head -n 1 "$TOOLS_RUNS/dk-version.out" 2>/dev/null | grep -q '^urnet-tools '; then
        pass "docker-version" "urnet-docker version responds: $(head -n 1 "$TOOLS_RUNS/dk-version.out")"
    else
        fail "docker-version" "urnet-docker version exited $T_RC: $(head -n 1 "$TOOLS_RUNS/dk-version.err" 2>/dev/null)"
    fi
else
    skip "docker-version" "UNSUPPORTED" "urnet-docker not supplied in BIN_DIR"
fi

# --------------------------------------------- (d) command-tree parity ------
say "command-tree parity (v33 inventory) (d)"

# _responds <bin> [args...]: 0 when `<bin> [args...] -h` exits 0 with output.
_responds() {
    _bin=$1
    shift
    _o="$(timeout 15 "$_bin" "$@" -h 2>&1)"
    _rc=$?
    [ "$_rc" = "0" ] && [ -n "$_o" ]
}

# parity_check <label> <binary>; rows arrive on stdin as name|aliases
parity_check() {
    _label=$1
    _bin=$2
    _total=0
    _bad=""
    while IFS='|' read -r _name _aliases; do
        [ -n "$_name" ] || continue
        _total=$((_total + 1))
        _responds "$_bin" "$_name" || _bad="$_bad $_name"
        if [ -n "$_aliases" ]; then
            _oldifs=$IFS
            IFS=','
            for _alias in $_aliases; do
                [ -n "$_alias" ] || continue
                _responds "$_bin" "$_alias" || _bad="$_bad $_alias(alias-of-$_name)"
            done
            IFS=$_oldifs
        fi
    done
    if [ -z "$_bad" ]; then
        pass "parity-$_label" "all $_total documented v33 commands answer -h (aliases included)"
    else
        fail "parity-$_label" "missing/not responding:$_bad"
    fi
}

# Rows mirror internal/urnettools/v33_command_surface_test.go
# TestV33CommandSurfaceParity (the authoritative table, pinned as a Go test).
parity_check "v33-commands" "$TOOLS_BIN" <<'EOF'
providers|list,ps
status|
top|
sn-status|
start|
stop|
restart|
update|
idle-update|
self-update|selfupdate
logs|
summary|
version|
default|
session|
turbo|
auto|
eco|
lowmode|
ramlogs|
optimize|
hotswap|hot-swap
fast-auth|fastauth
get|show
set|
rename|set-node-name
auth|
choose-network|choose_network
wallet|
proxy|
report|
reinstall|
uninstall|
auto-update|autoupdate
auto-start|autostart
self-heal|selfheal
show-ip|ip-detect,ipdetect
history|audit
autopilot|
enforcement|
baseline|
metrics|
smart-dialer|smart_dialer
profile|profiles
dashboard|dash,panel
config|
usage|
__do-restart|
EOF

WALLET_BAD=""
for _sub in status challenge set sign; do
    _responds "$TOOLS_BIN" wallet "$_sub" || WALLET_BAD="$WALLET_BAD wallet-$_sub"
done
if [ -z "$WALLET_BAD" ]; then
    pass "parity-wallet-subs" "wallet status/challenge/set/sign all answer -h"
else
    fail "parity-wallet-subs" "not responding:$WALLET_BAD"
fi

_proxy_err="$("$TOOLS_BIN" proxy 2>&1 | head -c 600)"
PROXY_BAD=""
for _sub in add paste clear remove refresh add-source remove-source health traffic ids remove-dead trim audit; do
    case "$_proxy_err" in
        *"$_sub"*) : ;;
        *) PROXY_BAD="$PROXY_BAD $_sub" ;;
    esac
done
if [ -z "$PROXY_BAD" ]; then
    pass "parity-proxy-subs" "the proxy dispatcher names every v33 verb (add/paste/clear/remove/refresh/add-source/remove-source/health/traffic/ids/remove-dead/trim/audit)"
else
    fail "parity-proxy-subs" "the proxy dispatcher no longer names:$PROXY_BAD"
fi

# urnet-docker tree, when the binary travels in the payload.
if [ -x "$DOCKER_BIN" ]; then
    # Rows mirror TestV33DockerCommandSurfaceParity in the same test file.
    parity_check "docker-commands" "$DOCKER_BIN" <<'EOF'
providers|list,ps
status|
top|
sn-status|
start|
stop|
restart|
logs|
auth|
choose-network|choose_network
summary|
report|
update|self-update,selfupdate
idle-update|
version|
proxy|
self-heal|selfheal
set|
rename|
fast-auth|fastauth
session|
exec|
direct|
usage|
EOF
else
    skip "parity-docker-commands" "UNSUPPORTED" "urnet-docker not supplied in BIN_DIR"
fi

# ------------------------------------------------------------------ report --

# Stop the surface daemon before the report so the process table is clean.
if [ -n "$CURRENT_PID" ]; then
    if stop_current; then
        pass "run-cleanup" "surface daemon stopped cleanly on SIGTERM"
    else
        fail "run-cleanup" "surface daemon needed SIGKILL after ${STOP_TIMEOUT_S}s of SIGTERM"
    fi
fi

if [ -n "$OUT_DIR" ]; then
    mkdir -p "$OUT_DIR/logs" 2>/dev/null || true
    for _f in "$LOGDIR"/*.out "$LOGDIR"/*.err; do
        [ -f "$_f" ] || continue
        cp "$_f" "$OUT_DIR/logs/" 2>/dev/null || true
    done
    printf 'runs=%s\n' "$(tr '\n' ' ' <"$WORK_DIR/runs.log" 2>/dev/null)" >>"$REPORT_FILE"
fi

echo
echo "==================== acceptance-matrix summary ===================="
printf 'os:     %s\n' "${OS_PRETTY:-unknown}"
printf 'libc:   %s\n' "$LIBC"
printf 'checks: %s passed, %s failed, %s skipped\n' "$PASSES" "$FAILS" "$SKIPS"
echo
printf 'PASS=%s FAIL=%s SKIP=%s\n' "$PASSES" "$FAILS" "$SKIPS"
if [ "$FAILS" -gt 0 ]; then
    echo
    echo "FAIL items:"
    [ -n "$REPORT_FILE" ] && grep '^FAIL ' "$REPORT_FILE" 2>/dev/null || true
fi
if [ "$SKIPS" -gt 0 ]; then
    echo
    echo "SKIP items (reasons):"
    [ -n "$REPORT_FILE" ] && grep '^SKIP-' "$REPORT_FILE" 2>/dev/null || true
fi
if [ "$FAILS" -gt 0 ]; then
    printf 'RESULT: FAIL\n'
    record "RESULT: FAIL"
    exit 1
fi
printf 'RESULT: PASS\n'
record "RESULT: PASS"
exit 0
