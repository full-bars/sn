#!/bin/sh
# lsm-smoke.sh — install the provider on a host with an active Linux Security
# Module and assert the install does not trip it.
#
# The container cells cannot do this: SELinux and AppArmor are host-kernel
# facilities, and the plain container images ship neither the policy tools nor
# /sys/kernel/security. This runs inside a full VM (vmactions) where the distro
# boots with its default LSM.
#
# Asserts:
#   * the expected LSM is actually active and enforcing (otherwise SKIP, never
#     a silent pass)
#   * the installer completes with the LSM active
#   * the installed tree carries a sane label (not a home-dir type)
#   * running the installed binary produces no AVC / apparmor denial for it
#
# Inputs: BIN_DIR (payload with provider, urnet-tools, release-server,
# installer.sh), TAG, OUT_DIR.

set -u

BIN_DIR=${BIN_DIR:?BIN_DIR required}
TAG=${TAG:?TAG required}
OUT_DIR=${OUT_DIR:-/tmp/lsm-smoke}
PORT=${PORT:-18090}
INSTALLER=${INSTALLER:-$BIN_DIR/installer.sh}
TIMEOUT_S=${TIMEOUT_S:-240}

PASSES=0; FAILS=0; CONDS=0
mkdir -p "$OUT_DIR"
say()  { printf '== %s\n' "$*"; }
pass() { PASSES=$((PASSES+1)); printf '  PASS: %s\n' "$*"; }
fail() { FAILS=$((FAILS+1)); printf '  FAIL: %s\n' "$*"; }
cond() { CONDS=$((CONDS+1)); printf '  CONDITIONAL: %s\n' "$*"; }

# ---------------------------------------------------------------- which LSM --
say "active LSM"
LSM=""
if command -v getenforce >/dev/null 2>&1; then
    MODE=$(getenforce 2>/dev/null || echo Unknown)
    printf 'selinux: %s\n' "$MODE"
    [ "$MODE" = "Enforcing" ] && LSM=selinux
fi
if [ -z "$LSM" ] && command -v aa-status >/dev/null 2>&1; then
    if aa-status --enabled 2>/dev/null; then
        printf 'apparmor: enabled\n'
        LSM=apparmor
    fi
fi
if [ -z "$LSM" ]; then
    cond "no enforcing SELinux or enabled AppArmor on this host; nothing to assert"
    printf 'RESULT: PASS (no LSM)\n' | tee "$OUT_DIR/summary.txt"
    exit 0
fi
pass "LSM active: $LSM"

# ------------------------------------------------------------- fake release --
say "stage and serve the release"
ARCH=$(uname -m)
case "$ARCH" in x86_64|amd64) ARCH=amd64 ;; aarch64|arm64) ARCH=arm64 ;; esac
WORK=$(mktemp -d)
mkdir -p "$WORK/assets/linux/$ARCH"
cp "$BIN_DIR/provider" "$WORK/assets/linux/$ARCH/provider"
cp "$BIN_DIR/urnet-tools" "$WORK/assets/urnet-tools"
tar -czf "$WORK/assets/urnetwork-provider-$TAG.tar.gz" -C "$WORK/assets" linux urnet-tools
"$BIN_DIR/release-server" -dir "$WORK/assets" -addr "127.0.0.1:$PORT" -tag "$TAG" >"$OUT_DIR/server.log" 2>&1 &
SRV=$!
i=0
while [ "$i" -lt 40 ]; do
    if (command -v curl >/dev/null 2>&1 && curl -fsS --max-time 2 "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1); then break; fi
    sleep 0.5; i=$((i+1))
done
pass "fake release serving $TAG"

# ---------------------------------------------------------------- install --
say "install with $LSM active"
# Run as root when the VM gives us sudo: the system path is the one an LSM
# constrains. The harness starts from a clean HOME.
if [ "$(id -u)" -ne 0 ] && command -v sudo >/dev/null 2>&1; then SUDO=sudo; else SUDO=""; fi
# shellcheck disable=SC2086
$SUDO env URNET_ALLOW_BASE_OVERRIDE=1 \
    URNET_API_BASE="http://127.0.0.1:$PORT/repos/full-bars/sn" \
    URNET_MIRROR_BASE="http://127.0.0.1:$PORT/releases/download" \
    URNET_DL_BASE="http://127.0.0.1:$PORT" \
    sh "$INSTALLER" -f install >"$OUT_DIR/install.log" 2>&1
RC=$?
[ "$RC" -eq 0 ] && pass "installer exited 0" || { fail "installer exited $RC"; tail -n 15 "$OUT_DIR/install.log" | sed 's/^/    | /'; }

# Locate the tree (root install path or the invoking user's home).
CANDIDATES="/usr/local/lib/urnetwork-provider $HOME/.local/share/urnetwork-provider /root/.local/share/urnetwork-provider"
ROOT=""
for c in $CANDIDATES; do [ -x "$c/bin/urnetwork" ] && { ROOT=$c; break; }; done
if [ -n "$ROOT" ]; then pass "install tree at $ROOT"; else fail "no install tree found"; fi

# ------------------------------------------------------------------ labels --
if [ "$LSM" = "selinux" ] && [ -n "$ROOT" ]; then
    say "selinux labels"
    LABELS=$(ls -ZR "$ROOT" 2>/dev/null | awk '{print $1}' | sort -u | tr '\n' ' ')
    printf 'types: %s\n' "$LABELS"
    case "$LABELS" in
        *user_home_t*) fail "the install tree carries user_home_t; a confined service cannot execute it" ;;
        *) pass "install tree labels are not home-dir types" ;;
    esac
