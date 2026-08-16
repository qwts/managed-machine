#!/usr/bin/env bash
# Homebrew prefix ownership and brew_run. The prefix stays with an admin-group
# owner (typically `admin`). Mutating brew commands run as that owner through
# the authorization dialog; they never chown the prefix to a non-admin user.

user_in_admin_group() {
    local user="$1"
    local groups
    groups="$(id -nG "$user" 2>/dev/null || true)"
    [[ " $groups " == *" admin "* ]]
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

brew_prefix_owner() {
    local prefix
    prefix="$(brew_prefix_path)" || return 1
    if stat -c '%U' "$prefix" >/dev/null 2>&1; then
        stat -c '%U' "$prefix"
    else
        stat -f '%Su' "$prefix" 2>/dev/null || true
    fi
}

# Preferred owner: explicit override, then the `admin` account when it is in
# the admin group, then the existing prefix owner if they are in admin.
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
    owner="$(brew_prefix_owner 2>/dev/null || true)"
    if [[ -n "$owner" ]] && user_in_admin_group "$owner"; then
        printf '%s\n' "$owner"
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
    if [[ -z "$owner" || "$owner" == "$(id -un)" ]]; then
        command brew "$@"
        return
    fi
    brew_run_as_owner_with_github_auth "$owner" "$brew_bin" "$@"
}
