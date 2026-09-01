#!/usr/bin/env bash
# Homebrew prefix ownership and brew_run. The prefix stays with an admin-group
# owner (typically `admin`). Mutating brew commands run as that owner through
# the authorization dialog; they never chown the prefix to a non-admin user.

user_in_admin_group() {
    local user="$1"
    local groups
    # Handle numeric or parenthesized ids like "502" or "(502)" when DirectoryService is sandboxed.
    local clean="${user//[()]/}"
    # Direct UID match to admin — independent of file group ownership (admin:staff layout)
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local admin_uid
        admin_uid="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid" && "$clean" == "$admin_uid" ]]; then
            return 0
        fi
        # Also check via prefix when /Users/admin not yet available
        if [[ -z "$admin_uid" ]]; then
            admin_uid="$(stat -f '%u' /opt/homebrew 2>/dev/null || stat -f '%u' /usr/local 2>/dev/null || stat -f '%u' /opt 2>/dev/null || true)"
            # Only treat as admin if that prefix/prefix-parent is admin-group or owned by admin home
            if [[ -n "$admin_uid" && "$clean" == "$admin_uid" ]]; then
                local g
                g="$(stat -f '%Sg' /Users/admin 2>/dev/null || stat -f '%Sg' /opt/homebrew 2>/dev/null || stat -f '%Sg' /opt 2>/dev/null || true)"
                # If we couldn't stat a group, still recognize the UID match (e.g. admin:staff)
                if [[ -z "$g" || "$g" == "admin" || "$g" == "staff" ]]; then
                    # Verify the UID actually belongs to an admin home or admin-owned prefix
                    if [[ -d /Users/admin ]] || [[ -d /opt/homebrew ]] || [[ -d /opt ]]; then
                        return 0
                    fi
                fi
            fi
        fi
    fi
    groups="$(id -nG "$user" 2>/dev/null || true)"
    [[ " $groups " == *" admin "* ]] && return 0
    # Fallback for "admin" when DirectoryService is sandboxed
    if [[ "$user" == "admin" && -d /Users/admin ]]; then
        return 0
    fi
    # Fallback: numeric uid that matches admin uid but id failed
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local admin_uid2
        admin_uid2="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid2" && "$admin_uid2" == "$clean" ]]; then
            return 0
        fi
        # Also check prefix ownership: if prefix owned by this uid, treat as admin when admin exists
        local prefix_uid
        prefix_uid="$(stat -f '%u' /opt/homebrew 2>/dev/null || stat -f '%u' /usr/local 2>/dev/null || stat -f '%u' /opt 2>/dev/null || true)"
        if [[ -n "$prefix_uid" && "$prefix_uid" == "$clean" ]]; then
            if [[ -d /Users/admin ]]; then
                return 0
            fi
            local prefix_group
            prefix_group="$(stat -f '%Sg' /opt/homebrew 2>/dev/null || stat -f '%Sg' /usr/local 2>/dev/null || stat -f '%Sg' /opt 2>/dev/null || true)"
            [[ "$prefix_group" == "admin" ]] && return 0
        fi
    fi
    return 1
}

brew_prefix_path() {
    if command -v brew >/dev/null 2>&1; then
        brew --prefix 2>/dev/null && return 0
    fi
    if [[ -x /opt/homebrew/bin/brew ]]; then
        printf '%s\n' /opt/homebrew
        return 0
    fi
    if [[ -x /usr/local/bin/brew ]]; then
        printf '%s\n' /usr/local
        return 0
    fi
    return 1
}

resolve_brew_owner_name() {
    local owner="$1"
    local clean="${owner//[()]/}"
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local admin_uid
        admin_uid="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid" && "$clean" == "$admin_uid" ]]; then
            printf 'admin\n'
            return 0
        fi
        # Try to resolve UID to name (may fail when DirectoryService sandboxed)
        local resolved
        resolved="$(id -nu "$clean" 2>/dev/null || true)"
        if [[ -n "$resolved" && "$resolved" != "$clean" ]]; then
            printf '%s\n' "$resolved"
            return 0
        fi
        resolved="$(/usr/bin/dscl . -search /Users UniqueID "$clean" 2>/dev/null | /usr/bin/awk 'NR==1{print $1}' || true)"
        if [[ -n "$resolved" && "$resolved" != "$clean" ]]; then
            printf '%s\n' "$resolved"
            return 0
        fi
    fi
    printf '%s\n' "$owner"
}

brew_prefix_owner() {
    local prefix
    prefix="$(brew_prefix_path)" || return 1
    local owner
    if stat -c '%U' "$prefix" >/dev/null 2>&1; then
        owner="$(stat -c '%U' "$prefix" 2>/dev/null || true)"
    else
        owner="$(stat -f '%Su' "$prefix" 2>/dev/null || true)"
    fi
    # Normalize parenthesized numeric ids like "(502)" to "502" for comparison
    owner="${owner//[()]/}"
    printf '%s\n' "$owner"
}

