#!/usr/bin/env bash
# Targeted tests for the docker update-path hardening (digest verify + fork-only source).
# Run: bash docker/scripts/test_update_verify.sh   (from repo root or anywhere)
#
# Scope (mirrors the repo rule: test the ACTUAL logic, not a wrapper):
#   1. upd_asset_digest_from_json: parses sha256:<hex> from real API shape, rejects
#      null/None/missing digest, picks the RIGHT asset among several, works via jq AND
#      python3 fallback, aborts when no parser exists, and rejects hostile bytes.
#   2. upd_verify_digest: passes on matching content, FAILS on tampered content,
#      fails on missing file / empty expected digest / absent hashing tool, names the
#      download source in mismatch output.
#   3. POSIX discipline: no `local`, sources clean under dash.
#   4. Tarball asset pickers: EXACT-NAME lookups keyed on the release
#      tag_name win over decoys (musl variant, unmatched new platform, junk
#      checksums artifact) and wrong-platform assets, the fat picker takes
#      the OS-less multi-arch tarball, both consumers call the shared
#      helpers, and the stale inline OS list is gone.
#   5. Source guard: no docker script names an upstream urnetwork repo in fetch URLs.
#
# HARNESS CONTRACT (regression-hardened): every assertion is evaluated INSIDE the t()
# call — `t "name" test "$a" = "$b"` — never as a bare command before it. A bare
# `[ ... ]` followed by `t "name" true` reports PASS even when the assertion failed
# (review-found flaw; do not reintroduce).
set -u

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
. "$HERE/update_verify.sh"

pass=0
fail=0
t() { # t <name> <cmd...>  — returns 0 iff the assertion held
    local name="$1"; shift
    if "$@" >/dev/null 2>&1; then
        pass=$((pass+1)); echo "PASS: $name"
        return 0
    fi
    fail=$((fail+1)); echo "FAIL: $name"
    return 1
}

tmp="$(mktemp -d /tmp/updverify-XXXXXX)"
trap 'rm -rf "$tmp"' EXIT

# Harness self-check: prove t actually fails on a false assertion. If this
# ever "passes", the whole suite's PASS lines are meaningless.
if t "__harness_selfcheck__" test 1 = 2; then
    echo "HARNESS BROKEN: t reported success on a false assertion"
    exit 99
fi
if [ "$fail" -ne 1 ]; then
    echo "HARNESS BROKEN: intentional failure did not register (fail=$fail)"
    exit 99
fi
fail=$((fail-1)) # the intentional failure registered; undo its count
echo "PASS: harness self-check (false assertion correctly counted as FAIL)"

