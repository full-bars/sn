#!/usr/bin/env bash
# b1_identity_gate must wait for the provider to write .provider.key (while the
# provider is still running) instead of stopping it as soon as the socket is up.
# Uses a fake provider that binds the socket first and writes the seed 2s later.
set -u  # the harness itself runs without errexit
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d); trap '[ -n "${KEEP:-}" ] || rm -rf "$work"' EXIT

# extract the function under test verbatim from the harness
awk '/^b1_identity_gate\(\) \{/{p=1} p{print} p&&/^}/{exit}' "$here/distro-features.sh" >"$work/fn.sh"
[ -s "$work/fn.sh" ] || { echo "could not extract b1_identity_gate" >&2; exit 2; }

cat >"$work/fake-provider" <<'PY'
#!/usr/bin/env python3
import os, socket, sys, time
d = os.path.join(os.environ["HOME"], ".urnetwork")
s = socket.socket(socket.AF_UNIX); s.bind(os.path.join(d, "provider.sock"))
time.sleep(float(os.environ.get("KEY_DELAY", "2")))
open(os.path.join(d, ".provider.key"), "w").write("seed")
time.sleep(30)
PY
chmod +x "$work/fake-provider"

WORK=$work MINER_BIN=$work/fake-provider JWT_FILE=/nonexistent
export WORK MINER_BIN JWT_FILE
RESULT=""
record() { RESULT="$RESULT$1 $2\n"; }
firstline() { head -n 1; }
miner_help() { return 1; }
# shellcheck disable=SC1090
. "$work/fn.sh"

b1_identity_gate
printf "$RESULT" | grep -q '^PASS identity-gate-fresh' && echo "ok: waited for the seed" \
    || { printf 'FAIL: %b\n' "$RESULT" >&2; exit 1; }
