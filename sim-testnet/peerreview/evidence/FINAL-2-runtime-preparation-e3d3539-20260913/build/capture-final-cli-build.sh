#!/usr/bin/env bash
set -u -o pipefail
umask 077
runtime='/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/runtime-final-cli-prepared-unbound-20260913T1851Z'
body="$runtime/commands/final-cli-build.command"
: "$FINAL_WORKSPACE" "$SOURCE_PAIR" "$EXPECTED_SN" "$WARM_GOCACHE" "$PRIVATE_GOMODCACHE"
for p in "$runtime" "$runtime/commands" "$runtime/capture" "$runtime/meta" "$runtime/logs" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin" "$body" "$0" "$SOURCE_PAIR" "$WARM_GOCACHE" "$PRIVATE_GOMODCACHE"; do [[ -e "$p" && ! -L "$p" ]] || exit 125; done
for p in "$runtime" "$runtime/commands" "$runtime/capture" "$runtime/meta" "$runtime/logs" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin" "$WARM_GOCACHE" "$PRIVATE_GOMODCACHE"; do [[ "$(stat -c %a "$p")" == 700 ]] || exit 125; done
[[ -f "$SOURCE_PAIR" && "$(stat -c %a "$SOURCE_PAIR")" == 600 ]] || exit 125
for f in outer.stdout outer.stderr outer.pid outer.exit outer.exit.pending outer.started-at outer.finished-at launcher.sha256 launcher.stat warm-gocache.before.stat private-gomodcache.before.stat; do [[ ! -e "$runtime/capture/$f" && ! -L "$runtime/capture/$f" ]] || exit 125; done
printf '%s\n' "$$" > "$runtime/capture/outer.pid"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/capture/outer.started-at"
sha256sum "$body" "$0" "$SOURCE_PAIR" > "$runtime/capture/launcher.sha256"
stat -c '%a %s %i %n' "$body" "$0" "$SOURCE_PAIR" > "$runtime/capture/launcher.stat"
stat -c '%F %a %s %i %n' "$WARM_GOCACHE" > "$runtime/capture/warm-gocache.before.stat"
stat -c '%F %a %s %i %n' "$PRIVATE_GOMODCACHE" > "$runtime/capture/private-gomodcache.before.stat"
set +e
bash "$body" > "$runtime/capture/outer.stdout" 2> "$runtime/capture/outer.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$runtime/capture/outer.exit.pending"
mv -- "$runtime/capture/outer.exit.pending" "$runtime/capture/outer.exit"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/capture/outer.finished-at"
exit "$status"
