#!/bin/sh
# Pins the auth-code hand-off in start_jwt.sh.
#
# The environment-auth branch used to call `auth "" -f`, passing an empty
# argument. The binary then fell back to its interactive prompt, which reads
# stdin, and in a container with no TTY that is EOF and a panic. The code has
# to be handed to the binary as the argument.
#
# Static on purpose: start_jwt.sh brings up vnstat and an HTTP server before it
# reaches auth, so a functional test here would be a container test.
set -eu

script="$(dirname "$0")/start_jwt.sh"
fail=0

if grep -q 'auth "" -f' "$script"; then
    echo "FAIL: $script calls auth with an empty argument; the code is not forwarded"
    grep -n 'auth "" -f' "$script"
    fail=1
else
    echo "PASS: no empty-argument auth call"
fi

forwarded=$(grep -c 'auth "\$URNETWORK_AUTH_CODE" -f' "$script" || true)
if [ "$forwarded" -ne 2 ]; then
    echo "FAIL: expected 2 call sites forwarding \$URNETWORK_AUTH_CODE (initial and re-auth), found $forwarded"
    fail=1
else
    echo "PASS: both auth call sites forward \$URNETWORK_AUTH_CODE"
fi

if ! sh -n "$script" 2>/dev/null; then
    echo "FAIL: $script does not parse"
    fail=1
else
    echo "PASS: $script parses"
fi

if [ "$fail" -ne 0 ]; then
    echo "Results: FAILED"
    exit 1
fi
echo "Results: all passed"
