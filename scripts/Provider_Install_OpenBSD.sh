#!/bin/sh
# urnet-tools: URnetwork provider manager (OpenBSD)
# Author: full-bars (GitHub), onlyinthe707 / "mesocyclone" (Discord)
# Based on: Ar Rakin, Ryan Mello (original)
# https://github.com/full-bars/sn

me="$(basename "$0")"

show_help() {
    echo "Usage: $me [options] <command>"
    echo ""
    echo "Core Commands:"
    echo "  install [<version>]      Download and install the provider + rc.d service"
    echo "  start                     Start the provider"
    echo "  stop                      Stop the provider"
    echo "  restart                   Restart the provider"
    echo "  status                    Show provider service status"
    echo "  update [<version>]        Upgrade to latest (or specified version)"
    echo "  version                   Show installed version"
    echo ""
    echo "Performance & Tuning:"
    echo "  hot-restart <on|off>      Reuse client JWT identities across restarts"
    echo "  self-heal [on|off]        Auto-regulate proxies (load gate + cleanup) (default: off)"
    echo ""
    echo "Session:"
    echo "  session save <file>       Export identity+proxy state (encrypted)"
    echo "  session load <file>       Import identity+proxy state, then restart"
    echo ""
    echo "Proxy Management:"
    echo "  proxy refresh             Re-read configs and hot-reload proxies"
    echo "  proxy remove-dead         Interactively prune dead/degraded/failing"
    echo "  proxy summary             Fleet summary (sources, health, counts)"
    echo ""
    echo "Options:"
    echo "  -h, --help                Show this help"
    echo "  -v, --version             Show version"
    echo "  -f, --force               Skip confirmation prompts"
    echo ""
    echo "https://github.com/full-bars/sn"
}

# --- helpers ---

pr_err() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
}

pr_info() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@"
}

pr_warn() {
    fmt="$1"; shift
    printf "%s: $fmt\n" "$me" "$@" >&2
}

# get_asset_digest_from_api_response returns the sha256 hex for a named asset.
# Empty means the release has no digest; a present but malformed digest returns
# __INVALID_SHA256__ so callers refuse it rather than downgrading to no digest.
get_asset_digest_from_api_response() {
    _rel="$1"
    _ast="$2"
    _dig=""
    if command -v jq > /dev/null 2>&1; then
        _dig="$(printf '%s' "$_rel" | jq -r --arg a "$_ast" '
            (.assets[]? | select(.name == $a) | .digest) as $d
            | if $d == null or $d == "" then ""
              elif ($d | type) != "string" then "__INVALID_SHA256__"
              else ($d | sub("^sha256:"; "")) as $h
                | if ($h | test("^[0-9a-fA-F]{64}$")) then $h else "__INVALID_SHA256__" end
              end
        ' 2>/dev/null || true)"
    elif command -v python3 > /dev/null 2>&1; then
        _dig="$(printf '%s' "$_rel" | python3 -c '
import sys, json, re
try:
    doc = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for asset in doc.get("assets", []):
    if asset.get("name") == sys.argv[1]:
        val = asset.get("digest")
        if val is None or val == "":
            print("")
        elif not isinstance(val, str):
            print("__INVALID_SHA256__")
        else:
            if val.startswith("sha256:"):
                val = val[len("sha256:"):]
            print(val if re.fullmatch(r"[0-9a-fA-F]{64}", val) else "__INVALID_SHA256__")
        break
else:
    print("")
' "$_ast" 2>/dev/null || true)"
    fi
    printf '%s' "$_dig"
}

# release_lists_asset returns 0 when the release JSON in $1 lists an asset named
# $2. An error body (rate limit, outage) or unparseable JSON lists nothing, so
# the caller treats it as unavailable metadata.
release_lists_asset() {
    [ -n "$1" ] || return 1
    if command -v jq > /dev/null 2>&1; then
        printf '%s' "$1" | jq -e --arg a "$2" 'any(.assets[]?; .name == $a)' > /dev/null 2>&1
    elif command -v python3 > /dev/null 2>&1; then
        printf '%s' "$1" | python3 -c '
import sys, json
try:
    doc = json.load(sys.stdin)
    sys.exit(0 if any(a.get("name") == sys.argv[1] for a in doc.get("assets", [])) else 1)
except Exception:
    sys.exit(1)
' "$2" > /dev/null 2>&1
    else
        return 1
    fi
}

compute_sha256_file() {
    _f="$1"
    [ -f "$_f" ] || return 1
    if command -v sha256 > /dev/null 2>&1; then
        sha256 -q "$_f"
    elif command -v openssl > /dev/null 2>&1; then
        openssl dgst -sha256 "$_f" | awk '{print $NF}'
    elif command -v sha256sum > /dev/null 2>&1; then
        sha256sum "$_f" | awk '{print $1}'
    else
        return 1
    fi
}

verify_sha256_file() {
    _f="$1"
    _exp="$2"
    [ -n "$_exp" ] && [ -f "$_f" ] || return 1
    _act="$(compute_sha256_file "$_f")"
    [ -n "$_act" ] && [ "$_act" = "$_exp" ]
}

