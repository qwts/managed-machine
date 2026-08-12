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
#   1. Ensure Homebrew is installed and owned by the current user.
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

# 2. Ensure brew prefix is owned by the current user.
ensure_brew_ownership() {
    local prefix owner
    prefix="$(brew_prefix)" || {
        err "could not locate brew prefix"
        exit 1
    }
    owner="$(stat -f '%Su' "$prefix" 2>/dev/null || stat -c '%U' "$prefix" 2>/dev/null || true)"
    if [[ -z "$owner" ]]; then
        err "could not determine owner of $prefix"
        exit 1
    fi
    if [[ "$owner" != "$(whoami)" ]]; then
        # Escalate through the macOS Authorization Services dialog rather than
        # terminal sudo: it works for non-admin invokers and keeps passwords
        # off the terminal. (Standalone inline equivalent of lib/elevate.sh —
        # this script runs before the repo exists.)
        if command -v osascript >/dev/null 2>&1; then
            echo "Homebrew prefix ($prefix) is owned by '$owner' — requesting administrator authorization to fix ownership (system dialog)..."
            if osascript \
                -e 'on run argv' \
                -e 'do shell script "/usr/sbin/chown -R " & quoted form of (item 1 of argv) & " " & quoted form of (item 2 of argv) with prompt "managed-machine needs administrator access to fix Homebrew ownership." with administrator privileges' \
                -e 'end run' \
                "$(whoami)" "$prefix" >/dev/null 2>&1; then
                echo "Homebrew ownership fixed: $prefix now owned by $(whoami)"
                return 0
            fi
            echo "Administrator authorization was cancelled or unavailable." >&2
        fi
        cat >&2 <<EOF
Error: Homebrew prefix ($prefix) is owned by '$owner', not you ($(whoami)).
Fix ownership first (an administrator will be asked to authorize), then
re-run this installer:

  sudo chown -R $(whoami) "$prefix"
EOF
        exit 1
    fi
}

# 3. Ensure gh is installed, authenticated, and wired into git credentials.
#
# The tap and its formula resources are private HTTPS repositories. gh's git
# credential helper is the one authentication mechanism they rely on; SSH is
# provisioned later by setup-gh and is never required to install.
ensure_gh_access() {
    if ! command -v gh >/dev/null 2>&1; then
        echo "Installing GitHub CLI (needed to access the private tap)..."
        brew install gh
    fi
    if ! gh auth status -h github.com >/dev/null 2>&1; then
        cat >&2 <<'EOF'
Error: GitHub CLI is not authenticated, and the managed-machine tap is a
private repository. Authenticate first, then re-run this installer:

  gh auth login -h github.com
EOF
        exit 1
    fi
    gh auth setup-git -h github.com
    echo "GitHub CLI authenticated; git will use gh credentials for github.com over HTTPS."
}

# 4. Ensure the tap is present, trusted, and updated.
ensure_tap_trusted() {
    # Newer Homebrew refuses to load formulae from untrusted third-party taps.
    # Trust exactly this tap, and say so; older Homebrew has no trust command.
    if brew commands 2>/dev/null | grep -qx "trust"; then
        echo "Trusting Homebrew tap $TAP (scoped to this tap only)..."
        brew trust "$TAP"
    fi
}

ensure_tap() {
    if ! brew tap-info "$TAP" 2>/dev/null | grep -q "Installed"; then
        echo "Tapping $TAP over authenticated HTTPS..."
        brew tap "$TAP" "$REPO_URL"
    fi
    ensure_tap_trusted
    brew update >/dev/null 2>&1 || true
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
        brew upgrade managed-machine 2>/dev/null || true
        cat <<EOF

managed-machine is installed and up to date. Use it directly:

  managed-machine              # run full bootstrap
  managed-machine --update     # brew update + safe setup re-runs
  managed-machine setup <name> # run one setup script (bin or setup-bin)
  managed-machine --help       # show usage

EOF
        exit 0
    fi

    echo "Installing managed-machine..."
    brew install managed-machine

    echo "Running managed-machine --bootstrap (terminal mode auto-detected)..."
    managed-machine --bootstrap

    cat <<EOF

managed-machine installed and bootstrapped. Future updates:

  managed-machine --update

EOF
}

main "$@"
