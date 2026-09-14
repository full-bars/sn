#!/usr/bin/env bash
# Frozen, capture-local adaptation of the completed b580 owner. Terra executes.
set -u -o pipefail
umask 077
workspace=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/workspace-physical
server=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/workspace-physical/server
runtime=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/published-monitor1-race-p1
runner=/home/by/urnetwork/temp/sn-initial-six-qualification-20260913/helper-go-tools-ebe70a/runtime/bin/qualification-ebe70a
runner_receipt=/home/by/urnetwork/temp/sn-initial-six-qualification-20260913/helper-go-tools-ebe70a/runtime/bin/qualification-ebe70a.sha256
declarations=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/monitor-inputs.sha256
source_table=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/inputs/sources.tsv
source_hashes=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/inputs/source-manifests.sha256
root_list=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/monitor-inputs/published-monitor1.roots.txt
selector='^TestPublishedMigrationMonitorExtenderArtifacts$'
outcomes=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/monitor-inputs/published-monitor1.outcomes.tsv
literals=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/monitor-inputs/published-monitor1.literals.tsv
binary=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/server-0f095a-v1/published-monitor1-race-p1/capture/server-race.testbin
package=.
import_path=github.com/urnetwork/server
gomodcache=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/modcache-v2
gocache=/home/by/urnetwork/temp/sn-server-migration-monitor-qualification-20260913/runtime/cohort-diagnostic-b580-normal-p1/gocache
outer="$runtime/outer"
capture="$runtime/capture"
lock="$workspace/sn/deploy/testnet/release.lock.yml"
[[ ! -e "$outer" && ! -e "$capture" ]] || exit 125
[[ -x "$runner" && -f "$runner_receipt" ]] || exit 125
sha256sum --check --strict --quiet "$runner_receipt" || exit 125
sha256sum --check --strict --quiet "$declarations" || exit 125
for directory in "$runtime" "$runtime/tmp" "$runtime/gotmp" "$gomodcache" "$gocache"; do
  [[ -d "$directory" && ! -L "$directory" && "$(stat -c '%a' "$directory")" == 700 ]] || exit 125
done
mkdir -m 700 "$outer" "$capture" || exit 125
printf '%s\n' "$$" > "$outer/outer.pid"
date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$outer/started-at"
sha256sum "$declarations" "$runner_receipt" "$source_table" "$source_hashes" "$lock" > "$outer/input.sha256"
source "$server/local/release-gate-services.sh"
services_started=0
cleanup_status=0
cleanup() {
  if (( services_started )); then
    release_gate_services_cleanup > "$outer/cleanup.stdout" 2> "$outer/cleanup.stderr" || cleanup_status=$?
  fi
}
trap cleanup EXIT
export TMPDIR="$runtime/tmp" GOTMPDIR="$runtime/gotmp" GOCACHE="$gocache" GOMODCACHE="$gomodcache" GOMAXPROCS=4 GOFLAGS='-mod=readonly -p=4' GOPROXY=off GOSUMDB=off GONOPROXY=none GONOSUMDB=none GOPRIVATE='' GOVCS='*:off' GOWORK=off GOENV=off GOTOOLCHAIN=local
export WARP_ENV=local WARP_SERVICE=test WARP_DOMAIN=bringyour.com WARP_BLOCK=test WARP_VERSION=0.0.0 WARP_TEST_ENV_FAIL_FAST=1
fence_status=0
sha256sum --check --strict --quiet "$source_hashes" > "$outer/source.before.manifests.stdout" 2> "$outer/source.before.manifests.stderr" || fence_status=1
while IFS=$'\t' read -r repo expected_head manifest; do
  observed_head="$(git -C "$workspace/$repo" rev-parse HEAD)" || fence_status=1
  printf '%s\t%s\n' "$repo" "$observed_head" >> "$outer/source.before.heads.tsv"
  [[ "$observed_head" == "$expected_head" ]] || fence_status=1
  "$runner" fence "$workspace/$repo" "$manifest" > "$outer/source.before.$repo.json" 2> "$outer/source.before.$repo.stderr" || fence_status=1
done < "$source_table"
printf '%s\n' "$fence_status" > "$outer/fence.before.exit"
build_status=125
if (( fence_status == 0 )); then
  printf 'go test -c -race -p=4 -o %s .\n' "$binary" > "$outer/build.command.txt"
  date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$outer/build.started-at"
  (cd "$server/$package" && timeout --kill-after=30s 1200s go test -c -race -p=4 -o "$binary" .) > "$capture/build.stdout" 2> "$capture/build.stderr"
  build_status=$?
  date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$outer/build.finished-at"
  if (( build_status == 0 )); then
    sha256sum "$binary" > "$binary.sha256" || build_status=1
    stat -c '%a %s %n' "$binary" > "$binary.mode-and-size.txt" || build_status=1
  fi