# http_fetch <url> [output-file]
#
# One downloader for every fetch the installer makes. OpenBSD ships ftp(1) in
# the base system and it speaks https, so a clean host can download without any
# package. curl and wget are used only when ftp is missing.
#
# A nonzero status means "no such asset": ftp, curl -f and wget all exit nonzero
# on an HTTP error status, so a caller that treats failure that way keeps
# working with every backend.
#
# Writes to stdout when no output file is given, which is what the JSON and tag
# reads need.
http_fetch() {
    _url="$1"
    _out="${2:-}"
    if command -v ftp >/dev/null 2>&1; then
        if [ -n "$_out" ]; then
            ftp -V -o "$_out" "$_url"
        else
            ftp -V -o - "$_url"
        fi
        return $?
    fi
    if command -v curl >/dev/null 2>&1; then
        if [ -n "$_out" ]; then
            curl -fsSL "$_url" -o "$_out"
        else
            curl -fsSL "$_url"
        fi
        return $?
    fi
    if command -v wget >/dev/null 2>&1; then
        if [ -n "$_out" ]; then
            wget -q -O "$_out" "$_url"
        else
            wget -q -O - "$_url"
        fi
        return $?
    fi
    pr_err "no downloader found: need ftp, curl or wget on PATH"
    return 1
}

# --- service account resolution ---
#
# resolve_service_user prints the account the provider must run as. Order:
# an explicit override, then DOAS_USER or SUDO_USER (who actually invoked doas
# or sudo; $USER is "root" there), then logname (the login session owner, which survives a
# plain `su -`), then the current uid. $USER alone is NOT trusted: it is
# unset under cron and is "root" for a plain root login.
resolve_service_user() {
    if [ -n "${SERVICE_USER_OVERRIDE:-}" ]; then
        printf '%s\n' "$SERVICE_USER_OVERRIDE"
        return
    fi
    if [ -n "${URNETWORK_USER:-}" ]; then
        printf '%s\n' "$URNETWORK_USER"
        return
    fi
    if id -u urnetwork >/dev/null 2>&1; then
        printf '%s\n' "urnetwork"
        return
    fi
    if [ -n "${DOAS_USER:-}" ]; then
        printf '%s\n' "$DOAS_USER"
        return
    fi
    if [ -n "${SUDO_USER:-}" ]; then
        printf '%s\n' "$SUDO_USER"
        return
    fi
    _lu="$(logname 2>/dev/null || true)"
    if [ -n "$_lu" ] && [ "$_lu" != "root" ]; then
        printf '%s\n' "$_lu"
        return
    fi
    id -un
}

# user_home prints a user's home directory from the passwd database. $HOME is
# not usable when running under doas or sudo (it may still be root's), and the service
# user must be able to read the install and state directories.
user_home() {
    _h="$(awk -F: -v u="$1" '$1 == u {print $6; exit}' /etc/passwd 2>/dev/null)"
    printf '%s\n' "${_h:-$HOME}"
}

# user_shell prints the login shell of a user from the passwd database. The
# invoking user's $SHELL is not the service user's shell, and writing a bash
# fragment into a tcsh rc file is worse than not writing one.
user_shell() {
    _s="$(awk -F: -v u="$1" '$1 == u {print $7; exit}' /etc/passwd 2>/dev/null)"
    printf '%s\n' "${_s:-/bin/sh}"
}

# --- paths ---
#
# The binary lives in the SAME location the Go tool's discovery fallback looks
# (openbsdLibexecProviderPath in internal/urnettools/discover_openbsd.go), so a
# stopped install is still discoverable and lifecycle commands have a target.
# Keep the two in step.

service_name="urnetwork"
rc_script="/etc/rc.d/$service_name"

# Paths: the install tree is root-owned at /usr/local/libexec/urnetwork-provider
# (with chmod go-w), while the state directory (~/.urnetwork) lives in the
# service user's home.
service_user="$(resolve_service_user)"
service_home="$(user_home "$service_user")"
install_path="/usr/local/libexec/urnetwork-provider"
provider_bin="$install_path/bin/urnetwork"
state_dir="$service_home/.urnetwork"
github_api="https://api.github.com/repos/full-bars/sn"
github_raw="https://raw.githubusercontent.com/full-bars/sn/refs/heads/main"

# --- rc.d ---

# priv runs a command as root, through doas (the OpenBSD default) or sudo when
# we are not already root. rcctl and the rc.d script's own mkdir/install need
# root, and a non-root run that fails silently is how an install could report
# success with nothing running.
#
# PRIV_NOPROMPT=1 adds -n so a password prompt fails at once instead of
# blocking. Both doas and sudo read the password from the terminal.
priv() {
    if [ "$(id -u)" = "0" ]; then
        "$@"
    elif command -v doas > /dev/null 2>&1 && [ -f /etc/doas.conf ]; then
        doas ${PRIV_NOPROMPT:+-n} "$@"
    elif command -v sudo > /dev/null 2>&1; then
        sudo ${PRIV_NOPROMPT:+-n} "$@"
    else
        pr_err "root is required to run: %s" "$*"
        pr_err "set up doas (/etc/doas.conf), install sudo (pkg_add sudo) or re-run as root"
        return 1
    fi
}

load_service() {
    priv rcctl start "$service_name"
}

unload_service() {
    priv rcctl stop "$service_name"
}

restart_service() {
    priv rcctl restart "$service_name"
}

service_running() {
    # The script must exist first, or rcctl errors on a fresh box.
    [ -x "$rc_script" ] || return 1
    # `rcctl check` matches the process table with pgrep(1), which any user can
    # read, so it needs no privilege.
    rcctl check "$service_name" >/dev/null 2>&1
}

# enable_service turns on start at boot. rcctl enable adds a non-base service to
# pkg_scripts in /etc/rc.conf.local. OpenBSD cannot tell "never configured"
# from "disabled by the operator", so the caller enables only on a fresh
# install and an update leaves the operator's choice alone.
enable_service() {
    priv rcctl enable "$service_name"
}

