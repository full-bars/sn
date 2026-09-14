#!/usr/bin/env bash
set -u -o pipefail
umask 077
runtime='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime'
base="$runtime/active4-race-confirmations-v7"; id=active4-race-p2; directory="$base/confirmations/$id"
runner='/home/by/urnetwork/temp/sn-initial-six-qualification-20260913/helper-go-tools-ebe70a/runtime/bin/qualification-ebe70a'
parent="$runtime/captures/active4-race-p1-v3"; parent_outer="$runtime/outer/active4-race-p1-v3/outer.exit"
binary="$parent/build-sim-testnet-race.testbin"; converter="$parent/test2json"
source_root='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/workspace-physical/sn'
workspace='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/workspace-physical'
source_jobs="$source_root/scripts/release-gate-jobs.sh"
[[ -x "$runner" && -x "$binary" && -x "$converter" && -f "$source_jobs" ]] || exit 125
sha256sum --check --strict --quiet "$directory/INPUTS.sha256" || exit 125
sha256sum --check --strict --quiet "$directory/parent-proofs.sha256" || exit 125
sha256sum --check --strict --quiet "$runner.sha256" || exit 125
[[ "$(cat "$parent_outer")" == 0 ]] || exit 125
grep -Fq '"state":"passed"' "$parent/status.json" || exit 125
grep -Fq '"source_unchanged":true' "$parent/status.json" || exit 125
cmp -s "$parent/source.before.json" "$parent/source.after.json" || exit 125
write_state() {
  local output="$1" name repo expected_head expected_manifest expected_status manifest actual_manifest actual_head actual_status
  : > "$output" || return 1
  while IFS=$'\t' read -r name repo expected_head expected_manifest expected_status manifest; do
    actual_manifest="$(sha256sum "$manifest" | awk '{print $1}')" || return 1
    actual_head="$(env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 git --no-pager --no-optional-locks -c core.fsmonitor=false -c core.untrackedCache=false -C "$repo" rev-parse HEAD)" || return 1
    actual_status="$(env GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 git --no-pager --no-optional-locks -c core.fsmonitor=false -c core.untrackedCache=false -C "$repo" status --porcelain=v1 -z | sha256sum | awk '{print $1}')" || return 1
    [[ "$actual_head" == "$expected_head" && "$actual_manifest" == "$expected_manifest" && "$actual_status" == "$expected_status" ]] || return 1
    printf '%s\t%s\t%s\t%s\t%s\n' "$name" "$repo" "$actual_head" "$actual_manifest" "$actual_status" >> "$output" || return 1
  done < "$directory/source-roots.tsv"
}
write_state "$directory/source.before.tsv" || exit 125
sha256sum "$binary" > "$directory/binary.before.sha256" || exit 125
stat -c '%a %s %n' "$binary" > "$directory/binary.before.stat" || exit 125
cmp "$directory/binary.expected.stat" "$directory/binary.before.stat" || exit 125
sha256sum "$converter" > "$directory/converter.before.sha256" || exit 125
stat -c '%a %s %n' "$converter" > "$directory/converter.before.stat" || exit 125
cmp "$directory/converter.expected.stat" "$directory/converter.before.stat" || exit 125
run_owned() {
  local stage="$1"
  local request="$directory/$stage.request.json"
  local joined="$directory/$stage.joined"
  [[ ! -e "$joined" ]] || return 125
  bash -c 'set -euo pipefail
sn_repo=$1; workspace=$2; export QUALIFICATION_RUNNER=$3 QUALIFICATION_REQUEST=$4
source "$5"
release_gate_jobs_init
qualification_phase() { "$QUALIFICATION_RUNNER" worker "$QUALIFICATION_REQUEST"; }
release_gate_start qualification qualification_phase
qualification_status=0
release_gate_complete || qualification_status=$?
if (( release_gate_active == 0 )) && [[ "$release_gate_services_cleaned" == 1 ]]; then
  (set -o noclobber; printf "joined\n" > "$6") || exit 125
else
  exit 125
fi
exit "$qualification_status"' retained-binary-owner "$source_root" "$workspace" "$runner" "$request" "$source_jobs" "$joined"
}
run_owned list; list_status=$?
printf '%s\n' "$list_status" > "$directory/list.outer.exit"
(( list_status == 0 )) || exit "$list_status"
cmp "$directory/expected-list.txt" "$directory/list.stdout" || exit 1
run_owned body; body_status=$?
printf '%s\n' "$body_status" > "$directory/body.outer.exit"
(( body_status == 0 )) || exit "$body_status"
run_owned events; events_status=$?
printf '%s\n' "$events_status" > "$directory/events.outer.exit"
(( events_status == 0 )) || exit "$events_status"
set +e
"$runner" replay "$directory/events.stdout" "$directory/outcomes.tsv" "$directory/literals.tsv" github.com/urfoundation/sn/sim-testnet "$directory/body.outer.exit" > "$directory/replay.stdout" 2> "$directory/replay.stderr"
replay_status=$?
set -e
printf '%s\n' "$replay_status" > "$directory/replay.exit"
(( replay_status == 0 )) || exit "$replay_status"
sha256sum "$binary" > "$directory/binary.after.sha256" || exit 125
stat -c '%a %s %n' "$binary" > "$directory/binary.after.stat" || exit 125
cmp "$directory/binary.before.sha256" "$directory/binary.after.sha256" || exit 1
cmp "$directory/binary.before.stat" "$directory/binary.after.stat" || exit 1
sha256sum "$converter" > "$directory/converter.after.sha256" || exit 125
stat -c '%a %s %n' "$converter" > "$directory/converter.after.stat" || exit 125
cmp "$directory/converter.before.sha256" "$directory/converter.after.sha256" || exit 1
cmp "$directory/converter.before.stat" "$directory/converter.after.stat" || exit 1
write_state "$directory/source.after.tsv" || exit 125
cmp "$directory/source.before.tsv" "$directory/source.after.tsv" || exit 1
printf 'pass\n' > "$directory/result"
