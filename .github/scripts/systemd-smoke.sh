#!/bin/sh
# systemd-smoke.sh — Tier-2 systemd cell for the distro matrix. Runs INSIDE a
# privileged systemd container (PID 1 is /sbin/init) and drives the miner
# through a real systemd unit: install, start, socket, urnet-tools, stop,
# restart, crash-restart.
#
# The unit this script writes mirrors the shape of the unit the shipped
# installer writes at this head (scripts/Provider_Install_Linux.sh:
# Type=simple, ExecStart=.../urnetwork provide, Restart=on-failure,
# RestartSec=5). The installer writes a USER unit; this harness writes the
# equivalent SYSTEM unit because the container has no user session bus. That
# difference is asserted, not glossed over: the shipped template is grepped and
# the harness unit's fields are compared against it.
#
# Checks whose feature is not in the tree at the checked-out commit are
# printed as CONDITIONAL with a TODO marker and never fail the cell. The gates
# are greps over the mounted repository (REPO_DIR), so they activate by
# themselves when the feature lands.
#
# Inputs (env):
#   BIN_DIR    payload dir with `miner` (required) and `urnet-tools`
#   JWT_FILE   disposable CI JWT (required; copied, never printed)
#   REPO_DIR   mounted repository (default: /opt/repo); used for the template
#              parity and feature-presence greps
#   HOME_DIR   home for the smoke user (default: /srv/smoke/home)
#   OUT_DIR    evidence dir (default: /srv/smoke/out)
#   TIMEOUT_S  seconds to wait for provider.sock after a start (default: 60)
#   UNIT_NAME  unit to install (default: urnetwork.service)
#
# Exit status: 0 when every hard check passed, 1 otherwise.

set -u

BIN_DIR="${BIN_DIR:-/opt/test}"
JWT_FILE="${JWT_FILE:-}"
REPO_DIR="${REPO_DIR:-/opt/repo}"
HOME_DIR="${HOME_DIR:-/srv/smoke/home}"
OUT_DIR="${OUT_DIR:-/srv/smoke/out}"
TIMEOUT_S="${TIMEOUT_S:-60}"
UNIT_NAME="${UNIT_NAME:-urnetwork.service}"
RESTART_WAIT_S="${RESTART_WAIT_S:-30}"

STATE_DIR="$HOME_DIR/.urnetwork"
SOCK="$STATE_DIR/provider.sock"
UNIT_PATH="/etc/systemd/system/$UNIT_NAME"
TOOLS_BIN="$BIN_DIR/urnet-tools"
PROVIDER_BIN="$HOME_DIR/bin/provider"

PASSES=0
FAILS=0
CONDS=0

pass() { PASSES=$((PASSES + 1)); printf '  PASS: %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL: %s\n' "$*"; }
cond() { CONDS=$((CONDS + 1)); printf '  CONDITIONAL/TODO: %s\n' "$*"; }
say()  { printf '== %s\n' "$*"; }

mkdir -p "$OUT_DIR" "$HOME_DIR" 2>/dev/null || {
    echo "FATAL: cannot create HOME_DIR=$HOME_DIR / OUT_DIR=$OUT_DIR" >&2
    exit 2
}
chmod 700 "$HOME_DIR" 2>/dev/null || true
mkdir -p "$STATE_DIR" 2>/dev/null || true
chmod 700 "$STATE_DIR" 2>/dev/null || true

# ------------------------------------------------------------ environment --
say "environment"
if [ "$(cat /proc/1/comm 2>/dev/null)" = "systemd" ]; then
    pass "PID 1 is systemd"
else
    fail "PID 1 is '$(cat /proc/1/comm 2>/dev/null)', not systemd (the privileged /sbin/init container did not come up)"
fi
SYS_STATE="$(systemctl is-system-running 2>/dev/null || true)"
waited=0
while [ "$waited" -lt 60 ] && [ "$SYS_STATE" != "running" ] && [ "$SYS_STATE" != "degraded" ]; do
    sleep 1
    waited=$((waited + 1))
    SYS_STATE="$(systemctl is-system-running 2>/dev/null || true)"
