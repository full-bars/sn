#!/usr/bin/env bash
set -euo pipefail
umask 077
runtime='/home/by/urnetwork/temp/sn-warp-admission-integration-20260913/runtime-final-cli-prepared-unbound-20260913T1851Z'
: "$FINAL_WORKSPACE" "$SOURCE_PAIR" "$EXPECTED_SN" "$WARM_GOCACHE" "$PRIVATE_GOMODCACHE"
workspace="$FINAL_WORKSPACE"
source_root="$workspace/sn"
binary="$runtime/bin/sim-testnet-final"
snapshot() {
  local output="$1" repo root physical head status status_sha gitdir status_len
  : > "$output"
  for repo in sn server operator-proxy connect sdk glog goidenticons proxy userwireguard vault xops config warp; do
    root="$workspace/$repo"
    [[ -d "$root" && -e "$root/.git" && ! -L "$root" ]] || return 125
    physical="$(readlink -f -- "$root")"
    [[ -d "$physical" && -e "$physical/.git" ]] || return 125
    gitdir="$(git -C "$root" rev-parse --absolute-git-dir)"
    [[ -d "$gitdir" ]] || return 125
    head="$(git -C "$root" rev-parse --verify 'HEAD^{commit}')"
    status="$(git -C "$root" status --porcelain=v1 --untracked-files=all)"
    [[ -z "$status" ]] || return 125
    status_sha="$(printf '%s' "$status" | sha256sum | awk '{print $1}')"
    status_len="$(printf '%s' "$status" | wc -c)"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' "$repo" "$root" "$physical" "$head" "$status_len" "$status_sha" >> "$output"
  done
}
for p in "$runtime" "$runtime/tmp" "$runtime/gotmp" "$runtime/bin" "$WARM_GOCACHE" "$PRIVATE_GOMODCACHE"; do [[ -d "$p" && ! -L "$p" && "$(stat -c %a "$p")" == 700 ]] || exit 125; done
[[ "$EXPECTED_SN" =~ ^[0-9a-f]{40}$ && -d "$source_root/.git" && ! -L "$source_root" && -f "$SOURCE_PAIR" && ! -L "$SOURCE_PAIR" && "$(stat -c %a "$SOURCE_PAIR")" == 600 && ! -e "$binary" ]] || exit 125
[[ "$(git -C "$source_root" rev-parse HEAD)" == "$EXPECTED_SN" ]] || exit 125
[[ "$(wc -l < "$SOURCE_PAIR")" == 13 ]] || exit 125
snapshot "$runtime/meta/repos.before.tsv"
cmp -s "$SOURCE_PAIR" "$runtime/meta/repos.before.tsv" || exit 125
sha256sum "$runtime/meta/repos.before.tsv" > "$runtime/meta/repos.before.sha256"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/logs/build.started-at"
set +e
(
  cd "$source_root"
  exec env GOMAXPROCS=4 GOFLAGS='-mod=readonly -p=4' GOPROXY=off GOSUMDB=off GOWORK=off GOENV=off GOTOOLCHAIN=local GOVCS='*:off' TMPDIR="$runtime/tmp" GOTMPDIR="$runtime/gotmp" GOCACHE="$WARM_GOCACHE" GOMODCACHE="$PRIVATE_GOMODCACHE" go build -trimpath -buildvcs=true -o "$binary" ./sim-testnet
) > "$runtime/logs/build.stdout" 2> "$runtime/logs/build.stderr"
build_status=$?
set -e
printf '%s\n' "$build_status" > "$runtime/logs/build.exit"
date -u +%Y-%m-%dT%H:%M:%SZ > "$runtime/logs/build.finished-at"
snapshot "$runtime/meta/repos.after.tsv"
cmp -s "$runtime/meta/repos.before.tsv" "$runtime/meta/repos.after.tsv"
sha256sum "$runtime/meta/repos.after.tsv" > "$runtime/meta/repos.after.sha256"
(( build_status == 0 )) || exit "$build_status"
[[ -f "$binary" && ! -L "$binary" && "$(stat -c %a "$binary")" == 700 ]] || exit 1
sha256sum "$binary" > "$runtime/meta/cli.sha256"
stat -c '%a %s %i %n' "$binary" > "$runtime/meta/cli.stat"
go version -m "$binary" > "$runtime/meta/cli.buildinfo.txt"
rg -q '^\s*build\s+-trimpath=true$' "$runtime/meta/cli.buildinfo.txt"
rg -q '^\s*build\s+vcs=git$' "$runtime/meta/cli.buildinfo.txt"
rg -q "^\s*build\s+vcs.revision=$EXPECTED_SN$" "$runtime/meta/cli.buildinfo.txt"
rg -q '^\s*build\s+vcs.modified=false$' "$runtime/meta/cli.buildinfo.txt"
before_sha="$(awk '{print $1}' "$runtime/meta/repos.before.sha256")"
after_sha="$(awk '{print $1}' "$runtime/meta/repos.after.sha256")"
binary_sha="$(awk '{print $1}' "$runtime/meta/cli.sha256")"
binary_bytes="$(stat -c %s "$binary")"
printf '{\n  "kind":"final-stamped-sim-testnet-cli-build",\n  "build_exit":0,\n  "repos_before_after_identical":true,\n  "repos_before_sha256":"%s",\n  "repos_after_sha256":"%s",\n  "artifact":{"path":"%s","sha256":"%s","bytes":%s,"mode":"0700"},\n  "vcs":{"revision":"%s","trimpath":true,"buildvcs":true,"modified":false},\n  "warm_gocache":"%s",\n  "private_gomodcache":"%s"\n}\n' "$before_sha" "$after_sha" "$binary" "$binary_sha" "$binary_bytes" "$EXPECTED_SN" "$WARM_GOCACHE" "$PRIVATE_GOMODCACHE" > "$runtime/RESULT-FINAL-CLI-BUILD.json"
sha256sum "$runtime/RESULT-FINAL-CLI-BUILD.json" > "$runtime/RESULT-FINAL-CLI-BUILD.json.sha256"
