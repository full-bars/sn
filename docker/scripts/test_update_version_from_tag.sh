#!/bin/sh
# Regression test: the version recorded after an in-place update must come from
# the RELEASE TAG, never from the asset url.
#
# sn's start_nightly.sh is a monolith that calls main at the bottom, so this test
# sources a copy with the entrypoint call stripped: the real func_check_update
# runs, but the bootstrap does not. curl is stubbed, so no network is touched.
#
# The bug this locks down: the version was scraped out of the asset url with
# `sed -E 's#.*/download/v([^/]+)/.*#\1#'`. Any url of a different shape passed
# through unchanged, so version.txt received the whole url; the next check then
# compared that to the real tag, judged the box stale, and re-downloaded the
# archive forever. sn scraped the url in the COMPARE as well, so both halves are
# covered here.
#
# Every case runs twice, once with jq and once with jq hidden from PATH, so both
# the jq parser and the grep fallback are exercised. Further cases: a release
# with no usable tag must abort before installing anything, and a failed version
# probe must not reuse the previous call's tag.
#
# Run: sh docker/scripts/test_update_version_from_tag.sh
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
cleanup() { rm -rf "${WORK:?}"; }
trap cleanup EXIT

# sn's scheme: a dated tag with a build code and the repo suffix
TAG="v2026.10.30-1085641150-sn"
BARE="2026.10.30-1085641150-sn"
OLD_BARE="2026.10.29-1000000000-sn"

mkdir -p "$WORK/build/linux/amd64"
printf '#!/bin/sh\necho fixture-provider\n' > "$WORK/build/linux/amd64/provider"
chmod +x "$WORK/build/linux/amd64/provider"
tar -czf "$WORK/asset.tar.gz" -C "$WORK/build" linux/amd64/provider
DIGEST=$(sha256sum "$WORK/asset.tar.gz" | cut -d' ' -f1)

# deliberately NOT shaped /download/v<tag>/ - a version parser reading the url
# must fail this test. The name still matches urnetwork-provider-*.tar.gz so the
# no-jq asset picker accepts it.
ARCH_URL="https://downloads.example.test/assets/urnetwork-provider-$TAG-linux-amd64.tar.gz"

# a PATH holding every tool on the real PATH except jq
NOJQ_BIN="$WORK/nojq-bin"
mkdir -p "$NOJQ_BIN"
oldifs=$IFS
IFS=:
for d in $PATH; do
    [ -d "$d" ] || continue
    for f in "$d"/*; do
        n=${f##*/}
        [ "$n" = jq ] && continue
        [ -e "$NOJQ_BIN/$n" ] || ln -s "$f" "$NOJQ_BIN/$n" 2>/dev/null
    done
done
IFS=$oldifs
REAL_PATH=$PATH

# mkrelease <tag_name json fragment with trailing comma, or empty>
mkrelease() {
    cat > "$WORK/release.json" <<EOF
{$1"prerelease":false,"draft":false,"assets":[
  {"name":"urnetwork-provider-$TAG-linux-amd64.tar.gz","browser_download_url":"$ARCH_URL","digest":"sha256:$DIGEST"}
]}
EOF
}

# --- harness: strip the entrypoint call, then source the real script ---------
BODY="$WORK/start_nightly_body.sh"
sed '/^main "\$@"$/d' "$HERE/start_nightly.sh" > "$BODY"

APP="$WORK/app"
HOME_FIX="$WORK/home"

FAILS=0
MODE=""
t() { # t <name> <cmd...>
    name="$1"
    shift
    if "$@"; then
        printf 'PASS  [%s] %s\n' "$MODE" "$name"
    else
        printf 'FAIL  [%s] %s\n' "$MODE" "$name"
        FAILS=$((FAILS + 1))
    fi
}
assert_eq() { [ "$2" = "$3" ]; }

# A fresh install dir holding a previous release: an old binary and old version.
fresh_state() {
    rm -rf "${APP:?}" "${HOME_FIX:?}"
    mkdir -p "$APP" "$HOME_FIX/.urnetwork"
    printf 'old-binary\n' > "$APP/urnetwork_amd64_nightly"
    printf '%s\n' "$OLD_BARE" > "$VERSION_FILE"
    rm -f "$WORK/api_fail"
}
# content plus inode: a replaced binary changes the inode even if bytes match
bin_sig() { printf '%s %s' "$(cat "$APP/urnetwork_amd64_nightly")" "$(ls -i "$APP/urnetwork_amd64_nightly" | cut -d' ' -f1)"; }

curl() {
    # the API call is: curl -sfL -w '%{http_code}' -o <file> <url>
    # the download is: curl -sfL --connect-timeout 30 <url> -o <file>
    out=""
    want_code=""
    prev=""
    for a in "$@"; do
        [ "$prev" = "-o" ] && out="$a"
        case "$a" in *'%{http_code}'*) want_code=1 ;; esac
        prev="$a"
    done
    case "$*" in
        *releases/latest*)
            # curl runs in command substitutions, so the failure budget lives in
            # a file
            if [ -s "$WORK/api_fail" ]; then
                n=$(cat "$WORK/api_fail")
                if [ "$n" -gt 0 ]; then
                    echo $((n - 1)) > "$WORK/api_fail"
                    return 22
                fi
            fi
            [ -n "$out" ] && cat "$WORK/release.json" > "$out"
            ;;
        *)
            [ -n "$out" ] && cp "$WORK/asset.tar.gz" "$out"
            ;;
    esac
    [ -n "$want_code" ] && printf '200'
    return 0
}