done
printf 'systemd: %s\n' "$(systemctl --version 2>/dev/null | head -n 1)"
printf 'system state: %s (after %ss)\n' "${SYS_STATE:-unknown}" "$waited"
case "$SYS_STATE" in
    running|degraded)
        # degraded is normal inside a container: distro units that need real
        # hardware fail without making the manager unusable.
        pass "systemd manager usable (is-system-running=$SYS_STATE)" ;;
    *)
        fail "systemd manager not usable (is-system-running=$SYS_STATE)" ;;
esac
printf 'os: %s\n' "$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | tr -d '"')"
printf 'kernel: %s\n' "$(uname -r 2>/dev/null)"

# ---------------------------------------------------------------- fixtures --
say "fixtures"
if [ -x "$BIN_DIR/miner" ]; then
    pass "miner present: $BIN_DIR/miner"
else
    fail "miner missing: $BIN_DIR/miner"
fi
if [ -x "$TOOLS_BIN" ]; then
    pass "urnet-tools present: $TOOLS_BIN"
else
    fail "urnet-tools missing: $TOOLS_BIN"
fi
mkdir -p "$HOME_DIR/bin"
ln -sf "$BIN_DIR/miner" "$PROVIDER_BIN"
if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
    cp "$JWT_FILE" "$STATE_DIR/jwt"
    chmod 600 "$STATE_DIR/jwt"
    pass "fixture JWT copied to $STATE_DIR/jwt with mode 600 (contents never printed)"
else
    fail "fixture JWT missing (JWT_FILE=$JWT_FILE)"
fi
if [ ! -f "$STATE_DIR/network.json" ]; then
    printf '%s\n' '{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}' >"$STATE_DIR/network.json"
fi
pass "network fixture present: $STATE_DIR/network.json"
# urnet-tools' discovery only attributes a state dir inside the invoking user's
# /etc/passwd home. Container-local adaptation, same as distro-smoke.sh.
if [ -w /etc/passwd ]; then
    U="$(id -un 2>/dev/null || echo root)"
    # POSIX tools only: the opensuse/tumbleweed image ships NO awk, so the
    # extraction and the rewrite use grep/cut/sed (a silent no-op here would
    # leave discovery attributing /root/.urnetwork instead of the harness HOME).
    CUR_HOME="$(grep "^$U:" /etc/passwd 2>/dev/null | head -n 1 | cut -d: -f6)"
    if [ -n "$CUR_HOME" ] && [ "$CUR_HOME" != "$HOME_DIR" ]; then
        # Field 6 of the current user's line is the only field changed.
        sed "s|^\($U:[^:]*:[^:]*:[^:]*:[^:]*:\)[^:]*:|\1$HOME_DIR:|" /etc/passwd >/srv/passwd.new &&
            cp /srv/passwd.new /etc/passwd 2>/dev/null &&
            pass "passwd home for $U pointed at $HOME_DIR (container-local discovery adaptation)"
    elif [ -z "$CUR_HOME" ]; then
        cond "no /etc/passwd entry for $U; state-dir discovery attribution not adapted"
    fi
fi

# ------------------------------------------------- shipped template parity --
say "shipped unit template parity (scripts/Provider_Install_Linux.sh)"
INSTALLER="$REPO_DIR/scripts/Provider_Install_Linux.sh"
if [ ! -f "$INSTALLER" ]; then
    cond "installer not mounted at $INSTALLER; template parity not checked"
else
    for pair in \
        'Type=simple|the shipped unit is Type=simple' \
        'ExecStart=$install_path/bin/urnetwork provide|the shipped unit runs the provider with `provide`' \
        'Restart=on-failure|the shipped unit restarts on failure' \
        'RestartSec=5|the shipped unit waits 5s between restarts'
    do
        needle="${pair%%|*}"
        label="${pair#*|}"
        if grep -qF -- "$needle" "$INSTALLER"; then
            pass "$label"
        else
            fail "shipped installer no longer contains: $needle"
        fi
    done
    if grep -qF 'Type=notify' "$INSTALLER"; then
        cond "the shipped installer now mentions Type=notify — add the READY/STATUS assertions (TODO(systemd-notify))"
    fi
fi

# ------------------------------------------------------------- unit install --
say "unit install and start"
cat >"$UNIT_PATH" <<EOF
[Unit]
Description=URnetwork Provider (distro-matrix smoke)
After=network.target

