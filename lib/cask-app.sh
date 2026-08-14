#!/usr/bin/env bash
# Signed Homebrew cask app installs. Only homebrew/cask tokens on the
# allowlist are accepted; download hosts and Developer ID Team IDs are
# checked so a shadowed or third-party cask cannot land an impostor app.

managed_machine_system_appdir() {
    printf '%s\n' "${MANAGED_MACHINE_SYSTEM_APPDIR:-/Applications}"
}

managed_machine_user_appdir() {
    printf '%s\n' "${HOME}/Applications"
}

ensure_brew_on_path() {
    if command -v brew >/dev/null 2>&1; then
        return 0
    fi
    local brew
    for brew in /opt/homebrew/bin/brew /usr/local/bin/brew; do
        if [[ -x "$brew" ]]; then
            eval "$("$brew" shellenv)"
            return 0
        fi
    done
    return 1
}

# token|app_name|team_id|url_hosts|homepage_hosts
cask_allowlist_row() {
    case "$1" in
        visual-studio-code)
            printf '%s\n' 'Visual Studio Code.app|UBF8T346G9|update.code.visualstudio.com,code.visualstudio.com|code.visualstudio.com'
            ;;
        cursor)
            printf '%s\n' 'Cursor.app|VDXQ22DGB9|downloads.cursor.com|cursor.com,www.cursor.com'
            ;;
        claude)
            printf '%s\n' 'Claude.app|Q6L2SF6YDW|downloads.claude.ai|claude.com,www.claude.com'
            ;;
        antigravity)
            printf '%s\n' 'Antigravity.app|EQHXZ8M8AV|storage.googleapis.com|antigravity.google'
            ;;
        antigravity-ide)
            printf '%s\n' 'Antigravity IDE.app|EQHXZ8M8AV|edgedl.me.gvt1.com|antigravity.google'
            ;;
        *)
            return 1
            ;;
    esac
}

cask_qualified_token() {
    printf 'homebrew/cask/%s\n' "$1"
}

resolve_cask_appdir() {
    local override="${1:-}"
    local system_appdir user_appdir
    if [[ -n "$override" ]]; then
        printf '%s\n' "$override"
        return 0
    fi
    system_appdir="$(managed_machine_system_appdir)"
    user_appdir="$(managed_machine_user_appdir)"
    if [[ -d "$system_appdir" && -w "$system_appdir" ]]; then
        printf '%s\n' "$system_appdir"
    else
        printf '%s\n' "$user_appdir"
    fi
}

find_cask_app() {
    local app_name="$1"
    local override="${2:-}"
    local dir
    for dir in "$override" "$(managed_machine_system_appdir)" "$(managed_machine_user_appdir)"; do
        [[ -n "$dir" && -d "$dir/$app_name" ]] || continue
        printf '%s/%s\n' "$dir" "$app_name"
        return 0
    done
    return 1
}

# Verify brew will install the official homebrew/cask formula: exact token,
# tap, a real sha256 (not no_check), and vendor download/homepage hosts.
verify_cask_source() {
    local token="$1"
    local url_hosts="$2"
    local homepage_hosts="$3"
    local json
    json="$(brew info --json=v2 --cask "$(cask_qualified_token "$token")")" || {
        echo "Error: could not read Homebrew cask metadata for $token" >&2
        return 1
    }
    EXPECT_TOKEN="$token" ALLOWED_URL_HOSTS="$url_hosts" ALLOWED_HOME_HOSTS="$homepage_hosts" \
        python3 -c '
import json, os, sys
from urllib.parse import urlparse

def norm_host(value):
    host = (value or "").strip().lower()
    if host.startswith("www."):
        host = host[4:]
    return host

def hosts(value):
    return {norm_host(h) for h in value.split(",") if h.strip()}

data = json.load(sys.stdin)
casks = data.get("casks") or []
if len(casks) != 1:
    sys.stderr.write("Error: expected exactly one cask record\n")
    sys.exit(1)
cask = casks[0]
expect = os.environ["EXPECT_TOKEN"]
if cask.get("token") != expect:
    sys.stderr.write("Error: cask token %r does not match %r\n" % (cask.get("token"), expect))
    sys.exit(1)
if cask.get("tap") != "homebrew/cask":
    sys.stderr.write("Error: refusing cask %s from tap %r; only homebrew/cask is allowed\n" % (expect, cask.get("tap")))
    sys.exit(1)
digest = (cask.get("sha256") or "").lower()
if digest in ("", "no_check") or len(digest) != 64 or any(ch not in "0123456789abcdef" for ch in digest):
    sys.stderr.write(f"Error: refusing cask {expect}: Homebrew did not publish a sha256 checksum\n")
    sys.exit(1)
url_host = norm_host(urlparse(cask.get("url") or "").hostname)
home_host = norm_host(urlparse(cask.get("homepage") or "").hostname)
if url_host not in hosts(os.environ["ALLOWED_URL_HOSTS"]):
    sys.stderr.write(f"Error: refusing cask {expect}: download host {url_host!r} is not on the allowlist\n")
    sys.exit(1)
if home_host not in hosts(os.environ["ALLOWED_HOME_HOSTS"]):
    sys.stderr.write(f"Error: refusing cask {expect}: homepage host {home_host!r} is not on the allowlist\n")
    sys.exit(1)
' <<<"$json"
}

