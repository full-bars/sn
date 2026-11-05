#!/bin/sh
# Self-test for the distro-features.sh coupling fixes (POSIX sh, no network, no
# miner). It sources the harness under test (main() neutralised in a temp
# copy), then runs the harness's OWN independent-group loop (extracted from the
# file, not re-implemented here) with the three group bodies stubbed. Each stub
# records whether it can see a sentinel an earlier group left in STATE_DIR.
#   usage: sh test_distro_features_isolation.sh <path-to-distro-features.sh>
# Exit 0 = isolated and policy holds, 1 = a coupling reproduced.
set -u
H="${1:?harness path}"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
sed 's/^main "\$@"$/:/' "$H" >"$T/h.sh"
FAILS=0
fail() { printf 'TEST-FAIL %s\n' "$*"; FAILS=$((FAILS + 1)); }
ok()   { printf 'TEST-OK   %s\n' "$*"; }

# ---- 1. state isolation: a sentinel left by one group must not reach the next
( 
  BIN_DIR=/nonexistent STATE_HOME="$T/home" FEATURE_WORK="$T/work" FEATURE_REPORT="$T/report"
  export BIN_DIR STATE_HOME FEATURE_WORK FEATURE_REPORT
  . "$T/h.sh" >/dev/null 2>&1
  mkdir -p "$STATE_DIR" "$WORK"
  seen=""
  b2_url_sources() { [ -e "$STATE_DIR/proxy" ] && seen="$seen url"; printf 'leak\n' >"$STATE_DIR/proxy"; }
  b2_manipulations() { [ -e "$STATE_DIR/proxy" ] && seen="$seen manip"; printf 'leak\n' >"$STATE_DIR/proxy"; }
  b3_surface() { [ -e "$STATE_DIR/proxy" ] && seen="$seen surface"; printf 'leak\n' >"$STATE_DIR/proxy"; }
  printf 'seed\n' >"$STATE_DIR/proxy"      # what block 1 / an earlier group left
  CHK_ORDER="url manip surface"
  loop="$(sed -n '/^        for g in \$CHK_ORDER; do$/,/^        done$/p' "$T/h.sh")"
  [ -n "$loop" ] || { echo "no group loop found"; exit 2; }
  eval "$loop"
  if [ -n "$seen" ]; then echo "INHERITED:$seen"; exit 1; fi
  echo "CLEAN"; exit 0
) >"$T/iso.out" 2>&1
rc=$?
if [ "$rc" = 0 ]; then ok "groups never see a predecessor's state ($(tail -1 "$T/iso.out"))"
else fail "state inherited across groups: $(tail -1 "$T/iso.out")"; fi

# ---- 2. skip policy: a mandatory skip fails, an optional one stays green
(
  BIN_DIR=/nonexistent STATE_HOME="$T/home2" FEATURE_WORK="$T/work2" FEATURE_REPORT="$T/report2"
  export BIN_DIR STATE_HOME FEATURE_WORK FEATURE_REPORT
  mkdir -p "$T/home2"
  . "$T/h.sh" >/dev/null 2>&1
  record SKIP-NO-NETWORK identity "provider not live" >/dev/null
  record SKIP-NO-NETWORK url-fetch-source1 "list host unreachable" >/dev/null
  [ "$FAIL_N" = 1 ] && [ "$SKIPN_N" = 1 ] && [ "$MSKIP_N" = 1 ] || { echo "FAIL_N=$FAIL_N SKIPN_N=$SKIPN_N MSKIP_N=${MSKIP_N:-unset}"; exit 1; }
  exit 0
) >"$T/pol.out" 2>&1
rc=$?
if [ "$rc" = 0 ]; then ok "mandatory SKIP-NO-NETWORK -> FAIL, optional stays a skip"
else fail "skip policy: $(tail -1 "$T/pol.out")"; fi

printf 'self-test: %s failure(s)\n' "$FAILS"
[ "$FAILS" = 0 ]
