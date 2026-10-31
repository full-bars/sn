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
# Run: sh docker/scripts/test_update_version_from_tag.sh
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
WORK=$(mktemp -d)
BODY=""
cleanup() { rm -rf "$WORK"; [ -n "$BODY" ] && rm -f "$BODY"; }
trap cleanup EXIT

FAILS=0
t() { # t <name> <cmd...>
    name="$1"
    shift
    if "$@"; then
        printf 'PASS  %s\n' "$name"
    else
        printf 'FAIL  %s\n' "$name"
        FAILS=$((FAILS + 1))
    fi
}
assert_eq() { [ "$2" = "$3" ]; }

# sn's scheme: a dated tag with a build code and the repo suffix
TAG="v2026.10.30-1085641150-sn"
BARE="2026.10.30-1085641150-sn"

mkdir -p "$WORK/build/linux/amd64"
printf '#!/bin/sh\necho fixture-provider\n' > "$WORK/build/linux/amd64/provider"
chmod +x "$WORK/build/linux/amd64/provider"
tar -czf "$WORK/asset.tar.gz" -C "$WORK/build" linux/amd64/provider
DIGEST=$(sha256sum "$WORK/asset.tar.gz" | cut -d' ' -f1)

# deliberately NOT shaped /download/v<tag>/ - a version parser reading the url
# must fail this test
ARCH_URL="https://downloads.example.test/assets/provider-linux-amd64.tar.gz"

cat > "$WORK/release.json" <<EOF
{"tag_name":"$TAG","prerelease":false,"draft":false,"assets":[
  {"name":"urnetwork-provider-$TAG-linux-amd64.tar.gz","browser_download_url":"$ARCH_URL","digest":"sha256:$DIGEST"}
]}
EOF

# --- harness: strip the entrypoint call, then source the real script ---------
BODY="$HERE/.test_body_$$.sh"
sed '/^main "\$@"$/d' "$HERE/start_nightly.sh" > "$BODY"

APP="$WORK/app"
HOME_FIX="$WORK/home"
mkdir -p "$APP" "$HOME_FIX"
# set these BEFORE sourcing: the script runs resolve_state_dir at the top level
# and carries `set -e`, both of which would otherwise reach into this test
APP_DIR="$APP"
VERSION_FILE="$APP/version.txt"
HOME="$HOME_FIX"
export HOME
URNETWORK_STATE_DIR="$HOME_FIX/.urnetwork"
export URNETWORK_STATE_DIR

A_SYS_ARCH=amd64
export A_SYS_ARCH
. "$BODY"
set +e
# the script hard-sets APP_DIR=/app and VERSION_FILE from it at the top level,
# so redirect them to the fixture AFTER sourcing
APP_DIR="$APP"
VERSION_FILE="$APP/version.txt"

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
            [ -n "$out" ] && cat "$WORK/release.json" > "$out"
            ;;
        *)
            [ -n "$out" ] && cp "$WORK/asset.tar.gz" "$out"
            ;;
    esac
    [ -n "$want_code" ] && printf '200'
    return 0
}

printf '%s\n' "2026.10.29-1000000000-sn" > "$VERSION_FILE"
func_check_update > "$WORK/run.log" 2>&1

RECORDED=$(cat "$VERSION_FILE" 2>/dev/null)

t "records the release tag, not the url" assert_eq x "$RECORDED" "$BARE"
t "recorded version is not the download url" assert_eq x "$([ "$RECORDED" = "$ARCH_URL" ] && echo url || echo ok)" "ok"
t "recorded version is not empty" assert_eq x "$([ -n "$RECORDED" ] && echo ok || echo empty)" "ok"

func_check_update > "$WORK/run2.log" 2>&1
t "second run skips: already at latest" assert_eq x "$(grep -c 'Already at latest provider version' "$WORK/run2.log")" "1"

if [ "$FAILS" -ne 0 ]; then
    printf '\n%d assertion(s) failed\n' "$FAILS"
    printf '\n--- run.log ---\n'; cat "$WORK/run.log"
    exit 1
fi
printf '\nall assertions passed\n'
