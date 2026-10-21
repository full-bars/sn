#!/bin/sh
# Digest-verification helpers for the docker self-update scripts.
#
# The provider tarball is fetched from GitHub releases; the release API
# exposes a per-asset sha256 digest ("sha256:<hex>") since GitHub added the
# field. Verifying BEFORE extract/install closes the last gap in the update
# path: a tampered or corrupted tarball can no longer be swapped onto the
# running provider. On mismatch the caller must abort and leave the existing
# binary untouched.
#
# STRICT POSIX: this file is sourced by both /bin/bash (start_update.sh,
# urnet-tools.sh) and /bin/sh (start_nightly.sh, busybox ash on Alpine), so
# it must not assume bash. `local` is left out entirely — every helper keeps
# its state in uniquely-named globals (UPD_ prefix) because POSIX sh has no
# function-local scope.
#
# TOOL FALLBACKS mirror scripts/Provider_Install_Linux.sh: JSON parsing
# prefers jq, falls back to python3; hashing prefers sha256sum, falls back
# to openssl. All-toolless environments still abort (fail-safe), they just
# get one clear log line saying exactly which tools are missing.

# upd_json_digest_field <release-json> <asset-name>
# Prints the bare hex sha256 for <asset-name>, or returns non-zero when no
# parser is available or the asset carries no usable digest. Control chars
# are stripped from the input first (tr -d) to match the installer's
# hardening against hostile JSON bytes.
upd_json_digest_field() {
    UPD_raw=""
    UPD_json_clean="$(printf '%s' "$1" | tr -d '\000-\037')"
    if command -v jq >/dev/null 2>&1; then
        UPD_raw="$(printf '%s' "$UPD_json_clean" | jq -r --arg a "$2" \
            '.assets[] | select(.name == $a) | .digest // empty' 2>/dev/null)"
    elif command -v python3 >/dev/null 2>&1; then
        UPD_raw="$(printf '%s' "$UPD_json_clean" | python3 -c 'import sys, json
try:
    data = json.load(sys.stdin)
    asset = sys.argv[1]
    for a in data.get("assets", []):
        if a.get("name") == asset:
            print(a.get("digest") or "")
            break
except (json.JSONDecodeError, KeyError, ValueError):
    print("")
' "$2" 2>/dev/null)"
    else
        echo "upd_verify: neither jq nor python3 available; cannot extract expected digest from release JSON" >&2
    fi
    unset UPD_json_clean
    [ -n "$UPD_raw" ] || return 1
    # Normalize: strip "sha256:" prefix; null/None (legacy assets) -> reject.
    UPD_raw="${UPD_raw#sha256:}"
    case "$UPD_raw" in
        null|None|"") return 1 ;;
    esac
    printf '%s\n' "$UPD_raw"
}

# upd_asset_digest_from_json <release-json> <asset-name>
# Kept as the stable caller-facing name used by all update paths.
upd_asset_digest_from_json() {
    upd_json_digest_field "$@"
}

# --- Tarball asset selection -----------------------------------------------
# A release ships one per-arch tarball per platform/GOARCH (name carries an
# -<os>-<goarch> token: linux, freebsd, darwin, windows, ...) plus one OS-less
# multi-arch fat tarball (urnetwork-provider-vX.tar.gz). Every platform is a
# wanted build; the updater either takes the linux-<arch> tarball for this
# container or, for tooling, the fat tarball. The fat tarball is recognized
# by having NO OS token, so the list below must name EVERY platform we ship:
# an unlisted platform makes its tarball indistinguishable from the fat
# tarball and the updater installs the wrong platform's binary. Add the
# platform to this list when releases gain a new one.
UPD_PER_ARCH_OS_RES="darwin|freebsd|linux|windows"

# upd_pick_arch_tarball_name <release-json> <goarch>
# Prints the name of the linux-<goarch> per-arch tarball, or nothing when the
# release carries none (the caller then falls back to the fat tarball).
upd_pick_arch_tarball_name() {
    printf '%s' "$1" | jq -r --arg arch "$2" \
        '.assets[] | .name |
         select((startswith("urnetwork-provider-")) and (endswith(".tar.gz")) and
                (contains("linux-" + $arch)))
      ' 2>/dev/null | head -n1
}

# upd_pick_fat_tarball_name <release-json>
# Prints the name of the OS-less multi-arch fat tarball, or nothing when the
# release carries none.
upd_pick_fat_tarball_name() {
    printf '%s' "$1" | jq -r --arg os "$UPD_PER_ARCH_OS_RES" \
        '.assets[] | .name |
         select((startswith("urnetwork-provider-")) and (endswith(".tar.gz")) and
                ((test("-(" + $os + ")-")) | not))
      ' 2>/dev/null | head -n1
}

# upd_sha256_of <file>
# Prints the hex sha256 of $1 via sha256sum, falling back to openssl.
upd_sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    elif command -v openssl >/dev/null 2>&1; then
        openssl dgst -sha256 "$1" | awk '{print $2}'
    else
        echo "upd_verify: neither sha256sum nor openssl available; cannot hash downloaded file" >&2
        return 2
    fi
}

# upd_verify_digest <file> <expected-hex> <source-label>
# Compares sha256(file) against expected hex. <source-label> ("primary",
# "mirror", ...) appears in mismatch errors so logs say WHERE bad bytes came
# from. Exit codes: 2 = no hashing tool, 1 = mismatch.
upd_verify_digest() {
    UPD_actual="$(upd_sha256_of "$1")" || return $?
    if [ -z "$UPD_actual" ] || [ "$UPD_actual" != "$2" ]; then
        echo "[ERROR] Digest mismatch for $1 (${3:-download})" >&2
        echo "[ERROR]   source:   ${3:-download}" >&2
        echo "[ERROR]   expected: $2" >&2
        echo "[ERROR]   actual:   ${UPD_actual:-<empty - tool missing or unreadable file>}" >&2
        unset UPD_actual
        return 1
    fi
    unset UPD_actual
    return 0
}
