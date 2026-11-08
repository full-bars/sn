#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
mkdir "$tmp/bin"
sed -n '/^ensure_extender_cap ()/,/^)/p' "$root/scripts/Provider_Install_Linux.sh" > "$tmp/helper"
. "$tmp/helper"
cat > "$tmp/bin/getcap" <<'MOCK'
#!/bin/sh
[ "${1:-}" != -n ] || shift
[ "${GETCAP_FAIL:-0}" != 1 ] || exit 1
[ ! -s "$CAP_STATE" ] || printf '%s %s\n' "$1" "$(cat "$CAP_STATE")"
MOCK
cat > "$tmp/bin/setcap" <<'MOCK'
#!/bin/sh
printf '%s\n' "$@" > "$CAP_CALL"
[ "${SETCAP_FAIL:-0}" != 1 ] || exit 1
if [ "$1" = -n ]; then
    printf '%s [rootid=%s]\n' "$3" "$2" > "$CAP_STATE"
else
    printf '%s\n' "$1" > "$CAP_STATE"
fi
MOCK
REAL_AWK=$(command -v awk); export REAL_AWK
cat > "$tmp/bin/awk" <<'MOCK'
#!/bin/sh
for arg do
    [ "$arg" != /proc/self/status ] || exit 0
done
exec "$REAL_AWK" "$@"
MOCK
cat > "$tmp/bin/sudo" <<'MOCK'
#!/bin/sh
[ "${1:-}" != -n ] || shift
exec "$@"
MOCK
chmod +x "$tmp/bin/getcap" "$tmp/bin/setcap" "$tmp/bin/sudo" "$tmp/bin/awk"
PATH="$tmp/bin:$PATH"; export PATH
CAP_STATE="$tmp/state"; CAP_CALL="$tmp/call"; export CAP_STATE CAP_CALL
binary="$tmp/provider with ' quote"
: > "$binary"

printf '%s\n' 'cap_net_raw=ep' > "$CAP_STATE"
ensure_extender_cap "$binary" 2> "$tmp/warning"
[ -e "$CAP_CALL" ] || { cat "$tmp/warning" >&2; echo 'FAIL: setcap was not called' >&2; exit 1; }
case $(cat "$CAP_CALL") in *cap_net_raw=ep*) ;; *) echo 'FAIL: existing cap_net_raw was discarded' >&2; exit 1;; esac
case $(cat "$CAP_CALL") in *cap_net_bind_service+ep*) ;; *) echo 'FAIL: bind capability was not merged' >&2; exit 1;; esac
printf '%s\n' 'PASS: existing capability preserved'

: > "$CAP_STATE"
ensure_extender_cap "$binary" 2> "$tmp/warning"
[ "$(sed -n '1p' "$CAP_CALL")" = 'cap_net_bind_service=+ep' ] || { echo 'FAIL: empty capability set' >&2; exit 1; }
echo 'PASS: empty capability set'

printf '%s\n' 'cap_net_raw=ep' > "$CAP_STATE"
rm -f "$CAP_CALL"
GETCAP_FAIL=1; export GETCAP_FAIL
ensure_extender_cap "$binary" 2> "$tmp/warning" || true
[ ! -e "$CAP_CALL" ] || { echo 'FAIL: setcap ran when getcap failed' >&2; exit 1; }
case $(cat "$tmp/warning") in *'sudo setcap cap_net_bind_service=+ep'*) echo 'FAIL: unsafe fallback after getcap failure' >&2; exit 1;; esac
unset GETCAP_FAIL
echo 'PASS: getcap failure is safe'

printf '%s\n' 'cap_net_raw=ep' > "$CAP_STATE"
SETCAP_FAIL=1; export SETCAP_FAIL
ensure_extender_cap "$binary" 2> "$tmp/warning"
case $(cat "$tmp/warning") in *'cap_net_raw=ep cap_net_bind_service+ep'*) ;; *) echo 'FAIL: fallback omitted existing capability' >&2; exit 1;; esac
unset SETCAP_FAIL
manual=$(cat "$tmp/warning")
manual=${manual#*then run: }
sh -c "$manual"
[ "$(sed -n '2p' "$CAP_CALL")" = "$binary" ] || { echo 'FAIL: manual command did not quote binary path' >&2; exit 1; }
echo 'PASS: safe manual fallback and quoted path'

printf '%s\n' 'cap_net_raw,cap_chown=ep cap_net_admin+p [rootid=1000]' > "$CAP_STATE"
ensure_extender_cap "$binary" 2> "$tmp/warning"
[ "$(sed -n '1p' "$CAP_CALL")" = '-n' ] && [ "$(sed -n '2p' "$CAP_CALL")" = '1000' ] || { echo 'FAIL: namespace rootid lost' >&2; exit 1; }
[ "$(sed -n '3p' "$CAP_CALL")" = 'cap_net_raw,cap_chown=ep cap_net_admin+p cap_net_bind_service+ep' ] || { echo 'FAIL: capability clauses lost' >&2; exit 1; }
echo 'PASS: grouped clauses and namespace rootid'

printf '%s\n' 'invalid;touch' > "$CAP_STATE"
rm -f "$CAP_CALL"
ensure_extender_cap "$binary" 2> "$tmp/warning"
[ ! -e "$CAP_CALL" ] || { echo 'FAIL: unrecognized capabilities changed' >&2; exit 1; }
case $(cat "$tmp/warning") in *'sudo setcap'*) echo 'FAIL: unsafe fallback for invalid capabilities' >&2; exit 1;; esac
echo 'PASS: unrecognized capabilities refused'
