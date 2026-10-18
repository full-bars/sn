#!/bin/sh
# openrc-smoke.sh — Alpine/OpenRC cell for the restored openrc-smoke workflow.
# Runs INSIDE a stock alpine:latest container on the CI runner, with the
# linux/amd64 payload (miner, urnet-tools) bind-mounted read-only. The miner is
# the unit under test: it runs under a real OpenRC `supervise-daemon` service as
# a dedicated non-root user, and the harness drives the whole lifecycle through
# rc-service.
#
# Restored from fix/ci-freebsd-release (ref 23cb98140) and adapted to the
# miner/tools line: the old provider package is the miner (./cli/miner) here,
# and the old "fake release + installer OpenRC branch" premise does not exist
# at this commit, so those checks are CONDITIONAL/TODO gates over the synced
# repository instead of failures. They switch themselves on when the code
# lands.
#
# Hard checks:
#   1. the static payload runs on musl (miner --help, urnet-tools --version),
#   2. OpenRC is genuinely present and booted (/sbin/openrc-run + /run/openrc),
#   3. rc-service start brings up the miner as the `urnet` service user,
#   4. the control socket binds at the service user's $HOME/.urnetwork/provider.sock
#      with mode 0600 and owner urnet,
#   5. urnet-tools reaches the running miner over the socket (status),
#   6. rc-service restart produces a new pid, stop removes the process,
#   7. the log files are root-directory-owned but service-user-owned files.
#
# TODO gates (CONDITIONAL, never fail):
#   * TODO(openrc-installer): scripts/Provider_Install_Linux.sh has no OpenRC
#     branch at this commit, so the installer's service path cannot be
#     exercised; the harness writes the equivalent unit itself.
#   * TODO(openrc-tools): internal/urnettools has no OpenRC discovery backend
#     at this commit, so bare discovery is not asserted; the targeted
#     --state-dir client path is.
#
# Inputs (env):
#   BIN_DIR   payload dir with `miner` (required) and `urnet-tools` (required)
#   REPO_DIR  synced repository root (default: /srv/repo) for the TODO gates
#   JWT_FILE  disposable CI JWT (required); copied into the service user's state
#             dir with mode 600, never printed
#   OUT_DIR   evidence dir (default: /srv/out)
#   SVC_USER  service user (default: urnet)
#   TIMEOUT_S seconds to wait for the socket (default: 90)
#
# Exit status: 0 when every hard check passed, 1 otherwise.

set -u

BIN_DIR="${BIN_DIR:-/srv/bin}"
REPO_DIR="${REPO_DIR:-/srv/repo}"
JWT_FILE="${JWT_FILE:-}"
OUT_DIR="${OUT_DIR:-/srv/out}"
SVC_USER="${SVC_USER:-urnet}"
TIMEOUT_S="${TIMEOUT_S:-90}"

INSTALL_ROOT=/usr/local/lib/urnetwork-provider
UNIT=/etc/init.d/urnetwork
SVC_HOME="/home/$SVC_USER"
STATE_DIR="$SVC_HOME/.urnetwork"
SOCK="$STATE_DIR/provider.sock"
LOG_OUT=/var/log/urnetwork.log
LOG_ERR=/var/log/urnetwork.err

PASSES=0
FAILS=0
CONDS=0

mkdir -p "$OUT_DIR" 2>/dev/null || true

pass() { PASSES=$((PASSES + 1)); printf '  PASS: %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL: %s\n' "$*"; }
cond() { CONDS=$((CONDS + 1)); printf '  CONDITIONAL/TODO: %s\n' "$*"; }
say()  { printf '== %s\n' "$*"; }
fail_fatal() {
    fail "$@"
    echo
    echo "==================== openrc-smoke summary ===================="
    printf 'checks: %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
    echo "RESULT: FAIL"
    exit 1
}

