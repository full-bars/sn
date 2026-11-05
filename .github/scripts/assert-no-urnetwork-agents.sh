#!/bin/sh
# Fail if a launch agent belonging to urnetwork remains.
set -eu

dir=${1:?usage: assert-no-urnetwork-agents.sh DIRECTORY}
if [ ! -d "$dir" ]; then
    if [ -e "$dir" ] || [ -L "$dir" ]; then
        echo "FAIL: cannot inspect agent directory: $dir" >&2
        exit 1
    fi
    echo "OK: agent directory absent: $dir"
    exit 0
fi
if ! ls -A "$dir" > /dev/null; then
    echo "FAIL: cannot inspect agent directory: $dir" >&2
    exit 1
fi
for file in "$dir"/*urnetwork*; do
    if [ -e "$file" ] || [ -L "$file" ]; then
        echo "FAIL: leftover urnetwork agent: $file" >&2
        exit 1
    fi
done
echo "OK: no urnetwork agent plists"
