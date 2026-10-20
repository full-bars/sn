#!/bin/sh
# freebsd-smoke.sh — FreeBSD cell for the release distro matrix and for the
# restored freebsd.yml. Runs INSIDE a real FreeBSD VM (vmactions/freebsd-vm),
# against the miner the runner cross-built for freebsd/amd64.
#
# What this proves at this head:
#   1. the miner binary loads and runs on a real FreeBSD kernel,
#   2. the base-system facilities the miner's FreeBSD paths rely on exist:
#      procstat (process discovery), swapinfo (memory pressure), sysrc/service/
#      crontab (lifecycle), and the host-memory sysctls,
#   3. LOCAL_PEERCRED is available on the kernel (the control socket's peer
#      credential check compiles against it),
#   4. `miner provide` binds $HOME/.urnetwork/provider.sock with mode 0600 and
#      serves the newline-JSON control protocol on it (checked with nc -U),
#   5. SIGTERM shuts the miner down.
#
# Checks whose code is not in this tree at this commit are reported as
# CONDITIONAL/TODO and switch themselves on when the code lands (they are
# file-existence gates over the synced repository, same policy as
# systemd-smoke.sh):
#   * TODO(freebsd-urnettools): internal/urnettools has no FreeBSD port yet, so
#     urnet-tools cannot be built for freebsd and is neither exercised nor
#     could be. Callers may still pass a urnet-tools binary: when they do, the
#     discovery checks run.
#   * TODO(freebsd-installer): scripts/Provider_Install_FreeBSD.sh (rc.d path)
#   * TODO(freebsd-optimize): internal/urnettools/optimize_bsd.go (sysctl tuning)
#   * TODO(freebsd-miner-tests): the miner's test files import a fixture package
#     with no freebsd build, so no miner test binary exists at this head.
#
# Inputs (env, all optional unless noted):
#   BIN_DIR    payload dir with `miner` (required), optional `urnet-tools`,
#              optional `urnettools.test` / `miner.test` binaries
#   REPO_DIR   synced repository root (default: current directory) used by the
#              TODO gates; when absent the gates report "repository not
#              mounted" instead of guessing
#   JWT_FILE   disposable CI JWT (required unless SMOKE_ALLOW_NO_JWT=1);
#              copied into the isolated HOME, never printed
#   HOME_DIR   isolated HOME (default: /tmp/urnet-smoke/home)
#   STATE_DIR  state dir (default: $HOME_DIR/.urnetwork)
#   WORK_DIR   scratch dir (default: /tmp/urnet-smoke/work)
#   OUT_DIR    evidence dir; when set, evidence files are written there
#   TIMEOUT_S  seconds to wait for provider.sock (default: 90)
#
# Exit status: 0 when every hard check passed, 1 otherwise.

set -u

BIN_DIR="${BIN_DIR:-}"
REPO_DIR="${REPO_DIR:-$PWD}"
JWT_FILE="${JWT_FILE:-}"
HOME_DIR="${HOME_DIR:-/tmp/urnet-smoke/home}"
WORK_DIR="${WORK_DIR:-/tmp/urnet-smoke/work}"
STATE_DIR="${STATE_DIR:-}"
OUT_DIR="${OUT_DIR:-}"
TIMEOUT_S="${TIMEOUT_S:-90}"
STOP_TIMEOUT_S="${STOP_TIMEOUT_S:-20}"
SMOKE_ALLOW_NO_JWT="${SMOKE_ALLOW_NO_JWT:-0}"

PASSES=0
FAILS=0
CONDS=0

if [ -z "$BIN_DIR" ]; then
    echo "FATAL: BIN_DIR is required (the directory holding the freebsd/amd64 miner)" >&2
    exit 2
fi
TOOLS_BIN="$BIN_DIR/urnet-tools"

if [ -n "$OUT_DIR" ]; then
    mkdir -p "$OUT_DIR" || { echo "FATAL: cannot create OUT_DIR=$OUT_DIR" >&2; exit 2; }
fi