# install_wrapper_tool downloads this script as the urnet-tools stand-in.
# Staged to a temp file and renamed so a running tool is never truncated in
# place (ETXTBSY would otherwise break `update` while the tool is executing).
install_wrapper_tool() {
    _td="$1"
    http_fetch "$github_raw/scripts/Provider_Install_OpenBSD.sh" "$_td/urnet-tools.wrapper" 2>/dev/null || return 1
    chmod 755 "$_td/urnet-tools.wrapper"
    priv mv -f "$_td/urnet-tools.wrapper" "$install_path/bin/urnet-tools"
    # mv keeps the invoker's ownership when the temp dir is on the same
    # filesystem, so the owner must be set explicitly: see do_install.
    priv chown root:wheel "$install_path/bin/urnet-tools" 2>/dev/null || \
        pr_warn "could not chown %s to root:wheel; a non-root owner can rewrite a binary root runs" "$install_path/bin/urnet-tools"
    priv chmod 755 "$install_path/bin/urnet-tools"
}

# --- rc.d script ---
#
# One template, emitted on stdout by write_rc_script_body so the root and
# non-root paths in do_install cannot drift apart. write_rc_script just wraps it
# into the file.

write_rc_script() {
    write_rc_script_body "${1:-$(resolve_service_user)}" > "$rc_script" || return 1
    chmod 755 "$rc_script"
}

# write_rc_script_body [user] emits the rc.d script on stdout so it can be piped
# to `priv tee` without a second copy of the template drifting.
#
# $1 is the account the provider runs as. It defaults to the invoking user, but
# the caller must pass the resolved SUDO_USER-derived name: under sudo, $USER is
# commonly "root", and a service that runs the internet-facing relay as root is
# exactly what this must not produce.
#
# Paths are quoted in every command that interpolates them, because an install
# under a home directory containing a space would otherwise break `cd` and
# `su -c` at boot.
write_rc_script_body() {
    run_user="${1:-$(resolve_service_user)}"
    cat <<RCSCRIPT
#!/bin/ksh
#
# rc.d(8) script for the URnetwork provider. Written by Provider_Install_OpenBSD.sh.
#
#   rcctl enable $service_name                start at boot
#   rcctl start|stop|restart|check $service_name
#   rcctl set $service_name flags "<flags>"   extra flags, added after "provide"

# The default process expression includes "provide" from daemon.
daemon="$provider_bin provide"
daemon_user="$run_user"
logdir="/var/log/$service_name"

. /etc/rc.d/rc.subr

daemon_logger="daemon.info"

# Let the version-specific default rc_start background the foreground provider.
rc_bg=YES
rc_reload=NO

rc_pre() {
    install -d -o root -g wheel -m 755 "\$logdir"
}

rc_cmd \$1
RCSCRIPT
}

# assert_state_path_safe runs BEFORE anything is installed. A symlink anywhere
# in the service user's own path sends root's mkdir and chmod somewhere else,
# and checking only the leaf misses a symlinked parent. It must happen early:
# refusing after the binary has been renamed into place leaves a half-done
# install. System ancestors above the home are not checked, because legitimately
# symlinked system paths (/home -> /usr/home) would make every install refuse.
assert_state_path_safe() {
    if [ -L "$service_home" ]; then
        pr_err "%s is a symlink; refusing to install through it" "$service_home"
        exit 1
    fi
    if [ -L "$state_dir" ]; then
        pr_err "%s is a symlink; refusing to create or chmod through it" "$state_dir"
        exit 1
    fi
}

