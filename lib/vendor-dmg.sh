#!/usr/bin/env bash
# Install macOS desktop apps by fetching the vendor DMG directly. For apps
# with no homebrew/cask token, so install_signed_cask_app cannot cover them.
# Policy comes from the config-repo catalog (vendor-dmg rows), never from
# brew metadata: a pinned https URL on an allowlisted host (or per-arch
# url_arm64/url_x86_64 pairs), the DMG sha256 (likewise per-arch capable),
# and the Developer ID Team ID. Identity and integrity checks mirror
# signed-cask; a row with allow_rolling_url trades the checksum for
# mandatory Gatekeeper notarization, exactly like chrome's signed-cask row.
# A row with sparkle true names a Sparkle appcast in url; the enclosure
# URL is resolved at download time and must also sit on url_hosts. An
# allowlist entry that starts with "." matches that host and its subdomains
# (CDN hostnames that rotate per request).
#
# A bundle already on disk converges in place: it must first prove its Team
# ID (an impostor is never touched, only reported), then a pinned-version
# row replaces it when the installed version drifts, otherwise the run is a
# no-op. Rows without a version accept presence plus a valid signature.
# `managed-machine adopt` stays cask-only; vendor-dmg occupiers need no
# adopt step because install handles them directly.

# shellcheck source=cask-app.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/cask-app.sh"

# app_name|team_id|url|sha256|url_hosts|version|allow_rolling_url|sparkle — policy
# comes from the config-repo catalog, not brew metadata. The url/sha256 pair
# is resolved for this machine's architecture (per-arch url_arm64/url_x86_64
# pairs fall back to the single url/sha256 pair).
vendor_dmg_allowlist_row() {
    catalog_query dmg-row "$1" "$(uname -m)"
}

# Verify the vendor URL before anything is downloaded.
# mode=host checks https and the allowlist only (an appcast, before the
# enclosure is known). mode=full also requires a real sha256, or no_check
# behind allow_rolling_url.
vendor_dmg_verify_url() {
    local mode="$1"
    local name="$2"
    local url="$3"
    local url_hosts="$4"
    local sha256="${5:-}"
    local allow_rolling="${6:-}"
    NAME="$name" URL="$url" ALLOWED_URL_HOSTS="$url_hosts" DIGEST="$sha256" \
        ALLOW_ROLLING_URL="$allow_rolling" MODE="$mode" \
        python3 -c '
import os, sys
from urllib.parse import urlparse

def norm_host(value):
    host = (value or "").strip().lower()
    if host.startswith("www."):
        host = host[4:]
    return host

def host_allowed(hostname, allowed_raw):
    host = norm_host(hostname)
    for entry in allowed_raw.split(","):
        entry = entry.strip().lower()
        if not entry:
            continue
        if entry.startswith("."):
            suffix = norm_host(entry[1:])
            if suffix and (host == suffix or host.endswith("." + suffix)):
                return True
        elif host == norm_host(entry):
            return True
    return False

name = os.environ["NAME"]
url = urlparse(os.environ["URL"])
if url.scheme != "https" or not url.hostname or url.username or url.password:
    sys.stderr.write(f"Error: refusing vendor DMG {name}: URL must be https\n")
    sys.exit(1)
if not host_allowed(url.hostname, os.environ["ALLOWED_URL_HOSTS"]):
    sys.stderr.write(f"Error: refusing vendor DMG {name}: download host {norm_host(url.hostname)!r} is not on the allowlist\n")
    sys.exit(1)
if os.environ.get("MODE") == "host":
    sys.exit(0)
digest = (os.environ["DIGEST"] or "").lower()
allow_rolling = os.environ.get("ALLOW_ROLLING_URL") == "1"
if digest == "no_check" and allow_rolling:
    sys.stderr.write(
        f"Note: {name} ships a rolling vendor URL with no published checksum; "
        "integrity rests on notarized Developer ID verification (allow_rolling_url)\n"
    )
elif digest in ("", "no_check") or len(digest) != 64 or any(ch not in "0123456789abcdef" for ch in digest):
    sys.stderr.write(f"Error: refusing vendor DMG {name}: no usable sha256 checksum\n")
    sys.exit(1)
'
}