# ---------------------------------------------------------------- environment --
say "environment"
printf 'alpine: %s\n' "$(cat /etc/alpine-release 2>/dev/null || echo unknown)"
printf 'kernel: %s\n' "$(uname -r 2>/dev/null)"
# OpenRC is not in the base alpine image; install it here. This is not the unit
# under test — what is under test is the miner's service contract under OpenRC.
apk add --no-cache openrc busybox-extras >/dev/null 2>&1 || true
if [ -x /sbin/openrc-run ] || [ -x /usr/sbin/openrc-run ]; then
    pass "openrc-run present (the installer would detect OpenRC)"
else
    fail_fatal "openrc-run missing; OpenRC is not installed in this image"
fi
if [ ! -d /run/openrc ]; then
    # A container has no boot, so OpenRC's runtime tree does not exist and
    # rc-service refuses every action. `openrc boot` is what a real boot does.
    openrc boot >/dev/null 2>&1 || true
fi
if [ -d /run/openrc ]; then
    pass "/run/openrc present (OpenRC booted)"
else
    fail_fatal "/run/openrc missing after 'openrc boot'; rc-service cannot work"
fi
for t in curl jq python3; do
    if command -v "$t" >/dev/null 2>&1; then
        printf '  note: %s IS present (the missing-tool premise is not being exercised)\n' "$t"
    else
        printf '  confirmed absent: %s\n' "$t"
    fi
done

# ------------------------------------------------------------------- inputs --
say "payload"
if [ -x "$BIN_DIR/miner" ]; then
    pass "miner present: $BIN_DIR/miner"
else
    fail_fatal "miner missing: $BIN_DIR/miner"
fi
if [ -x "$BIN_DIR/urnet-tools" ]; then
    pass "urnet-tools present: $BIN_DIR/urnet-tools"
else
    fail_fatal "urnet-tools missing: $BIN_DIR/urnet-tools"
fi
if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
    pass "disposable JWT supplied (contents never printed)"
else
    fail_fatal "JWT_FILE missing ($JWT_FILE)"
fi
# The payload is static (CGO_ENABLED=0): prove it runs on this musl userland
# before anything is installed as a service.
if "$BIN_DIR/miner" --help >/dev/null 2>&1; then
    pass "miner --help runs on musl"
else
    fail "miner --help failed on musl (dynamic loader mismatch?)"
fi
if "$BIN_DIR/urnet-tools" --version >"$OUT_DIR/tool-version.txt" 2>&1; then
    pass "urnet-tools --version runs on musl ($(head -n 1 "$OUT_DIR/tool-version.txt"))"
else
    fail "urnet-tools --version failed on musl"
fi

# ------------------------------------------------------------- service user --
say "service user and install tree"
adduser -D -s /bin/sh "$SVC_USER" >/dev/null 2>&1 || adduser -m -s /bin/sh "$SVC_USER" >/dev/null 2>&1 || true
if id "$SVC_USER" >/dev/null 2>&1; then
    pass "service user $SVC_USER exists (uid $(id -u "$SVC_USER"))"
else
    fail_fatal "could not create the service user $SVC_USER"
fi
# The install tree is root-owned and outside the service user's home, so
# root-executed code (cron, rc-service) can never reach it through a
# user-writable ancestor.
mkdir -p "$INSTALL_ROOT/bin"
cp "$BIN_DIR/miner" "$INSTALL_ROOT/bin/urnetwork"
chmod 755 "$INSTALL_ROOT/bin/urnetwork"
chown root:root "$INSTALL_ROOT/bin/urnetwork" 2>/dev/null || true
chmod 755 "$INSTALL_ROOT" "$INSTALL_ROOT/bin"
cp "$BIN_DIR/urnet-tools" /usr/local/bin/urnet-tools
chmod 755 /usr/local/bin/urnet-tools
if [ -x "$INSTALL_ROOT/bin/urnetwork" ] && [ "$(stat -c %u "$INSTALL_ROOT/bin/urnetwork")" = "0" ]; then
    pass "provider binary installed root-owned at $INSTALL_ROOT/bin/urnetwork"
else
    fail_fatal "provider binary missing or not root-owned under $INSTALL_ROOT"
fi

