#!/usr/bin/env bash
# Curlable one-shot installer for managed-machine.
#
#   curl -fsSL https://raw.githubusercontent.com/qwts/managed-machine/main/install.sh | bash
#
# Flow:
#   1. Ensure Homebrew is installed and owned by the current user.
#   2. If managed-machine is already installed: ensure tap is present, update,
#      and tell the user to use it directly.
#   3. If not installed: tap, install, and run managed-machine --bootstrap.
set -euo pipefail

TAP="qwts/managed-machine"
REPO_URL="git@github.com:qwts/managed-machine.git"

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
        cat >&2 <<EOF
Error: Homebrew prefix ($prefix) is owned by '$owner', not you ($(whoami)).
Fix ownership first, then re-run this installer:

  sudo chown -R $(whoami) "$prefix"

If you are not an admin user, run this installer as an admin user.
EOF
        exit 1
    fi
}

# 3. Ensure the tap is present and updated.
ensure_tap() {
    if ! brew tap-info "$TAP" 2>/dev/null | grep -q "Installed"; then
        echo "Tapping $TAP..."
        brew tap "$TAP" "$REPO_URL"
    fi
    brew update >/dev/null 2>&1 || true
}

# 4. Is managed-machine already installed via brew?
is_managed_machine_installed() {
    brew list --versions managed-machine >/dev/null 2>&1
}

main() {
    ensure_brew_installed
    ensure_brew_ownership
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
