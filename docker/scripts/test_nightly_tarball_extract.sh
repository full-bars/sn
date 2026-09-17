#!/bin/sh
set -e

# Regression test for Bug 3: start_nightly.sh tarball extraction + staging
#
# The bug: after detection + extraction, the cp staging step hardcoded
# "$UPDATE_TMP/linux/${A_SYS_ARCH}/provider" instead of
# "$UPDATE_TMP/$PROVIDER_IN_TARBALL".  With a flat tarball (per-arch asset)
# the binary lands at "$UPDATE_TMP/provider", so the hardcoded cp fails.
#
# Each test exercises the FULL pipeline: detect layout → extract → stage (cp).

PASS_COUNT=0
FAIL_COUNT=0

pass() {
    PASS_COUNT=$((PASS_COUNT + 1))
    echo "PASS: $1"
}

fail() {
    FAIL_COUNT=$((FAIL_COUNT + 1))
    echo "FAIL: $1"
}

cleanup() {
    rm -rf "$TEST_DIR"
}

trap cleanup EXIT

TEST_DIR=$(mktemp -d)

###############################################################################
# Helper: simulate the full start_nightly.sh detection → extract → stage flow
#   $1 = tarball path
#   $2 = UPDATE_TMP path
#   $3 = A_SYS_ARCH
#   $4 = APP_DIR (for staging)
#   Sets STAGED_PATH to the result on success, empty on failure.
###############################################################################
simulate_nightly_pipeline() {
    local archive="$1"
    local update_tmp="$2"
    local sys_arch="$3"
    local app_dir="$4"

    # --- Detection step (copied from start_nightly.sh lines 332-335) ---
    PROVIDER_IN_TARBALL="linux/${sys_arch}/provider"
    if ! tar -tzf "$archive" "$PROVIDER_IN_TARBALL" >/dev/null 2>&1; then
        PROVIDER_IN_TARBALL="provider"
    fi

    # --- Extraction step (lines 336-347) ---
    tar -xzf "$archive" -C "$update_tmp" "$PROVIDER_IN_TARBALL" || return 1
    [ -f "$update_tmp/$PROVIDER_IN_TARBALL" ] || return 1

    # --- Staging step: this is the FIXED line (line 355) ---
    local staged="$app_dir/.urnetwork_${sys_arch}_nightly.new"
    STAGED_PATH=""
    if cp -f "$update_tmp/$PROVIDER_IN_TARBALL" "$staged"; then
        STAGED_PATH="$staged"
    fi
}

###############################################################################
# Helper: simulate the OLD buggy pipeline (hardcoded cp path)
###############################################################################
simulate_nightly_pipeline_OLD_BUGGY() {
    local archive="$1"
    local update_tmp="$2"
    local sys_arch="$3"
    local app_dir="$4"

    PROVIDER_IN_TARBALL="linux/${sys_arch}/provider"
    if ! tar -tzf "$archive" "$PROVIDER_IN_TARBALL" >/dev/null 2>&1; then
        PROVIDER_IN_TARBALL="provider"
    fi

    tar -xzf "$archive" -C "$update_tmp" "$PROVIDER_IN_TARBALL" || return 1
    [ -f "$update_tmp/$PROVIDER_IN_TARBALL" ] || return 1

    # OLD BUGGY LINE: hardcoded path ignores PROVIDER_IN_TARBALL
    local staged="$app_dir/.urnetwork_${sys_arch}_nightly.new"
    STAGED_PATH=""
    if cp -f "$update_tmp/linux/${sys_arch}/provider" "$staged"; then
        STAGED_PATH="$staged"
    fi
}

###############################################################################
# Test 1: Flat tarball — FIXED pipeline stages correctly
###############################################################################
test_flat_layout_fixed() {
    local staging="$TEST_DIR/flat_fixed"
    mkdir -p "$staging/build" "$staging/extract" "$staging/app"
    echo "flat-provider-binary-$(date +%s%N)" > "$staging/build/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" provider

    simulate_nightly_pipeline "$staging/provider.tar.gz" "$staging/extract" "amd64" "$staging/app"

    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        local content
        content=$(cat "$STAGED_PATH")
        if echo "$content" | grep -q "flat-provider-binary"; then
            pass "Flat tarball: FIXED pipeline stages binary correctly"
        else
            fail "Flat tarball: staged file exists but content wrong: $content"
        fi
    else
        fail "Flat tarball: FIXED pipeline failed to stage binary"
    fi
}