do_install() {
    # Before the download, before the binary, before any root write.
    assert_state_path_safe
    version="${1:-latest}"

    # Refuse BEFORE downloading: an install that runs the internet-facing
    # relay as root is a privilege escalation the operator never asked for.
    # resolve_service_user only settles on root when the caller is root with
    # no sudo parent and names no override, so this never blocks a normal
    # `sudo install` (SUDO_USER is used) - only a genuine root login.
    if [ "$service_user" = "root" ]; then
        pr_err "refusing to install a service that runs as root"
        pr_err "re-run as a normal user (sudo is fine: SUDO_USER is used),"
        pr_err "or name an account: SERVICE_USER_OVERRIDE=<user> %s install" "$me"
        exit 1
    fi

    if [ "$version" = "latest" ]; then
        release_url="$github_api/releases/latest"
    else
        release_url="$github_api/releases/tags/$version"
    fi

    pr_info "Fetching release info..."
    release_json="$(http_fetch "$release_url" 2>/dev/null)"
    # python3 is not part of the OpenBSD base system, so parsing with it alone
    # made a default `latest` install fail on a clean host. jq first, then
    # python3, then a sed extraction that needs nothing beyond base.
    tag=""
    if command -v jq > /dev/null 2>&1; then
        tag="$(printf '%s' "$release_json" | jq -r '.tag_name // empty' 2>/dev/null)"
    fi
    if [ -z "$tag" ] && command -v python3 > /dev/null 2>&1; then
        tag="$(printf '%s' "$release_json" | python3 -c 'import json,sys; print(json.load(sys.stdin).get("tag_name") or "")' 2>/dev/null)"
    fi
    if [ -z "$tag" ]; then
        tag="$(printf '%s' "$release_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
    fi

    # GitHub API failed or rate-limited: for an explicit version we can trust
    # the caller's tag directly. For "latest" there is no trustworthy fallback
    # here: the branded mirror serves the 3.23-fix fork's tags, so resolving
    # through it would install a different repo's provider.
    if [ -z "$tag" ]; then
        if [ "$version" != "latest" ]; then
            tag="$version"
        else
            pr_err "GitHub API did not return a release tag; pass an explicit version, e.g. install v2026.10.22-1078667780"
            exit 1
        fi
    fi

    if [ -z "$tag" ]; then
        pr_err "Could not fetch release info from %s" "$release_url"
        exit 1
    fi

    arch="$(uname -m)"
    case "$arch" in
        amd64|x86_64)  goarch="amd64" ;;
        arm64|aarch64) goarch="arm64" ;;
        *)             pr_err "Unsupported architecture: %s" "$arch"; exit 1 ;;
    esac

    provider_asset="urnetwork-provider-$tag.tar.gz"
    tool_asset="urnet-tools-openbsd-$goarch"

    # Fail closed. The digest comes from the release metadata, so the tarball is
    # installed only when that metadata lists it. An unreachable or rate-limited
    # API used to leave the digest empty and install the tarball unverified.
    if ! command -v jq > /dev/null 2>&1 && ! command -v python3 > /dev/null 2>&1; then
        pr_err "Neither jq nor python3 is installed, so the release digest cannot be read."
        pr_err "Install one (for example: pkg_add jq) and re-run. Refusing to install an unverified provider binary."
        exit 1
    fi
    _rel_extra=""
    if ! release_lists_asset "$release_json" "$provider_asset"; then
        _rel_extra="$(http_fetch "$github_api/releases/tags/$tag" 2>/dev/null || true)"
        if release_lists_asset "$_rel_extra" "$provider_asset"; then
            release_json="$_rel_extra"
        fi
    fi
    if ! release_lists_asset "$release_json" "$provider_asset"; then
        if [ -z "$release_json" ] && [ -z "$_rel_extra" ]; then
            pr_err "Could not fetch release metadata for %s from the GitHub API (unreachable or rate-limited)." "$tag"
        else
            pr_err "Release %s metadata does not list the provider asset '%s' (an API error or rate limit returns no assets)." "$tag" "$provider_asset"
        fi
        pr_err "Refusing to install an unverified provider binary."
        exit 1
    fi
    provider_digest="$(get_asset_digest_from_api_response "$release_json" "$provider_asset")"
    if [ "$provider_digest" = "__INVALID_SHA256__" ]; then
        pr_err "Release %s has an invalid sha256 digest for provider asset '%s'." "$tag" "$provider_asset"
        pr_err "Refusing to install an unverified provider binary."
        exit 1
    fi
    # Validate the tool asset's digest from this SAME metadata snapshot, before
    # anything on disk is touched. Checking it at the tool step instead put the
    # refusal after the provider had already been backed up and replaced, so a
    # malformed tool digest produced a partial install: a new provider on disk,
    # stale version metadata, no service restart and a leaked backup. A release
    # asset with no digest must read as ABSENT, not as the four characters
    # "null", so an unknown digest means "cannot verify" and takes the same
    # deliberate path.
    tool_digest="$(get_asset_digest_from_api_response "$release_json" "$tool_asset")"
    if [ "$tool_digest" = "__INVALID_SHA256__" ]; then
        pr_err "Release %s has an invalid sha256 digest for tool asset '%s'." "$tag" "$tool_asset"
        pr_err "Refusing to install an unverified urnet-tools."
        exit 1
    fi

    tarball_url="https://github.com/full-bars/sn/releases/download/$tag/$provider_asset"
    pr_info "Downloading %s..." "$tarball_url"

    tmpdir="$(mktemp -d)"
    if ! http_fetch "$tarball_url" "$tmpdir/provider.tar.gz"; then
        pr_err "Failed to download %s" "$tarball_url"
        rm -rf "$tmpdir"
        exit 1
    fi

    if [ -n "$provider_digest" ]; then
        if ! verify_sha256_file "$tmpdir/provider.tar.gz" "$provider_digest"; then
            actual="$(compute_sha256_file "$tmpdir/provider.tar.gz")"
            pr_err "SHA-256 digest mismatch for provider asset '%s': expected %s, got %s" "$provider_asset" "$provider_digest" "${actual:-<unknown>}"
            rm -rf "$tmpdir"
            exit 1
        fi
        pr_info "Provider asset sha256 verified (%s)" "$provider_asset"
    else
        pr_warn "Release %s has no sha256 digest for '%s'; continuing without verification" "$tag" "$provider_asset"
    fi

    tar -xzf "$tmpdir/provider.tar.gz" -C "$tmpdir" || {
        pr_err "Extract failed"
        rm -rf "$tmpdir"
        exit 1
    }

    provider_src="$tmpdir/openbsd/$goarch/provider"
    if [ ! -f "$provider_src" ]; then
        pr_err "OpenBSD/%s binary not found in release tarball" "$goarch"
        rm -rf "$tmpdir"
        exit 1
    fi

    # The tree is root-owned at /usr/local/libexec, which a normal user cannot
    # create. Every write below goes through priv so a non-root install
    # elevates instead of failing at the first mkdir. (This was a regression
    # from moving the tree out of the service user's home.)
    priv mkdir -p "$install_path/bin" || { pr_err "Failed to create %s" "$install_path"; rm -rf "$tmpdir"; exit 1; }
    # `cp` onto a running binary fails with ETXTBSY ("Text file busy"), which
    # is exactly the `update` case: the provider is still executing the file
    # being replaced. Staging next to the target and renaming is atomic, is
    # allowed over a running image, and never leaves a half-written binary.
    # Staging an empty or non-executable file produces a provider that fails at
    # exec time, after the old binary has already been renamed away.
    # -f and -s, not -x: the binary is chmod 755 after staging, so a tarball
    # that lost the exec bit installs fine, while a directory or an empty file
    # still has to be refused before it replaces a working binary.
    if [ ! -f "$provider_src" ] || [ ! -s "$provider_src" ]; then
        pr_err "Refusing to install %s: not a non-empty regular file" "$provider_src"
        rm -rf "$tmpdir"; exit 1
    fi
    staged="$install_path/bin/.urnetwork.new.$$"
    priv cp "$provider_src" "$staged" || { pr_err "Failed to stage binary"; rm -f "$staged"; rm -rf "$tmpdir"; exit 1; }
    priv chmod 755 "$staged"

    provider_backup="$install_path/bin/.urnetwork.bak.$$"
    had_old_binary=0
    if [ -f "$provider_bin" ]; then
        had_old_binary=1
        _live_bin="$provider_bin"
        if ! priv ln -f "$_live_bin" "$provider_backup" 2>/dev/null; then
            priv cp -p "$_live_bin" "$provider_backup" 2>/dev/null || \
            priv cp "$_live_bin" "$provider_backup" 2>/dev/null || {
                # No rollback copy means no way back, so the live binary stays.
                pr_err "Could not back up %s; refusing to replace it without a rollback copy" "$provider_bin"
                priv rm -f "$staged" "$provider_backup" 2>/dev/null || true
                rm -rf "$tmpdir"; exit 1
            }
        fi
    fi

    priv mv -f "$staged" "$provider_bin" || {
        pr_err "Failed to install binary"
        rm -f "$staged"
        if [ "$had_old_binary" -eq 1 ] && [ ! -f "$provider_bin" ] && [ -f "$provider_backup" ]; then
            priv mv -f "$provider_backup" "$provider_bin" 2>/dev/null || true
        fi
        rm -rf "$tmpdir"
        exit 1
    }

    # Install the tool: the Go urnet-tools binary shipped as a release asset,
    # digest-verified; fall back to this shell script for releases that
    # predate the Go asset.
    # tool_asset and its validated tool_digest were resolved from the release
    # metadata before any file on disk was touched, so reaching here means the
    # digest is either absent (a release that predates the Go asset) or a real
    # sha256. Nothing to refuse now, and refusing here would be too late.

    if [ -n "$tool_digest" ]; then
        if http_fetch "https://github.com/full-bars/sn/releases/download/$tag/$tool_asset" "$tmpdir/$tool_asset" 2>/dev/null; then
            # OpenBSD ships sha256(1); fall back to openssl where it is absent.
            if command -v sha256 > /dev/null 2>&1; then
                actual="$(sha256 -q "$tmpdir/$tool_asset")"
            elif command -v openssl > /dev/null 2>&1; then
                actual="$(openssl dgst -sha256 "$tmpdir/$tool_asset" | awk '{print $NF}')"
            else
                actual=""
            fi
            if [ -n "$actual" ] && [ "$actual" = "$tool_digest" ]; then
                priv mv -f "$tmpdir/$tool_asset" "$install_path/bin/urnet-tools"
                # the download ran as the INVOKING user, and mv is a
                # rename (owner preserved) whenever $tmpdir shares the
                # filesystem, so the owner has to be set here rather than
                # inherited. The tool is run by root - `sudo urnet-tools
                # update` and the weekly auto-update entry - so a
                # SUDO_USER-owned copy would let the service user rewrite a
                # binary root executes. The chown -R below is deliberately
                # gated on id -u 0 and does not run on this sudo path.
                priv chown root:wheel "$install_path/bin/urnet-tools" 2>/dev/null || \
                    pr_warn "could not chown %s to root:wheel; a non-root owner can rewrite a binary root runs" "$install_path/bin/urnet-tools"
                priv chmod 755 "$install_path/bin/urnet-tools"
            else
                pr_warn "urnet-tools sha256 mismatch, falling back to shell wrapper"
                install_wrapper_tool "$tmpdir" || true
            fi
        else
            pr_warn "urnet-tools download failed, falling back to shell wrapper"
            install_wrapper_tool "$tmpdir" || true
        fi
    else
        # Without jq or python3 the digest cannot be read, and the wrapper is
        # fetched from the branch, not from $tag. Say so rather than installing
        # an unpinned script as if it were the release asset.
        pr_warn "no jq or python3 on this host: cannot verify the urnet-tools digest, falling back to the shell wrapper fetched from the branch (not from %s)" "$tag"
        install_wrapper_tool "$tmpdir" || true
    fi

    # The install tree stays ROOT-OWNED and group/other-writable-never. The
    # provider binary is executed by root on every hot-swap and by the rc.d
    # script on restart, so a service-user-writable binary is a privilege
    # boundary the installer must not create. Only the STATE directory belongs
    # to the service user: that is where its own config and JWT live.
    if [ "$(id -u)" = "0" ]; then
        if [ "$service_user" != "root" ]; then
            # Re-check here, not only at the top of do_install: the service
            # user can replace the directory with a symlink between the two,
            # and these are the root writes that would follow it. -h keeps the
            # recursive chown from dereferencing anything that appears.
            if [ -L "$state_dir" ]; then
                pr_err "%s became a symlink during install; refusing to write through it" "$state_dir"
                rm -rf "$tmpdir"; exit 1
            fi
            mkdir -p "$state_dir"
            chown -R -h "$service_user" "$state_dir" 2>/dev/null || true
            chmod 700 "$state_dir" 2>/dev/null || true
        fi
        chown -R root:wheel "$install_path" 2>/dev/null || \
            chown -R root "$install_path" 2>/dev/null || true
        chmod -R go-w "$install_path" 2>/dev/null || true
        # The binaries must stay executable by the service user while the
        # directories above them are not writable by anyone else.
        chmod 755 "$install_path" "$install_path/bin" 2>/dev/null || true
        [ -f "$provider_bin" ] && chmod 755 "$provider_bin" 2>/dev/null || true
    fi

    # Written through priv (doas, then sudo) when not root. A fresh install also
    # enables start at boot. An update leaves the operator's choice alone,
    # because rcctl cannot tell "never configured" from "disabled".
    if [ "$(id -u)" != "0" ]; then
        pr_info "Installing the rc.d service requires root; re-running that step with doas or sudo..."
    fi
    if ! priv mkdir -p "$(dirname "$rc_script")" ||
       ! write_rc_script_body "$service_user" | priv tee "$rc_script" > /dev/null ||
       ! priv chmod 755 "$rc_script"; then
        pr_warn "Could not write %s. Install the service manually as root:" "$rc_script"
        pr_warn "  %s install-service" "$me"
        exit 1
    fi
    if [ "${UPDATING:-0}" != "1" ]; then
        enable_service || pr_warn "Could not enable %s at boot; run: rcctl enable %s" "$service_name" "$service_name"
    fi

    # Start policy: a fresh install starts the service; an update restarts it
    # only if it was already running, so a deliberately stopped provider is not
    # resurrected by an unattended `update` from cron. Failures are fatal -
    # reporting "installed" for a service that did not start is a false green.
    # service_was_running is recorded BEFORE the restart: a failed restart leaves
    # the service stopped, so checking afterwards would skip the rollback restart.
    service_ok=1
    service_was_running=0
    if service_running; then
        service_was_running=1
        restart_service || { service_ok=0; pr_err "Failed to restart the service after install"; }
    elif [ "${UPDATING:-0}" != "1" ]; then
        load_service || { service_ok=0; pr_err "Failed to start the service"; }
    else
        pr_info "Service is not running; leaving it stopped (run '%s start' when ready)" "$me"
    fi

    if [ "$service_ok" -ne 1 ]; then
        if [ "$had_old_binary" -eq 1 ] && [ -f "$provider_backup" ]; then
            if ! priv mv -f "$provider_backup" "$provider_bin"; then
                pr_err "Could not restore the previous binary from %s; the service is NOT running. Put that file back at %s and start the service with: %s start" "$provider_backup" "$provider_bin" "$me"
            else
                pr_err "Restored previous binary after service restart failure"
                if [ "$service_was_running" -eq 1 ]; then
                    if restart_service; then
                        pr_warn "Service restarted on the previous binary; the update did not apply."
                    else
                        pr_err "The service did NOT restart on the previous binary and is stopped. Start it with: %s start" "$me"
                    fi
                else
                    pr_warn "The service was not running before the install, so it was left stopped."
                fi
            fi
        fi
        rm -rf "$tmpdir"
        exit 1
    fi
    if [ "$had_old_binary" -eq 1 ] && ! priv rm -f "$provider_backup" 2>/dev/null; then
        # The install is still good. Nothing later reads provider_backup, so say
        # where the stray copy is rather than dropping the failure silently.
        pr_warn "Could not remove the backup binary at %s; it is left on disk" "$provider_backup"
    fi

    # Written only now that the service answers. Writing it before the start
    # left the version file naming the failed release after a rollback. The
    # version file lives in a directory the service user can write to, so a
    # symlink planted at that name would redirect root's write. Write beside it
    # and rename over it: rename replaces the entry instead of following it.
    ver_tmp="$install_path/.version.new.$$"
    printf '%s\n' "$tag" | priv tee "$ver_tmp" > /dev/null && \
        priv mv -f "$ver_tmp" "$install_path/version"

    # PATH hint goes into the SERVICE user's shell rc, once. Under sudo $HOME
    # is root's, and root's PATH never contains the service user's bin dir, so
    # the old check re-appended a duplicate line on every install and update.
    if [ "${UPDATING:-0}" != "1" ]; then
        _shell="$(user_shell "$service_user")"
        shell_rc="$service_home/.profile"
        case "$_shell" in
            */zsh)  shell_rc="$service_home/.zshrc" ;;
            */bash) shell_rc="$service_home/.bash_profile" ;;
            */tcsh|*/csh) shell_rc="$service_home/.cshrc" ;;
        esac
        # The service user owns this directory, so a symlink planted at the rc
        # file's name sent root's append (and chown) into a file of their
        # choosing. Refuse a link outright; the provider is installed by now,
        # so warn and carry on instead of failing the whole install.
        if [ -L "$shell_rc" ]; then
            pr_warn "Refusing to edit %s: it is a symlink. Add %s/bin to PATH by hand" "$shell_rc" "$install_path"
        else
            # Everything below runs AS THE SERVICE USER. Root must not reopen a
            # path inside a directory that user owns: between the check above
            # and any root cat/chmod/chown/mv the name can be swapped for a
            # link, and root would follow it. The owner editing their own rc
            # crosses no privilege boundary, so the whole edit - copy, append,
            # mode, rename - is handed to that user and root only reports on it.
            case "$_shell" in
                */tcsh|*/csh) rc_line="setenv PATH \"$install_path/bin:\$PATH\"" ;;
                *)      rc_line="export PATH=\"$install_path/bin:\$PATH\"" ;;
            esac
            # Pick HOW the edit runs BEFORE the heredoc: `su` is only there to
            # cross a privilege boundary. As a non-root user it prompts for a
            # password, and an unattended install would sit at that prompt;
            # someone who IS the service user needs no switch at all; anyone
            # else cannot write that rc, so warn and skip. The provider is
            # installed by now - none of these paths may fail the install.
            #
            # The script goes in on stdin and is run by /bin/sh explicitly.
            # `su -c` would otherwise hand it to the login shell, and a tcsh
            # service user cannot parse POSIX sh. The login name must come
            # BEFORE -c: su reads an earlier -c as a login class, not as
            # the command (its -m keeps the environment, so these assignments
            # reach the script). One heredoc body, one runner: the mode only
            # decides which command receives that same stdin.
            _rc_user="$(id -un 2>/dev/null)"
            _rc_skip=0
            if [ "$_rc_user" = "$service_user" ] || [ "$(id -u 2>/dev/null)" = 0 ]; then
                _rc_edit() { # <rc> <line> <bin>: feed the stdin script to the owner
                    SHELLRC_RC="$1" SHELLRC_LINE="$2" SHELLRC_BIN="$3"
                    export SHELLRC_RC SHELLRC_LINE SHELLRC_BIN
                    if [ "$_rc_user" = "$service_user" ]; then
                        /bin/sh -s
                    else
                        su -m "$service_user" -c '/bin/sh -s'
                    fi
                }
            else
                _rc_skip=1
            fi
            if [ "$_rc_skip" = 1 ]; then
                pr_warn "Skipped adding %s/bin to PATH in %s: this installer runs as %s, not root or %s. Add it by hand" \
                    "$install_path" "$shell_rc" "$_rc_user" "$service_user"
            else
                _rc_edit "$shell_rc" "$rc_line" "$install_path/bin" <<'SHELLRC_END'