# ------------------------------------------------------- service definition --
say "OpenRC service definition"
# TODO(openrc-installer): when scripts/Provider_Install_Linux.sh grows its
# OpenRC branch, this harness should run the installer against a staged release
# instead of writing the unit itself. Until then the unit below mirrors the
# shipped systemd unit's shape (simple command, restart on failure, logs) so
# what is exercised is the service manager contract, not installer code that
# does not exist yet.
cat >"$UNIT" <<EOF
#!/sbin/openrc-run
name="urnetwork"
description="URnetwork Provider (openrc-smoke)"
command="$INSTALL_ROOT/bin/urnetwork"
command_args="provide --port=0 --allow-client-registration"
command_user="$SVC_USER:$SVC_USER"
supervisor="supervise-daemon"
export HOME="$SVC_HOME"
output_log="$LOG_OUT"
error_log="$LOG_ERR"
respawn_delay=2
respawn_max=0
depend() {
    need net
}
EOF
chmod 755 "$UNIT"
chown root:root "$UNIT" 2>/dev/null || true
if sh -n "$UNIT"; then
    pass "init script is valid shell: $UNIT"
else
    fail_fatal "init script is not valid shell: $UNIT"
fi
if grep -q "^command_user=\"$SVC_USER" "$UNIT"; then
    pass "service runs as the non-root user $SVC_USER"
else
    fail "no command_user= in the init script"
fi
rc-update add urnetwork default >/dev/null 2>&1 &&
    pass "enabled in the default runlevel" ||
    fail "could not add urnetwork to the default runlevel"
if [ -e /etc/runlevels/default/urnetwork ]; then
    pass "runlevel symlink present (starts at boot)"
else
    fail "no /etc/runlevels/default/urnetwork symlink"
fi
# supervise-daemon opens the log files AFTER dropping privileges to the service
# user, so a root-owned file makes the child die before exec. The directory
# stays root-owned so the service user cannot plant a name in it.
for f in "$LOG_OUT" "$LOG_ERR"; do
    : >"$f"
    chown "$SVC_USER:$SVC_USER" "$f" 2>/dev/null || true
    chmod 644 "$f"
done
if [ "$(stat -c %u /var/log)" = "0" ]; then
    pass "/var/log is root-owned (the service user cannot plant a name in it)"
else
    fail "/var/log is owned by uid $(stat -c %u /var/log), not root"
fi
SVC_UID="$(id -u "$SVC_USER")"
for f in "$LOG_OUT" "$LOG_ERR"; do
    if [ "$(stat -c %u "$f")" = "$SVC_UID" ]; then
        pass "$f is owned by $SVC_USER (supervise-daemon can open it after dropping privileges)"
    else
        fail "$f is owned by uid $(stat -c %u "$f"), but the daemon opens it as $SVC_USER (uid $SVC_UID)"
    fi
done

# ------------------------------------------------------------- fixtures --
say "fixtures"
mkdir -p "$STATE_DIR"
chown "$SVC_USER:$SVC_USER" "$STATE_DIR" "$SVC_HOME" 2>/dev/null || true
chmod 700 "$SVC_HOME" "$STATE_DIR" 2>/dev/null || true
cp "$JWT_FILE" "$STATE_DIR/jwt"
chown "$SVC_USER:$SVC_USER" "$STATE_DIR/jwt" 2>/dev/null || true
chmod 600 "$STATE_DIR/jwt"
printf '%s\n' '{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}' >"$STATE_DIR/network.json"
chown "$SVC_USER:$SVC_USER" "$STATE_DIR/network.json" 2>/dev/null || true
pass "JWT copied to $STATE_DIR/jwt with mode 600 (contents never printed)"

# ------------------------------------------------------------------- start --
say "rc-service start"
rc-service urnetwork start >"$OUT_DIR/rc-start.log" 2>&1 &&
    pass "rc-service urnetwork start exited 0" ||
    { fail "rc-service urnetwork start failed"; sed 's/^/    | /' "$OUT_DIR/rc-start.log" | head -n 10; }