run_mode() {
    MODE="$1"
    if [ "$MODE" = nojq ]; then
        PATH="$NOJQ_BIN"
    else
        PATH="$REAL_PATH"
    fi
    export PATH
    if [ "$MODE" = nojq ]; then
        t "jq is hidden" assert_eq x "$(command -v jq >/dev/null 2>&1 && echo found || echo hidden)" "hidden"
    elif ! command -v jq >/dev/null 2>&1; then
        printf 'SKIP  [jq] jq is not installed on this host\n'
        return 0
    fi

    # set these BEFORE sourcing: the script runs resolve_state_dir at the top
    # level and carries `set -e`, both of which would otherwise reach into this
    # test
    APP_DIR="$APP"
    VERSION_FILE="$APP/version.txt"
    HOME="$HOME_FIX"
    export HOME
    URNETWORK_STATE_DIR="$HOME_FIX/.urnetwork"
    export URNETWORK_STATE_DIR
    A_SYS_ARCH=amd64
    export A_SYS_ARCH
    mkdir -p "$APP" "$HOME_FIX"
    . "$BODY"
    set +e
    # the script hard-sets APP_DIR=/app and VERSION_FILE from it at the top
    # level, so redirect them to the fixture AFTER sourcing
    APP_DIR="$APP"
    VERSION_FILE="$APP/version.txt"

    # --- case 1: the recorded version is the tag, not the url ---------------
    fresh_state
    mkrelease "\"tag_name\":\"$TAG\","
    func_check_update > "$WORK/run.log" 2>&1
    RECORDED=$(cat "$VERSION_FILE" 2>/dev/null)
    t "records the release tag, not the url" assert_eq x "$RECORDED" "$BARE"
    t "recorded version is not the download url" assert_eq x "$([ "$RECORDED" = "$ARCH_URL" ] && echo url || echo ok)" "ok"
    t "recorded version is not empty" assert_eq x "$([ -n "$RECORDED" ] && echo ok || echo empty)" "ok"
    t "the new provider was installed" assert_eq x "$(cat "$APP/urnetwork_amd64_nightly")" "$(cat "$WORK/build/linux/amd64/provider")"
    func_check_update > "$WORK/run2.log" 2>&1
    t "second run skips: already at latest" assert_eq x "$(grep -c 'Already at latest provider version' "$WORK/run2.log")" "1"

    # --- case 2: no usable tag must abort before anything is installed ------
    for variant in 'absent:' 'empty:"tag_name":"",'; do
        label=${variant%%:*}
        frag=${variant#*:}
        fresh_state
        mkrelease "$frag"
        before=$(bin_sig)
        func_check_update > "$WORK/run_notag.log" 2>&1
        t "tag $label: aborts with the no-tag error" assert_eq x "$(grep -c 'carries no tag' "$WORK/run_notag.log")" "1"
        t "tag $label: installed binary untouched" assert_eq x "$(bin_sig)" "$before"
        t "tag $label: version.txt unchanged" assert_eq x "$(cat "$VERSION_FILE")" "$OLD_BARE"
        t "tag $label: no update-pending marker" assert_eq x "$([ -e "$HOME_FIX/.urnetwork/update-pending" ] && echo set || echo clear)" "clear"
    done

    # --- case 3: a failed probe must not reuse the previous call's tag ------
    # Prime LATEST_VERSION_CHECK with a successful call for the old release,
    # then let the version probe fail once while the install fetch succeeds.
    fresh_state
    mkrelease "\"tag_name\":\"v$OLD_BARE\","
    : > "$VERSION_FILE"
    func_check_update > "$WORK/run_prime.log" 2>&1
    mkrelease "\"tag_name\":\"$TAG\","
    printf 'old-binary\n' > "$APP/urnetwork_amd64_nightly"
    printf '%s\n' "$OLD_BARE" > "$VERSION_FILE"
    before=$(bin_sig)
    echo 1 > "$WORK/api_fail"
    func_check_update > "$WORK/run_stale.log" 2>&1
    t "stale tag: aborts instead of installing under the old tag" assert_eq x "$(grep -c 'carries no tag' "$WORK/run_stale.log")" "1"
    t "stale tag: installed binary untouched" assert_eq x "$(bin_sig)" "$before"
    t "stale tag: version.txt unchanged" assert_eq x "$(cat "$VERSION_FILE")" "$OLD_BARE"

    return "$FAILS"
}

TOTAL=0
for m in jq nojq; do
    printf '=== mode: %s ===\n' "$m"
    ( run_mode "$m" )
    TOTAL=$((TOTAL + $?))
done

if [ "$TOTAL" -ne 0 ]; then
    printf '\n%d assertion(s) failed\n' "$TOTAL"
    exit 1
fi
printf '\nall assertions passed\n'
