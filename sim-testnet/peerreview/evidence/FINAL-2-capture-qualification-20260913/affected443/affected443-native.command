#!/usr/bin/env bash
set -euo pipefail
umask 077
runtime=/home/by/urnetwork/temp/sn-capture-serial-owners-20260913/runtime-capture-b78-qualification-20260913T1146Z-v5
workspace=/home/by/urnetwork/temp/sn-capture-serial-owners-20260913/workspace-physical-b78
sn_repo="$workspace/sn"
selector=$(< "$runtime/input/capture.selector.txt")
snapshot() {
  local output="$1" repo root head parent upstream remote status origin manifest
  printf '%s\n' $'repo\troot\thead\tupstream_or_parent\tremote_head\torigin\tstatus_bytes\tstatus_sha256\tmanifest' > "$output"
  for repo in sn server operator-proxy connect sdk glog goidenticons proxy userwireguard vault xops config; do
    if [[ "$repo" == vault ]]; then root=/home/by/urnetwork/vault; else root="$workspace/$repo"; fi
    [[ -d "$root" && ! -L "$root" && -d "$root/.git" && ! -L "$root/.git" ]] || return 125
    head=$(git -C "$root" rev-parse --verify 'HEAD^{commit}')
    status=$(git -C "$root" status --porcelain=v1 --untracked-files=all)
    [[ -z "$status" ]] || return 125
    origin=$(git -C "$root" config --get remote.origin.url)
    if [[ "$repo" == sn ]]; then
      [[ "$head" == b78b672c37db0076513b25c613d5102cc0d16574 ]] || return 125
      parent=$(git -C "$root" rev-parse --verify 'HEAD^')
      [[ "$parent" == 0dcb5c85408f2020adeae94fe5dd912722eaf63d ]] || return 125
      upstream="frozen-unpublished-parent/$parent"; remote=''
      manifest=sn-b78.source.manifest
    else
      upstream=$(git -C "$root" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}')
      remote=$(git -C "$root" rev-parse --verify "$upstream^{commit}")
      [[ "$head" == "$remote" ]] || return 125
      if [[ "$repo" == config ]]; then manifest=config.qualification.source.manifest; else manifest="$repo.v2.source.manifest"; fi
    fi
    printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "$repo" "$root" "$head" "$upstream" "$remote" "$origin" "${#status}" "$(printf '%s' "$status" | sha256sum | awk '{print $1}')" "$manifest" >> "$output"
  done
}
for p in "$runtime" "$runtime/custody" "$runtime/input" "$runtime/commands" "$runtime/outer" "$runtime/tmp" "$runtime/gotmp" "$runtime/gocache"; do
  [[ -d "$p" && ! -L "$p" && "$(stat -c '%a' "$p")" == 700 ]] || exit 125
done
[[ -d "$sn_repo" && ! -L "$sn_repo" && -d "$sn_repo/.git" && ! -L "$sn_repo/.git" ]] || exit 125
[[ -f "$runtime/custody/sn-b78.source.manifest" && ! -L "$runtime/custody/sn-b78.source.manifest" && "$(stat -c '%a' "$runtime/custody/sn-b78.source.manifest")" == 600 ]] || exit 125
sha256sum --strict -c "$runtime/custody/PREPARED.sha256"
snapshot "$runtime/custody/source.native.before.tsv"
cmp -s "$runtime/custody/source.before.tsv" "$runtime/custody/source.native.before.tsv"
( cd "$sn_repo" && sha256sum --strict -c "$runtime/custody/sn-b78.source.manifest" )
cat "$runtime/input/capture.roots.txt" "$runtime/input/capture-evidence.roots.txt" "$runtime/input/capture-renewal.roots.txt" "$runtime/input/capture-revision.roots.txt" "$runtime/input/capture-lifecycle.roots.txt" "$runtime/input/capture-private.roots.txt" "$runtime/input/capture-prior.roots.txt" "$runtime/input/capture-population.roots.txt" "$runtime/input/capture-metadata.roots.txt" | LC_ALL=C sort > "$runtime/custody/capture-phase-union.native.txt"
awk 'seen[$0]++ {bad=1} END {exit bad}' "$runtime/custody/capture-phase-union.native.txt" > "$runtime/custody/capture-phase-duplicates.native.txt"
cmp -s "$runtime/custody/capture-phase-union.native.txt" "$runtime/custody/capture-all454.sorted.txt"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/custody/capture-list.started-at"
(
  cd "$sn_repo"
  env GOMAXPROCS=4 GOFLAGS='-mod=readonly -p=4' GOPROXY=off GOSUMDB=off GOWORK=off GOENV=off GOTOOLCHAIN=local GOVCS='*:off' TMPDIR="$runtime/tmp" GOTMPDIR="$runtime/gotmp" GOCACHE="$runtime/gocache" GOMODCACHE=/home/by/go/pkg/mod go test ./sim-testnet -list "$selector"
) > "$runtime/custody/capture-list.stdout" 2> "$runtime/custody/capture-list.stderr"
sed -n '/^Test/p' "$runtime/custody/capture-list.stdout" | LC_ALL=C sort > "$runtime/custody/capture-list.actual.txt"
cmp -s "$runtime/custody/capture-all454.sorted.txt" "$runtime/custody/capture-list.actual.txt"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/custody/capture-list.finished-at"
export WARP_TEST_ENV_FAIL_FAST=1
export TMPDIR="$runtime/tmp" GOTMPDIR="$runtime/gotmp" GOCACHE="$runtime/gocache"
export GOPROXY=off GOSUMDB=off GOWORK=off GOENV=off GOTOOLCHAIN=local GOVCS='*:off' GOMODCACHE=/home/by/go/pkg/mod
export RELEASE_GATE_CONCURRENT_GATES=2 RELEASE_GATE_JOBS=3 RELEASE_GATE_DIAGNOSTIC=0
source "$sn_repo/scripts/release-gate-jobs.sh"
release_gate_jobs_init
printf '%s\n' "$release_gate_root" > "$runtime/custody/native-gate-root"
source "$runtime/commands/capture-affected443.native.sh"
status=0
release_gate_complete || status=$?
trap - EXIT INT TERM
snapshot "$runtime/custody/source.native.after.tsv" || status=$?
( cd "$sn_repo" && sha256sum --strict -c "$runtime/custody/sn-b78.source.manifest" ) || status=$?
cmp -s "$runtime/custody/source.native.before.tsv" "$runtime/custody/source.native.after.tsv" || status=$?
printf '%s\n' "$status" > "$runtime/custody/native.status"
exit "$status"
