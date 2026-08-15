#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_BIN="$TEST_DIR/bin"
BREW_LOG="$TEST_DIR/brew.log"
trap 'rm -rf "$TEST_DIR"' EXIT
mkdir -p "$TEST_BIN"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"

user_in_admin_group "$(id -un)" || true
preferred="$(preferred_brew_owner)"
[[ -n "$preferred" ]]

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
EOF
chmod +x "$TEST_BIN/brew"
PATH="$TEST_BIN:/usr/bin:/bin" brew_run install hello
grep -Fxq 'install hello' "$BREW_LOG"

# When brew runs as another user, the invoking gh token is forwarded so
# private tap/formula clones use the authenticated session.
ELEVATE_LOG="$TEST_DIR/elevate.log"
cat >"$TEST_BIN/gh" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" == auth && "$2" == token ]]; then
    echo 'gho_testtoken'
    exit 0
fi
exit 1
EOF
chmod +x "$TEST_BIN/gh"
brew_is_system_prefix() { return 0; }
brew_prefix_owner() { printf 'otheradmin\n'; }
elevate_as_user() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
}
PATH="$TEST_BIN:/usr/bin:/bin" brew_run tap qwts/managed-machine
grep -Fq 'HOMEBREW_GITHUB_API_TOKEN=gho_testtoken' "$ELEVATE_LOG"
grep -Fq 'GIT_CONFIG_KEY_0=http.https://github.com/.extraheader' "$ELEVATE_LOG"
grep -Fq 'tap qwts/managed-machine' "$ELEVATE_LOG"

echo 'brew helper tests passed'