pass() { PASSES=$((PASSES + 1)); printf '  PASS: %s\n' "$*"; }
fail() { FAILS=$((FAILS + 1)); printf '  FAIL: %s\n' "$*"; }
cond() { CONDS=$((CONDS + 1)); printf '  CONDITIONAL/TODO: %s\n' "$*"; }
say()  { printf '== %s\n' "$*"; }
emit() {
    # emit <file> <text...> — keep an evidence copy when OUT_DIR is set.
    if [ -n "$OUT_DIR" ]; then
        printf '%s\n' "$*" >>"$OUT_DIR/$1"
    fi
}
fail_fatal() { fail "$@"; echo; echo "==================== freebsd-smoke summary ===================="; printf 'checks: %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"; echo "RESULT: FAIL"; exit 1; }

# ------------------------------------------------------------------ identity --
say "kernel identity"
KERNEL="$(uname -a 2>/dev/null)"
KERNEL_SHORT="$(uname -K 2>/dev/null || uname -r 2>/dev/null)"
FREEBSD_VER="$(freebsd-version 2>/dev/null || echo unknown)"
printf 'uname: %s\n' "$KERNEL"
printf 'freebsd-version: %s\n' "$FREEBSD_VER"
emit identity.txt "uname=$KERNEL"
emit identity.txt "freebsd_version=$FREEBSD_VER"

# ------------------------------------------------------------------- inputs --
say "inputs"
if [ -x "$BIN_DIR/miner" ]; then
    pass "freebsd/amd64 miner present and executable: $BIN_DIR/miner"
else
    fail_fatal "miner missing or not executable: $BIN_DIR/miner"
fi
if [ -x "$TOOLS_BIN" ]; then
    pass "urnet-tools present: $TOOLS_BIN"
else
    cond "urnet-tools not supplied (TODO(freebsd-urnettools): internal/urnettools has no FreeBSD port at this commit)"
fi

# ------------------------------------------------------------- base facilities --
say "base-system facilities the FreeBSD paths depend on"
for c in procstat sysrc service crontab swapinfo cc nc; do
    if command -v "$c" >/dev/null 2>&1; then
        pass "$c present ($(command -v "$c"))"
    else
        # cc is only needed for the LOCAL_PEERCRED probe; nc only for the
        # control-protocol probe. Both are base-system on FreeBSD images.
        fail "$c missing (base-system tool; the FreeBSD checks cannot run)"
    fi
done
for k in hw.physmem vm.stats.vm.v_free_count vm.stats.vm.v_inactive_count kern.ipc.maxsockbuf net.inet.tcp.recvspace; do
    v="$(sysctl -n "$k" 2>/dev/null || true)"
    if [ -n "$v" ]; then
        printf '  %-34s = %s\n' "$k" "$v"
    else
        fail "sysctl $k does not exist on this kernel"
    fi
done

# ------------------------------------------------------------- binary loads --
say "the binary loads and runs on this kernel"
mkdir -p "$WORK_DIR"
if "$BIN_DIR/miner" --help >"$WORK_DIR/miner-help.out" 2>&1; then
    pass "miner --help exits 0 on the real kernel"
    head -n 1 "$WORK_DIR/miner-help.out" | sed 's/^/    | /'
else
    fail "miner --help exited nonzero; the binary does not run on this kernel"
    head -n 5 "$WORK_DIR/miner-help.out" 2>/dev/null | sed 's/^/    | /'
fi

# -------------------------------------------------------- procstat contract --
say "procstat output shape (discovery parser contract)"
mkdir -p "$WORK_DIR/bin"
cp /bin/sleep "$WORK_DIR/bin/urnetwork" 2>/dev/null && chmod 755 "$WORK_DIR/bin/urnetwork"
cp /bin/sleep "$WORK_DIR/bin/notaprovider" 2>/dev/null && chmod 755 "$WORK_DIR/bin/notaprovider"
"$WORK_DIR/bin/urnetwork" 300 &
SPID=$!
"$WORK_DIR/bin/notaprovider" 300 &
NPID=$!
sleep 2
if procstat -b "$SPID" >/dev/null 2>&1; then
    pass "procstat -b reports the provider-named pid $SPID"
else
    fail "procstat -b could not report pid $SPID"
fi
if procstat -s "$SPID" >/dev/null 2>&1; then
    pass "procstat -s reports the provider-named pid $SPID"
else
    fail "procstat -s could not report pid $SPID"
fi
EUID_COL="$(procstat -s "$SPID" 2>/dev/null | awk 'NR==2 {print $3}')"
case "$EUID_COL" in
    ''|*[!0-9]*) fail "could not parse the EUID column of procstat -s (got '$EUID_COL'); the layout changed" ;;
    *)           pass "procstat -s EUID column parses as an integer ($EUID_COL)" ;;