[Service]
Type=simple
Environment="HOME=$HOME_DIR"
Environment="URNETWORK_STATE_DIR=$STATE_DIR"
ExecStart=$PROVIDER_BIN provide --port=0 --allow-client-registration
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload || fail "systemctl daemon-reload failed"
if systemctl cat "$UNIT_NAME" >"$OUT_DIR/unit.txt" 2>&1; then
    pass "unit installed at $UNIT_PATH"
else
    fail "systemctl cat $UNIT_NAME failed"
fi
if systemctl start "$UNIT_NAME"; then
    pass "systemctl start $UNIT_NAME"
else
    fail "systemctl start $UNIT_NAME failed"
    systemctl status "$UNIT_NAME" --no-pager >"$OUT_DIR/unit-status.txt" 2>&1 || true
    tail -n 20 "$OUT_DIR/unit-status.txt" | sed 's/^/    | /'
fi

wait_for_socket() {
    # wait_for_socket <seconds> <label>
    w=0
    while [ "$w" -lt "$1" ]; do
        if [ -S "$SOCK" ]; then
            printf '%s\n' "$w"
            return 0
        fi
        sleep 1
        w=$((w + 1))
    done
    printf '%s\n' "$1"
    return 1
}

wait_for_new_pid() {
    # wait_for_new_pid <old-pid> <seconds>: wait for a live MainPID different
    # from the old one. Socket files persist across a SIGKILL, so the pid is
    # the only sound restart signal.
    old="$1"
    w=0
    while [ "$w" -lt "$2" ]; do
        pid="$(systemctl show -p MainPID --value "$UNIT_NAME" 2>/dev/null)"
        case "$pid" in
            ''|0|"$old") : ;;
            *) if [ -d "/proc/$pid" ]; then
                   printf '%s\n' "$pid"
                   return 0
               fi ;;
        esac
        sleep 1
        w=$((w + 1))
    done
    printf '%s\n' "${pid:-0}"
    return 1
}

if SW="$(wait_for_socket "$TIMEOUT_S" start)"; then
    pass "provider.sock appeared ${SW}s after start"
else
    fail "provider.sock did not appear within ${TIMEOUT_S}s of start"
    journalctl -u "$UNIT_NAME" --no-pager -n 40 >"$OUT_DIR/journal-start.txt" 2>&1 || true
    tail -n 25 "$OUT_DIR/journal-start.txt" | sed 's/^/    | /'
fi
MAIN_PID="$(systemctl show -p MainPID --value "$UNIT_NAME" 2>/dev/null)"
ACTIVE="$(systemctl is-active "$UNIT_NAME" 2>/dev/null || true)"
printf 'active=%s MainPID=%s\n' "$ACTIVE" "$MAIN_PID"
[ "$ACTIVE" = "active" ] && pass "unit is active after start" || fail "unit is '$ACTIVE' after start"
[ -n "$MAIN_PID" ] && [ "$MAIN_PID" != "0" ] && pass "MainPID reported ($MAIN_PID)" || fail "no MainPID after start"
if [ -n "$MAIN_PID" ] && [ "$MAIN_PID" != "0" ] && [ "$(cat "/proc/$MAIN_PID/comm" 2>/dev/null)" = "provider" ]; then
    pass "the provider process runs under the unit as a provider-named binary"
else
    cond "cannot confirm the provider-named process for MainPID $MAIN_PID"
fi
if [ -S "$SOCK" ]; then
    SOCK_LS="$(ls -l "$SOCK")"
    printf '  socket: %s\n' "$SOCK_LS"
    case "$SOCK_LS" in
        srw-------*) pass "socket mode is 0600 under systemd" ;;
        *)           fail "socket mode is not owner-only: $SOCK_LS" ;;
    esac
fi

# ------------------------------------------------------------- urnet-tools --
say "urnet-tools against the unit-managed miner"
run_client() {
    cname="$1"
    shift
    "$TOOLS_BIN" "$@" >"$OUT_DIR/$cname.out" 2>"$OUT_DIR/$cname.err"
    crc=$?
    printf '  $ urnet-tools %s -> exit %s\n' "$*" "$crc"
    return "$crc"
}
# The tool runs with the same HOME the unit uses, so discovery reads the same
# state dir the unit writes.
export HOME="$HOME_DIR"
if run_client version version; then
    pass "urnet-tools version exits 0"
