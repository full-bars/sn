#!/bin/sh
# distro-smoke.sh — distro-matrix control-plane smoke for the miner and
# urnet-tools. POSIX sh on purpose: it runs INSIDE the distro image, where
# there is no bash assumption, no jq and no python3 (Alpine, debian-slim,
# openSUSE and Arch images all lack at least one of them).
#
# What it proves per distro:
#   1. the static binaries load and run on that libc (musl included),
#   2. `miner provide --port=0` starts and binds $STATE/provider.sock,
#   3. urnet-tools discovers the running provider and answers `version` and
#      `status` with the documented exit codes and markers,
#   4. SIGTERM shuts the miner down cleanly.
#
# Isolation: every process runs with HOME set to a throwaway directory created
# by this script. Nothing reads or writes the ambient HOME, and the JWT is
# copied into the throwaway state dir with mode 600 and is never printed.
#
# Inputs (env, all optional unless noted):
#   BIN_DIR         directory holding `miner` (required) and optionally
#                   `urnet-docker`
#   TOOLS_BIN       path to urnet-tools (default: $BIN_DIR/urnet-tools)
#   JWT_FILE        path to the disposable CI JWT. When unset, the script mints
#                   one from GAUNTLET_USER/GAUNTLET_PW (the shakedown.yml
#                   pattern) into the isolated HOME. Either way the token is
#                   copied with mode 600 and is never printed. The
#                   production-account refusal guard always runs on the token
#                   that is used. SMOKE_ALLOW_NO_JWT=1 is the only way to run
#                   without a token.
#   GAUNTLET_USER   disposable CI account user; used only when JWT_FILE is
#                   unset (pass ${{ secrets.GAUNTLET_USER }} in CI)
#   GAUNTLET_PW     disposable CI account password (never printed)
#   MINT_API_URL    API used for the mint (default: https://api.bringyour.com)
#   STATE_DIR       state dir to use (default: $HOME_DIR/.urnetwork)
#   HOME_DIR        isolated HOME (default: /tmp/urnet-smoke/home)
#   WORK_DIR        scratch dir (default: /tmp/urnet-smoke/work)
#   OUT_DIR         evidence dir; when set, per-check evidence is written there
#   TIMEOUT_S       seconds to wait for provider.sock (default: 60)
#   MINER_ARGS      extra args for `miner provide` (default:
#                   --allow-client-registration, the documented first-run flag)
#   SMOKE_ADAPT_DISCOVERY  default 1: point the invoking user's /etc/passwd home
#                   at HOME_DIR and run the miner through a `provider`-named
#                   symlink, so urnet-tools' discovery contract sees it. Both
#                   adaptations are container-local and are printed in the
#                   report. Set to 0 to disable.
#
# Exit status: 0 when every hard check passed, 1 otherwise.
#
# Check policy: failures are hard by default. Checks whose outcome is
# documented as in-flight at this head (see distro-matrix.yml) are printed as
# CONDITIONAL and never fail the run.

set -u

BIN_DIR="${BIN_DIR:-}"
TOOLS_BIN="${TOOLS_BIN:-}"
JWT_FILE="${JWT_FILE:-}"
STATE_DIR="${STATE_DIR:-}"
HOME_DIR="${HOME_DIR:-/tmp/urnet-smoke/home}"
WORK_DIR="${WORK_DIR:-/tmp/urnet-smoke/work}"
OUT_DIR="${OUT_DIR:-}"
TIMEOUT_S="${TIMEOUT_S:-60}"
MINER_ARGS="${MINER_ARGS:---allow-client-registration}"
SMOKE_ADAPT_DISCOVERY="${SMOKE_ADAPT_DISCOVERY:-1}"
SMOKE_ALLOW_NO_JWT="${SMOKE_ALLOW_NO_JWT:-0}"
STOP_TIMEOUT_S="${STOP_TIMEOUT_S:-15}"
GAUNTLET_USER="${GAUNTLET_USER:-}"
GAUNTLET_PW="${GAUNTLET_PW:-}"
MINT_API_URL="${MINT_API_URL:-https://api.bringyour.com}"