# Require a valid Developer ID signature from the expected Team ID.
verify_app_signature() {
    local app="$1"
    local team_id="$2"
    local detail team
    if ! codesign --verify --deep --strict "$app" 2>/dev/null; then
        echo "Error: $app failed codesign verification" >&2
        return 1
    fi
    detail="$(codesign -dv --verbose=2 "$app" 2>&1)" || {
        echo "Error: could not read the code signature for $app" >&2
        return 1
    }
    if ! grep -Fq 'Authority=Developer ID Application' <<<"$detail"; then
        echo "Error: $app is not signed with Developer ID Application" >&2
        return 1
    fi
    team="$(sed -n 's/^TeamIdentifier=//p' <<<"$detail" | head -1)"
    if [[ "$team" != "$team_id" ]]; then
        echo "Error: $app Team ID is ${team:-missing}, expected $team_id" >&2
        return 1
    fi
}

# install_signed_cask_app <token> [appdir-override]
install_signed_cask_app() {
    local token="$1"
    local override="${2:-}"
    local row app_name team_id url_hosts homepage_hosts appdir installed qualified

    row="$(cask_allowlist_row "$token")" || {
        echo "Error: $token is not on the signed-cask allowlist" >&2
        return 1
    }
    IFS='|' read -r app_name team_id url_hosts homepage_hosts <<<"$row"

    if ! ensure_brew_on_path; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "Error: python3 is required to verify Homebrew cask metadata" >&2
        return 1
    fi

    if installed="$(find_cask_app "$app_name" "$override")"; then
        verify_app_signature "$installed" "$team_id" || return 1
        echo "$app_name already installed: $installed"
        brew list --cask --versions "$token" 2>/dev/null || true
        return 0
    fi

    verify_cask_source "$token" "$url_hosts" "$homepage_hosts" || return 1

    appdir="$(resolve_cask_appdir "$override")"
    if [[ -z "$override" && "$appdir" == "$(managed_machine_user_appdir)" ]]; then
        echo "note: $(managed_machine_system_appdir) requires administrator access — installing to $appdir instead"
    fi
    mkdir -p "$appdir"

    qualified="$(cask_qualified_token "$token")"
    echo "Installing $app_name from $qualified into $appdir..."
    brew install --cask "$qualified" --appdir="$appdir"

    installed="$(find_cask_app "$app_name" "$override")" || {
        echo "Install finished but $app_name was not found." >&2
        return 1
    }
    verify_app_signature "$installed" "$team_id" || return 1
    echo "$app_name installed: $installed"
    brew list --cask --versions "$token" 2>/dev/null || true
}

cask_app_status() {
    local token="$1"
    local app_name="$2"
    local override="${3:-}"
    local path brew_ver
    path=""
    if path="$(find_cask_app "$app_name" "$override")"; then
        :
    else
        path=""
    fi
    brew_ver=""
    if command -v brew >/dev/null 2>&1; then
        brew_ver="$(brew list --cask --versions "$token" 2>/dev/null | awk 'NR == 1 { print $2 }' || true)"
        brew_ver="$(sanitize_version "$brew_ver")"
    fi
    if [[ -n "$brew_ver" && -n "$path" ]]; then
        printf '%s (%s)\n' "$brew_ver" "$path"
        return 0
    fi
    if [[ -n "$brew_ver" ]]; then
        printf '%s\n' "$brew_ver"
        return 0
    fi
    if [[ -n "$path" ]]; then
        printf 'installed (%s)\n' "$path"
        return 0
    fi
    printf 'missing\n'
}