vendor_dmg_verify_host() {
    vendor_dmg_verify_url host "$@"
}

# Verify the vendor URL before anything is downloaded: https only, host on
# the row allowlist, and a real sha256 — or no_check only behind the
# allow_rolling_url opt-in.
vendor_dmg_verify_source() {
    vendor_dmg_verify_url full "$@"
}

# Fetch a Sparkle appcast and print the first enclosure URL. The enclosure
# host is checked against the same allowlist before it is returned.
vendor_dmg_sparkle_enclosure() {
    local name="$1"
    local appcast="$2"
    local url_hosts="$3"
    local tmp enclosure
    tmp="$(mktemp)"
    # shellcheck disable=SC2064
    trap 'rm -f "$tmp"; trap - RETURN' RETURN
    if ! curl -fsSL --max-time 60 -o "$tmp" "$appcast"; then
        echo "Error: downloading the $name appcast failed" >&2
        return 1
    fi
    if ! enclosure="$(python3 - "$name" "$tmp" <<'PY'
import sys
import xml.etree.ElementTree as ET

name, path = sys.argv[1], sys.argv[2]
try:
    root = ET.parse(path).getroot()
except ET.ParseError:
    sys.stderr.write(f"Error: {name} appcast is not valid XML\n")
    sys.exit(1)
enclosure = ""
for el in root.iter():
    if el.tag.endswith("enclosure") and (el.attrib.get("url") or "").strip():
        enclosure = el.attrib["url"].strip()
        break
if not enclosure:
    sys.stderr.write(f"Error: {name} appcast has no enclosure URL\n")
    sys.exit(1)
sys.stdout.write(enclosure)
PY
)"; then
        return 1
    fi
    vendor_dmg_verify_host "$name" "$enclosure" "$url_hosts" || return 1
    printf '%s\n' "$enclosure"
}

# Catalog-name appdir overrides. Muse keeps the env var the cask path used.
vendor_dmg_appdir_override() {
    case "$1" in
        muse-app) printf '%s\n' "${MANAGED_MACHINE_MUSE_APPDIR:-}" ;;
        *) printf '\n' ;;
    esac
}

# Installed bundle version from the on-disk Info.plist. Empty when unreadable.
vendor_dmg_bundle_version() {
    local app="$1"
    python3 -c '
import plistlib, sys
try:
    with open(sys.argv[1], "rb") as fh:
        info = plistlib.load(fh)
except Exception:
    print("")
    sys.exit(0)
print(info.get("CFBundleShortVersionString") or info.get("CFBundleVersion") or "")
' "$app/Contents/Info.plist" 2>/dev/null || true
}

vendor_dmg_find_app() {
    find_cask_app "$1" "${2:-}"
}

vendor_dmg_find_in_mount() {
    local mnt="$1"
    local app_name="$2"
    local found
    if [[ -d "$mnt/$app_name" ]]; then
        printf '%s/%s\n' "$mnt" "$app_name"
        return 0
    fi
    found="$(find "$mnt" -maxdepth 2 -name "$app_name" -type d 2>/dev/null | head -1 || true)"
    if [[ -n "$found" ]]; then
        printf '%s\n' "$found"
        return 0
    fi
    return 1
}

