#!/bin/sh
# Require the named test to exist and propagate a failing test's status.
set -eu
package=${1:?usage: run-focused-test.sh PACKAGE TEST}
test_name=${2:?usage: run-focused-test.sh PACKAGE TEST}
names=$(go test -list "^${test_name}$" "$package")
found=0
for name in $names; do
    if [ "$name" = "$test_name" ]; then
        found=1
    fi
done
if [ "$found" -ne 1 ]; then
    echo "FAIL: test $test_name not found in $package" >&2
    exit 1
fi
go test -run "^${test_name}$" -count=1 -timeout 30s "$package" -v
