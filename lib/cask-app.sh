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

# app_name|team_id|url_hosts|homepage_hosts|allow_rolling_url — policy comes
# from the config-repo catalog, not a hardcoded allowlist.
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
        muse) printf '%s\n' "${MANAGED_MACHINE_MUSE_APPDIR:-}" ;;
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
# A row with allow_rolling_url accepts no_check, and only no_check: the vendor
# serves one rolling URL, so notarization is the integrity check that remains.
verify_cask_source() {
    local token="$1"
    local url_hosts="$2"
    local homepage_hosts="$3"
    local allow_rolling="${4:-}"
    local json
    json="$(brew info --json=v2 --cask "$(cask_qualified_token "$token")")" || {
        echo "Error: could not read Homebrew cask metadata for $token" >&2
        return 1
    }
    EXPECT_TOKEN="$token" ALLOWED_URL_HOSTS="$url_hosts" ALLOWED_HOME_HOSTS="$homepage_hosts" \
        ALLOW_ROLLING_URL="$allow_rolling" \
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
allow_rolling = os.environ.get("ALLOW_ROLLING_URL") == "1"
if digest == "no_check" and allow_rolling:
    sys.stderr.write(
        f"Note: {expect} ships a rolling vendor URL with no published checksum; "
        "integrity rests on notarized Developer ID verification (allow_rolling_url)\n"
    )
elif digest in ("", "no_check") or len(digest) != 64 or any(ch not in "0123456789abcdef" for ch in digest):
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

# Require Apple notarization for the expected Team ID. <reason> explains why
# this check is the one being relied on, and is quoted on success.
verify_notarized_by_gatekeeper() {
    local app="$1"
    local team_id="$2"
    local reason="$3"
    local assessment

    if ! command -v spctl >/dev/null 2>&1; then
        echo "Error: $app: $reason, and spctl is unavailable to check notarization" >&2
        return 1
    fi
    assessment="$(spctl -a -t exec -vv "$app" 2>&1)" || {
        if [[ "$assessment" == *"a sealed resource is missing or invalid"* ]]; then
            echo "Error: $app: $reason, and Gatekeeper rejected it (a sealed resource is missing or invalid — bundle was modified after signing; remove $app and run setup to reinstall cleanly)" >&2
        else
            echo "Error: $app: $reason, and Gatekeeper rejected it" >&2
        fi
        return 1
    }
    if ! grep -Fq 'source=Notarized Developer ID' <<<"$assessment"; then
        echo "Error: $app: $reason, and it is not notarized" >&2
        return 1
    fi
    # The Gatekeeper origin must name the same team the signature claimed.
    if ! grep -Fq "($team_id)" <<<"$assessment"; then
        echo "Error: $app Gatekeeper origin does not name Team ID $team_id" >&2
        return 1
    fi
    echo "$app: $reason; accepted on notarized Developer ID ($team_id)"
}

# Require a valid Developer ID signature from the expected Team ID.
#
# Identity (Developer ID authority + Team ID) is always required. Bundle
# integrity is then satisfied by either check:
#
#   1. codesign --verify --deep --strict, or
#   2. the Gatekeeper assessment, which additionally proves the bundle was
#      notarized by Apple — something codesign alone never checks.
#
# The fallback exists because --deep --strict rejects a bundle whose nested
# helpers carry com.apple.FinderInfo/quarantine detritus ("resource fork,
# Finder information, or similar detritus not allowed"). Homebrew Cask stamps
# those attributes during extraction, so Chromium-based apps fail a check the
# vendor's signature and notarization both pass.
#
# require_notarized forces path 2 even when codesign succeeds. Callers set it
# for a checksumless rolling-URL row, where notarization is the only integrity
# guarantee left: passing codesign alone would install an unnotarized build
# with no checksum behind it.
verify_app_signature() {
    local app="$1"
    local team_id="$2"
    local require_notarized="${3:-}"
    local detail team

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

    if [[ "$require_notarized" == "1" ]]; then
        verify_notarized_by_gatekeeper "$app" "$team_id" \
            "the vendor publishes no checksum for this rolling URL"
        return
    fi

    if codesign --verify --deep --strict "$app" 2>/dev/null; then
        return 0
    fi

    verify_notarized_by_gatekeeper "$app" "$team_id" \
        "codesign --deep --strict rejected bundle detritus"
}

# install_signed_cask_app <token> [appdir-override]
install_signed_cask_app() {
    local token="$1"
    local override="${2:-}"
    local row app_name team_id url_hosts homepage_hosts allow_rolling appdir installed qualified install_status

    row="$(cask_allowlist_row "$token")" || {
        echo "Error: $token is not on the signed-cask allowlist" >&2
        return 1
    }
    IFS='|' read -r app_name team_id url_hosts homepage_hosts allow_rolling <<<"$row"
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

    if declare -F elevate_plan_step_has_recorded_status >/dev/null \
        && [[ -n "${MANAGED_MACHINE_CURRENT_STEP:-}" ]] \
        && elevate_plan_step_has_recorded_status "$MANAGED_MACHINE_CURRENT_STEP" signed-cask; then
        local plan_rc=0
        elevate_plan_step_status "$MANAGED_MACHINE_CURRENT_STEP" signed-cask || plan_rc=$?
        if [[ "$plan_rc" -ne 0 ]]; then
            return "$plan_rc"
        fi
        verify_cask_source "$token" "$url_hosts" "$homepage_hosts" "$allow_rolling" || return 1
        installed="$(find_cask_app "$app_name" "$override")" || {
            echo "Install finished but $app_name was not found." >&2
            return 1
        }
        if ! verify_app_signature "$installed" "$team_id" "$allow_rolling"; then
            rm -rf -- "$installed" 2>/dev/null || true
            return 1
        fi
        echo "$app_name installed: $installed"
        brew list --cask --versions "$token" 2>/dev/null || true
        return 0
    fi

    if installed="$(find_cask_app "$app_name" "$override")"; then
        if ! cask_has_receipt "$token"; then
            echo "Skipped: $app_name exists at $installed but is not a Homebrew cask install" >&2
            return "${MANAGED_MACHINE_SKIPPED_EXIT:-76}"
        fi
        verify_cask_source "$token" "$url_hosts" "$homepage_hosts" "$allow_rolling" || return 1
        verify_app_signature "$installed" "$team_id" "$allow_rolling" || return 1
        echo "$app_name already installed: $installed"
        brew list --cask --versions "$token" || true
        return 0
    fi

    verify_cask_source "$token" "$url_hosts" "$homepage_hosts" "$allow_rolling" || return 1

    appdir="$(resolve_cask_appdir "$override")"
    if ! mkdir -p "$appdir" 2>/dev/null; then
        elevate_run "create $appdir" /bin/mkdir -p "$appdir" || return $?
    fi

    qualified="$(cask_qualified_token "$token")"
    echo "Installing $app_name from $qualified into $appdir..."
    install_status=0
    brew_run install --cask "$qualified" --appdir="$appdir" || install_status=$?
    # An unavailable administrator dialog is a skip, not a failure (#110):
    # pass it on instead of reporting the app as missing.
    if [[ "$install_status" -eq "${MANAGED_MACHINE_SKIPPED_EXIT:-76}" \
        || "$install_status" -eq "${MANAGED_MACHINE_DEFERRED_EXIT:-75}" ]]; then
        return "$install_status"
    fi

    installed="$(find_cask_app "$app_name" "$override")" || {
        echo "Install finished but $app_name was not found." >&2
        return 1
    }
    verify_app_signature "$installed" "$team_id" "$allow_rolling" || return 1
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

# True when a real Homebrew cask adopt will need root to normalize bundle
# ownership to <owner>:admin before brew runs chmod/chgrp as <owner>.
adopt_bundle_needs_chown() {
    local bundle="$1" owner="$2"
    local brew_bin bundle_owner bundle_group
    brew_bin="$(command -v brew 2>/dev/null || true)"
    [[ -n "$brew_bin" ]] && brew_is_system_prefix "$brew_bin" || return 1
    bundle_owner="$(stat -c '%U' "$bundle" 2>/dev/null || stat -f '%Su' "$bundle" 2>/dev/null || true)"
    bundle_owner="${bundle_owner//[()]/}"
    if [[ "$bundle_owner" =~ ^[0-9]+$ ]]; then
        bundle_owner="$(resolve_brew_owner_name "$bundle_owner")"
    fi
    bundle_group="$(stat -c '%G' "$bundle" 2>/dev/null || stat -f '%Sg' "$bundle" 2>/dev/null || true)"
    [[ "$bundle_owner" != "$owner" || "$bundle_group" != "admin" ]]
}

# Run `brew install --cask --adopt` with root ownership normalization when
# brew_needs_elevation or adopt_bundle_needs_chown is true, so Homebrew Cask's
# post-adopt chmod/chgrp succeeds as the brew owner without invoking sudo (#205).
brew_adopt_cask_in_appdir() {
    local qualified="$1"
    local appdir="$2"
    local dest="$3"
    local step_name="${4:-${MANAGED_MACHINE_CURRENT_STEP:-adopt}}"
    local owner=""

    if declare -F elevate_plan_step_in_plan >/dev/null \
        && elevate_plan_step_in_plan "$step_name" "migrate-cask-adopt"; then
        if declare -F elevate_plan_step_has_recorded_status >/dev/null \
            && elevate_plan_step_has_recorded_status "$step_name" "migrate-cask-adopt"; then
            local plan_rc=0
            elevate_plan_step_status "$step_name" "migrate-cask-adopt" || plan_rc=$?
            return "$plan_rc"
        fi
        echo "Error: administrator authorization was cancelled or failed — did not adopt $(basename "$dest")" >&2
        return 1
    fi

    owner="$(resolved_brew_owner 2>/dev/null || id -un)"
    if ! brew_needs_elevation && ! adopt_bundle_needs_chown "$dest" "$owner"; then
        brew_run install --cask --adopt "$qualified" --appdir="$appdir"
        return $?
    fi

    local session_dir plan_file plan_sha rc=0
    local old_umask
    old_umask="$(umask)"
    umask 077
    session_dir="$(mktemp -d "${TMPDIR:-/tmp}/mm-adopt-single.XXXXXX")"
    chmod 700 "$session_dir"
    mkdir -p "$session_dir/status" "$session_dir/detail" "$session_dir/out"
    plan_file="$session_dir/actions.tsv"
    printf 'act-1\t%s\tmigrate-cask-adopt\t%s\t%s\t%s\t--appdir=%s\tadopt %s into %s\n' \
        "$step_name" "$owner" "$qualified" "$dest" "$appdir" "$(basename "$dest")" "$appdir" >"$plan_file"
    umask "$old_umask"
    plan_sha="$(shasum -a 256 "$plan_file" | awk '{print $1}')"

    MANAGED_MACHINE_APPROVED_PLAN_FILE="$plan_file" \
    MANAGED_MACHINE_APPROVED_PLAN_SHA256="$plan_sha" \
    MANAGED_MACHINE_PLAN_SESSION_DIR="$session_dir" \
        elevate_run_plan "adopt-${plan_sha:0:12}" "adopt $(basename "$dest") into Homebrew" "$plan_file" "$session_dir" || rc=$?

    if [[ -f "$session_dir/status/act-1" ]]; then
        rc=0
        MANAGED_MACHINE_APPROVED_PLAN_FILE="$plan_file" \
        MANAGED_MACHINE_APPROVED_PLAN_SHA256="$plan_sha" \
        MANAGED_MACHINE_PLAN_SESSION_DIR="$session_dir" \
            elevate_plan_step_status "$step_name" "migrate-cask-adopt" || rc=$?
    fi
    rm -rf "$session_dir"
    return "$rc"
}

# Pre-verify candidate tokens and authorize all privileged adopt actions
# (bundle move into /Applications, bundle chown to brew_owner:admin, and
# brew install --cask --adopt as brew_owner) under a single macOS authorization
# dialog before the per-token adopt loop runs.
adopt_stage_and_authorize_plan() {
    local session_dir="$1"
    shift
    [[ $# -gt 0 ]] || return 0
    ensure_brew_on_path || return 0
    command -v python3 >/dev/null 2>&1 || return 0

    local owner="" brew_elev=0 sys_brew=0
    owner="$(resolved_brew_owner 2>/dev/null || id -un)"
    if brew_needs_elevation; then
        brew_elev=1
    fi
    if brew_is_system_prefix "$(command -v brew 2>/dev/null || true)"; then
        sys_brew=1
    fi
    [[ "$brew_elev" -eq 1 || "$sys_brew" -eq 1 ]] || return 0

    local old_umask
    old_umask="$(umask)"
    umask 077
    mkdir -p "$session_dir/status" "$session_dir/detail" "$session_dir/out"
    chmod 700 "$session_dir"
    local plan_file="$session_dir/actions.tsv"
    : >"$plan_file"
    umask "$old_umask"

    local token row app_name team_id url_hosts homepage_hosts allow_rolling
    local override installed appdir dest qualified
    local act_idx=0 adopt_count=0 last_app_name=""
    for token in "$@"; do
        [[ -n "$token" ]] || continue
        row="$(cask_allowlist_row "$token" 2>/dev/null)" || continue
        IFS='|' read -r app_name team_id url_hosts homepage_hosts allow_rolling <<<"$row"
        [[ -n "$app_name" && -n "$team_id" && -n "$url_hosts" && -n "$homepage_hosts" ]] || continue
        cask_has_receipt "$token" 2>/dev/null && continue
        override="$(cask_appdir_override_for_token "$token")"
        installed="$(find_cask_app "$app_name" "$override" 2>/dev/null)" || continue
        cask_app_is_running "$installed" && continue
        verify_app_signature "$installed" "$team_id" "$allow_rolling" >/dev/null 2>&1 || continue
        verify_cask_source "$token" "$url_hosts" "$homepage_hosts" "$allow_rolling" >/dev/null 2>&1 || continue

        appdir="$(resolve_cask_appdir "$override")"
        dest="$appdir/$app_name"
        if [[ "$installed" != "$dest" && -e "$dest" ]]; then
            continue
        fi
        if [[ "$brew_elev" -eq 0 ]] && ! adopt_bundle_needs_chown "$installed" "$owner"; then
            continue
        fi
        if [[ "$installed" != "$dest" ]]; then
            act_idx=$((act_idx + 1))
            printf 'act-%d\tadopt:%s\tmigrate-app-mv\troot\t%s\t%s\t%s\tmove %s to %s\n' \
                "$act_idx" "$token" "$installed" "$dest" "$appdir" "$app_name" "$appdir" >>"$plan_file"
        fi
        qualified="$(cask_qualified_token "$token")"
        act_idx=$((act_idx + 1))
        printf 'act-%d\tadopt:%s\tmigrate-cask-adopt\t%s\t%s\t%s\t--appdir=%s\tadopt %s into %s\n' \
            "$act_idx" "$token" "$owner" "$qualified" "$dest" "$appdir" "$app_name" "$appdir" >>"$plan_file"
        adopt_count=$((adopt_count + 1))
        last_app_name="$app_name"
    done

    [[ -s "$plan_file" ]] || return 0

    local plan_sha plan_label
    plan_sha="$(shasum -a 256 "$plan_file" | awk '{print $1}')"
    if [[ "$adopt_count" -eq 1 ]]; then
        plan_label="adopt $last_app_name into Homebrew"
    else
        plan_label="adopt $adopt_count apps into Homebrew"
    fi
    MANAGED_MACHINE_APPROVED_PLAN_FILE="$plan_file"
    MANAGED_MACHINE_APPROVED_PLAN_SHA256="$plan_sha"
    MANAGED_MACHINE_PLAN_SESSION_DIR="$session_dir"
    export MANAGED_MACHINE_APPROVED_PLAN_FILE MANAGED_MACHINE_APPROVED_PLAN_SHA256 MANAGED_MACHINE_PLAN_SESSION_DIR

    elevate_run_plan "adopt-${plan_sha:0:12}" "$plan_label" "$plan_file" "$session_dir"
}

# adopt_signed_cask_app <token> [appdir-override]
#
# Take over a vendor-installed allowlisted app with Homebrew --adopt.
# Returns 0 on success, MANAGED_MACHINE_ADOPT_SKIPPED when skipped, 1 on failure.
adopt_signed_cask_app() {
    local token="$1"
    local override="${2:-}"
    local row app_name team_id url_hosts homepage_hosts allow_rolling
    local installed system_appdir appdir dest parent qualified adopt_step

    row="$(cask_allowlist_row "$token")" || {
        echo "skipped: $token — not on the signed-cask allowlist"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
    }
    IFS='|' read -r app_name team_id url_hosts homepage_hosts allow_rolling <<<"$row"

    if ! ensure_brew_on_path; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if ! command -v python3 >/dev/null 2>&1; then
        echo "Error: python3 is required to verify Homebrew cask metadata" >&2
        return 1
    fi

    adopt_step="${MANAGED_MACHINE_CURRENT_STEP:-adopt:$token}"
    if declare -F elevate_plan_step_in_plan >/dev/null \
        && elevate_plan_step_in_plan "$adopt_step"; then
        appdir="$(resolve_cask_appdir "$override")"
        qualified="$(cask_qualified_token "$token")"
        echo "Adopting $app_name from $qualified into $appdir..."
        if declare -F elevate_plan_step_has_recorded_status >/dev/null \
            && elevate_plan_step_has_recorded_status "$adopt_step"; then
            local plan_rc=0
            elevate_plan_step_status "$adopt_step" || plan_rc=$?
            [[ "$plan_rc" -eq 0 ]] || return "$plan_rc"
            installed="$(find_cask_app "$app_name" "$override")" || {
                echo "Error: $token — $app_name was not found after adopt" >&2
                return 1
            }
            echo "complete: $token — $app_name adopted: $installed"
            return 0
        fi
        echo "Error: administrator authorization was cancelled or failed — did not adopt $app_name" >&2
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

    if ! verify_app_signature "$installed" "$team_id" "$allow_rolling"; then
        echo "skipped: $token — $app_name failed Developer ID / Team ID verification"
        return "$MANAGED_MACHINE_ADOPT_SKIPPED"
    fi

    verify_cask_source "$token" "$url_hosts" "$homepage_hosts" "$allow_rolling" || return 1

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
        if ! mkdir -p "$appdir" 2>/dev/null; then
            elevate_run "create $appdir" /bin/mkdir -p "$appdir" || return $?
        fi
        parent="$(dirname "$installed")"
        if [[ ! -w "$parent" || ! -w "$appdir" ]]; then
            elevate_run "move $app_name to $appdir" /bin/mv "$installed" "$dest" || return $?
        else
            /bin/mv "$installed" "$dest" || return 1
        fi
        installed="$dest"
    fi

    if ! mkdir -p "$appdir" 2>/dev/null; then
        elevate_run "create $appdir" /bin/mkdir -p "$appdir" || return $?
    fi
    qualified="$(cask_qualified_token "$token")"
    echo "Adopting $app_name from $qualified into $appdir..."
    brew_adopt_cask_in_appdir "$qualified" "$appdir" "$dest" "$adopt_step" || return $?
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