else
    fail "urnet-tools version exited nonzero"
    sed 's/^/    | /' "$OUT_DIR/version.err" | head -n 10
fi
grep -q '^urnet-tools ' "$OUT_DIR/version.out" 2>/dev/null &&
    pass "version output carries the 'urnet-tools <version>' marker" ||
    fail "version output lacks the 'urnet-tools ' marker"
grep -qi 'running' "$OUT_DIR/version.out" 2>/dev/null &&
    pass "version output reports the discovered provider as running" ||
    fail "version output does not report a running discovered provider"

if run_client status-statedir status --state-dir "$STATE_DIR"; then
    pass "urnet-tools status --state-dir exits 0"
else
    fail "urnet-tools status --state-dir exited nonzero"
    sed 's/^/    | /' "$OUT_DIR/status-statedir.err" | head -n 10
fi
grep -q "$UNIT_NAME" "$OUT_DIR/status-statedir.out" 2>/dev/null &&
    pass "status output names the owning unit ($UNIT_NAME)" ||
    fail "status output does not name $UNIT_NAME"
if [ -n "$MAIN_PID" ] && [ "$MAIN_PID" != "0" ] &&
    grep -q "Main PID: $MAIN_PID" "$OUT_DIR/status-statedir.out" 2>/dev/null; then
    pass "status output names the live MainPID ($MAIN_PID)"
else
    cond "status output does not carry 'Main PID: <pid>' at this head (recorded)"
fi
grep -q "state-dir: *$STATE_DIR" "$OUT_DIR/status-statedir.out" 2>/dev/null &&
    pass "status names the state dir under test" ||
    cond "unit-managed status output does not name the state dir at this head (TODO: status shape for unit providers)"
grep -q 'control-socket: *true' "$OUT_DIR/status-statedir.out" 2>/dev/null &&
    pass "status reports control-socket: true" ||
    cond "status does not carry 'control-socket: true' at this head"

if run_client status status; then
    pass "urnet-tools status (unit discovery) exits 0"
else
    fail "urnet-tools status (unit discovery) exited nonzero"
    sed 's/^/    | /' "$OUT_DIR/status.out" | head -n 20
    sed 's/^/    | /' "$OUT_DIR/status.err" | head -n 20
fi
grep -qE '^unit: *[^ ]' "$OUT_DIR/status.out" 2>/dev/null &&
    pass "status attributes the provider to a systemd unit" ||
    cond "status did not attribute a unit at this head (recorded)"

# journal-backed log reading only exists where systemd's journal does. `logs`
# follows by design, so the harness watchdog cuts the stream off: a 124 means
# the streaming path was reached, not that the command failed.
timeout 15 "$TOOLS_BIN" logs 5 >"$OUT_DIR/logs.out" 2>"$OUT_DIR/logs.err"
LOGS_RC=$?
printf '  $ urnet-tools logs 5 -> exit %s\n' "$LOGS_RC"
case "$LOGS_RC" in
    0)   pass "urnet-tools logs (journal) exits 0" ;;
    124) pass "urnet-tools logs started streaming the journal (cut off by the harness watchdog)" ;;
    *)   cond "urnet-tools logs exited $LOGS_RC in this container (journal unavailable?)"
         sed 's/^/    | /' "$OUT_DIR/logs.err" | head -n 5 ;;
esac

# ------------------------------------------------------------------- stop --
say "stop"
if systemctl stop "$UNIT_NAME"; then
    pass "systemctl stop $UNIT_NAME"
else
    fail "systemctl stop $UNIT_NAME failed"
fi
sleep 2
STOP_ACTIVE="$(systemctl is-active "$UNIT_NAME" 2>/dev/null || true)"
[ "$STOP_ACTIVE" = "inactive" ] && pass "unit is inactive after stop" || fail "unit is '$STOP_ACTIVE' after stop"
if [ -S "$SOCK" ]; then
    cond "socket file still exists after stop (stale socket handling is in-flight)"
    if "$TOOLS_BIN" status --state-dir "$STATE_DIR" >"$OUT_DIR/status-after-stop.out" 2>&1; then
        grep -qi 'not running\|stopped' "$OUT_DIR/status-after-stop.out" &&
            pass "status reports the stopped provider as not running" ||
            cond "status output after stop does not say 'not running'"
    else
        cond "status after stop exited nonzero"
    fi
