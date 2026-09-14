#!/usr/bin/env bash
set -u -o pipefail
umask 077
base='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/active4-race-confirmations-v7'; id='active4-race-p2'; command='/home/by/urnetwork/temp/sn-carried-fleet-qualification-20260913/runtime/active4-race-confirmations-v7/commands/active4-race-p2.command'; outer="$base/outer/$id"; tmp="$base/tmp/$id"; gotmp="$base/gotmp/$id"
for p in "$base" "$base/commands" "$base/outer" "$base/tmp" "$base/gotmp" "$command" "$tmp" "$gotmp"; do [[ -e "$p" && ! -L "$p" ]] || exit 125; done
for p in "$base" "$base/commands" "$base/outer" "$base/tmp" "$base/gotmp" "$tmp" "$gotmp"; do [[ "$(stat -c '%a' "$p")" == 700 ]] || exit 125; done
for suffix in pid started-at inputs.sha256 stdout stderr exit finished-at; do [[ ! -e "$outer.$suffix" && ! -L "$outer.$suffix" ]] || exit 125; done
printf '%s\n' "$$" > "$outer.pid"
date -u +%Y-%m-%dT%H:%M:%SZ > "$outer.started-at"
sha256sum "$command" "$base/confirmations/$id/INPUTS.sha256" "$base/confirmations/$id/parent-proofs.sha256" > "$outer.inputs.sha256"
set +e
env TMPDIR="$tmp" GOTMPDIR="$gotmp" RELEASE_GATE_DIAGNOSTIC=0 "$command" > "$outer.stdout" 2> "$outer.stderr"
status=$?
set -e
printf '%s\n' "$status" > "$outer.exit"
date -u +%Y-%m-%dT%H:%M:%SZ > "$outer.finished-at"
exit "$status"
