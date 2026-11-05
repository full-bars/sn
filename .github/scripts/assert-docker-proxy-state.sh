#!/bin/sh
# Compare the complete configured proxy set, not command exit status.
set -eu
container=$1
expected=$2
actual=$(mktemp)
raw=$(mktemp)
trap 'rm -f "$actual" "$raw"' EXIT HUP INT TERM
docker exec "$container" sh -c '
  set -e
  if [ -f /root/.urnetwork/proxy ]; then
    jq -r "(.servers // {}) | keys[]" /root/.urnetwork/proxy
  fi
' > "$raw"
LC_ALL=C sort "$raw" > "$actual"
if ! diff -u "$expected" "$actual"; then
  echo "FAIL: unexpected proxy state in $container" >&2
  exit 1
fi
echo "PASS: proxy state in $container matches $expected"