fi
printf '%s\n' "$build_status" > "$outer/build.exit"
services_status=125
if (( fence_status == 0 && build_status == 0 )); then
  services_started=1
  release_gate_services_start "$outer" "$workspace" "$lock" > "$outer/services.stdout" 2> "$outer/services.stderr"
  services_status=$?
  if (( services_status == 0 )); then
    RELEASE_GATE_SERVICE_ENV="$release_gate_service_root/environment.sh"
    if [[ ! -f "$RELEASE_GATE_SERVICE_ENV" || -L "$RELEASE_GATE_SERVICE_ENV" ]]; then
      services_status=125
    else
      cp --preserve=mode,timestamps "$RELEASE_GATE_SERVICE_ENV" "$outer/environment.sh" || services_status=1
      source "$RELEASE_GATE_SERVICE_ENV" || services_status=1
      source "$server/test-env.sh" > "$outer/test-env.stdout" 2> "$outer/test-env.stderr" || services_status=1
      test_env_validate_suite_resource_manifest "$TEST_ENV_SUITE_RESOURCE_MANIFEST" "$WARP_VAULT_HOME" "$WARP_CONFIG_HOME" >> "$outer/test-env.stdout" 2>> "$outer/test-env.stderr" || services_status=1
    fi
  fi
fi
printf '%s\n' "$services_status" > "$outer/services.exit"
list_status=125
body_status=125
convert_status=125
verify_status=125
if (( fence_status == 0 && build_status == 0 && services_status == 0 )); then
  printf '%s -test.list=%q\n' "$binary" "$selector" > "$outer/list.command.txt"
  (cd "$server/$package" && timeout --kill-after=30s 1200s "$binary" "-test.list=$selector") > "$capture/list.stdout" 2> "$capture/list.stderr"
  list_status=$?
  if (( list_status == 0 )); then
    LC_ALL=C sort "$capture/list.stdout" > "$capture/compiled.roots.txt" || list_status=1
    cmp "$root_list" "$capture/compiled.roots.txt" > "$capture/list.compare.stdout" 2> "$capture/list.compare.stderr" || list_status=1
    [[ -s "$capture/compiled.roots.txt" ]] || list_status=1
    sha256sum --check --strict --quiet "$binary.sha256" >> "$capture/list.compare.stdout" 2>> "$capture/list.compare.stderr" || list_status=1
  fi
fi
printf '%s\n' "$list_status" > "$outer/list.exit"
if (( list_status == 0 )); then
  printf '%s -test.run=%q -test.v=test2json -test.count=1 -test.parallel=4 -test.timeout=900s\n' "$binary" "$selector" > "$outer/body.command.txt"
  date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$outer/body.started-at"
  (cd "$server/$package" && timeout --kill-after=30s 960s "$binary" "-test.run=$selector" -test.v=test2json -test.count=1 -test.parallel=4 -test.timeout=900s) > "$capture/body.stdout" 2> "$capture/body.stderr"
  body_status=$?
  date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$outer/body.finished-at"
  printf '%s\n' "$body_status" > "$outer/body.exit"
  (cd "$server/$package" && timeout --kill-after=30s 1200s go tool test2json -t -p "$import_path" < "$capture/body.stdout") > "$capture/events.json" 2> "$capture/events.stderr"
  convert_status=$?
  if (( convert_status == 0 )); then
    "$runner" replay "$capture/events.json" "$outcomes" "$literals" "$import_path" "$outer/body.exit" > "$capture/verification.json" 2> "$capture/verification.stderr"
    verify_status=$?
  fi
  sha256sum --check --strict --quiet "$binary.sha256" > "$outer/binary.after.stdout" 2> "$outer/binary.after.stderr" || verify_status=1
fi
printf '%s\n' "$body_status" > "$outer/body.exit"
printf '%s\n' "$convert_status" > "$outer/convert.exit"
printf '%s\n' "$verify_status" > "$outer/verify.exit"
cleanup
services_started=0
printf '%s\n' "$cleanup_status" > "$outer/cleanup.exit"
after_status=0
sha256sum --check --strict --quiet "$source_hashes" > "$outer/source.after.manifests.stdout" 2> "$outer/source.after.manifests.stderr" || after_status=1
while IFS=$'\t' read -r repo expected_head manifest; do
  observed_head="$(git -C "$workspace/$repo" rev-parse HEAD)" || after_status=1
  printf '%s\t%s\n' "$repo" "$observed_head" >> "$outer/source.after.heads.tsv"
  [[ "$observed_head" == "$expected_head" ]] || after_status=1
  "$runner" fence "$workspace/$repo" "$manifest" > "$outer/source.after.$repo.json" 2> "$outer/source.after.$repo.stderr" || after_status=1
done < "$source_table"
printf '%s\n' "$after_status" > "$outer/fence.after.exit"
status=0
if (( fence_status != 0 || build_status != 0 || services_status != 0 || list_status != 0 || body_status != 0 || convert_status != 0 || verify_status != 0 || cleanup_status != 0 || after_status != 0 )); then status=1; fi
printf '%s\n' "$status" > "$outer/outer.exit"
date -u +%Y-%m-%dT%H:%M:%S.%NZ > "$outer/finished-at"
exit "$status"