# Runs as the service user, who owns both the rc file and its directory.
# Exit: 0 wrote it, 10 already active, anything else failed and left the rc
# byte-for-byte alone (the temp is removed on every failure path).
set -u
rc="${SHELLRC_RC:-}"
line="${SHELLRC_LINE:-}"
bin="${SHELLRC_BIN:-}"
[ -n "$rc" ] && [ -n "$line" ] && [ -n "$bin" ] || exit 7
# mv would move *into* a directory and cat would block forever on a FIFO.
if [ -e "$rc" ] || [ -L "$rc" ]; then
    [ -f "$rc" ] || exit 8
fi
# Only an ACTIVE entry counts: a line whose first non-space character is not
# '#'. A commented-out export used to suppress the append for good.
if [ -f "$rc" ]; then
    if awk -v want="$bin" '
        { line = $0; sub(/^[[:space:]]+/, "", line) }
        line !~ /^#/ && index(line, want) > 0 { found = 1; exit }
        END { exit(found ? 0 : 1) }
    ' "$rc"; then
        exit 10
    fi
fi
# mktemp is O_EXCL and runs as the owner: no name can be planted ahead of it.
tmp="$(mktemp "$rc.new.XXXXXX")" || exit 3
if [ -f "$rc" ] && ! cat "$rc" > "$tmp"; then
    rm -f "$tmp"
    exit 4