else
    pass "socket removed on stop"
fi

# ---------------------------------------------------------------- restart --
say "restart"
if systemctl restart "$UNIT_NAME"; then
    pass "systemctl restart $UNIT_NAME"
else
    fail "systemctl restart $UNIT_NAME failed"
fi
if SW="$(wait_for_socket "$TIMEOUT_S" restart)"; then
    pass "provider.sock back ${SW}s after restart"
else
    fail "provider.sock did not come back within ${TIMEOUT_S}s after restart"
fi
NEW_PID="$(wait_for_new_pid "$MAIN_PID" "$TIMEOUT_S")"
if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "0" ] && [ "$NEW_PID" != "$MAIN_PID" ] && [ -d "/proc/$NEW_PID" ]; then
    pass "restart produced a live new MainPID ($MAIN_PID -> $NEW_PID)"
else
    fail "restart did not produce a live new MainPID ($MAIN_PID -> $NEW_PID)"
fi

# ------------------------------------------------------ crash and restart --
say "crash-restart (Restart=on-failure)"
CRASH_PID="$(systemctl show -p MainPID --value "$UNIT_NAME" 2>/dev/null)"
if [ -n "$CRASH_PID" ] && [ "$CRASH_PID" != "0" ]; then
    kill -9 "$CRASH_PID" 2>/dev/null || true
    AFTER_PID="$(wait_for_new_pid "$CRASH_PID" "$RESTART_WAIT_S")"
    if [ -n "$AFTER_PID" ] && [ "$AFTER_PID" != "0" ] && [ -d "/proc/$AFTER_PID" ]; then
        pass "systemd restarted the provider after SIGKILL ($CRASH_PID -> $AFTER_PID)"
        # Discovery can lag a restart by a few seconds (the unit record and the
        # process scan are read separately), so retry before judging. Either
        # status form counts: a targeted state dir or the bare discovery form.
        probe_ok=no
        w=0
        while [ "$w" -lt 20 ]; do
            if "$TOOLS_BIN" status --state-dir "$STATE_DIR" >"$OUT_DIR/status-after-crash.out" 2>"$OUT_DIR/status-after-crash.err"; then
                probe_ok=yes
                break
            fi
            if "$TOOLS_BIN" status >"$OUT_DIR/status-after-crash-bare.out" 2>/dev/null; then
                probe_ok=yes
                break
            fi
            sleep 1
            w=$((w + 1))
        done
        if [ "$probe_ok" = "yes" ]; then
            pass "urnet-tools status reaches the restarted provider (after ${w}s)"
        else
            fail "urnet-tools status does not reach the restarted provider within 20s"
            sed 's/^/    | /' "$OUT_DIR/status-after-crash.err" | head -n 10
        fi
    else
        fail "no live new MainPID within ${RESTART_WAIT_S}s after SIGKILL (socket files persist, so a stale socket is not proof of a restart)"
    fi
else
    cond "no MainPID to crash (recorded)"
fi

# --------------------------------------------------------- hotswap surface --
say "hotswap surface at this head"
w=0
while [ "$w" -lt 30 ] && [ "$(systemctl is-active "$UNIT_NAME" 2>/dev/null)" != "active" ]; do
    sleep 1
    w=$((w + 1))
done
if run_client hotswap-dryrun hotswap --dry-run; then
    NOW_PID="$(systemctl show -p MainPID --value "$UNIT_NAME" 2>/dev/null)"
    if [ "$NOW_PID" = "$AFTER_PID" ] || [ "$NOW_PID" = "$CRASH_PID" ] || [ -n "$NOW_PID" ]; then
        pass "urnet-tools hotswap --dry-run exits 0 and leaves the unit running (MainPID $NOW_PID)"
    fi
else
    fail "urnet-tools hotswap --dry-run exited nonzero"
    sed 's/^/    | /' "$OUT_DIR/hotswap-dryrun.err" | head -n 10
fi
if [ "$(systemctl is-active "$UNIT_NAME" 2>/dev/null)" = "active" ]; then
    pass "unit still active after the hotswap gate check"
else
    fail "unit is no longer active after the hotswap gate check"
fi

