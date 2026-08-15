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
#      credential helper so private HTTPS clones work before any SSH key
#      exists (setup-gh provisions SSH later).
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
ensure_brew_installed() {
    if command -v brew >/dev/null 2>&1; then
        return 0
    fi
    echo "Homebrew not found. Installing..."
    /bin/bash -c "$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)"
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
    stat -f '%Su' "$prefix" 2>/dev/null || stat -c '%U' "$prefix" 2>/dev/null || true
}

user_in_admin_group() {
    local groups
    groups="$(id -nG "$1" 2>/dev/null || true)"
    [[ " $groups " == *" admin "* ]]
}

preferred_brew_owner() {
    if id -u admin >/dev/null 2>&1 && user_in_admin_group admin; then
        printf 'admin\n'
        return 0
    fi
    printf '%s\n' "$(whoami)"
}

# Forward the invoking user's GitHub credentials into brew-as-owner. sudo -H
# uses the owner's HOME, which does not have gh's git credential helper, so
# private tap/formula clones would otherwise fail after ensure_gh_access.
github_auth_env_for_brew_owner() {
    local token header
    command -v gh >/dev/null 2>&1 || return 1
    token="$(gh auth token 2>/dev/null)" || return 1
    [[ -n "$token" ]] || return 1
    header="AUTHORIZATION: basic $(printf 'x-access-token:%s' "$token" | /usr/bin/base64 | tr -d '\n')"
    GITHUB_AUTH_ENV=(
        /usr/bin/env
        "HOMEBREW_GITHUB_API_TOKEN=${token}"
        "GH_TOKEN=${token}"
        GIT_CONFIG_COUNT=1
        GIT_CONFIG_KEY_0=http.https://github.com/.extraheader
        "GIT_CONFIG_VALUE_0=${header}"
    )
}

# Run brew (or any command) as the prefix owner when this user does not own it.
run_as_brew_owner() {
    local prefix owner
    local -a auth_env=()
    prefix="$(brew_prefix)" || {
        err "could not locate brew prefix"
        exit 1
    }
    owner="$(prefix_owner "$prefix")"
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
    if github_auth_env_for_brew_owner; then
        auth_env=("${GITHUB_AUTH_ENV[@]}")
    fi
    echo "Homebrew prefix ($prefix) is owned by '$owner' — requesting administrator authorization to run brew as $owner..."
    osascript \
        -e 'on run argv' \
        -e 'set lbl to item 1 of argv' \
        -e 'set cmd to ""' \
        -e 'repeat with i from 2 to count of argv' \
        -e 'set cmd to cmd & quoted form of (item i of argv as text) & " "' \
        -e 'end repeat' \
        -e 'do shell script cmd with prompt ("managed-machine needs administrator access to " & lbl & ".") with administrator privileges' \
        -e 'end run' \
        "run brew as $owner" /usr/bin/sudo -H -u "$owner" "${auth_env[@]}" "$@" >/dev/null
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
# provisioned later by setup-gh and is never required to install.
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
    if ! brew tap-info "$TAP" 2>/dev/null | grep -q "Installed"; then
        echo "Tapping $TAP over authenticated HTTPS..."
        run_as_brew_owner brew tap "$TAP" "$REPO_URL"
    fi
    ensure_tap_trusted
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
