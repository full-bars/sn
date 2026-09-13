#!/usr/bin/env bash
set -u -o pipefail
umask 077
runtime=/home/by/urnetwork/temp/sn-dependency-ancestry-admission-20260913/runtime-bootstrap-cli-ancestry-prepared-20260913T1925Z
body="$runtime/commands/bootstrap-cli-build.command"
for path in "$runtime" "$runtime/commands" "$runtime/capture" "$runtime/meta" "$runtime/logs" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin" "$body" "$0"; do
  [[ -e "$path" && ! -L "$path" ]] || exit 125
done
for path in "$runtime" "$runtime/commands" "$runtime/capture" "$runtime/meta" "$runtime/logs" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin"; do
  [[ "$(stat -c '%a' "$path")" == 700 ]] || exit 125
done
for artifact in outer.stdout outer.stderr outer.pid outer.exit outer.exit.pending outer.started-at outer.finished-at launcher.sha256 launcher.stat warm-gocache.before.stat private-gomodcache.before.stat source-pair.sha256; do
  [[ ! -e "$runtime/capture/$artifact" && ! -L "$runtime/capture/$artifact" ]] || exit 125
done
: "${FINAL_WORKSPACE:?FINAL_WORKSPACE is required}"
: "${SOURCE_PAIR:?SOURCE_PAIR is required}"
: "${EXPECTED_SN:?EXPECTED_SN is required}"
: "${WARM_GOCACHE:?WARM_GOCACHE is required}"
: "${PRIVATE_GOMODCACHE:?PRIVATE_GOMODCACHE is required}"
printf '%s\n' "$$" > "$runtime/capture/outer.pid"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/capture/outer.started-at"
sha256sum "$body" "$0" > "$runtime/capture/launcher.sha256"
stat -c '%a %s %i %n' "$body" "$0" > "$runtime/capture/launcher.stat"
sha256sum "$SOURCE_PAIR" > "$runtime/capture/source-pair.sha256"
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