fi
# Without this the entry is glued onto the last line; when the file already
# ends with a newline, adding another would leave a blank line.
last="$(tail -c 1 "$tmp" 2>/dev/null | wc -l | tr -d ' ')"
if [ -s "$tmp" ] && [ "${last:-0}" -eq 0 ]; then
    ( printf '\n' ) >> "$tmp" || { rm -f "$tmp"; exit 5; }
fi
# Subshell so a failed write cannot take this script down before the cleanup;
# the status is checked, and a half-written temp is never renamed over the rc.
( printf '%s\n' "$line" ) >> "$tmp" || { rm -f "$tmp"; exit 5; }
mode="$(stat -c '%a' "$rc" 2>/dev/null || stat -f '%Lp' "$rc" 2>/dev/null)"
chmod "${mode:-644}" "$tmp" 2>/dev/null || { rm -f "$tmp"; exit 6; }
mv -f "$tmp" "$rc" || { rm -f "$tmp"; exit 6; }
SHELLRC_END
                case "$?" in
                    0)  pr_info "Added %s to PATH in %s" "$install_path/bin" "$shell_rc" ;;
                    10) : ;;
                    *)  pr_warn "Could not update %s; add %s/bin to PATH by hand" "$shell_rc" "$install_path" ;;
                esac
            fi
        fi
    fi

    rm -rf "$tmpdir"

    pr_info "URnetwork provider %s installed" "$tag"
    pr_info "  Binary:  %s" "$provider_bin"
    pr_info "  Service: %s" "$rc_script"
    pr_info "  Data:    %s" "$state_dir"
    pr_info ""
    pr_info "Commands:  urnet-tools start|stop|restart|status|hot-restart|session|proxy"
    pr_info "Restart your terminal or run 'hash -r' for urnet-tools to be found"
}

