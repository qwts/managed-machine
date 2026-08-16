#!/usr/bin/env bash
# Signed Homebrew cask app installs and adopt. Only homebrew/cask tokens on
# the allowlist are accepted; download hosts and Developer ID Team IDs are
# checked so a shadowed or third-party cask cannot land an impostor app.
# Vendor-installed occupiers are taken over with `managed-machine adopt`,
# not by setup-* (which still refuses a non-cask bundle).

managed_machine_system_appdir() {
    printf '%s\n' "${MANAGED_MACHINE_SYSTEM_APPDIR:-/Applications}"
}

managed_machine_user_appdir() {
    printf '%s\n' "${HOME}/Applications"
}

# token|app_name|team_id|url_hosts|homepage_hosts — policy comes from the
# config-repo catalog, not a hardcoded allowlist.
cask_allowlist_row() {
    catalog_query cask-row "$1"
}

cask_allowlist_tokens() {
    catalog_query cask-tokens
}

cask_setup_name_for_token() {
    catalog_query cask-name "$1"
}

cask_alias_for_token() {
    local name token
    token="$1"
    name="$(catalog_query cask-name "$token")" || return 1
    if [[ "$name" != "$token" ]]; then
        printf '%s\n' "$name"
        return 0
    fi
    return 1
}

cask_token_from_name() {
    catalog_query cask-resolve "$1"
}

cask_appdir_override_for_token() {
    case "$1" in
        visual-studio-code) printf '%s\n' "${MANAGED_MACHINE_VSCODE_APPDIR:-}" ;;
        cursor) printf '%s\n' "${MANAGED_MACHINE_CURSOR_APPDIR:-}" ;;
        claude) printf '%s\n' "${MANAGED_MACHINE_CLAUDE_APPDIR:-}" ;;
        antigravity) printf '%s\n' "${MANAGED_MACHINE_ANTIGRAVITY_APPDIR:-}" ;;
        antigravity-ide) printf '%s\n' "${MANAGED_MACHINE_ANTIGRAVITY_IDE_APPDIR:-}" ;;
        lm-studio) printf '%s\n' "${MANAGED_MACHINE_LMSTUDIO_APPDIR:-}" ;;
        *) printf '\n' ;;
    esac
}

print_adoptable_cask_names() {
    local token alias
    echo
    echo "Available apps (cask token; aliases accepted):"
    while IFS= read -r token; do
        [[ -n "$token" ]] || continue
        if alias="$(cask_alias_for_token "$token")"; then
            printf '  %s  (%s)\n' "$token" "$alias"
        else
            printf '  %s\n' "$token"
        fi
    done < <(cask_allowlist_tokens)
}

cask_qualified_token() {
    printf 'homebrew/cask/%s\n' "$1"
}