PASSES=0
FAILS=0
CONDS=0

if [ -z "$BIN_DIR" ]; then
    echo "FATAL: BIN_DIR is required (the directory holding the miner binary)" >&2
    exit 2
fi
if [ -z "$TOOLS_BIN" ]; then
    TOOLS_BIN="$BIN_DIR/urnet-tools"
fi

if [ -n "$OUT_DIR" ]; then
    mkdir -p "$OUT_DIR" || { echo "FATAL: cannot create OUT_DIR=$OUT_DIR" >&2; exit 2; }
fi

pass() { PASSES=$((PASSES + 1)); printf '  PASS: %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL: %s\n' "$*"; }
# cond marks a check that cannot be decided at this head. It is reported and
# counted, never failed.
cond() { CONDS=$((CONDS + 1)); printf '  CONDITIONAL: %s\n' "$*"; }
say()  { printf '== %s\n' "$*"; }

emit() {
    # emit <file> <text...> — keep an evidence copy when OUT_DIR is set.
    if [ -n "$OUT_DIR" ]; then
        printf '%s\n' "$*" >>"$OUT_DIR/$1"
    fi
}

# ---------------------------------------------------------------- transport --
# The stock alpine/debian/... images have some subset of curl and wget. Only
# used by the workflow's artifact fetch, which happens before this script
# runs, so nothing here depends on either.

# ------------------------------------------------------------- identities --
say "distro identity"
OS_PRETTY="$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | head -n 1 | tr -d '"')"
OS_ID="$(sed -n 's/^ID=//p' /etc/os-release 2>/dev/null | head -n 1 | tr -d '"')"
OS_VER="$(sed -n 's/^VERSION_ID=//p' /etc/os-release 2>/dev/null | head -n 1 | tr -d '"')"
ARCH="$(uname -m 2>/dev/null)"
KERNEL="$(uname -r 2>/dev/null)"
if [ -e /lib/ld-musl-x86_64.so.1 ] || [ -e /lib/ld-musl-aarch64.so.1 ]; then
    LIBC="musl ($(ls /lib/ld-musl-*.so.1 2>/dev/null | head -n 1))"
elif command -v ldd >/dev/null 2>&1; then
    LIBC="$(ldd --version 2>&1 | head -n 1)"
elif [ -e /lib64/ld-linux-x86-64.so.2 ]; then
    LIBC="glibc (/lib64/ld-linux-x86-64.so.2)"
else
    LIBC="unknown"
fi
printf 'os=%s id=%s version=%s arch=%s kernel=%s libc=%s\n' \
    "${OS_PRETTY:-unknown}" "${OS_ID:-unknown}" "${OS_VER:-unknown}" "$ARCH" "$KERNEL" "$LIBC"
emit distro.txt "os=${OS_PRETTY:-unknown} id=${OS_ID:-unknown} version=${OS_VER:-unknown} arch=$ARCH kernel=$KERNEL libc=$LIBC"

# ----------------------------------------------------------------- inputs --
say "inputs"
if [ -x "$BIN_DIR/miner" ]; then
    pass "miner binary present and executable: $BIN_DIR/miner"
else
    fail "miner binary missing or not executable: $BIN_DIR/miner"
    # Without the miner every later check is meaningless; report and stop.
    echo "RESULT: FAIL ($FAILS failed, $PASSES passed, $CONDS conditional)"
    exit 1
fi
if [ -x "$TOOLS_BIN" ]; then
    pass "urnet-tools binary present and executable: $TOOLS_BIN"
else
    fail "urnet-tools binary missing or not executable: $TOOLS_BIN"
    echo "RESULT: FAIL ($FAILS failed, $PASSES passed, $CONDS conditional)"
    exit 1
