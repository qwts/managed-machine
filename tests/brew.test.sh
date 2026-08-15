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

echo 'brew helper tests passed'