resolve_cask_appdir() {
    local override="${1:-}"
    if [[ -n "$override" ]]; then
        printf '%s\n' "$override"
        return 0
    fi
    managed_machine_system_appdir
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

cask_has_receipt() {
    local token="$1"
    local out
    out="$(brew list --cask --versions "$token" 2>/dev/null)" || return 1
    [[ "$out" == "$token "* || "$out" == "$token" ]]
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
    if [[ -z "$app_name" || -z "$team_id" || -z "$url_hosts" || -z "$homepage_hosts" ]]; then
        echo "Error: $token is missing Team ID or vendor host allowlists; refusing unverified desktop cask" >&2
        return 1
    fi

    if ! ensure_brew_on_path; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "Error: python3 is required to verify Homebrew cask metadata" >&2
        return 1
    fi

    if installed="$(find_cask_app "$app_name" "$override")"; then
        if ! cask_has_receipt "$token"; then
            echo "Error: $app_name exists at $installed but is not a Homebrew cask install" >&2
            return 1
        fi
        verify_cask_source "$token" "$url_hosts" "$homepage_hosts" || return 1
        verify_app_signature "$installed" "$team_id" || return 1
        echo "$app_name already installed: $installed"
        brew list --cask --versions "$token" || true
        return 0
    fi

    verify_cask_source "$token" "$url_hosts" "$homepage_hosts" || return 1

    appdir="$(resolve_cask_appdir "$override")"
    mkdir -p "$appdir" 2>/dev/null || elevate_run "create $appdir" /bin/mkdir -p "$appdir" || return 1

    qualified="$(cask_qualified_token "$token")"
    echo "Installing $app_name from $qualified into $appdir..."
    brew_run install --cask "$qualified" --appdir="$appdir"

    installed="$(find_cask_app "$app_name" "$override")" || {
        echo "Install finished but $app_name was not found." >&2
        return 1
    }
    verify_app_signature "$installed" "$team_id" || return 1
    echo "$app_name installed: $installed"
    brew list --cask --versions "$token" 2>/dev/null || true
}

# adopt_signed_cask_app returns this when the app was skipped (not an error).
MANAGED_MACHINE_ADOPT_SKIPPED="${MANAGED_MACHINE_SKIPPED_EXIT:-76}"

cask_app_is_running() {
    local app="$1"
    command -v lsof >/dev/null 2>&1 || return 1
    # +D walks nested helpers (e.g. Cursor Helper.app/Contents/MacOS/...) so a
    # leftover helper that still has the bundle mapped is treated as running.
    [[ -n "$(lsof -t +D "$app" 2>/dev/null || true)" ]]
}

# adopt_signed_cask_app <token> [appdir-override]
#
# Take over a vendor-installed allowlisted app with Homebrew --adopt.
# Returns 0 on success, MANAGED_MACHINE_ADOPT_SKIPPED when skipped, 1 on failure.
adopt_signed_cask_app() {
    local token="$1"
    local override="${2:-}"
    local row app_name team_id url_hosts homepage_hosts
    local installed system_appdir appdir dest parent qualified

    row="$(cask_allowlist_row "$token")" || {
        echo "skipped: $token — not on the signed-cask allowlist"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
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

    if cask_has_receipt "$token"; then
        echo "skipped: $token — already has a Homebrew cask receipt"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
    fi

    if ! installed="$(find_cask_app "$app_name" "$override")"; then
        echo "skipped: $token — $app_name is missing from disk"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
    fi

    if cask_app_is_running "$installed"; then
        echo "skipped: $token — $app_name is running at $installed; quit then re-run: managed-machine adopt $token"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
    fi

    if ! verify_app_signature "$installed" "$team_id"; then
        echo "skipped: $token — $app_name failed Developer ID / Team ID verification"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
    fi

    verify_cask_source "$token" "$url_hosts" "$homepage_hosts" || return 1

    system_appdir="$(managed_machine_system_appdir)"
    if [[ -n "$override" ]]; then
        appdir="$override"
    else
        appdir="$system_appdir"
    fi

    dest="$appdir/$app_name"
    if [[ "$installed" != "$dest" ]]; then
        if [[ -e "$dest" ]]; then
            echo "Error: $token — $dest already exists; not overwriting $installed" >&2
            return 1
        fi
        mkdir -p "$appdir" 2>/dev/null || elevate_run "create $appdir" /bin/mkdir -p "$appdir" || return 1
        parent="$(dirname "$installed")"
        if [[ ! -w "$parent" || ! -w "$appdir" ]]; then
            elevate_run "move $app_name to $appdir" /bin/mv "$installed" "$dest" || return 1
        else
            /bin/mv "$installed" "$dest" || return 1
        fi
        installed="$dest"
    fi

    mkdir -p "$appdir" 2>/dev/null || elevate_run "create $appdir" /bin/mkdir -p "$appdir" || return 1
    qualified="$(cask_qualified_token "$token")"
    echo "Adopting $app_name from $qualified into $appdir..."
    brew_run install --cask --adopt "$qualified" --appdir="$appdir" || return 1
    echo "complete: $token — $app_name adopted: $installed"
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
