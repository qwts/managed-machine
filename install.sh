#!/usr/bin/env bash
# One-shot installer for managed-machine.
#
# The repository is private, so fetch the installer through an authenticated
# GitHub CLI instead of raw.githubusercontent.com (which returns 404):
#
#   gh api -H "Accept: application/vnd.github.raw" \
#     repos/qwts/managed-machine/contents/install.sh | bash
#
# Flow:
#   1. Ensure Homebrew is installed. Prefix ownership stays with an admin-group
#      user (typically `admin`); this installer never chown's it to the
#      invoking user.
#   2. Ensure gh is installed and authenticated, and wire gh as the git
#      credential helper so private HTTPS clones work. No SSH key is created;
#      SSH enrollment is the explicit `managed-machine ssh enroll` step.
#   3. If managed-machine is already installed: ensure tap is present, update,
#      and tell the user to use it directly.
#   4. If not installed: tap over authenticated HTTPS, trust the tap when
#      Homebrew requires it, install, and run managed-machine --bootstrap.
set -euo pipefail

TAP="qwts/managed-machine"
REPO_URL="https://github.com/qwts/managed-machine.git"

err() { echo "Error: $*" >&2; }

# Resolve brew prefix (common macOS locations).
brew_prefix() {
    if command -v brew >/dev/null 2>&1; then
        brew --prefix
    elif [[ -x /opt/homebrew/bin/brew ]]; then
        /opt/homebrew/bin/brew --prefix
    elif [[ -x /usr/local/bin/brew ]]; then
        /usr/local/bin/brew --prefix
    else
        return 1
    fi
}

# 1. Ensure Homebrew is present.
# When /opt is admin-owned, the install must run as the preferred brew owner
# (typically `admin`) via the authorization dialog, not as the invoking user.
ensure_brew_installed() {
    if command -v brew >/dev/null 2>&1; then
        return 0
    fi
    # Check direct prefixes even if not on PATH
    if [[ -x /opt/homebrew/bin/brew ]]; then
        eval "$(/opt/homebrew/bin/brew shellenv 2>/dev/null)" || true
        if command -v brew >/dev/null 2>&1; then
            return 0
        fi
    fi
    if [[ -x /usr/local/bin/brew ]]; then
        eval "$(/usr/local/bin/brew shellenv 2>/dev/null)" || true
        if command -v brew >/dev/null 2>&1; then
            return 0
        fi
    fi
    local owner current
    owner="$(preferred_brew_owner)"
    current="$(whoami)"
    if [[ "$owner" != "$current" ]]; then
        if ! command -v osascript >/dev/null 2>&1; then
            err "Homebrew not installed and prefix should be owned by '$owner' — re-run from a GUI session so the administrator dialog can install as $owner."
            exit 1
        fi
        echo "Homebrew not found. Installing as $owner (administrator dialog)..."
        # install.sh has its own elevate helpers; use run_as_brew_owner-style elevation
        # by invoking the brew installer as the owner via osascript
        local owner_home
        owner_home="$(/usr/bin/dscl . -read "/Users/$owner" NFSHomeDirectory 2>/dev/null | /usr/bin/awk '{print $2}')"
        if [[ -z "$owner_home" || ! -d "$owner_home" ]]; then
            if [[ -x /opt/homebrew/bin/brew ]]; then
                owner_home=/opt/homebrew/var/mm-home
            elif [[ -x /usr/local/bin/brew ]]; then
                owner_home=/usr/local/var/mm-home
            else
                owner_home=/tmp/mm-home-$owner
            fi
        fi
        local sudo_owner
        sudo_owner="$(sudo_user_arg "$owner")"
        osascript \
            -e 'on run argv' \
            -e 'set lbl to item 1 of argv' \
            -e 'set cmd to ""' \
            -e 'repeat with i from 2 to count of argv' \
            -e 'set cmd to cmd & quoted form of (item i of argv as text) & " "' \
            -e 'end repeat' \
            -e 'do shell script cmd with prompt ("managed-machine needs administrator access to " & lbl & ".") with administrator privileges' \
            -e 'end run' \
            "install Homebrew as $owner" /usr/bin/sudo -u "$sudo_owner" /usr/bin/env \
            HOME="$owner_home" \
            PATH=/opt/homebrew/bin:/usr/local/bin:/usr/sbin:/usr/bin:/bin \
            /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)" >/dev/null
    else
        echo "Homebrew not found. Installing..."
        /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
    fi
    # Source brew shellenv so this script can use it immediately
    local prefix
    prefix="$(brew_prefix)" || {
        err "could not locate brew after install"
        exit 1
    }
    # shellcheck disable=SC1090
    eval "$("$prefix/bin/brew" shellenv)"
}