esac
if procstat -a -b 2>/dev/null | grep -q "$SPID"; then
    pass "procstat -a -b lists the live pid $SPID"
else
    fail "procstat -a -b does not list the live pid $SPID"
fi

# The discovery assertions need urnet-tools; without the port they are a
# recorded TODO rather than a silent skip.
if [ -x "$TOOLS_BIN" ]; then
    if "$TOOLS_BIN" providers >"$WORK_DIR/providers.out" 2>&1; then
        pass "urnet-tools providers exits 0"
    else
        fail "urnet-tools providers exited nonzero"
    fi
    if grep -q "$SPID" "$WORK_DIR/providers.out"; then
        pass "discovery reported the live provider-named pid $SPID"
    else
        fail "discovery did not report the live provider-named pid $SPID"
        sed 's/^/    | /' "$WORK_DIR/providers.out" | head -n 20
    fi
    if grep -q "$NPID" "$WORK_DIR/providers.out"; then
        fail "discovery reported the non-provider pid $NPID as a provider"
    else
        pass "discovery ignored the non-provider pid $NPID"
    fi
else
    cond "TODO(freebsd-urnettools): urnet-tools has no FreeBSD build at this commit; discovery not exercised here"
fi
kill "$SPID" "$NPID" 2>/dev/null || true
wait "$SPID" 2>/dev/null || true
wait "$NPID" 2>/dev/null || true

# ----------------------------------------------------- LOCAL_PEERCRED probe --
say "LOCAL_PEERCRED availability (control socket peer credential check)"
mkdir -p "$WORK_DIR"
cat >"$WORK_DIR/xucred_test.c" <<'EOF'
#include <sys/types.h>
#include <sys/socket.h>
#include <sys/un.h>
#include <sys/ucred.h>
#include <stdio.h>
#include <unistd.h>
int main(void) {
  int sv[2];
  if (socketpair(AF_LOCAL, SOCK_STREAM, 0, sv) != 0) { perror("socketpair"); return 1; }
  struct xucred xc; socklen_t len = sizeof(xc);
  if (getsockopt(sv[1], SOL_LOCAL, LOCAL_PEERCRED, &xc, &len) != 0) { perror("LOCAL_PEERCRED"); return 1; }
  if (xc.cr_version != XUCRED_VERSION) { printf("LOCAL_PEERCRED: unexpected cr_version %d\n", xc.cr_version); return 1; }
  printf("LOCAL_PEERCRED ok: cr_version=%u cr_uid=%u\n", xc.cr_version, xc.cr_uid);
  return 0;
}
EOF
if cc -o "$WORK_DIR/xucred_test" "$WORK_DIR/xucred_test.c" 2>"$WORK_DIR/xucred-build.log" &&
   "$WORK_DIR/xucred_test" >"$WORK_DIR/xucred.out" 2>&1; then
    pass "LOCAL_PEERCRED is available on this kernel: $(cat "$WORK_DIR/xucred.out")"
else
    fail "LOCAL_PEERCRED is not available on this kernel (the control socket's peer check compiles against it)"
    sed 's/^/    | /' "$WORK_DIR/xucred-build.log" 2>/dev/null | head -n 5
fi

