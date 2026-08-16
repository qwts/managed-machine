#!/usr/bin/env bash
# The repository is private: every documented install path must be an
# authenticated fetch, and no bootstrap-time repository URL may require SSH
# before setup-gh has provisioned a key.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"

INSTALL_API_PATH='repos/qwts/managed-machine/contents/install.sh'

# README and the skill document the same authenticated installer command.
grep -q "$INSTALL_API_PATH" "$ROOT/README.md"
grep -q "$INSTALL_API_PATH" "$ROOT/skills/SKILL.md"
grep -q "$INSTALL_API_PATH" "$ROOT/install.sh"

# raw.githubusercontent.com returns 404 for private repositories; it must not
# be documented as an install path anywhere.
if grep -rn 'raw.githubusercontent.com/qwts' "$ROOT/README.md" "$ROOT/skills/SKILL.md" "$ROOT/install.sh"; then
    echo 'unauthenticated raw.githubusercontent.com install path is documented' >&2
    exit 1
fi

# Bootstrap-time repository URLs are authenticated HTTPS, never SSH. Match
# concrete repository URLs only: lib/config-repo.sh legitimately pattern-matches
# the SSH form to migrate legacy remotes.
for f in install.sh Formula/managed-machine.rb setup-bin lib/config-repo.sh; do
    if grep -n 'git@github.com:qwts' "$ROOT/$f"; then
        echo "SSH repository URL in $f runs before setup-gh provisions SSH" >&2
        exit 1
    fi
done

# The installer authenticates gh and wires the git credential helper before
# tapping the private repository.
grep -q 'gh auth setup-git' "$ROOT/install.sh"
grep -q 'ensure_gh_access' "$ROOT/install.sh"

# Private brew operations run as the prefix owner must carry the invoking
# user's GitHub credentials without putting the token in process arguments.
grep -q 'write_brew_github_auth_run' "$ROOT/install.sh"
grep -q 'mm-gh-token' "$ROOT/install.sh"
grep -q 'brew-github-auth-run' "$ROOT/lib/brew.sh"
if grep -n 'HOMEBREW_GITHUB_API_TOKEN=\${token}' "$ROOT/install.sh" "$ROOT/lib/brew.sh"; then
    echo 'GitHub token must not be interpolated into argv' >&2
    exit 1
fi
if grep -n 'github_auth_env_for_brew_owner' "$ROOT/install.sh"; then
    echo 'argv-based github_auth_env_for_brew_owner should be gone' >&2
    exit 1
fi

echo 'install docs tests passed'
