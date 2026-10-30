#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "$0")/../.." && pwd)
tmp=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/clamav-test.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/bin" "$tmp/artifacts" "$tmp/empty" "$tmp/db"
# The scanner refuses to run without a database; the harness supplies one so
# the case table tests verdict handling rather than signature freshness.
printf 'test\n' > "$tmp/db/main.cvd"
printf 'safe\n' > "$tmp/artifacts/provider"
cat > "$tmp/bin/clamscan" <<'MOCK'
#!/usr/bin/env bash
if [[ $1 != --infected ]]; then
  case "$CASE" in
    self-clean) echo "$2: OK"; exit 0 ;;
    self-error) echo 'database unavailable' >&2; exit 2 ;;
    self-wrong) echo "$2: Other.Test FOUND"; exit 1 ;;
    *) echo "$2: Eicar-Test-Signature FOUND"; exit 1 ;;
  esac
fi
case "$CASE" in
  detection) echo 'release_tmp/provider: Test.Malware FOUND'; exit 1 ;;
  scanner-error) echo 'database unavailable' >&2; exit 2 ;;
  error-with-detection) echo 'release_tmp/provider: Test.Malware FOUND'; echo 'read error' >&2; exit 2 ;;
  inconsistent) echo 'release_tmp/provider: Test.Malware FOUND'; exit 0 ;;
  missing-detection) echo 'scanner returned 1 without a match' >&2; exit 1 ;;
  *) exit 0 ;;
esac
MOCK
chmod +x "$tmp/bin/clamscan"

for case_name in clean detection scanner-error self-clean self-error self-wrong inconsistent missing-detection error-with-detection; do
  report="$tmp/$case_name.report"
  if output=$(PATH="$tmp/bin:$PATH" CASE="$case_name" CLAMAV_DB_DIR="$tmp/db" CLAMAV_REPORT_FILE="$report" bash "$root/.github/scripts/clamav-scan.sh" "$tmp/artifacts" 2>&1); then
    rc=0
  else
    rc=$?
  fi
  if [[ $case_name == clean ]]; then
    [[ $rc -eq 0 && $output == *'ClamAV: clean'* && $output == *'EICAR self-test passed'* && $(<"$report") == *'Clean: no malware detected and EICAR self-test passed.'* ]] || { echo "FAIL $case_name: $output"; exit 1; }
  else
    [[ $rc -ne 0 && $output != *'ClamAV: clean'* ]] || { echo "FAIL $case_name: rc=$rc $output"; exit 1; }
  fi
  case "$case_name" in
    detection) [[ $output == *'Test.Malware FOUND'* && $output == *'detected malware'* && $(<"$report") == *'Detection:'* ]] ;;
    scanner-error) [[ $output == *'database unavailable'* && $output == *'scanner failure'* ]] ;;
    self-*) [[ $output == *'EICAR self-test failed'* && $(<"$report") == *'EICAR self-test failed'* ]] ;;
    error-with-detection) [[ $output == *'Test.Malware FOUND'* && $output == *'scanner failure'* ]] ;;
    inconsistent|missing-detection) [[ $output == *'inconsistent result'* && $(<"$report") == *'Scanner failure or inconsistent result'* ]] ;;
  esac || { echo "FAIL $case_name: assertion failed (rc=$rc) $output"; exit 1; }
  printf 'PASS %s (exit %s)\n' "$case_name" "$rc"
done
if output=$(PATH="$tmp/bin:$PATH" CASE=clean CLAMAV_DB_DIR="$tmp/db" CLAMAV_REPORT_FILE="$tmp/empty.report" bash "$root/.github/scripts/clamav-scan.sh" "$tmp/empty" 2>&1); then
  echo 'FAIL empty scan target was reported clean'
  exit 1
fi
[[ $output == *'scan target contains no files'* && $output != *'ClamAV: clean'* ]]
echo 'PASS empty-target (exit 1)'