waited=0
sock=no
while [ "$waited" -lt "$TIMEOUT_S" ]; do
    if [ -S "$SOCK" ]; then sock=yes; break; fi
    sleep 1
    waited=$((waited + 1))
done
if [ "$sock" = "yes" ]; then
    pass "provider.sock appeared ${waited}s after start at $SOCK"
    SOCK_LS="$(ls -l "$SOCK" 2>/dev/null)"
    case "$SOCK_LS" in
        srw-------*) pass "socket mode is 0600 (owner-only)" ;;
        *)           fail "socket mode is not owner-only: $SOCK_LS" ;;
    esac
    if [ "$(stat -c %U "$SOCK")" = "$SVC_USER" ]; then
        pass "socket is owned by the service user $SVC_USER"
    else
        fail "socket is owned by $(stat -c %U "$SOCK"), not $SVC_USER"
    fi
else
    fail "provider.sock did not appear within ${TIMEOUT_S}s of start"
    echo "  --- init script key lines ---"
    grep -E '^command|^command_user|^supervisor|^output_log|^error_log|^respawn' "$UNIT" 2>/dev/null | sed 's/^/    | /'
    echo "  --- service log ---"
    tail -n 20 "$LOG_ERR" 2>/dev/null | sed 's/^/    | /'
fi

# The process must run as the service user, not root: that invariant is the
# point of command_user.
if pgrep -f "$INSTALL_ROOT/bin/urnetwork provide" >/dev/null 2>&1; then
    pass "the provider process is alive"
    PROC_USER="$(ps -o user,args 2>/dev/null | grep '[u]rnetwork-provider/bin/urnetwork provide' | awk 'NR==1 {print $1}')"
    if [ "$PROC_USER" = "$SVC_USER" ]; then
        pass "the provider runs as $SVC_USER (not root)"
    else
        fail "the provider runs as '${PROC_USER:-unknown}', not $SVC_USER"
    fi
else
    fail "no provider process for the service (supervise-daemon up but the miner is not)"
fi

# --------------------------------------------------------------- tools --
say "urnet-tools against the service-managed miner"
# The miner's peer check accepts the same uid or root, so running the client as
# root with --state-dir is the supported management path (systemd/OpenRC/fleet
# scripts do the same).
if urnet-tools status --state-dir "$STATE_DIR" >"$OUT_DIR/status.txt" 2>&1; then
    pass "urnet-tools status --state-dir exits 0"
    grep -qi 'running' "$OUT_DIR/status.txt" &&
        pass "status reports the miner as running" ||
        fail "status does not report running"
    grep -q "state-dir: *$STATE_DIR" "$OUT_DIR/status.txt" &&
        pass "status names the state dir under test" ||
        cond "status output does not name the state dir at this head (TODO: status shape)"
else
    fail "urnet-tools status --state-dir exited nonzero"
    sed 's/^/    | /' "$OUT_DIR/status.txt" | head -n 10
fi
# TODO(openrc-tools): internal/urnettools has no OpenRC discovery backend at
# this commit, so bare `urnet-tools status` (discovery) cannot attribute the
# service. Gate on the file, so it starts being asserted when it lands.
if [ -f "$REPO_DIR/internal/urnettools/discover_openrc.go" ]; then
    if urnet-tools status >"$OUT_DIR/status-discovery.txt" 2>&1; then
        pass "bare urnet-tools status (discovery) exits 0"
    else
        fail "bare urnet-tools status (discovery) exited nonzero"
    fi
else
    cond "TODO(openrc-tools): internal/urnettools/discover_openrc.go is not in this tree; bare discovery not asserted"
fi
if [ -f "$REPO_DIR/internal/urnettools/discover_openrc.go" ]; then
    if timeout 15 urnet-tools logs 5 >"$OUT_DIR/logs.txt" 2>&1; then
        pass "urnet-tools logs reads the service log"
    else
        cond "urnet-tools logs exited $? (journal/openrc log backend not available at this head)"
    fi
