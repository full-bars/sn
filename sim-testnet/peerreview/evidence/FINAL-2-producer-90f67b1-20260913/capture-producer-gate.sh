#!/usr/bin/env bash
# This owner preserves the producer gate's literal command, raw outer streams,
# direct outer PID, and true terminal exit. It deliberately sets no Go budget.
set -u -o pipefail

readonly runtime_root=/home/by/urnetwork/temp/sn-final-gates-b78-prepared-20260913T1216Z/producer
readonly private_tmp="$runtime_root/tmp"
readonly capture_root="$runtime_root/capture"
readonly command_file="$runtime_root/producer-gate.command"
readonly capture_owner="$runtime_root/capture-producer-gate.sh"

if (( $# != 0 )); then
  printf 'usage: %s\n' "$capture_owner" >&2
  exit 64
fi

umask 077
for directory in "$runtime_root" "$private_tmp" "$capture_root"; do
  if [[ ! -d "$directory" || -L "$directory" ]]; then
    printf 'producer gate capture requires an existing non-symlink directory: %s\n' "$directory" >&2
    exit 125
  fi
done
for file in "$command_file" "$capture_owner"; do
  if [[ ! -f "$file" || -L "$file" ]]; then
    printf 'producer gate capture requires an existing non-symlink file: %s\n' "$file" >&2
    exit 125
  fi
done
for artifact in outer.stdout outer.stderr outer.pid outer.exit outer.exit.pending outer.started-at outer.finished-at launcher.sha256 launcher.stat; do
  if [[ -e "$capture_root/$artifact" || -L "$capture_root/$artifact" ]]; then
    printf 'producer gate capture refuses to overwrite retained artifact: %s\n' "$capture_root/$artifact" >&2
    exit 125
  fi
done

date -u +%Y-%m-%dT%H:%M:%SZ > "$capture_root/outer.started-at" || exit 125
sha256sum -- "$command_file" "$capture_owner" > "$capture_root/launcher.sha256" || exit 125
stat -c '%a %s %n' -- "$command_file" "$capture_owner" > "$capture_root/launcher.stat" || exit 125

bash "$command_file" > "$capture_root/outer.stdout" 2> "$capture_root/outer.stderr" &
outer_pid=$!
capture_status=0
if ! printf '%s\n' "$outer_pid" > "$capture_root/outer.pid"; then
  capture_status=125
fi

outer_status=0
if wait "$outer_pid"; then
  outer_status=0
else
  outer_status=$?
fi

if ! printf '%s\n' "$outer_status" > "$capture_root/outer.exit.pending"; then
  capture_status=125
elif ! mv -- "$capture_root/outer.exit.pending" "$capture_root/outer.exit"; then
  capture_status=125
fi
if ! date -u +%Y-%m-%dT%H:%M:%SZ > "$capture_root/outer.finished-at"; then
  capture_status=125
fi
if (( capture_status != 0 )); then
  printf 'producer gate capture failed after outer exit=%s; retained artifacts may be incomplete\n' "$outer_status" >&2
  exit "$capture_status"
fi
exit "$outer_status"