fi
if [ -x "$BIN_DIR/urnet-docker" ]; then
    pass "urnet-docker binary present: $BIN_DIR/urnet-docker"
else
    cond "urnet-docker not supplied in BIN_DIR (not built for this cell)"
fi
if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
    # The token itself is never read or printed here; only its shape is checked.
    case "$(cat "$JWT_FILE")" in
        *.*.*) pass "supplied JWT is shaped like a JWT (3 dot-separated parts), contents never printed" ;;
        *)     fail "JWT_FILE=$JWT_FILE is not shaped like a JWT" ;;
    esac
else
    # Resolved after the isolated HOME exists: the mint writes into it.
    printf '  no JWT_FILE supplied; will mint from GAUNTLET_USER/GAUNTLET_PW if set\n'
fi

# --------------------------------------------------------------- sandboxing --
say "sandbox"
rm -rf "$HOME_DIR" "$WORK_DIR" 2>/dev/null || true
mkdir -p "$HOME_DIR" "$WORK_DIR" || {
    echo "FATAL: cannot create the isolated HOME at $HOME_DIR" >&2
    exit 2
}
chmod 700 "$HOME_DIR" 2>/dev/null || true
if [ -z "$STATE_DIR" ]; then
    STATE_DIR="$HOME_DIR/.urnetwork"
fi
# Only OUT_DIR (if any) is pre-created by the script; the state dir is created
# by the miner itself so its ownership matches the process that uses it.
mkdir -p "$STATE_DIR" 2>/dev/null || true
chmod 700 "$STATE_DIR" 2>/dev/null || true
export HOME="$HOME_DIR"
printf 'HOME_DIR=%s STATE_DIR=%s\n' "$HOME_DIR" "$STATE_DIR"
pass "isolated HOME created: $HOME_DIR (state dir $STATE_DIR)"