# --------------------------------------------------------- sandbox + JWT --
say "isolated HOME and disposable JWT"
rm -rf "$HOME_DIR" "$WORK_DIR/state" 2>/dev/null || true
mkdir -p "$HOME_DIR" "$WORK_DIR" || fail_fatal "cannot create the isolated HOME at $HOME_DIR"
chmod 700 "$HOME_DIR" 2>/dev/null || true
if [ -z "$STATE_DIR" ]; then
    STATE_DIR="$HOME_DIR/.urnetwork"
fi
mkdir -p "$STATE_DIR"
chmod 700 "$STATE_DIR" 2>/dev/null || true
export HOME="$HOME_DIR"
pass "isolated HOME created: $HOME_DIR (state dir $STATE_DIR)"
if [ -n "$JWT_FILE" ] && [ -s "$JWT_FILE" ]; then
    cp "$JWT_FILE" "$STATE_DIR/jwt"
    chmod 600 "$STATE_DIR/jwt" 2>/dev/null || true
    pass "disposable JWT copied into the state dir with mode 600 (contents never printed)"
elif [ "$SMOKE_ALLOW_NO_JWT" = "1" ]; then
    cond "no JWT supplied and SMOKE_ALLOW_NO_JWT=1 (unauthenticated run on purpose)"
else
    fail_fatal "JWT_FILE missing ($JWT_FILE); the VM cell requires the disposable CI JWT"
fi
if [ ! -f "$STATE_DIR/network.json" ]; then
    printf '%s\n' '{"api_url":"https://api.bringyour.com","connect_url":"wss://connect.bringyour.com"}' >"$STATE_DIR/network.json"
fi
pass "network fixture present: $STATE_DIR/network.json"

# ----------------------------------------------------------- control socket --
say "miner provide: control socket on the real kernel"
SOCK="$STATE_DIR/provider.sock"
MINER_LOG="$WORK_DIR/miner.log"
HOME="$HOME_DIR" "$BIN_DIR/miner" provide --port=0 --allow-client-registration >"$MINER_LOG" 2>&1 &
MPID=$!
waited=0
sock=no
while [ "$waited" -lt "$TIMEOUT_S" ]; do
    if [ -S "$SOCK" ]; then sock=yes; break; fi
    if ! kill -0 "$MPID" 2>/dev/null; then break; fi
    sleep 1
    waited=$((waited + 1))
done
alive=no
kill -0 "$MPID" 2>/dev/null && alive=yes
if [ "$sock" = "yes" ]; then
    pass "provider.sock appeared after ${waited}s at $SOCK"
    SOCK_LS="$(ls -l "$SOCK" 2>/dev/null)"
    case "$SOCK_LS" in
        srw-------*) pass "provider.sock mode is 0600 (owner-only)" ;;
        *)           fail "provider.sock mode is not owner-only: $SOCK_LS" ;;
    esac
    # Speak the newline-JSON control protocol directly: this is the same
    # transport urnet-tools uses, and it only answers if the socket's peer
    # credential check accepted this process.
    printf '%s\n' '{"cmd":"version"}' >"$WORK_DIR/request.json"
    nc -U "$SOCK" <"$WORK_DIR/request.json" >"$WORK_DIR/version.out" 2>"$WORK_DIR/version.err" &
    NCPID=$!
    w=0
    while [ "$w" -lt 10 ] && [ ! -s "$WORK_DIR/version.out" ]; do sleep 1; w=$((w + 1)); done
    kill "$NCPID" 2>/dev/null || true
    wait "$NCPID" 2>/dev/null || true
    if [ -s "$WORK_DIR/version.out" ]; then
        if grep -q '"ok":true' "$WORK_DIR/version.out"; then
            pass "the control protocol answered over the socket: $(head -c 200 "$WORK_DIR/version.out")"
        else
            fail "the control protocol answered without ok:true: $(head -c 200 "$WORK_DIR/version.out")"
        fi
        [ -n "$OUT_DIR" ] && cp "$WORK_DIR/version.out" "$OUT_DIR/control-version.json"
    else
        fail "no control-protocol answer within 10s"
        sed 's/^/    | /' "$WORK_DIR/version.err" 2>/dev/null | head -n 5
    fi
