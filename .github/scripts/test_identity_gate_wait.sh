#!/usr/bin/env bash
# Exercise the actual distro check against deterministic provider fixtures.
# Usage: GATE_SERVER_BIN=/path/to/release-server sh test_identity_gate_wait.sh MODE
# MODE: old, request-only, key-only, zero-exit, gated.
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
result=0
record() {
    printf '%s %s: %s\n' "$1" "$2" "$3"
    [ "$1" != FAIL ] || result=1
}
miner_help() { return 1; }
. "$work/fn.sh"
b1_identity_gate
exit "$result"
