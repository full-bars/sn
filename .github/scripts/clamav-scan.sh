#!/usr/bin/env bash
# Report ClamAV results without confusing a detection (exit 1) with an error.
set -euo pipefail

report=${CLAMAV_REPORT_FILE:?CLAMAV_REPORT_FILE is required}
target=${1:?scan target is required}
eicar=$(mktemp)
trap 'rm -f "$eicar"' EXIT
# EICAR contains literal dollar signs and must not undergo shell expansion.
# shellcheck disable=SC2016
printf 'X5O!P%%@AP[4\PZX54(P^)7CC)7}$EICAR-STANDARD-ANTIVIRUS-TEST-FILE!$H+H*' > "$eicar"
printf '### ClamAV\n' > "$report"

if self_output=$(clamscan --no-summary "$eicar" 2>&1); then
  self_rc=0
else
  self_rc=$?
fi
printf '%s\n' "$self_output"
eicar_pattern='[Ee][Ii][Cc][Aa][Rr][^[:cntrl:]]* FOUND'
self_ok=0
if [[ $self_rc -eq 1 && $self_output =~ $eicar_pattern ]]; then
  self_ok=1
  echo 'EICAR self-test passed (scanner detected EICAR)'
else
  echo "::error::EICAR self-test failed (exit $self_rc); scanner is not trusted"
  printf 'Scanner failure: EICAR self-test failed (exit %s).\n' "$self_rc" >> "$report"
fi

if [[ -d $target ]]; then
  if ! first_file=$(find "$target" -type f -print -quit 2>&1); then
    echo "::error::ClamAV could not inspect the scan target: $first_file"
    echo 'Scanner failure: could not inspect the scan target.' >> "$report"
    exit 1
  elif [[ -z $first_file ]]; then
    echo '::error::ClamAV scan target contains no files'
    echo 'Scanner failure: scan target contains no files.' >> "$report"
    exit 1
  fi
fi

if scan_output=$(clamscan --infected --no-summary -r "$target" 2>&1); then
  scan_rc=0
else
  scan_rc=$?
fi
if [[ -n $scan_output ]]; then
  printf '%s\n' "$scan_output"
  # Markdown fences are literal, not shell command substitutions.
  # shellcheck disable=SC2016
  printf '```text\n%s\n```\n' "$scan_output" >> "$report"
fi

if [[ $scan_rc -eq 1 && $scan_output == *' FOUND'* ]]; then
  echo '::error::ClamAV detected malware in release artifacts'
  echo 'Detection: ClamAV found malware in release artifacts; see details above.' >> "$report"
  exit 1
elif [[ $scan_rc -ne 0 || $scan_output == *' FOUND'* ]]; then
  echo "::error::ClamAV scanner failure or inconsistent result (exit $scan_rc)"
  printf 'Scanner failure or inconsistent result (exit %s); see scan log.\n' "$scan_rc" >> "$report"
  exit 1
elif [[ $self_ok -ne 1 ]]; then
  exit 1
fi

echo 'ClamAV: clean'
echo 'Clean: no malware detected and EICAR self-test passed.' >> "$report"