# 2. Homebrew prefix stays with an admin-group owner. Never chown it to a
# non-admin invoking user — that was the old installer, and it broke
# multi-user prefixes owned by `admin`.
prefix_owner() {
    local prefix="$1"
    local owner
    owner="$(stat -f '%Su' "$prefix" 2>/dev/null || stat -c '%U' "$prefix" 2>/dev/null || true)"
    owner="${owner//[()]/}"
    printf '%s\n' "$owner"
}

resolve_owner_to_name() {
    local owner="$1"
    local clean="${owner//[()]/}"
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local admin_uid
        admin_uid="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid" && "$clean" == "$admin_uid" ]]; then
            printf 'admin\n'
            return 0
        fi
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

sudo_user_arg() {
    local user="$1"
    local clean="${user//[()]/}"
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local resolved
        resolved="$(resolve_owner_to_name "$clean")"
        if [[ "$resolved" != "$clean" && "$resolved" != "$user" ]]; then
            printf '%s\n' "$resolved"
            return 0
        fi
        # Fall back to numeric sudo syntax
        printf '#%s\n' "$clean"
        return 0
    fi
    printf '%s\n' "$user"
}

user_in_admin_group() {
    local user="$1"
    local clean="${user//[()]/}"
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local admin_uid
        admin_uid="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid" && "$clean" == "$admin_uid" ]]; then
            return 0
        fi
    fi
    local groups
    groups="$(id -nG "$user" 2>/dev/null || true)"
    [[ " $groups " == *" admin "* ]] && return 0
    if [[ "$user" == "admin" && -d /Users/admin ]]; then
        return 0
    fi
    if [[ "$clean" =~ ^[0-9]+$ ]]; then
        local admin_uid2
        admin_uid2="$(stat -f '%u' /Users/admin 2>/dev/null || true)"
        if [[ -n "$admin_uid2" && "$admin_uid2" == "$clean" ]]; then
            return 0
        fi
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

preferred_brew_owner() {
    if id -u admin >/dev/null 2>&1 && user_in_admin_group admin; then
        printf 'admin\n'
        return 0
    fi
    if [[ -d /Users/admin ]]; then
        printf 'admin\n'
        return 0
    fi
    # Also check prefix owner is admin-group (handles /opt when prefix not yet created)
    local owner
    owner="$(prefix_owner "$(brew_prefix 2>/dev/null || echo /opt/homebrew)" 2>/dev/null || true)"
    if [[ -z "$owner" ]]; then
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
        resolved="$(resolve_owner_to_name "$owner")"
        printf '%s\n' "$resolved"
        return 0
    fi
    printf '%s\n' "$(whoami)"
}