# Preferred owner: explicit override, then the `admin` account when it is in
# the admin group, then the existing prefix owner if they are in admin.
# Also handles DirectoryService sandbox where `id admin` fails but /Users/admin exists.
preferred_brew_owner() {
    local owner
    if [[ -n "${MANAGED_MACHINE_BREW_OWNER:-}" ]]; then
        printf '%s\n' "$MANAGED_MACHINE_BREW_OWNER"
        return 0
    fi
    if id -u admin >/dev/null 2>&1 && user_in_admin_group admin; then
        printf 'admin\n'
        return 0
    fi
    # Fallback when DirectoryService is sandboxed but /Users/admin exists
    if [[ -d /Users/admin ]]; then
        printf 'admin\n'
        return 0
    fi
    # Also check prefix ownership via /opt when /opt/homebrew not yet created
    owner="$(brew_prefix_owner 2>/dev/null || true)"
    if [[ -z "$owner" ]]; then
        # No prefix yet — check parent /opt or /usr/local owner
        for p in /opt/homebrew /usr/local /opt; do
            if [[ -e "$p" ]]; then
                owner="$(stat -f '%Su' "$p" 2>/dev/null || true)"
                owner="${owner//[()]/}"
                [[ -n "$owner" ]] && break
            fi
        done
    fi
    if [[ -n "$owner" ]] && user_in_admin_group "$owner"; then
        local resolved
        resolved="$(resolve_brew_owner_name "$owner")"
        printf '%s\n' "$resolved"
        return 0
    fi
    printf '%s\n' "$(id -un)"
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

# True when this brew binary is the system prefix install (not a test stub).
brew_is_system_prefix() {
    local brew_bin="${1:-}"
    case "$brew_bin" in
        /opt/homebrew/bin/brew|/usr/local/bin/brew) return 0 ;;
        *) return 1 ;;
    esac
}

# Write the invoking user's gh token to a 600 file and run a command as the
# prefix owner through a root helper. The token is never placed in argv.
brew_run_as_owner_with_github_auth() {
    local owner="$1"
    local token tokenfile helper
    shift
    if ! command -v gh >/dev/null 2>&1; then
        elevate_as_user "run brew $*" "$owner" "$@"
        return
    fi
    token="$(gh auth token 2>/dev/null)" || token=""
    if [[ -z "$token" ]]; then
        elevate_as_user "run brew $*" "$owner" "$@"
        return
    fi
    helper="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/brew-github-auth-run"
    if [[ ! -f "$helper" ]]; then
        echo "Error: missing $helper" >&2
        return 1
    fi
    tokenfile="$(mktemp "${TMPDIR:-/tmp}/mm-gh-token.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$tokenfile"; trap - RETURN' RETURN
    umask 077
    printf '%s\n' "$token" >"$tokenfile"
    chmod 600 "$tokenfile"
    elevate_run "run brew as $owner" /bin/sh "$helper" "$owner" "$tokenfile" "$@"
}

# brew_run_script <sh script>: several brew commands behind one authorization
# prompt. The script runs under /bin/sh with brew on PATH, through the same
# owner and GitHub-auth path as brew_run, so a multi-step change (unpin,
# upgrade, pin) costs the operator one dialog instead of three.
brew_run_script() {
    local script="$1" brew_bin owner
    if ! brew_bin="$(command -v brew 2>/dev/null)"; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if ! brew_is_system_prefix "$brew_bin"; then
        /bin/sh -c "$script"
        return
    fi
    owner="$(brew_prefix_owner)"
    owner="$(resolve_brew_owner_name "$owner")"
    if [[ -z "$owner" || "$owner" == "$(id -un)" ]]; then
        /bin/sh -c "$script"
        return
    fi
    brew_run_as_owner_with_github_auth "$owner" /bin/sh -c "$script"
}

# Run brew as the prefix owner when the current user does not own it.
# Test stubs and a prefix already owned by this user run in-process.
brew_run() {
    local brew_bin owner
    if ! brew_bin="$(command -v brew 2>/dev/null)"; then
        echo "Error: brew required — run setup-brew first" >&2
        return 1
    fi
    if ! brew_is_system_prefix "$brew_bin"; then
        command brew "$@"
        return
    fi
    owner="$(brew_prefix_owner)"
    # Resolve numeric UID to name before comparison and elevation
    owner="$(resolve_brew_owner_name "$owner")"
    if [[ -z "$owner" || "$owner" == "$(id -un)" ]]; then
        command brew "$@"
        return
    fi
    brew_run_as_owner_with_github_auth "$owner" "$brew_bin" "$@"
}