do_install_service() {
    if [ "$(id -u)" != "0" ]; then
        pr_err "install-service needs root: doas %s install-service" "$me"
        exit 1
    fi
    # Running the provider as root because a bootstrap was run with sudo, or
    # because the box was logged into as root, would be a privilege
    # escalation the operator never asked for: refuse and say what to do.
    service_user="$(resolve_service_user)"
    if [ "$service_user" = "root" ]; then
        pr_err "refusing to install a service that runs as root"
        pr_err "re-run as a normal user, or set SERVICE_USER_OVERRIDE=<user>"
        exit 1
    fi
    mkdir -p "$(dirname "$rc_script")" || { pr_err "cannot create service directory"; exit 1; }
    write_rc_script "$service_user" || { pr_err "cannot write %s" "$rc_script"; exit 1; }
    rcctl enable "$service_name" || { pr_err "rcctl enable %s failed" "$service_name"; exit 1; }
    pr_info "Service installed at %s (runs as %s)" "$rc_script" "$service_user"
}

# --- runtime commands ---

do_start() {
    if service_running; then
        pr_info "Provider is already running"
        return 0
    fi
    if load_service; then
        pr_info "Provider started"
    else
        pr_err "Failed to start the provider"
        exit 1
    fi
}

do_stop() {
    if unload_service; then
        pr_info "Provider stopped"
    else
        pr_err "Failed to stop the provider"
        exit 1
    fi
}

do_restart() {
    if [ "$FORCE" != "1" ]; then
        printf "Restart the provider? [y/N]: "
        read -r yn < /dev/tty
        case "$yn" in
            [Yy]*) ;;
            *) pr_info "Aborted."; exit 0 ;;
        esac
    fi
    if restart_service; then
        pr_info "Provider restarted"
    else
        pr_err "Failed to restart the provider"
        exit 1
    fi
}

