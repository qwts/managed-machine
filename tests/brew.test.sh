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

# When brew runs as another user, the invoking gh token is forwarded via a
# 600 file so it never appears in osascript/sudo/env argv.
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
elevate_run() {
    printf '%s\n' "$*" >>"$ELEVATE_LOG"
    local arg
    for arg in "$@"; do
        if [[ "$arg" == *gho_testtoken* ]]; then
            echo "token leaked into argv: $arg" >&2
            exit 1
        fi
        if [[ -f "$arg" ]] && grep -qxF 'gho_testtoken' "$arg" 2>/dev/null; then
            cp "$arg" "$TEST_DIR/captured-token"
        fi
    done
}
PATH="$TEST_BIN:/usr/bin:/bin" brew_run tap qwts/managed-machine
grep -Fq 'tap qwts/managed-machine' "$ELEVATE_LOG"
grep -Fq 'brew-github-auth-run' "$ELEVATE_LOG"
grep -Fq 'otheradmin' "$ELEVATE_LOG"
! grep -Fq 'gho_testtoken' "$ELEVATE_LOG"
[[ "$(cat "$TEST_DIR/captured-token")" == 'gho_testtoken' ]]

echo 'brew helper tests passed'