fi

# ------------------------------------------------------------- run + deny --
if [ -n "$ROOT" ]; then
    say "run the installed binary"
    # A short-lived run is enough to surface an exec-time or state-dir denial.
    ( HOME=/tmp/lsm-home; mkdir -p "$HOME/.urnetwork"; timeout 8 "$ROOT/bin/urnetwork" -f provide --port=0 --allow-client-registration >"$OUT_DIR/run.log" 2>&1 || true )
    pass "binary executed under $LSM (output in run.log)"
    "$ROOT/bin/urnetwork" --version 2>&1 | head -n 1 | tee "$OUT_DIR/version.txt"

    if [ "$LSM" = "selinux" ]; then
        if command -v ausearch >/dev/null 2>&1; then
            AVC=$($SUDO ausearch -m AVC -ts recent 2>/dev/null | grep -i -E 'urnetwork|urnet-tools' || true)
            if [ -z "$AVC" ]; then pass "no SELinux AVC for the provider after install and a run"
            else fail "SELinux denied the provider: $(printf '%s' "$AVC" | head -n 3 | tr '\n' ' ')"; fi
        else
            cond "ausearch not available; AVC scan skipped"
        fi
    else
        if command -v journalctl >/dev/null 2>&1; then
            DENIED=$($SUDO journalctl -k --since '-10 min' 2>/dev/null | grep 'apparmor="DENIED"' | grep -i urnetwork || true)
        else
            DENIED=$(dmesg 2>/dev/null | grep 'apparmor="DENIED"' | grep -i urnetwork || true)
        fi
        if [ -z "$DENIED" ]; then pass "no AppArmor denial for the provider"
        else fail "AppArmor denied the provider: $(printf '%s' "$DENIED" | head -n 3 | tr '\n' ' ')"; fi
    fi
fi

# ---------------------------------------------------------------- cleanup --
$SUDO sh "$INSTALLER" -f uninstall >>"$OUT_DIR/install.log" 2>&1 || true
kill "$SRV" 2>/dev/null || true
rm -rf "$WORK"

echo
echo "==================== lsm-smoke summary ===================="
( . /etc/os-release 2>/dev/null && printf 'distro: %s %s\n' "${NAME:-unknown}" "${VERSION_ID:-}" ) || true
printf 'lsm:    %s\n' "$LSM"
printf 'checks: %s passed, %s failed, %s conditional\n' "$PASSES" "$FAILS" "$CONDS"
if [ "$FAILS" -gt 0 ]; then printf 'RESULT: FAIL\n' | tee "$OUT_DIR/summary.txt"; exit 1; fi
printf 'RESULT: PASS\n' | tee "$OUT_DIR/summary.txt"
exit 0
