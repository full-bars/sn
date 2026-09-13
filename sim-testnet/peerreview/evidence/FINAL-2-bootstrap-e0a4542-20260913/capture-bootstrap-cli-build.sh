#!/usr/bin/env bash
set -u -o pipefail
umask 077
runtime='/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/runtime-bootstrap-cli-e0a4542-20260913T1848Z-v2'
body="$runtime/commands/bootstrap-cli-build.command"
source_root='/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/workspace-physical/sn'
warm_cache='/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/runtime/qualification-warp-admission-v1/gocache/normal'
modcache='/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/runtime/qualification-warp-admission-v1/cache-prep-current16-v2/modcache'
for path in "$runtime" "$runtime/commands" "$runtime/capture" "$runtime/meta" "$runtime/logs" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin" "$body" "$0" "$warm_cache" "$modcache"; do
  [[ -e "$path" && ! -L "$path" ]] || exit 125
done
for path in "$runtime" "$runtime/commands" "$runtime/capture" "$runtime/meta" "$runtime/logs" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin" "$warm_cache" "$modcache"; do
  [[ "$(stat -c '%a' "$path")" == 700 ]] || exit 125
done
for artifact in outer.stdout outer.stderr outer.pid outer.exit outer.exit.pending outer.started-at outer.finished-at launcher.sha256 launcher.stat warm-gocache.before.stat private-gomodcache.before.stat; do
  [[ ! -e "$runtime/capture/$artifact" && ! -L "$runtime/capture/$artifact" ]] || exit 125
done
printf '%s\n' "$$" > "$runtime/capture/outer.pid"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/capture/outer.started-at"
sha256sum "$body" "$0" "$source_root/sim-testnet/README.md" > "$runtime/capture/launcher.sha256"
stat -c '%a %s %i %n' "$body" "$0" "$source_root/sim-testnet/README.md" > "$runtime/capture/launcher.stat"
stat -c '%F %a %s %i %n' "$warm_cache" > "$runtime/capture/warm-gocache.before.stat"
stat -c '%F %a %s %i %n' "$modcache" > "$runtime/capture/private-gomodcache.before.stat"
set +e
bash "$body" > "$runtime/capture/outer.stdout" 2> "$runtime/capture/outer.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$runtime/capture/outer.exit.pending"
mv -- "$runtime/capture/outer.exit.pending" "$runtime/capture/outer.exit"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/capture/outer.finished-at"
exit "$status"
