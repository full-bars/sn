#!/usr/bin/env bash
set -u -o pipefail
umask 077
source_root='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/workspace-physical/sn'
source_manifest='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/custody/manifests/sn.source.manifest'
body='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/commands/guards7-race-p1.command'
plan='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/plans/guards7-race-p1.json'
outer='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/outer/guards7-race-p1'
tmp='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/tmp/guards7-race-p1'
gotmp='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/gotmp/guards7-race-p1'
gocache='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/gocache/guards7-race-p1'
gomodcache='/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/modcache-v2'
capture='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/captures/guards7-race-p1'
[[ ! -e "$capture" && ! -e "$outer" ]] || exit 125
for dir in "$tmp" "$gotmp" "$gocache" "$gomodcache"; do
  [[ -d "$dir" && ! -L "$dir" && "$(stat -c '%a' "$dir")" == 700 ]] || exit 125
done
mkdir -m 700 "$outer" || exit 125
printf '%s\n' "$$" > "$outer/outer.pid"
date -u +%Y-%m-%dT%H:%M:%SZ > "$outer/started-at"
sha256sum "$body" "$plan" "$source_manifest" '/home/by/urnetwork/temp/sn-initial-six-qualification-20260913/helper-go-tools-ebe70a/runtime/bin/qualification-ebe70a.sha256' '/home/by/urnetwork/temp/sn-carried-fleet-preparation-20260913/handoff-v1/HANDOFF.sha256' > "$outer/input.sha256"
set +e
env TMPDIR="$tmp" GOTMPDIR="$gotmp" GOCACHE="$gocache" GOMODCACHE="$gomodcache" GOMAXPROCS=4 GOFLAGS='-mod=readonly -p=4' GOPROXY=off GOSUMDB=off GONOPROXY=none GONOSUMDB=none GOPRIVATE='' GOVCS='*:off' GOWORK=off GOENV=off GOTOOLCHAIN=local RELEASE_GATE_CONCURRENT_GATES=2 RELEASE_GATE_JOBS=1 RELEASE_GATE_DIAGNOSTIC=0 bash "$source_root/scripts/run-qualification-capture.sh" "$source_root" "$source_manifest" "$body" > "$outer/outer.stdout" 2> "$outer/outer.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$outer/outer.exit"
date -u +%Y-%m-%dT%H:%M:%SZ > "$outer/finished-at"
exit "$status"