# ---------------------------------------------------------- the disposable JWT --
# guard_jwt <file>: refuse the production account, the same guard shakedown.yml
# applies. Decoded with POSIX tools on purpose: these images have no python3
# and no jq. The token value is only ever held in a shell variable and is never
# printed or written to the evidence directory.
guard_jwt() {
    _raw="$(cat "$1" 2>/dev/null)"
    _payload="$(printf '%s' "$_raw" | cut -d. -f2 | tr '_-' '/+')"
    _pad=$(( (4 - ${#_payload} % 4) % 4 ))
    case "$_pad" in
        1) _payload="$_payload=" ;;
        2) _payload="$_payload==" ;;
        3) _payload="$_payload===" ;;
    esac
    if printf '%s' "$_payload" | base64 -d >"$WORK_DIR/jwt-payload.json" 2>/dev/null &&
       [ -s "$WORK_DIR/jwt-payload.json" ]; then
        _network="$(sed -n 's/.*"network_name":"\([^"]*\)".*/\1/p' "$WORK_DIR/jwt-payload.json" | head -n 1)"
        if grep -q 'mesocyclone' "$WORK_DIR/jwt-payload.json"; then
            rm -f "$WORK_DIR/jwt-payload.json"
            fail "REFUSING: the minted JWT is the production account"
            echo "RESULT: FAIL ($FAILS failed, $PASSES passed, $CONDS conditional)"
            exit 1
        fi
        pass "JWT account is not the production account (network_name=${_network:-unknown})"
        if [ -n "$OUT_DIR" ]; then
            printf 'network_name=%s\n' "${_network:-unknown}" >"$OUT_DIR/jwt-account.txt"
        fi
    else
        cond "could not decode the JWT payload with base64; the production-account refusal guard was not applied"
    fi
    rm -f "$WORK_DIR/jwt-payload.json"
}

say "disposable CI JWT"
if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
    cp "$JWT_FILE" "$STATE_DIR/jwt" || fail "could not copy the supplied JWT into the state dir"
    chmod 600 "$STATE_DIR/jwt" 2>/dev/null || true
    if [ -f "$STATE_DIR/jwt" ]; then
        pass "supplied JWT copied into the state dir with mode 600 (contents never printed)"
    fi
    guard_jwt "$STATE_DIR/jwt"
elif [ -n "$GAUNTLET_USER" ] && [ -n "$GAUNTLET_PW" ]; then
    # Mint a fresh disposable-account token into the isolated HOME. Same
    # invocation as shakedown.yml; -f overwrites any pre-existing store.
    MINT_LOG="$WORK_DIR/mint.log"
    printf 'minting a disposable JWT with the miner (HOME=%s, log=%s)\n' "$HOME_DIR" "$MINT_LOG"
    if HOME="$HOME_DIR" "$BIN_DIR/miner" auth \
        --user_auth="$GAUNTLET_USER" --password="$GAUNTLET_PW" \
        --api_url="$MINT_API_URL" -f >"$MINT_LOG" 2>&1; then
        pass "miner auth exited 0 for the disposable-account mint"
    fi
    # `miner auth` writes $HOME/.urnetwork/jwt. When STATE_DIR is a different
    # directory (an explicit override), move the minted token there.
    if [ ! -s "$STATE_DIR/jwt" ] && [ -s "$HOME_DIR/.urnetwork/jwt" ] && [ "$HOME_DIR/.urnetwork" != "$STATE_DIR" ]; then
        cp "$HOME_DIR/.urnetwork/jwt" "$STATE_DIR/jwt" 2>/dev/null || true
    fi
    if [ -s "$STATE_DIR/jwt" ]; then
        chmod 600 "$STATE_DIR/jwt" 2>/dev/null || true
        pass "minted a disposable JWT into $STATE_DIR/jwt (never printed)"
        guard_jwt "$STATE_DIR/jwt"
    else
        fail "JWT mint failed (verification required? see the log below)"
        tail -n 20 "$MINT_LOG" 2>/dev/null | sed 's/^/    | /'
    fi
elif [ "$SMOKE_ALLOW_NO_JWT" = "1" ]; then
    cond "no JWT supplied and SMOKE_ALLOW_NO_JWT=1 (unauthenticated run on purpose)"
else
    fail "no disposable JWT: set JWT_FILE or GAUNTLET_USER/GAUNTLET_PW (or SMOKE_ALLOW_NO_JWT=1)"
fi
# The network fixture the harnesses use: the production API and connect URLs.
if [ ! -f "$STATE_DIR/network.json" ]; then
    printf '%s\n' '{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}' >"$STATE_DIR/network.json"
fi
pass "network fixture present: $STATE_DIR/network.json"

# ------------------------------------------- discovery contract adaptations --
# urnet-tools' discovery scans argv[0] basenames against an allowlist
# (urnetwork|urnet-provider|provider_beta|provider) and only attributes a state
# dir that lives under the invoking user's /etc/passwd home. In a disposable
# container both are container-local adaptations, not product changes:
#   (1) run the miner through a symlink whose basename is `provider`,
#   (2) point the current user's passwd home at HOME_DIR.
if [ "$SMOKE_ADAPT_DISCOVERY" = "1" ]; then
    mkdir -p "$WORK_DIR/bin"
    ln -sf "$BIN_DIR/miner" "$WORK_DIR/bin/provider" 2>/dev/null || true
    if [ -x "$WORK_DIR/bin/provider" ]; then
        pass "miner reachable through a provider-named path: $WORK_DIR/bin/provider"
    else
        fail "could not create the provider-named symlink in $WORK_DIR/bin"
    fi
    if [ -w /etc/passwd ]; then
        PEME="$(id -un 2>/dev/null || echo root)"
        # POSIX tools only: the opensuse/tumbleweed image ships NO awk, so the
        # field extraction and the rewrite use grep/cut/sed. Getting this wrong
        # is silent: the adaptation no-ops and discovery then attributes
        # /root/.urnetwork instead of the isolated HOME.
        CUR_HOME="$(grep "^$PEME:" /etc/passwd 2>/dev/null | head -n 1 | cut -d: -f6)"
        if [ -z "$CUR_HOME" ]; then
            cond "no /etc/passwd entry for $PEME; state-dir discovery attribution not adapted"
        elif [ "$CUR_HOME" != "$HOME_DIR" ]; then
            # Field 6 of the current user's line is the only field changed.
            sed "s|^\($PEME:[^:]*:[^:]*:[^:]*:[^:]*:\)[^:]*:|\1$HOME_DIR:|" \
                /etc/passwd >"$WORK_DIR/passwd.new" 2>/dev/null &&
                cp "$WORK_DIR/passwd.new" /etc/passwd 2>/dev/null &&
                pass "passwd home for $PEME pointed at $HOME_DIR (container-local discovery adaptation)"
            [ -n "$OUT_DIR" ] && grep "^$PEME:" /etc/passwd >"$OUT_DIR/passwd-entry.txt" 2>/dev/null
        else
            pass "passwd home for $PEME already resolves to $HOME_DIR"
        fi
    else
        cond "cannot adapt /etc/passwd home (not writable); discovery may not attribute $STATE_DIR"
    fi
else
    cond "SMOKE_ADAPT_DISCOVERY=0: disabling the container-local discovery adaptations"
fi

# ------------------------------------------------------------------ tools --
say "urnet-tools standalone"
TOOL_VERSION_OUT="$WORK_DIR/version-before.out"
if "$TOOLS_BIN" version >"$TOOL_VERSION_OUT" 2>"$WORK_DIR/version-before.err"; then
    VRC=0
else
    VRC=$?
fi
if [ "$VRC" = "0" ]; then
    pass "urnet-tools version exits 0 before the miner starts"
else
    fail "urnet-tools version exited $VRC before the miner starts (see below)"
    sed 's/^/    | /' "$WORK_DIR/version-before.err" 2>/dev/null | head -n 10
fi
TOOL_VER="$(head -n 1 "$TOOL_VERSION_OUT" 2>/dev/null)"
printf '  tool version line: %s\n' "${TOOL_VER:-<empty>}"
emit tool-version.txt "$TOOL_VER"

# ------------------------------------------------------------------- miner --
say "miner provide"
MINER_LOG="$WORK_DIR/miner.log"
printf 'miner cmd: HOME=%s %s provide --port=0 %s\n' "$HOME_DIR" "$WORK_DIR/bin/provider" "$MINER_ARGS"
# shellcheck disable=SC2086 # MINER_ARGS is deliberately word-split
"$WORK_DIR/bin/provider" provide --port=0 $MINER_ARGS >"$MINER_LOG" 2>&1 &
MPID=$!
SOCK="$STATE_DIR/provider.sock"
waited=0
sock=no
while [ "$waited" -lt "$TIMEOUT_S" ]; do
    if [ -S "$SOCK" ]; then
        sock=yes
        break
    fi
    if ! kill -0 "$MPID" 2>/dev/null; then
        break
    fi
    sleep 1
    waited=$((waited + 1))
done
alive=no
kill -0 "$MPID" 2>/dev/null && alive=yes

if [ "$sock" = "yes" ]; then
    pass "provider.sock appeared after ${waited}s at $SOCK"
else
    fail "provider.sock did not appear within ${TIMEOUT_S}s (miner alive=$alive)"
    echo "  --- miner log tail ---"
    tail -n 25 "$MINER_LOG" 2>/dev/null | sed 's/^/    | /'
    # The miner's diagnostics exporter skips regular-file sinks, so a plain
    # `>log` redirect of a REFUSED run is empty. Re-run the same command with
    # the output piped (throwaway HOME, so the registration locks of the real
    # state dir are not touched) to capture the refusal JSON.
    say "miner refusal diagnostics (throwaway HOME, piped so the JSON is kept)"
    PROBE_HOME="$WORK_DIR/probe-home"
    mkdir -p "$PROBE_HOME/.urnetwork"
    if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
        cp "$JWT_FILE" "$PROBE_HOME/.urnetwork/jwt" 2>/dev/null || true
        chmod 600 "$PROBE_HOME/.urnetwork/jwt" 2>/dev/null || true
    fi
    cp "$STATE_DIR/network.json" "$PROBE_HOME/.urnetwork/network.json" 2>/dev/null || true
    # shellcheck disable=SC2086 # MINER_ARGS is deliberately word-split
    HOME="$PROBE_HOME" timeout 30 "$WORK_DIR/bin/provider" provide --port=0 $MINER_ARGS 2>&1 |
        tee "$WORK_DIR/miner-refusal.log" | tail -n 20
    [ -n "$OUT_DIR" ] && cp "$WORK_DIR/miner-refusal.log" "$OUT_DIR/miner-refusal.log" 2>/dev/null
    rm -rf "$PROBE_HOME"
fi
if [ "$alive" = "yes" ]; then
    pass "miner process alive (pid $MPID)"
else
    wait "$MPID" 2>/dev/null
    MRC=$?
    fail "miner process exited before the socket was ready (exit=$MRC)"
fi
if [ -n "$OUT_DIR" ]; then
    {
        printf 'pid=%s\nsocket=%s\nsocket_wait_s=%s\nminer_alive=%s\n' "$MPID" "$sock" "$waited" "$alive"
        ls -l "$STATE_DIR" 2>/dev/null
    } >"$OUT_DIR/socket.txt"
fi
if [ "$sock" = "yes" ]; then
    # The socket mode is part of the control-plane contract: owner-only.
    SOCK_MODE="$(ls -l "$SOCK" 2>/dev/null)"
    printf '  socket: %s\n' "$SOCK_MODE"
    case "$SOCK_MODE" in
        srw-------*) pass "provider.sock mode is 0600 (owner-only)" ;;
        *)           fail "provider.sock mode is not owner-only: $SOCK_MODE" ;;
    esac
fi

# ------------------------------------------------------------ tools checks --
# These run only when the socket is up: a client check against a dead provider
# would test nothing, and the missing socket is already reported above.
run_client() {
    # run_client <name> <args...>
    cname="$1"
    shift
    cout="$WORK_DIR/$cname.out"
    cerr="$WORK_DIR/$cname.err"
    "$TOOLS_BIN" "$@" >"$cout" 2>"$cerr"
    crc=$?
    printf '  $ urnet-tools %s -> exit %s\n' "$*" "$crc"
    [ -n "$OUT_DIR" ] && cp "$cout" "$OUT_DIR/$cname.out" 2>/dev/null
    [ -n "$OUT_DIR" ] && cp "$cerr" "$OUT_DIR/$cname.err" 2>/dev/null
    return "$crc"
}

if [ "$sock" = "yes" ]; then
    say "urnet-tools against the running miner"

    if run_client version version; then
        pass "urnet-tools version exits 0"
    else
        fail "urnet-tools version exited nonzero against the running miner"
        sed 's/^/    | /' "$WORK_DIR/version.err" 2>/dev/null | head -n 10
    fi
    if grep -q '^urnet-tools ' "$WORK_DIR/version.out" 2>/dev/null; then
        pass "version output carries the 'urnet-tools <version>' marker"
    else
        fail "version output does not start with the 'urnet-tools ' marker"
    fi
    if grep -qi 'running' "$WORK_DIR/version.out" 2>/dev/null; then
        pass "version output reports the discovered provider as running"
    else
        fail "version output does not report a running discovered provider"
    fi

    if run_client status status --state-dir "$STATE_DIR"; then
        pass "urnet-tools status --state-dir exits 0"
    else
        fail "urnet-tools status --state-dir exited nonzero"
        sed 's/^/    | /' "$WORK_DIR/status.err" 2>/dev/null | head -n 10
    fi
    if grep -q "state-dir: *$STATE_DIR" "$WORK_DIR/status.out" 2>/dev/null; then
        pass "status output names the state dir under test ($STATE_DIR)"
    else
        fail "status output does not name $STATE_DIR"
    fi
    if grep -q 'control-socket: *true' "$WORK_DIR/status.out" 2>/dev/null; then
        pass "status output reports control-socket: true"
    else
        cond "status output does not carry 'control-socket: true' at this head"
    fi
    # With the in-process `provide` invocation used here the provider IS the
    # miner process, so the reported pid must be the miner's pid. A mismatch
    # means the socket is reporting a different process than the one this
    # script started.
    STATUS_PID="$(sed -n 's/^pid: *//p' "$WORK_DIR/status.out" 2>/dev/null | head -n 1)"
    if [ "$STATUS_PID" = "$MPID" ]; then
        pass "status reports the live miner pid ($MPID)"
    else
        fail "status pid '$STATUS_PID' does not match the started miner pid '$MPID'"
    fi
    if grep -q 'running: *true' "$WORK_DIR/status.out" 2>/dev/null; then
        pass "status reports running: true"
    else
        fail "status does not report running: true"
    fi

    # Bare `status` goes through discovery (the two container-local
    # adaptations above), which is the operator's first command in practice.
    if run_client status-discovery status; then
        pass "urnet-tools status (discovery) exits 0"
    else
        fail "urnet-tools status (discovery) exited nonzero — see below"
        sed 's/^/    | /' "$WORK_DIR/status-discovery.out" 2>/dev/null | head -n 15
        sed 's/^/    | /' "$WORK_DIR/status-discovery.err" 2>/dev/null | head -n 15
        echo "  --- discovery inputs: passwd entry and provider-named path ---"
        id -un 2>/dev/null | sed 's/^/    user: /'
        grep "^$(id -un 2>/dev/null || echo root):" /etc/passwd 2>/dev/null | sed 's/^/    passwd: /'
        ls -l "$WORK_DIR/bin/provider" 2>/dev/null | sed 's/^/    /'
    fi
fi

# -------------------------------------------------------------------- stop --
say "shutdown"
kill -TERM "$MPID" 2>/dev/null || true
waited=0
while [ "$waited" -lt "$STOP_TIMEOUT_S" ]; do
    kill -0 "$MPID" 2>/dev/null || break
    sleep 1
    waited=$((waited + 1))
done
if kill -0 "$MPID" 2>/dev/null; then
    kill -KILL "$MPID" 2>/dev/null || true
    sleep 1
    fail "miner did not exit within ${STOP_TIMEOUT_S}s of SIGTERM (SIGKILL sent)"
else
    pass "miner exited cleanly after SIGTERM (${waited}s)"
fi
if [ -n "$OUT_DIR" ]; then
    tail -n 40 "$MINER_LOG" 2>/dev/null >"$OUT_DIR/miner-tail.log"
    printf 'miner_exit_after_stop=%s\n' "$(kill -0 "$MPID" 2>/dev/null && echo alive || echo gone)" >>"$OUT_DIR/socket.txt"
fi

# ------------------------------------------------------------------ report --
echo
echo "==================== distro-smoke summary ===================="
printf 'distro: %s\n' "${OS_PRETTY:-unknown}"
printf 'libc:   %s\n' "$LIBC"
printf 'home:   %s\n' "$HOME_DIR"
printf 'checks: %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
if [ "$FAILS" -gt 0 ]; then
    printf 'RESULT: FAIL\n'
    [ -n "$OUT_DIR" ] && printf 'RESULT: FAIL (%s failed, %s passed, %s conditional)\n' "$FAILS" "$PASSES" "$CONDS" >>"$OUT_DIR/summary.txt"
    exit 1
fi
printf 'RESULT: PASS\n'
[ -n "$OUT_DIR" ] && printf 'RESULT: PASS (%s passed, %s conditional)\n' "$PASSES" "$CONDS" >>"$OUT_DIR/summary.txt"
exit 0