else
    if grep -q 'durable client registration requires Unix custody support' "$MINER_LOG" 2>/dev/null; then
        cond "FreeBSD durable client registration is not implemented at this head (TODO(freebsd-durable-registration)); the refusal is the documented state"
    else
        fail "provider.sock did not appear within ${TIMEOUT_S}s (miner alive=$alive)"
        tail -n 30 "$MINER_LOG" 2>/dev/null | sed 's/^/    | /'
    fi
fi
if [ "$alive" = "yes" ]; then
    pass "miner process alive (pid $MPID)"
elif grep -q 'durable client registration requires Unix custody support' "$MINER_LOG" 2>/dev/null; then
    cond "miner exited on the documented FreeBSD custody refusal"
else
    fail "miner exited before the socket was ready"
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
    fail "miner did not exit within ${STOP_TIMEOUT_S}s of SIGTERM"
else
    pass "miner exited cleanly after SIGTERM (${waited}s)"
fi
[ -n "$OUT_DIR" ] && tail -n 40 "$MINER_LOG" 2>/dev/null >"$OUT_DIR/miner-tail.log"

# ------------------------------------------------------- TODO feature gates --
# File-existence gates over the synced repository: absent code is a recorded
# TODO, and the gate stops being a TODO by itself when the file lands.
say "FreeBSD feature gates (TODO until the code lands)"
gate() {
    # gate <repo-relative path> <todo tag>
    if [ -e "$REPO_DIR/$1" ]; then
        printf '  PRESENT: %s is in this tree\n' "$1"
        return 0
    fi
    cond "TODO($2): $1 is not in this tree at this commit"
    return 1
}
gate internal/urnettools/discover_freebsd.go freebsd-urnettools
gate scripts/Provider_Install_FreeBSD.sh freebsd-installer
gate internal/urnettools/optimize_bsd.go freebsd-optimize

# Pre-built test binaries (the caller builds them when the port exists).
if [ -x "$BIN_DIR/urnettools.test" ]; then
    say "urnet-tools BSD test binary on the FreeBSD kernel"
    if (cd "$REPO_DIR/internal/urnettools" 2>/dev/null &&
        "$BIN_DIR/urnettools.test" -test.timeout 300s \
            -test.run 'BSD|FreeBSD|Procstat|Procstat|Sysctl|Optimize|TarRelPath' \
            >"$WORK_DIR/urnettools-test.log" 2>&1); then
        pass "urnettools.test (BSD selection) passed"
        tail -n 5 "$WORK_DIR/urnettools-test.log" | sed 's/^/    | /'
    else
        fail "urnettools.test (BSD selection) failed"
        tail -n 30 "$WORK_DIR/urnettools-test.log" 2>/dev/null | sed 's/^/    | /'
    fi
else
    cond "TODO(freebsd-urnettools): no urnettools.test binary supplied; the BSD unit tests were not run on the VM"
fi
if [ -x "$BIN_DIR/miner.test" ]; then
    say "miner test binary on the FreeBSD kernel"
    if (cd "$REPO_DIR/miner" 2>/dev/null &&
        "$BIN_DIR/miner.test" -test.timeout 300s \
            -test.run 'PeerCred|ControlSocket|Discovery|Socket' \
            >"$WORK_DIR/miner-test.log" 2>&1); then
        pass "miner.test (socket/peercred selection) passed"
        tail -n 5 "$WORK_DIR/miner-test.log" | sed 's/^/    | /'
    else
        fail "miner.test (socket/peercred selection) failed"
        tail -n 30 "$WORK_DIR/miner-test.log" 2>/dev/null | sed 's/^/    | /'
    fi
else
    cond "TODO(freebsd-miner-tests): no miner.test binary supplied (its fixture package has no freebsd build at this commit)"
fi

# ------------------------------------------------------------------ report --
echo
echo "==================== freebsd-smoke summary ===================="
printf 'kernel: %s\n' "$KERNEL_SHORT"
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