do_status() {
    if service_running; then
        pr_info "Provider is running"
    else
        pr_info "Provider is stopped"
    fi
}

do_version() {
    if [ -f "$install_path/version" ]; then
        cat "$install_path/version"
    else
        pr_err "Provider not installed"
        exit 1
    fi
}

do_update() {
    version="${1:-latest}"
    # Tells do_install to restart (not start) the service and to leave the
    # operator's rc.conf choices and shell rc alone.
    UPDATING=1
    do_install "$version"
    pr_info "Update complete."
}

# write_state_flag sets a flag file (on/off) under state_dir.
#
# state_dir lives in the service user's home, so under sudo the writer may
# be root while the directory is user-controlled: a symlink planted at the
# flag path would redirect a root write anywhere on the filesystem. Refuse
# a symlink outright, and write through a temp file + rename(2) - rename
# replaces the destination dirent and never follows a trailing symlink, so
# even a race cannot turn the final step into an arbitrary write. 0644
# matches what a plain printf > would have created (umask 022), keeping
# the flag readable by the service user after a root run.
write_state_flag() {
    _flag="$state_dir/$1"
    _val="$2"
    if [ -h "$_flag" ]; then
        pr_err "refusing to write %s: it is a symlink" "$_flag"
        return 1
    fi
    mkdir -p "$state_dir"
    _tmp=$(mktemp "$state_dir/.flag.XXXXXX") || {
        pr_err "cannot create temp file under %s" "$state_dir"
        return 1
    }
    if ! { printf '%s\n' "$_val" > "$_tmp" && chmod 0644 "$_tmp" && mv -f "$_tmp" "$_flag"; }; then
        rm -f "$_tmp"
        pr_err "cannot write %s" "$_flag"
        return 1
    fi
}

do_hot_restart() {
    mode="${1:-}"
    file="$state_dir/hot_restart"
    case "$mode" in
        on)    write_state_flag hot_restart on  || exit 1; pr_info "hot-restart enabled" ;;
        off)   write_state_flag hot_restart off || exit 1; pr_info "hot-restart disabled" ;;
        status|"")
            if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                pr_info "hot-restart: on"
            else
                pr_info "hot-restart: off"
            fi
            ;;
        *) pr_err "Usage: urnet-tools hot-restart <on|off|status>"; exit 1 ;;
    esac
}

do_session() {
    action="${1:-}"
    shift 2>/dev/null || true
    case "$action" in
        save)
            [ -n "${1:-}" ] || { pr_err "Usage: urnet-tools session save <file>"; exit 1; }
            mkdir -p "$state_dir"
            "$provider_bin" session-save "$1" || { pr_err "session save failed"; exit 1; }
            pr_info "Session saved to %s" "$1"
            ;;
        load)
            [ -n "${1:-}" ] || { pr_err "Usage: urnet-tools session load <file>"; exit 1; }
            "$provider_bin" session-load "$1" || { pr_err "session load failed"; exit 1; }
            pr_info "Session loaded; restarting"
            restart_service
            ;;
        *) pr_err "Usage: urnet-tools session <save|load> <file>"; exit 1 ;;
    esac
}

do_proxy() {
    action="${1:-}"
    case "$action" in
        refresh)     "$provider_bin" proxy-refresh ;;
        remove-dead) "$provider_bin" proxy-remove-dead ;;
        summary)     "$provider_bin" proxy-summary ;;
        *)           pr_err "Usage: urnet-tools proxy <refresh|remove-dead|summary>"; exit 1 ;;
    esac
}

# --- arg parsing ---

FORCE=0
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help)    show_help; exit 0 ;;
        -v|--version) do_version; exit 0 ;;
        -f|--force)   FORCE=1; shift ;;
        *)            break ;;
    esac
done

operation="${1:-}"
[ -n "$operation" ] && shift

case "$operation" in
    install)         do_install "$@" ;;
    install-service) do_install_service ;;
    update)          do_update "$@" ;;
    start)           do_start ;;
    stop)            do_stop ;;
    restart)         do_restart ;;
    status)          do_status ;;
    version)         do_version ;;
    hot-restart)     do_hot_restart "$@" ;;
    self-heal)
        file="$state_dir/proxy_self_heal"
        case "${1:-}" in
            on) write_state_flag proxy_self_heal on  || exit 1; pr_info "Self-heal enabled" ;;
            off) write_state_flag proxy_self_heal off || exit 1; pr_info "Self-heal disabled" ;;
            status|"")
                if [ -f "$file" ] && [ "$(cat "$file" 2>/dev/null)" = "on" ]; then
                    pr_info "self-heal: on"
                else
                    pr_info "self-heal: off"
                fi
                if [ -f "$state_dir/pressure_status" ]; then
                    if command -v jq >/dev/null 2>&1; then
                        pr_info "$(jq -r '"pressure: \(.score) (target_pool=\(.target_pool), updated=\(.updated))"' \
                            "$state_dir/pressure_status" 2>/dev/null)"
                    else
                        cat "$state_dir/pressure_status"
                    fi
                fi
                ;;
            *) pr_err "Usage: urnet-tools self-heal [on|off|status]"; exit 1 ;;
        esac
        ;;
    session)         do_session "$@" ;;
    proxy)           do_proxy "$@" ;;
    auth)            "$provider_bin" auth "$@" ;;
    logs)            "$provider_bin" logs "$@" ;;
    "")
        show_help
        exit 0
        ;;
    *)
        pr_err "Unknown command: %s" "$operation"
        echo "Run '$me --help' for usage."
        exit 1
        ;;
esac