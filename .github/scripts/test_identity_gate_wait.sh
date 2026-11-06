#!/usr/bin/env bash
# Exercise the actual distro check against deterministic provider fixtures.
# Usage: GATE_SERVER_BIN=/path/to/release-server sh test_identity_gate_wait.sh MODE
# MODE: old, request-only, key-only, zero-exit, gated, no-curl.
# no-curl runs the gated fixture with curl removed from PATH; the check must
# report SKIP-UNSUPPORTED instead of a product FAIL.
set -u
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
mode=${1:?fixture mode required}
: "${GATE_SERVER_BIN:?build the fake release server first}"

awk '/^b1_identity_gate\(\) \{/{p=1} p{print} p&&/^}/{exit}' "$here/distro-features.sh" >"$work/fn.sh"
[ -s "$work/fn.sh" ] || exit 2
cat >"$work/fake-provider" <<'PY'
#!/usr/bin/env python3
import os, socket, sys, urllib.error, urllib.request
state = os.path.join(os.environ['HOME'], '.urnetwork')
sock = socket.socket(socket.AF_UNIX)
sock.bind(os.path.join(state, 'provider.sock'))
mode = os.environ['GATE_FIXTURE_MODE']
if mode in ('old', 'key-only'):
    open(os.path.join(state, '.provider.key'), 'w').close()
if mode in ('old', 'request-only'):
    api = next(x.split('=', 1)[1] for x in sys.argv if x.startswith('--api_url='))
    try:
        urllib.request.urlopen(urllib.request.Request(api + '/network/register-client-v1', data=b'{}'), timeout=3)
    except urllib.error.HTTPError:
        pass
print('registration requires --allow-client-registration')
sys.exit(0 if mode == 'zero-exit' else 1)
PY
chmod +x "$work/fake-provider"
WORK=$work MINER_BIN=$work/fake-provider JWT_FILE=/nonexistent BIN_DIR=''
GATE_FIXTURE_MODE=$mode
export WORK MINER_BIN JWT_FILE BIN_DIR GATE_SERVER_BIN GATE_FIXTURE_MODE
if [ "$mode" = no-curl ]; then
    GATE_FIXTURE_MODE=gated
    # A PATH that holds every tool except curl (wget may stay available).
    mkdir "$work/nocurl-bin"
    old_ifs=$IFS; IFS=:
    for d in $PATH; do
        [ -d "$d" ] || continue
        for f in "$d"/*; do
            b=${f##*/}
            [ "$b" = curl ] && continue
            [ -x "$f" ] && [ ! -e "$work/nocurl-bin/$b" ] && ln -s "$f" "$work/nocurl-bin/$b"
        done
    done
    IFS=$old_ifs
    PATH=$work/nocurl-bin
    if command -v curl >/dev/null 2>&1; then echo "curl still on PATH" >&2; exit 2; fi
fi
result=0
skipped=0
record() {
    printf '%s %s: %s\n' "$1" "$2" "$3"
    [ "$1" != FAIL ] || result=1
    [ "$1:$2" != SKIP-UNSUPPORTED:identity-gate-fresh ] || skipped=1
}
have() { command -v "$1" >/dev/null 2>&1; }
miner_help() { return 1; }
. "$work/fn.sh"
b1_identity_gate
if [ "$mode" = no-curl ] && [ "$skipped" != 1 ]; then
    echo "no-curl: identity-gate-fresh was not reported as SKIP-UNSUPPORTED" >&2
    result=1
fi
exit "$result"