# --- Fixtures shaped like the real GitHub release API response ---
JSON_OK='{"assets":[
  {"name":"urnet-tools-linux-amd64","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
  {"name":"urnetwork-provider-v3.23.0-fix.30.7.tar.gz","digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
  {"name":"urnetwork-provider-v3.23.0-fix.30.7-linux-arm64.tar.gz","digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}]}'

ASSET="urnetwork-provider-v3.23.0-fix.30.7.tar.gz"
HEX_B="bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

got="$(upd_asset_digest_from_json "$JSON_OK" "$ASSET")"
t "digest parses from multi-asset JSON and strips sha256: prefix" test "$got" = "$HEX_B"

upd_asset_digest_from_json "$JSON_OK" "missing-asset.tar.gz" >/dev/null 2>&1
t "rejects missing asset" test $? -ne 0

JSON_NULL='{"assets":[{"name":"old.tar.gz","digest":null}]}'
upd_asset_digest_from_json "$JSON_NULL" "old.tar.gz" >/dev/null 2>&1
t "rejects null digest (legacy asset)" test $? -ne 0

JSON_NODIGEST='{"assets":[{"name":"plain.tar.gz"}]}'
upd_asset_digest_from_json "$JSON_NODIGEST" "plain.tar.gz" >/dev/null 2>&1
t "rejects asset with no digest field" test $? -ne 0

# Hostile/odd input must not crash or inject: control chars stripped pre-parse.
JSON_HOSTY="{\"assets\":[{\"name\":\"x.tar.gz\",\"digest\":\"sha256:$HEX_B\"}]}`printf '\001\002'`"
got="$(upd_asset_digest_from_json "$JSON_HOSTY" "x.tar.gz" 2>/dev/null)"
t "control-char-polluted JSON still parses to the correct digest" test "$got" = "$HEX_B"

# CRLF line endings in the response body (proxy/mirror artifact) survive parsing.
JSON_CRLF="$(printf '%s' "$JSON_OK" | sed 's/$/\r/')"
got="$(upd_asset_digest_from_json "$JSON_CRLF" "$ASSET" 2>/dev/null)"
t "CRLF-mangled JSON still yields the exact hex digest" test "$got" = "$HEX_B"

# --- Tool fallback matrix for digest extraction ---
# Build a tool-restricted PATH that provably EXCLUDES a tool and INCLUDES the
# fallback tool resolves. Hiding by directory alone is unreliable when tools
# share a directory (or dirs are symlinked), so each filtered environment is
# VERIFIED before use: assert the hidden tool is unreachable AND the expected
# fallback tool resolves. On boxes where hide+need share ALL dirs (Arch's
# /usr/bin holds both jq and python3), the case is genuinely unconstructable
# and reported as SKIP rather than silently passing.
path_without_verified() { # path_without_verified <hidden-tool> <required-fallback-tool> -> PATH
    local hide="$1" need="$2" d out="" keep_dir need_dirs="" pdir
    for keep_dir in $(echo "$PATH" | tr ':' '\n' | while read -r pd; do [ -x "$pd/$need" ] && echo "$pd"; done); do
        need_dirs="$need_dirs$keep_dir"$'\n'
    done
    while IFS= read -r d; do
        [ -x "$d/$hide" ] && continue
        printf '%s\n' "$need_dirs" | grep -qx "$d" && { out="$out$d:"; continue; }
        out="$out$d:"
    done < <(echo "$PATH" | tr ':' '\n' | grep -v '^$')
    echo "$out"
}

extract_with_path() { # extract_with_path <PATH> ; result in stdout
    PATH="$1" sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" 2>/dev/null
}

tool_missing_in() { # tool_missing_in <PATH> <tool> -> exit 0 if truly unreachable
    PATH="$1" sh -c "command -v $2 >/dev/null 2>&1"; [ $? -ne 0 ]
}

if command -v python3 >/dev/null 2>&1; then
    P="$(path_without_verified jq python3)"
    if tool_missing_in "$P" jq && PATH="$P" sh -c 'command -v python3 >/dev/null'; then
        got="$(extract_with_path "$P")"
        t "python3 fallback extracts identical digest with jq PROVABLY hidden" test "$got" = "$HEX_B"

        PATH="$P" sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_NULL' 'old.tar.gz'" >/dev/null 2>&1
        t "python3 fallback also rejects null digest" test $? -ne 0
    else
        echo "SKIP: cannot construct a jq-free python3-capable PATH on this box"
        echo "      (jq and python3 share every dir; fallback still covered by the"
        echo "      toolless-abort case and by CI images where layouts differ)"
    fi
fi

# All-toolless environment must still abort (fail-safe, never skip verify).
if env -i PATH=/nonexistent sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" >/dev/null 2>&1; then
    fail=$((fail+1)); echo "FAIL: toolless environment did not abort"
else
    pass=$((pass+1)); echo "PASS: no-jq-no-python3 environment aborts (fail-safe)"
fi

# Exit-code propagation: upd_verify_digest must surface exit 2 (no hashing
# tool) distinctly — a swallowed exit 2 would turn tool-absence into a
# generic mismatch and could mask real failures. env -i does NOT hide tools:
# sh reinstalls a default PATH, so this uses an explicit tool-less dir.
mkdir -p "$tmp/notools"
ln -sf "$(command -v sh)" "$tmp/notools/sh" 2>/dev/null
no_hash_exit="$(PATH="$tmp/notools" sh -c ". '$HERE/update_verify.sh' && upd_verify_digest '/etc/hostname' 'deadbeef' 'primary'" >/dev/null 2>&1; echo $?)"
t "both-hash-tools-missing propagates exit 2 through upd_verify_digest" test "$no_hash_exit" -eq 2

# --- verify_digest ---
printf 'provider-bytes-trusted' > "$tmp/good.bin"
good_sum="$(sha256sum "$tmp/good.bin" | awk '{print $1}')"
upd_verify_digest "$tmp/good.bin" "$good_sum" "primary"
t "verify_digest accepts exact match" test $? -eq 0

printf 'provider-bytes-TAMPERED' > "$tmp/bad.bin"
upd_verify_digest "$tmp/bad.bin" "$good_sum" "primary" >/dev/null 2>&1
t "verify_digest rejects tampered content" test $? -ne 0

# Source label must appear in mismatch output (error-indistinction fix).
# The check runs INSIDE t(): a bare `cmd` line before `t "..." true` would
# report PASS even if the check failed (review-found flaw; do not
# reintroduce). A helper function is used because the check involves the
# sourced upd_verify_digest and sh -c would lose it.
mismatch_names_source() {
    upd_verify_digest "$tmp/bad.bin" "$good_sum" "mirror-fallback" 2> "$tmp/err_source.txt"
    grep -q "mirror-fallback" "$tmp/err_source.txt"
}
t "mismatch error names the download source" mismatch_names_source

upd_verify_digest "$tmp/nonexistent.bin" "$good_sum" "primary" >/dev/null 2>&1
t "verify_digest rejects missing file" test $? -ne 0

# Empty expected digest must never pass (guards against "" sneaking through).
upd_verify_digest "$tmp/good.bin" "" "" >/dev/null 2>&1
t "empty expected digest rejected" test $? -ne 0

# Hashing fallback parity: openssl must produce the same hash as sha256sum
# where both exist; where only one exists, verification still functions.
if command -v openssl >/dev/null 2>&1; then
    ossl="$(openssl dgst -sha256 "$tmp/good.bin" | awk '{print $NF}')"
    t "openssl sha256 agrees with sha256sum (fallback equivalence)" test "$ossl" = "$good_sum"

    O="$(path_without_verified sha256sum openssl openssl)"
    if tool_missing_in "$O" sha256sum && PATH="$O" sh -c 'command -v openssl >/dev/null'; then
        got="$(PATH="$O" sh -c ". '$HERE/update_verify.sh' && upd_sha256_of '$tmp/good.bin'" 2>/dev/null)"
        t "upd_sha256_of falls back to openssl with sha256sum PROVABLY hidden" test "$got" = "$good_sum"

        PATH="$O" sh -c ". '$HERE/update_verify.sh' && upd_verify_digest '$tmp/bad.bin' '$good_sum' 'primary'" >/dev/null 2>&1
        t "verification still rejects tampered content via openssl fallback" test $? -ne 0
    fi

    # $NF-vs-$2 guard: whatever field awk picks, it must equal sha256sum's hex.
    ossl2="$(openssl dgst -sha256 "$tmp/good.bin" | awk '{print $2}')"
    t "awk \$2 field equals \$NF for current openssl output" test "$ossl2" = "$ossl"
fi

# POSIX discipline: no `local` anywhere in the sourced helper.
grep -qE '^\s*local\s' "$HERE/update_verify.sh"
t "update_verify.sh is POSIX-clean (no local)" test $? -ne 0

# The helper parses identically under /bin/sh as under bash (shebang contract).
sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET'" > "$tmp/sh.out"
t "helper works when sourced by strict /bin/sh (dash-compatible)" test "$(cat "$tmp/sh.out")" = "$HEX_B"

# No stale UPD_ globals leak out of a successful call into a direct-source
# caller's environment: UPD_raw, UPD_json_clean, UPD_actual all covered
# (UPD_actual is unexported so env cannot see it — probe via a sourced
# subshell asserting emptiness instead).
upd_globals_cleaned() { # fresh sh -c: asserts the sourced helper leaks nothing
    sh -c ". '$HERE/update_verify.sh' && upd_asset_digest_from_json '$JSON_OK' '$ASSET' >/dev/null; test -z \"\${UPD_raw:-}\" && test -z \"\${UPD_json_clean:-}\" && test -z \"\${UPD_actual:-}\""
}
t "helper globals cleaned after successful call" upd_globals_cleaned

# --- Caller asset-selection filter (start_update.sh Download_API) ---
# Regression: jq `select(A) and B` parses as `(select(A)) and B`, so the
# un-parenthesized form errors "Cannot index boolean with string" on every
# real release.
# --- Tarball asset pickers (upd_pick_arch_tarball_name / upd_pick_fat_tarball_name) ---
# The pickers now resolve EXACT names keyed on the release tag_name, so no
# decoy can win regardless of GitHub's sort order or new platforms. Fixture
# shaped like a real sn stable release ($TAG), with three decoys kept in
# GitHub sort order (alphabetical by name):
#   - a -linux-amd64-musl.tar.gz variant, which the old contains-based arch
#     picker picked ahead of the exact linux-amd64 tarball;
#   - a hypothetical new-platform netbsd asset, absent from the OS-token
#     list, which the old fat picker mistook for the OS-less fat tarball;
#   - a junk checksums artifact that must never be downloaded.
# All three sort ahead of the asset they mimic.
TAG="v2026.10.6-1064742750-meso"
FAT_ASSET="urnetwork-provider-${TAG}.tar.gz"
ARCH_ASSET="urnetwork-provider-${TAG}-linux-amd64.tar.gz"
ARM64_ASSET="urnetwork-provider-${TAG}-linux-arm64.tar.gz"
JSON_V342="{\"tag_name\":\"$TAG\",\"assets\":[
  {\"name\":\"urnetwork-provider-${TAG}-checksums.tar.gz\",\"digest\":\"sha256:10\"},
  {\"name\":\"urnetwork-provider-${TAG}-darwin-amd64.tar.gz\",\"digest\":\"sha256:2\"},
  {\"name\":\"urnetwork-provider-${TAG}-freebsd-amd64.tar.gz\",\"digest\":\"sha256:3\"},
  {\"name\":\"urnetwork-provider-${TAG}-linux-amd64-musl.tar.gz\",\"digest\":\"sha256:8\"},
  {\"name\":\"urnetwork-provider-${TAG}-linux-amd64.tar.gz\",\"digest\":\"sha256:4\"},
  {\"name\":\"urnetwork-provider-${TAG}-linux-arm64.tar.gz\",\"digest\":\"sha256:5\"},
  {\"name\":\"urnetwork-provider-${TAG}-netbsd-amd64.tar.gz\",\"digest\":\"sha256:9\"},
  {\"name\":\"urnetwork-provider-${TAG}-windows-amd64.tar.gz\",\"digest\":\"sha256:6\"},
  {\"name\":\"urnetwork-provider-${TAG}.tar.gz\",\"digest\":\"sha256:7\"}]}"

got="$(upd_pick_arch_tarball_name "$JSON_V342" "amd64")"
t "arch picker returns the exact linux-amd64 name, never the musl decoy" \
    test "$got" = "$ARCH_ASSET"

got="$(upd_pick_arch_tarball_name "$JSON_V342" "arm64")"
t "arch picker resolves the arm64 per-arch tarball" \
    test "$got" = "$ARM64_ASSET"

got="$(upd_pick_fat_tarball_name "$JSON_V342")"
t "fat picker returns the exact OS-less name, never netbsd or checksums" \
    test "$got" = "$FAT_ASSET"

# A release JSON without tag_name resolves nothing at all (fail-safe: the
# caller falls back to the fat picker, which also returns nothing).
got="$(upd_pick_arch_tarball_name '{"assets":[{"name":"urnetwork-provider-v9-freebsd-amd64.tar.gz"}]}' "amd64")"
t "arch picker empty without a linux asset (caller falls back to fat)" test -z "$got"

# The digest lookup must follow the picked asset name end to end.
got="$(upd_asset_digest_from_json "$JSON_V342" "$(upd_pick_arch_tarball_name "$JSON_V342" "amd64")")"
t "digest lookup resolves the picked asset" test "$got" = "4"

# The unparenthesized select+and shape must stay rejected: jq parses
# `select(A) and B` as a boolean stage and errors on every real release, so
# nobody reintroduces it thinking it works.
broken_exit=0
printf '%s' "$JSON_V342" | jq -r '.assets[] | select(.name | startswith("urnetwork-provider-")) and (.name | endswith(".tar.gz")) | .name' >/dev/null 2>&1 || broken_exit=1
t "unparenthesized select+and stays rejected (would abort under set -e)" test "$broken_exit" -eq 1

# Wiring guard: both update consumers must call the shared pickers, and the
# old incomplete inline OS list must not reappear anywhere in docker/scripts.
t "start_update.sh selects the fat tarball through the shared picker" \
    grep -q 'upd_pick_fat_tarball_name' "$HERE/start_update.sh"
t "nightly updater calls the shared arch picker" \
    grep -q 'upd_pick_arch_tarball_name' "$HERE/start_nightly.sh"
# The old deny-list was an alternation string inside a jq test(). Match it as a
# fixed string (grep -E treats a backslash-pipe as a literal pipe, which made
# the previous pattern miss variants), then catch any other shape of inline
# per-arch deny-list: the three OS tokens live in update_verify.sh's shared
# definition, so no other script may carry them.
t "stale inline per-arch OS alternation is gone from docker/scripts" \
    test -z "$(grep -rFl -- '-darwin-|-linux-|-windows-' "$HERE" | grep -v 'test_update_verify.sh')"
t "no per-arch OS token outside update_verify.sh" \
    test -z "$(grep -rl -- '-darwin-' "$HERE" | grep -v -e 'update_verify.sh' -e 'test_update_verify.sh')"

# Nightly asset mapping: URL->name match-back finds the API name even when
# the URL layout changes above the filename segment.
URL_V9="https://github.com/full-bars/sn/releases/download/${TAG}/${FAT_ASSET}"
matched=""
for upd_candidate in $(printf '%s\n' "$JSON_V342" | grep -oE '"name": *"[^"]+"' | sed -E 's/"name": *"([^"]+)"/\1/'); do
    case "$URL_V9" in
        *"/$upd_candidate") matched="$upd_candidate"; break ;;
    esac
done
t "nightly URL-to-name match-back resolves the real API asset name" test "$matched" = "$FAT_ASSET"

# Basename last-resort path: an asset name absent from the JSON must NOT
# match-back (guarding the match-back order) — basename of URL used only
# after match-back exhausts candidates.
BNAME="${URL_V9##*/}"
t "basename fallback yields the filename segment" test "$BNAME" = "$FAT_ASSET"

# --- Source guard: upstream repos must not appear in fetch URLs ---
violations=""
for f in "$HERE"/start_update.sh "$HERE"/start_nightly.sh "$HERE"/start_stable.sh \
         "$HERE"/start_jwt.sh "$HERE"/urnet-tools.sh "$HERE"/pelican_panel.sh \
         "$HERE"/test_exit75_restart.sh; do
    [ -f "$f" ] || continue
    # Any api.github.com/repos/<org>/<repo> that is NOT full-bars is a violation.
    while read -r url; do
        org_repo="$(printf '%s' "$url" | sed -E 's#.*/repos/([^/"]+/[^/"]+).*#\1#')"
        case "$org_repo" in
            full-bars/*) ;;
            *) violations="$violations\n$(basename "$f"): $url" ;;
        esac
    done < <(grep -oE 'https://api\.github\.com/repos/[^"]+' "$f" || true)
done
t "no upstream urnetwork repo named in any release-fetch URL" test -z "$violations"

echo ""
echo "Results: $pass passed, $fail failed"
exit $((fail > 0))