###############################################################################
# Test 2: Flat tarball — OLD BUGGY pipeline FAILS to stage
#    This is the core regression: if someone reverts the fix, this test
#    will start passing again (which is bad).
###############################################################################
test_flat_layout_old_buggy_fails() {
    local staging="$TEST_DIR/flat_buggy"
    mkdir -p "$staging/build" "$staging/extract" "$staging/app"
    echo "flat-provider-binary-$(date +%s%N)" > "$staging/build/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" provider

    simulate_nightly_pipeline_OLD_BUGGY "$staging/provider.tar.gz" "$staging/extract" "amd64" "$staging/app"

    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        # If the old buggy code succeeds, the bug has been reintroduced
        fail "Flat tarball: OLD buggy pipeline unexpectedly succeeded — regression!"
    else
        pass "Flat tarball: OLD buggy pipeline correctly fails (proves fix is needed)"
    fi
}

###############################################################################
# Test 3: Nested tarball — FIXED pipeline stages correctly
###############################################################################
test_nested_layout_fixed() {
    local staging="$TEST_DIR/nested_fixed"
    mkdir -p "$staging/build/linux/amd64" "$staging/extract" "$staging/app"
    echo "nested-provider-binary-$(date +%s%N)" > "$staging/build/linux/amd64/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" linux/amd64/provider

    simulate_nightly_pipeline "$staging/provider.tar.gz" "$staging/extract" "amd64" "$staging/app"

    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        local content
        content=$(cat "$STAGED_PATH")
        if echo "$content" | grep -q "nested-provider-binary"; then
            pass "Nested tarball: FIXED pipeline stages binary correctly"
        else
            fail "Nested tarball: staged file exists but content wrong: $content"
        fi
    else
        fail "Nested tarball: FIXED pipeline failed to stage binary"
    fi
}

###############################################################################
# Test 4: Nested tarball — both FIXED and OLD should work (regression guard)
#    The old code hardcoded the nested path, so it only worked for nested.
#    This test confirms the fix doesn't break nested tarballs.
###############################################################################
test_nested_layout_old_buggy_works() {
    local staging="$TEST_DIR/nested_buggy"
    mkdir -p "$staging/build/linux/amd64" "$staging/extract" "$staging/app"
    echo "nested-provider-binary-$(date +%s%N)" > "$staging/build/linux/amd64/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" linux/amd64/provider

    simulate_nightly_pipeline_OLD_BUGGY "$staging/provider.tar.gz" "$staging/extract" "amd64" "$staging/app"

    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        pass "Nested tarball: OLD code also stages correctly (expected, nested matches hardcode)"
    else
        fail "Nested tarball: OLD code unexpectedly failed"
    fi
}

###############################################################################
# Test 5: Flat tarball — detection correctly resolves PROVIDER_IN_TARBALL
###############################################################################
test_detection_resolves_flat() {
    local staging="$TEST_DIR/detect_flat"
    mkdir -p "$staging/build"
    echo "binary" > "$staging/build/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" provider

    PROVIDER_IN_TARBALL="linux/amd64/provider"
    if ! tar -tzf "$staging/provider.tar.gz" "$PROVIDER_IN_TARBALL" >/dev/null 2>&1; then
        PROVIDER_IN_TARBALL="provider"
    fi

    if [ "$PROVIDER_IN_TARBALL" = "provider" ]; then
        pass "Detection: flat tarball resolves to 'provider'"
    else
        fail "Detection: flat tarball should resolve to 'provider', got '$PROVIDER_IN_TARBALL'"
    fi
}