# Write the invoking user's gh token to a 600 file. A root helper (keep in
# sync with lib/brew-github-auth-run) copies it into an owner-only dir and
# runs brew as that owner so the token never appears in osascript/sudo argv.
write_brew_github_auth_run() {
    cat >"$1" <<'ROOT'
#!/bin/sh
# Keep in sync with lib/brew-github-auth-run. osascript admin and
# sudo -H -u admin are login-less; the brew owner often has a stub home.
# Do not inherit TMPDIR: the owner cannot traverse another user's /var/folders.
# brew re-execs with env -i; GIT_CONFIG_* is dropped, so install ~/.gitconfig.
set -eu
PATH=/usr/sbin:/usr/bin:/bin
TMPDIR=/tmp
export PATH TMPDIR
if [ $# -lt 3 ]; then
    echo "Usage: brew-github-auth-run <owner> <tokenfile> <command> [args...]" >&2
    exit 1
fi
owner=$1
tokenfile=$2
shift 2
if [ ! -r "$tokenfile" ]; then
    echo "Error: GitHub token file is not readable" >&2
    exit 1
fi
owner_home=$(/usr/bin/dscl . -read "/Users/$owner" NFSHomeDirectory 2>/dev/null | /usr/bin/awk '{print $2}')
if [ -z "$owner_home" ] || [ ! -d "$owner_home" ]; then
    if [ -x /opt/homebrew/bin/brew ]; then
        owner_home=/opt/homebrew/var/mm-home
    elif [ -x /usr/local/bin/brew ]; then
        owner_home=/usr/local/var/mm-home
    else
        owner_home=/tmp/mm-home-$owner
    fi
    /bin/mkdir -p "$owner_home"
    /usr/sbin/chown "$owner" "$owner_home"
    /bin/chmod 700 "$owner_home"
fi
workdir=$(/usr/bin/mktemp -d /tmp/mm-gh-auth.XXXXXX)
gitconfig_home=$owner_home/.gitconfig
created_gitconfig=
cleanup() {
    if [ -n "${created_gitconfig:-}" ]; then
        /bin/rm -f "$gitconfig_home"
    elif [ -f "$gitconfig_home" ]; then
        # git config --file rewrites through a lock+rename: run it as the
        # owner and re-assert ownership so the file is never left root-owned.
        /usr/bin/sudo -u "$owner" /usr/bin/git config --file "$gitconfig_home" --unset-all include.path "$workdir/gitconfig" 2>/dev/null || true
        /usr/sbin/chown "$owner" "$gitconfig_home" 2>/dev/null || true
        /bin/chmod 600 "$gitconfig_home" 2>/dev/null || true
    fi
    /bin/rm -rf "$workdir"
}
trap cleanup EXIT INT TERM
/usr/sbin/chown "$owner" "$workdir"
/bin/chmod 700 "$workdir"
/bin/cp "$tokenfile" "$workdir/token"
/usr/sbin/chown "$owner" "$workdir/token"
/bin/chmod 600 "$workdir/token"
cat >"$workdir/cred" <<EOF
#!/bin/sh
if [ "\$1" = get ]; then
    printf 'username=x-access-token\\n'
    printf 'password=%s\\n' "\$(cat '$workdir/token')"
fi
EOF
cat >"$workdir/gitconfig" <<EOF
[credential "https://github.com"]
	helper = $workdir/cred
EOF
if [ ! -e "$gitconfig_home" ]; then
    /bin/cp "$workdir/gitconfig" "$gitconfig_home"
    created_gitconfig=1
else
    # The lock+rename rewrite runs as the owner so an existing .gitconfig is
    # never left root-owned.
    /usr/bin/sudo -u "$owner" /usr/bin/git config --file "$gitconfig_home" --add include.path "$workdir/gitconfig"
fi
/usr/sbin/chown "$owner" "$gitconfig_home" "$workdir/gitconfig"
/bin/chmod 600 "$gitconfig_home" "$workdir/gitconfig"
cat >"$workdir/run" <<EOF
#!/bin/sh
set -eu
PATH=/opt/homebrew/bin:/usr/local/bin:/usr/sbin:/usr/bin:/bin
export PATH
token=\$(cat '$workdir/token')
export HOMEBREW_GITHUB_API_TOKEN=\$token
export GH_TOKEN=\$token
export GIT_CONFIG_COUNT=1
export GIT_CONFIG_KEY_0=credential.https://github.com.helper
export GIT_CONFIG_VALUE_0='$workdir/cred'
exec "\$@"
EOF
/usr/sbin/chown "$owner" "$workdir/cred" "$workdir/run"
/bin/chmod 700 "$workdir/cred" "$workdir/run"
/usr/bin/sudo -u "$owner" /usr/bin/env \
    HOME="$owner_home" \
    PATH=/opt/homebrew/bin:/usr/local/bin:/usr/sbin:/usr/bin:/bin \
    "$workdir/run" "$@"
ROOT
}

# Run brew (or any command) as the prefix owner when this user does not own it.
run_as_brew_owner() {
    local prefix owner token tokenfile helper
    prefix="$(brew_prefix)" || {
        err "could not locate brew prefix"
        exit 1
    }
    owner="$(prefix_owner "$prefix")"
    owner="$(resolve_owner_to_name "$owner")"
    if [[ -z "$owner" || "$owner" == "$(whoami)" ]]; then
        "$@"
        return
    fi
    if [[ "$1" == brew ]]; then
        shift
        set -- "$prefix/bin/brew" "$@"
    fi
    if ! command -v osascript >/dev/null 2>&1; then
        err "Homebrew prefix ($prefix) is owned by '$owner', not you ($(whoami)). Re-run from a GUI session so the administrator dialog can run brew as $owner."
        exit 1
    fi
    echo "Homebrew prefix ($prefix) is owned by '$owner' — requesting administrator authorization to run brew as $owner..."
    token=""
    if command -v gh >/dev/null 2>&1; then
        token="$(gh auth token 2>/dev/null || true)"
    fi
    if [[ -z "$token" ]]; then
        local owner_home sudo_owner2
        sudo_owner2="$(sudo_user_arg "$owner")"
        owner_home="$(/usr/bin/dscl . -read "/Users/$owner" NFSHomeDirectory 2>/dev/null | /usr/bin/awk '{print $2}')"
        if [[ -z "$owner_home" || ! -d "$owner_home" ]]; then
            owner_home="$prefix/var/mm-home"
        fi
        osascript \
            -e 'on run argv' \
            -e 'set lbl to item 1 of argv' \
            -e 'set cmd to ""' \
            -e 'repeat with i from 2 to count of argv' \
            -e 'set cmd to cmd & quoted form of (item i of argv as text) & " "' \
            -e 'end repeat' \
            -e 'do shell script cmd with prompt ("managed-machine needs administrator access to " & lbl & ".") with administrator privileges' \
            -e 'end run' \
            "run brew as $owner" /usr/bin/sudo -u "$sudo_owner2" /usr/bin/env \
            HOME="$owner_home" \
            PATH=/opt/homebrew/bin:/usr/local/bin:/usr/sbin:/usr/bin:/bin \
            "$@" >/dev/null
        return
    fi
    tokenfile="$(mktemp "${TMPDIR:-/tmp}/mm-gh-token.XXXXXX")"
    helper="$(mktemp "${TMPDIR:-/tmp}/mm-gh-run.XXXXXX")"
    # shellcheck disable=SC2064
    trap 'rm -f "$tokenfile" "$helper"; trap - RETURN' RETURN
    umask 077
    printf '%s\n' "$token" >"$tokenfile"
    chmod 600 "$tokenfile"
    write_brew_github_auth_run "$helper"
    chmod 700 "$helper"
    osascript \
        -e 'on run argv' \
        -e 'set lbl to item 1 of argv' \
        -e 'set cmd to ""' \
        -e 'repeat with i from 2 to count of argv' \
        -e 'set cmd to cmd & quoted form of (item i of argv as text) & " "' \
        -e 'end repeat' \
        -e 'do shell script cmd with prompt ("managed-machine needs administrator access to " & lbl & ".") with administrator privileges' \
        -e 'end run' \
        "run brew as $owner" /bin/sh "$helper" "$owner" "$tokenfile" "$@" >/dev/null
}

ensure_brew_ownership() {
    local prefix owner preferred
    prefix="$(brew_prefix)" || {
        err "could not locate brew prefix"
        exit 1
    }
    owner="$(prefix_owner "$prefix")"
    if [[ -z "$owner" ]]; then
        err "could not determine owner of $prefix"
        exit 1
    fi
    preferred="$(preferred_brew_owner)"
    if user_in_admin_group "$owner"; then
        echo "Homebrew prefix ($prefix) is owned by '$owner' — leaving ownership unchanged"
        return 0
    fi
    if [[ "$owner" == "$(whoami)" ]]; then
        echo "Homebrew prefix ($prefix) is owned by you ($(whoami)); preferred owner is '$preferred'."
        echo "managed-machine will migrate ownership on bootstrap rather than taking the prefix now."
        return 0
    fi
    cat >&2 <<EOF
Error: Homebrew prefix ($prefix) is owned by '$owner', not you ($(whoami)).
Installs run as the prefix owner through the administrator dialog. If that
owner is wrong, fix it as an administrator, then re-run this installer.
EOF
    exit 1
}

# 3. Ensure gh is installed, authenticated, and wired into git credentials.
#
# The tap and its formula resources are private HTTPS repositories. gh's git
# credential helper is the one authentication mechanism they rely on; SSH is
# never required to install, and enrolling it is a separate explicit human
# action (`managed-machine ssh enroll`).
ensure_gh_access() {
    if ! command -v gh >/dev/null 2>&1; then
        echo "Installing GitHub CLI (needed to access the private tap)..."
        run_as_brew_owner brew install gh
    fi
    # Check only the ACTIVE account: `gh auth status` exits nonzero when any
    # stale or inactive account sits in the keyring, which must not block an
    # install that has usable active credentials.
    local active
    active="$(
        gh auth status -h github.com --json hosts --jq '
            (.hosts["github.com"] // [])[]
            | select(.active == true)
            | .login
        ' 2>/dev/null || true
    )"
    if [[ -z "$active" ]]; then
        cat >&2 <<'EOF'
Error: GitHub CLI has no active authenticated account, and the
managed-machine tap is a private repository. Authenticate first, then
re-run this installer:

  gh auth login -h github.com
EOF
        exit 1
    fi
    gh auth setup-git -h github.com
    echo "GitHub CLI authenticated as $active; git will use gh credentials for github.com over HTTPS."
}

# 4. Ensure the tap is present, trusted, and updated.
ensure_tap_trusted() {
    # Newer Homebrew refuses to load formulae from untrusted third-party taps.
    # Trust exactly this tap, and say so; older Homebrew has no trust command.
    if brew commands 2>/dev/null | grep -qx "trust"; then
        echo "Trusting Homebrew tap $TAP (scoped to this tap only)..."
        run_as_brew_owner brew trust "$TAP"
    fi
}

ensure_tap() {
    # Trust must precede the tap itself: `brew tap` loads every formula as a
    # syntax check, and that load is refused while the tap is untrusted.
    ensure_tap_trusted
    if ! brew tap-info "$TAP" 2>/dev/null | grep -q "Installed"; then
        echo "Tapping $TAP over authenticated HTTPS..."
        run_as_brew_owner brew tap "$TAP" "$REPO_URL"
    fi
    run_as_brew_owner brew update >/dev/null 2>&1 || true
}

# 5. Is managed-machine already installed via brew?
is_managed_machine_installed() {
    brew list --versions managed-machine >/dev/null 2>&1
}

main() {
    ensure_brew_installed
    ensure_brew_ownership
    ensure_gh_access
    ensure_tap

    if is_managed_machine_installed; then
        echo "Upgrading managed-machine..."
        run_as_brew_owner brew upgrade managed-machine 2>/dev/null || true
        cat <<EOF

managed-machine is installed and up to date. Use it directly:

  managed-machine              # run full bootstrap
  managed-machine --update     # brew update + safe setup re-runs
  managed-machine status       # installed versions and pins
  managed-machine setup <name> # run one setup script (bin or setup-bin)
  managed-machine adopt        # take over vendor-installed desktop apps
  managed-machine --help       # show usage

EOF
        exit 0
    fi

    echo "Installing managed-machine..."
    run_as_brew_owner brew install managed-machine

    echo "Running managed-machine --bootstrap (terminal mode auto-detected)..."
    managed-machine --bootstrap

    cat <<EOF

managed-machine installed and bootstrapped. Future updates:

  managed-machine --update

EOF
}

main "$@"