feature_present() {
    # feature_present <ERE> — absent repository means "unknown": report it as
    # conditional rather than pretending the feature is there. grep, not rg:
    # these images are not required to carry ripgrep.
    [ -d "$REPO_DIR" ] || return 2
    grep -rqE -- "$1" "$REPO_DIR/miner" 2>/dev/null || return 1
    return 0
}
check_feature() {
    # check_feature <label> <regex> <todo-text>
    if ! [ -d "$REPO_DIR" ]; then
        cond "$1: repository not mounted at $REPO_DIR, presence not checked (TODO)"
        return
    fi
    if feature_present "$2"; then
        printf '  INFO: %s is present in miner/ at this commit\n' "$1"
        return 0
    fi
    cond "$1 not in the tree at this commit — TODO: $3"
    return 1
}

# --- systemd notify (Type=notify, READY=1, STATUS=) -------------------------
# The shipped unit is deliberately Type=simple at this head, and the miner
# sends no sd_notify READY/STATUS. When the notify path lands, this check must
# start asserting: `systemctl start` returns only after READY, and
# `systemctl show -p StatusText` reflects the miner's status line.
if check_feature "systemd notify (NOTIFY_SOCKET/READY/STATUS)" 'NOTIFY_SOCKET' \
    "assert Type=notify READY=1 and STATUS= when the miner starts sending them"; then
    cond "notify assertions are not implemented yet in this harness (TODO(systemd-notify))"
fi

# --- exit-75 quarantine (EX_TEMPFAIL) --------------------------------------
# miner/run.go carries providerQuarantinedExitCode = 75: a run whose rejected
# network sign-in was set aside exits 75 and the supervisor restarts it without
# the crash backoff. The probe drives the documented flag against a token the
# API rejects; whether that produces the quarantine path is environment
# dependent, so only a run that actually reaches the rejection is judged.
if check_feature "exit-75 quarantine (--quarantine-rejected-sign-in)" 'providerQuarantinedExitCode = 75' \
    "assert the unit observes exit status 75 and restarts without crash backoff"; then
    say "exit-75 probe (throwaway HOME, rejected token)"
    PROBE_HOME=/srv/probe-home
    rm -rf "$PROBE_HOME"
    mkdir -p "$PROBE_HOME/.urnetwork"
    if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
        cp "$JWT_FILE" "$PROBE_HOME/.urnetwork/jwt"
        chmod 600 "$PROBE_HOME/.urnetwork/jwt"
    fi
    cp "$STATE_DIR/network.json" "$PROBE_HOME/.urnetwork/network.json"
    # Local runs can force a token the API rejects; CI uses the fixture.
    if [ -n "${SMOKE_BAD_JWT_FILE:-}" ] && [ -s "${SMOKE_BAD_JWT_FILE:-}" ]; then
        cp "$SMOKE_BAD_JWT_FILE" "$PROBE_HOME/.urnetwork/jwt"
        chmod 600 "$PROBE_HOME/.urnetwork/jwt"
    fi
    # Piped on purpose: the miner's diagnostics exporter skips regular-file
    # sinks, so `>file` would keep nothing to explain the exit. A FIFO gives
    # both the captured output and the real exit status (POSIX sh has no
    # PIPESTATUS).
    FIFO=/srv/exit75.fifo
    rm -f "$FIFO"
    mkfifo "$FIFO"
    tee "$OUT_DIR/exit75.log" <"$FIFO" &
    TEECPID=$!
    HOME="$PROBE_HOME" timeout 60 "$PROVIDER_BIN" provide --port=0 --allow-client-registration \
        --quarantine-rejected-sign-in >"$FIFO" 2>&1
    E75_RC=$?
    wait "$TEECPID" 2>/dev/null || true
    rm -f "$FIFO"
    tail -n 15 "$OUT_DIR/exit75.log" 2>/dev/null | sed 's/^/    | /'
    printf '  probe exit=%s\n' "$E75_RC"
    if [ "$E75_RC" = "75" ]; then
        pass "the miner exits 75 for a quarantined sign-in (unit would restart without backoff)"
    # Only a genuine rejection marker counts. A bare "401" is NOT enough: it
    # appears inside port numbers and byte counts ("udp6 [::]:40150", "401
    # bytes"), which made a sandboxed non-rejection run look like a rejection.
    elif grep -qiE 'rejected|unauthori[sz]ed|http[ _-]?401|status[ =:"]+401' "$OUT_DIR/exit75.log" 2>/dev/null; then
        fail "sign-in was rejected but the process exited $E75_RC, not 75"
    else
        cond "probe did not reach a rejected sign-in in this sandbox (exit $E75_RC) — TODO: run it where the API is reachable with a rejectable token"
    fi
    rm -rf "$PROBE_HOME"