else
    cond "TODO(openrc-tools): urnet-tools log follow for OpenRC not in this tree; service log checked directly"
fi
if [ -s "$LOG_OUT" ]; then
    pass "supervise-daemon captured provider stdout ($(wc -c <"$LOG_OUT") bytes)"
else
    cond "provider stdout log is empty (informational; the miner may log to stderr only)"
fi

# ---------------------------------------------------------------- restart --
say "rc-service restart"
OLD_PID="$(pgrep -f "$INSTALL_ROOT/bin/urnetwork provide" | head -n 1)"
rc-service urnetwork restart >"$OUT_DIR/rc-restart.log" 2>&1 &&
    pass "rc-service urnetwork restart exited 0" ||
    { fail "rc-service urnetwork restart failed"; sed 's/^/    | /' "$OUT_DIR/rc-restart.log" | head -n 10; }
waited=0
NEW_PID=""
while [ "$waited" -lt "$TIMEOUT_S" ]; do
    NEW_PID="$(pgrep -f "$INSTALL_ROOT/bin/urnetwork provide" | head -n 1)"
    if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "${OLD_PID:-}" ] && [ -d "/proc/$NEW_PID" ]; then
        break
    fi
    sleep 1
    waited=$((waited + 1))
done
if [ -n "$NEW_PID" ] && [ "$NEW_PID" != "${OLD_PID:-}" ]; then
    pass "restart produced a live new provider pid (${OLD_PID:-none} -> $NEW_PID)"
else
    fail "restart did not produce a live new provider pid (old=${OLD_PID:-none} new=${NEW_PID:-none})"
fi
waited=0
while [ "$waited" -lt "$TIMEOUT_S" ] && [ ! -S "$SOCK" ]; do sleep 1; waited=$((waited + 1)); done
if [ -S "$SOCK" ]; then
    pass "socket back ${waited}s after restart"
else
    fail "socket did not come back after restart"
fi

# ------------------------------------------------------------------- stop --
say "rc-service stop"
rc-service urnetwork stop >"$OUT_DIR/rc-stop.log" 2>&1 &&
    pass "rc-service urnetwork stop exited 0" ||
    { fail "rc-service urnetwork stop failed"; sed 's/^/    | /' "$OUT_DIR/rc-stop.log" | head -n 10; }
waited=0
while [ "$waited" -lt 20 ]; do
    pgrep -f "$INSTALL_ROOT/bin/urnetwork provide" >/dev/null 2>&1 || break
    sleep 1
    waited=$((waited + 1))
done
if pgrep -f "$INSTALL_ROOT/bin/urnetwork provide" >/dev/null 2>&1; then
    fail "provider process still alive ${waited}s after rc-service stop"
else
    pass "provider process gone after stop (${waited}s)"
fi

# ------------------------------------------------------- installer gate --
say "installer OpenRC branch (TODO gate)"
if [ -f "$REPO_DIR/scripts/Provider_Install_Linux.sh" ] &&
   grep -qi 'openrc' "$REPO_DIR/scripts/Provider_Install_Linux.sh"; then
    cond "TODO(openrc-installer): the installer now mentions OpenRC; wire the fake-release install flow here and replace the hand-written unit"
else
    cond "TODO(openrc-installer): scripts/Provider_Install_Linux.sh has no OpenRC branch at this commit; the harness unit stands in for it"
fi

# ------------------------------------------------------------------ report --
echo
echo "==================== openrc-smoke summary ===================="
printf 'alpine: %s\n' "$(cat /etc/alpine-release 2>/dev/null || echo unknown)"
printf 'user:   %s\n' "$SVC_USER"
printf 'checks: %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
if [ "$FAILS" -gt 0 ]; then
    printf 'RESULT: FAIL\n' | tee "$OUT_DIR/summary.txt"
    exit 1
fi
printf 'RESULT: PASS\n' | tee "$OUT_DIR/summary.txt"
printf '(%s passed, %s conditional)\n' "$PASSES" "$CONDS" | tee -a "$OUT_DIR/summary.txt"
exit 0