# install_vendor_dmg_from_catalog <name>
install_vendor_dmg_from_catalog() {
    local name="$1"
    local row app_name team_id url sha256 url_hosts version allow_rolling sparkle override
    row="$(vendor_dmg_allowlist_row "$name")" || {
        echo "Error: $name is not a vendor-dmg catalog row" >&2
        return 1
    }
    IFS='|' read -r app_name team_id url sha256 url_hosts version allow_rolling sparkle <<<"$row"
    if [[ -z "$app_name" || -z "$team_id" || -z "$url_hosts" || -z "$sha256" ]]; then
        echo "Error: $name is missing app_name, Team ID, url_hosts, or sha256; refusing unverified vendor DMG" >&2
        return 1
    fi
    if [[ -z "$url" ]]; then
        # A row that serves other architectures is not part of this install;
        # a row with no URL at all is malformed.
        if [[ -n "$(catalog_app_field "$name" url 2>/dev/null || true)" \
            || -n "$(catalog_app_field "$name" url_arm64 2>/dev/null || true)" \
            || -n "$(catalog_app_field "$name" url_x86_64 2>/dev/null || true)" ]]; then
            echo "Skipped: $name serves no build for $(uname -m)" >&2
            return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
        fi
        echo "Error: $name is missing url; refusing unverified vendor DMG" >&2
        return 1
    fi

    if ! command -v curl >/dev/null 2>&1; then
        echo "Error: curl is required to fetch the $app_name DMG" >&2
        return 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "Error: python3 is required to verify the vendor DMG download" >&2
        return 1
    fi
    if ! command -v hdiutil >/dev/null 2>&1; then
        echo "Error: hdiutil is required to mount the $app_name DMG (macOS only)" >&2
        return 1
    fi

    override="$(vendor_dmg_appdir_override "$name")"
    # A Sparkle row's catalog URL is the appcast. Check that host now, and
    # resolve the enclosure only when a download is actually required, so a
    # re-run of an already-installed app does not fetch the feed.
    if [[ "$sparkle" == "1" ]]; then
        vendor_dmg_verify_host "$name" "$url" "$url_hosts" || return 1
    else
        vendor_dmg_verify_source "$name" "$url" "$url_hosts" "$sha256" "$allow_rolling" || return 1
    fi

    local installed=""
    if installed="$(vendor_dmg_find_app "$app_name" "$override")"; then
        # A vendor- (or previous-run-) installed bundle converges in place,
        # so no adopt step exists for this kind. It must first prove its
        # Team ID: an impostor is never replaced, only reported.
        if ! verify_app_signature "$installed" "$team_id" "$allow_rolling"; then
            echo "Error: $installed failed Developer ID / Team ID verification — remove it manually, then re-run" >&2
            return 1
        fi
        local current=""
        current="$(vendor_dmg_bundle_version "$installed")"
        if [[ -z "$version" || "$current" == "$version" ]]; then
            if [[ -n "$current" ]]; then
                echo "$app_name already installed: $installed ($current)"
            else
                echo "$app_name already installed: $installed"
            fi
            return 0
        fi
        echo "$app_name at $installed is ${current:-unknown}; pinned version is $version — replacing..."
    fi

    if [[ "$sparkle" == "1" ]]; then
        url="$(vendor_dmg_sparkle_enclosure "$name" "$url" "$url_hosts")" || return 1
        vendor_dmg_verify_source "$name" "$url" "$url_hosts" "$sha256" "$allow_rolling" || return 1
    fi

    local tmp dmg mnt staged destdir dest
    if [[ -n "$installed" ]]; then
        destdir="$(dirname "$installed")"
        dest="$installed"
    else
        destdir="$(resolve_cask_appdir "$override")"
        dest="$destdir/$app_name"
    fi
    if ! mkdir -p "$destdir" 2>/dev/null; then
        elevate_run "create $destdir" /bin/mkdir -p "$destdir" || return $?
    fi

    tmp="$(mktemp -d "${TMPDIR:-/tmp}/mm-vendor-dmg.XXXXXX")"
    dmg="$tmp/app.dmg"
    echo "Downloading $app_name..."
    if ! curl -fsSL -o "$dmg" "$url"; then
        echo "Error: downloading the $app_name DMG failed" >&2
        rm -rf "$tmp"
        return 1
    fi
    if [[ "$sha256" != "no_check" ]]; then
        local actual=""
        actual="$(shasum -a 256 "$dmg" 2>/dev/null | awk '{print $1}' || true)"
        if [[ "$actual" != "$sha256" ]]; then
            echo "Error: refusing vendor DMG $name: checksum mismatch (download may be corrupt or substituted)" >&2
            rm -rf "$tmp"
            return 1
        fi
    fi

    mnt="$tmp/mnt"
    mkdir -p "$mnt"
    if ! hdiutil attach -nobrowse -readonly -mountpoint "$mnt" "$dmg" >/dev/null 2>&1; then
        echo "Error: could not mount the downloaded $app_name disk image" >&2
        rm -rf "$tmp"
        return 1
    fi
    if ! staged="$(vendor_dmg_find_in_mount "$mnt" "$app_name")"; then
        echo "Error: $app_name not found in the downloaded disk image" >&2
        hdiutil detach "$mnt" >/dev/null 2>&1 || true
        rm -rf "$tmp"
        return 1
    fi
    # The staged bundle is verified BEFORE anything under the destination is
    # touched, so a bad download can never clobber a good install.
    if ! verify_app_signature "$staged" "$team_id" "$allow_rolling"; then
        hdiutil detach "$mnt" >/dev/null 2>&1 || true
        rm -rf "$tmp"
        return 1
    fi

    # Only the copy/remove phase may elevate, and only when the destination
    # is not writable — one dialog per install.
    if [[ -w "$destdir" ]]; then
        if [[ -n "$installed" ]]; then
            rm -rf "$dest" || {
                echo "Error: could not remove $dest" >&2
                hdiutil detach "$mnt" >/dev/null 2>&1 || true
                rm -rf "$tmp"
                return 1
            }
        fi
        cp -R "$staged" "$dest" || {
            echo "Error: could not copy $app_name into $destdir" >&2
            hdiutil detach "$mnt" >/dev/null 2>&1 || true
            rm -rf "$tmp"
            return 1
        }
    else
        if [[ -n "$installed" ]]; then
            elevate_run "replace $app_name in $destdir" \
                /bin/sh -c 'rm -rf -- "$0" && cp -R -- "$1" "$0"' \
                "$dest" "$staged" || {
                hdiutil detach "$mnt" >/dev/null 2>&1 || true
                rm -rf "$tmp"
                return 1
            }
        else
            elevate_run "install $app_name into $destdir" \
                /bin/sh -c 'cp -R -- "$1" "$0"' \
                "$dest" "$staged" || {
                hdiutil detach "$mnt" >/dev/null 2>&1 || true
                rm -rf "$tmp"
                return 1
            }
        fi
    fi
    hdiutil detach "$mnt" >/dev/null 2>&1 || true
    rm -rf "$tmp"

    if ! verify_app_signature "$dest" "$team_id" "$allow_rolling"; then
        return 1
    fi
    local placed_version=""
    placed_version="$(vendor_dmg_bundle_version "$dest")"
    if [[ -n "$placed_version" ]]; then
        echo "$app_name installed: $dest ($placed_version)"
    else
        echo "$app_name installed: $dest"
    fi
}

vendor_dmg_status() {
    local name="$1"
    local app_name installed version pinned
    app_name="$(catalog_app_field "$name" app_name)" || {
        printf 'missing\n'
        return 0
    }
    if ! installed="$(vendor_dmg_find_app "$app_name" "$(vendor_dmg_appdir_override "$name")")"; then
        printf 'missing\n'
        return 0
    fi
    version="$(vendor_dmg_bundle_version "$installed")"
    pinned="$(catalog_app_field "$name" version 2>/dev/null || true)"
    if [[ -n "$version" && -n "$pinned" && "$version" != "$pinned" ]]; then
        printf '%s (%s; pinned %s)\n' "$version" "$installed" "$pinned"
        return 0
    fi
    if [[ -n "$version" ]]; then
        printf '%s (%s)\n' "$version" "$installed"
        return 0
    fi
    printf 'installed (%s)\n' "$installed"
}
