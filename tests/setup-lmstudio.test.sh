#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
BREW_LOG="$TEST_DIR/brew.log"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

cat >"$TEST_BIN/brew" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >>"$BREW_LOG"
case "$1" in
    list)
        grep -q '^install ' "$BREW_LOG"
        ;;
    install)
        for arg in "$@"; do
            case "$arg" in
                --appdir=*) mkdir -p "${arg#--appdir=}/LM Studio.app" ;;
            esac
        done
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

run_setup() {
    HOME="$TEST_HOME" \
    BREW_LOG="$BREW_LOG" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-lmstudio" "$@"
}

mkdir -p "$SYSTEM_APPDIR"
chmod 755 "$SYSTEM_APPDIR"
run_setup >"$TEST_DIR/system-install.out"
grep -Fq "LM Studio.app installed: $SYSTEM_APPDIR/LM Studio.app" "$TEST_DIR/system-install.out" \
    || grep -Fq "LM Studio installed: $SYSTEM_APPDIR/LM Studio.app" "$TEST_DIR/system-install.out"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"

: >"$BREW_LOG"
run_setup >"$TEST_DIR/rerun.out"
grep -Fq "already installed" "$TEST_DIR/rerun.out"
! grep -q '^install ' "$BREW_LOG"

CUSTOM="$TEST_DIR/custom-apps"
rm -rf "$SYSTEM_APPDIR/LM Studio.app"
: >"$BREW_LOG"
HOME="$TEST_HOME" BREW_LOG="$BREW_LOG" \
MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
MANAGED_MACHINE_LMSTUDIO_APPDIR="$CUSTOM" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/setup-lmstudio" >"$TEST_DIR/custom-install.out"
grep -Fq "$CUSTOM/LM Studio.app" "$TEST_DIR/custom-install.out"
grep -Fq -- "--appdir=$CUSTOM" "$BREW_LOG"

echo 'setup-lmstudio tests passed'
