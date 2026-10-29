#!/bin/sh
# Asserts that the image hands the environment auth code to the provider binary.
#
# The regression this pins: the environment branch called `auth "" -f`, so the
# binary fell back to its interactive prompt and panicked on EOF in a container
# with no TTY. A real auth code cannot be used in CI, so the provider binary is
# replaced by a recorder that writes the argv it was called with. The assertion
# is on that argv, not on a login succeeding.
#
# Usage: test_auth_argv_in_container.sh <image-ref> [overlay-dir]
#
# <overlay-dir>, when given, is copied over /app after the binary is stubbed.
# That is how the fixed start_jwt.sh is tested against an image that predates
# the fix.
set -eu

image="${1:?usage: $0 <image-ref> [overlay-dir]}"
overlay="${2:-}"
code="TESTCODE-9f3a2b"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

tag="auth-argv-test:$$"
mkdir -p "$work/argvdir" "$work/ctx"

copy_overlay=""
if [ -n "$overlay" ]; then
    [ -d "$overlay" ] || { echo "FAIL: overlay dir $overlay does not exist"; exit 1; }
    copy_overlay="COPY overlay/ /app/
RUN chmod +x /app/*.sh"
    mkdir -p "$work/ctx/overlay"
    cp -a "$overlay"/. "$work/ctx/overlay/"
fi

cat > "$work/ctx/Dockerfile" <<EOF
FROM $image
USER root
# Replace every provider binary with a recorder; the real one is kept aside so
# the image stays intact for anything else that inspects it.
RUN for f in /app/urnetwork_*_stable; do \\
      mv "\$f" "\$f.real"; \\
      printf '#!/bin/sh\\nprintf "%%s\\\\n" "\$*" >> /app/argvdir/argv.txt\\n' > "\$f"; \\
      chmod +x "\$f"; \\
    done
$copy_overlay
EOF

docker build -q -t "$tag" "$work/ctx" >/dev/null || { echo "FAIL: could not build the stub image from $image"; exit 1; }

docker run --rm -d --name "auth-argv-$$" \
    -e URNETWORK_AUTH_CODE="$code" \
    -e HOST_HOSTNAME=ci \
    -v "$work/argvdir:/app/argvdir" \
    "$tag" >/dev/null 2>&1 || { echo "FAIL: container did not start"; docker rmi -f "$tag" >/dev/null 2>&1; exit 1; }

# The script exits once auth is attempted (our recorder writes no JWT), so poll
# for the file rather than waiting on the container.
i=0
while [ "$i" -lt 30 ]; do
    [ -s "$work/argvdir/argv.txt" ] && break
    i=$((i+1)); sleep 1
done
docker rm -f "auth-argv-$$" >/dev/null 2>&1 || true
docker rmi -f "$tag" >/dev/null 2>&1 || true

if [ ! -s "$work/argvdir/argv.txt" ]; then
    echo "FAIL: the provider binary was never called; the auth branch did not run"
    exit 1
fi

echo "recorded argv:"
sed 's/^/  /' "$work/argvdir/argv.txt"

if ! grep -q "auth $code" "$work/argvdir/argv.txt"; then
    echo "FAIL: auth was called without the environment code; expected 'auth $code'"
    exit 1
fi
echo "PASS: auth received the environment code"