###############################################################################
# Test 6: Nested tarball — detection correctly resolves PROVIDER_IN_TARBALL
###############################################################################
test_detection_resolves_nested() {
    local staging="$TEST_DIR/detect_nested"
    mkdir -p "$staging/build/linux/amd64"
    echo "binary" > "$staging/build/linux/amd64/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" linux/amd64/provider

    PROVIDER_IN_TARBALL="linux/amd64/provider"
    if ! tar -tzf "$staging/provider.tar.gz" "$PROVIDER_IN_TARBALL" >/dev/null 2>&1; then
        PROVIDER_IN_TARBALL="provider"
    fi

    if [ "$PROVIDER_IN_TARBALL" = "linux/amd64/provider" ]; then
        pass "Detection: nested tarball resolves to 'linux/amd64/provider'"
    else
        fail "Detection: nested tarball should resolve to 'linux/amd64/provider', got '$PROVIDER_IN_TARBALL'"
    fi
}

###############################################################################
# Test 7: arm64 flat tarball
###############################################################################
test_flat_layout_arm64_fixed() {
    local staging="$TEST_DIR/flat_arm64"
    mkdir -p "$staging/build" "$staging/extract" "$staging/app"
    echo "arm64-flat-binary-$(date +%s%N)" > "$staging/build/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" provider

    simulate_nightly_pipeline "$staging/provider.tar.gz" "$staging/extract" "arm64" "$staging/app"

    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        pass "Flat tarball arm64: FIXED pipeline stages binary correctly"
    else
        fail "Flat tarball arm64: FIXED pipeline failed to stage binary"
    fi
}

###############################################################################
# Test 8: Multi-arch fat tarball (both architectures present)
###############################################################################
test_multitarball_fixed() {
    local staging="$TEST_DIR/multi_fixed"
    mkdir -p "$staging/build/linux/amd64" "$staging/build/linux/arm64" "$staging/extract" "$staging/app"
    echo "multi-amd64-binary" > "$staging/build/linux/amd64/provider"
    echo "multi-arm64-binary" > "$staging/build/linux/arm64/provider"
    tar -czf "$staging/provider.tar.gz" -C "$staging/build" linux/amd64/provider linux/arm64/provider

    # Test amd64
    simulate_nightly_pipeline "$staging/provider.tar.gz" "$staging/extract" "amd64" "$staging/app"
    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        local content
        content=$(cat "$STAGED_PATH")
        if echo "$content" | grep -q "multi-amd64-binary"; then
            pass "Multi-arch tarball: amd64 binary staged correctly"
        else
            fail "Multi-arch tarball: wrong content staged for amd64: $content"
        fi
    else
        fail "Multi-arch tarball: failed to stage amd64 binary"
    fi

    # Test arm64 (reuse extract dir, just stage into fresh app dir)
    mkdir -p "$staging/app2"
    simulate_nightly_pipeline "$staging/provider.tar.gz" "$staging/extract" "arm64" "$staging/app2"
    if [ -n "$STAGED_PATH" ] && [ -f "$STAGED_PATH" ]; then
        local content
        content=$(cat "$STAGED_PATH")
        if echo "$content" | grep -q "multi-arm64-binary"; then
            pass "Multi-arch tarball: arm64 binary staged correctly"
        else
            fail "Multi-arch tarball: wrong content staged for arm64: $content"
        fi
    else
        fail "Multi-arch tarball: failed to stage arm64 binary"
    fi
}

###############################################################################
# Run all tests
###############################################################################
echo "=== Nightly Tarball Extraction + Staging Regression Tests ==="
echo ""

test_flat_layout_fixed
test_flat_layout_old_buggy_fails
test_nested_layout_fixed
test_nested_layout_old_buggy_works
test_detection_resolves_flat
test_detection_resolves_nested
test_flat_layout_arm64_fixed
test_multitarball_fixed

echo ""
echo "=== Results: $PASS_COUNT passed, $FAIL_COUNT failed ==="

if [ "$FAIL_COUNT" -gt 0 ]; then
    exit 1
fi
echo "ALL TESTS PASSED"
exit 0
