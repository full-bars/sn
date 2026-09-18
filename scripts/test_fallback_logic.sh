#!/bin/bash

# DESIGN ADAPTATION: ported from the predecessor installer repo. Mock tags and the
# acceptance pattern use sn's tag scheme (v<YYYY>.<M>.<D>-<unixtime>-meso) instead
# of v3.23.0-fix.N; the pattern accepts any v20xx.* tag since sn tags may omit -meso.

# Mock environment
api_base="https://api.github.com/repos/full-bars/sn"

FAILS=0

get_version_from_api_response () 
{    
    if command -v jq > /dev/null; then
        echo "$1" | tr -d '\000-\037' | jq -r '.tag_name' 2>/dev/null
    else
        echo "$1" | tr -d '\000-\037' | python3 -c 'import sys, json;
try:
    data = json.load(sys.stdin)
    print(data["tag_name"])
except (json.JSONDecodeError, KeyError):
    print("")' 2>/dev/null
    fi
}

echo "=========================================="
echo "    TEST 1: GitHub API Rate Limited       "
echo "=========================================="
# Simulate curl -f exiting with 22 on 403 Rate Limit and producing no stdout
network_fetch_rate_limited() {
    return 22 
}

api_url="$api_base/releases/latest"
# Our fix adds "|| true" so it doesn't crash here
release="$(network_fetch_rate_limited "$api_url" 2>/dev/null || true)"
latest_version="$(get_version_from_api_response "$release" 2>/dev/null)"

echo "Version extracted from API response: '$latest_version'"

# Our fallback logic:
if [ -z "$latest_version" ]; then
    echo "--> [Fallback triggered] API failed, using web scraping trick..."
    if command -v curl > /dev/null; then
        # Hit github.com directly (no unauthenticated rate limits)
        tag_url=$(curl -Ls -o /dev/null -w %{url_effective} "https://github.com/full-bars/sn/releases/latest")
        echo "--> tag_url resolved to: $tag_url"
        if [ -n "$tag_url" ] && [ "$tag_url" != "https://github.com/full-bars/sn/releases/latest" ]; then
            latest_version="${tag_url##*/}"
        fi
    fi
fi

echo "Final latest_version: '$latest_version'"
if case "$latest_version" in v20[0-9][0-9].*) true ;; *) false ;; esac; then
    echo "✅ TEST 1 PASSED: Resiliently extracted version!"
elif [ -z "${tag_url:-}" ] || [ "$tag_url" = "https://github.com/full-bars/sn/releases/latest" ]; then
    # The fallback cannot be evaluated without a live redirect lookup; no
    # usable URL (offline environment, curl missing, redirect not resolved)
    # means SKIP, not fail.
    echo "⊘ SKIP: TEST 1 (live redirect lookup returned no URL; network may not be available)"
else
    echo "❌ TEST 1 FAILED"
    FAILS=$((FAILS + 1))
fi

echo ""
echo "=========================================="
echo "    TEST 2: Normal API Response           "
echo "=========================================="
# Simulate normal API response
network_fetch_success() {
    echo '{"tag_name": "v2026.9.17-1789646883-meso"}'
}

release="$(network_fetch_success "$api_url" 2>/dev/null || true)"
latest_version="$(get_version_from_api_response "$release" 2>/dev/null)"

echo "Version extracted from API response: '$latest_version'"

if [ "$latest_version" = "v2026.9.17-1789646883-meso" ]; then
    echo "✅ TEST 2 PASSED: Extracted cleanly without fallback."
else
    echo "❌ TEST 2 FAILED"
    FAILS=$((FAILS + 1))
fi

echo ""
if [ "$FAILS" -eq 0 ]; then
    echo "🎉 All fallback logic tests passed!"
    exit 0
else
    echo "🚨 $FAILS test(s) failed."
    exit 1
fi