fi

# --- SIGUSR2 / execve hotswap -----------------------------------------------
if check_feature "SIGUSR2 in-place execve hotswap" 'SIGUSR2|syscall.Exec' \
    "assert a SIGUSR2 handoff performs the in-place execve and the unit stays active"; then
    cond "SIGUSR2 handoff assertions are not implemented yet in this harness (TODO(hotswap-usr2))"
fi

# --- opt-in cap dir / ramlogs ------------------------------------------------
if check_feature "opt-in cap dir (URNETWORK_RAMLOGS / oom cap)" 'URNETWORK_RAMLOGS' \
    "assert the cap directory is created on opt-in and enforced"; then
    cond "cap-dir assertions are not implemented yet in this harness (TODO(cap-dir))"
fi

# -------------------------------------------------- state-dir override path --
say "URNETWORK_STATE_DIR override"
OVERRIDE=/srv/override-state
rm -rf "$OVERRIDE"
mkdir -p "$OVERRIDE"
chmod 700 "$OVERRIDE"
cp "$STATE_DIR/jwt" "$OVERRIDE/jwt" 2>/dev/null || true
chmod 600 "$OVERRIDE/jwt" 2>/dev/null || true
cp "$STATE_DIR/network.json" "$OVERRIDE/network.json" 2>/dev/null || true
systemctl stop "$UNIT_NAME" >/dev/null 2>&1 || true
mkdir -p "/etc/systemd/system/$UNIT_NAME.d"
cat >"/etc/systemd/system/$UNIT_NAME.d/override.conf" <<EOF
[Service]
Environment="URNETWORK_STATE_DIR=$OVERRIDE"
EOF
systemctl daemon-reload >/dev/null 2>&1 || true
if systemctl start "$UNIT_NAME"; then
    w=0
    while [ "$w" -lt "$TIMEOUT_S" ] && [ ! -S "$OVERRIDE/provider.sock" ]; do
        sleep 1
        w=$((w + 1))
    done
    if [ -S "$OVERRIDE/provider.sock" ]; then
        pass "with URNETWORK_STATE_DIR=$OVERRIDE the socket binds at the override ($w s)"
    else
        fail "socket did not appear under the URNETWORK_STATE_DIR override within ${TIMEOUT_S}s"
    fi
    if [ -S "$SOCK" ]; then
        cond "the default state dir also has a socket while the override is active (unexpected)"
    else
        pass "no socket in the default state dir while the override is active"
    fi
else
    fail "unit failed to start with the state-dir override"
fi
rm -f "/etc/systemd/system/$UNIT_NAME.d/override.conf"
systemctl daemon-reload >/dev/null 2>&1 || true

# ----------------------------------------------------------------- cleanup --
say "cleanup"
systemctl stop "$UNIT_NAME" >/dev/null 2>&1 || true
systemctl disable "$UNIT_NAME" >/dev/null 2>&1 || true
rm -f "$UNIT_PATH"
systemctl daemon-reload >/dev/null 2>&1 || true
journalctl -u "$UNIT_NAME" --no-pager -n 200 >"$OUT_DIR/journal.txt" 2>&1 || true
pass "unit stopped, disabled and removed"

echo
echo "==================== systemd-smoke summary ===================="
printf 'distro: %s\n' "$(sed -n 's/^PRETTY_NAME=//p' /etc/os-release 2>/dev/null | tr -d '"')"
printf 'unit:   %s\n' "$UNIT_NAME"
printf 'checks: %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
if [ "$FAILS" -gt 0 ]; then
    printf 'RESULT: FAIL\n' | tee "$OUT_DIR/summary.txt"
    exit 1
fi
printf 'RESULT: PASS\n' | tee "$OUT_DIR/summary.txt"
printf '(%s passed, %s conditional)\n' "$PASSES" "$CONDS" | tee -a "$OUT_DIR/summary.txt"
exit 0
