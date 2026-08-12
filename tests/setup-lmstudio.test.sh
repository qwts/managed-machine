#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
BREW_LOG="$TEST_DIR/brew.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"

# brew stub: `install --cask lm-studio --appdir=X` creates the app bundle in
# X; `list --cask lm-studio` succeeds only after an install. It never sudos.
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
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-lmstudio" "$@"
}

# 1+2. Unwritable system dir: install lands in ~/Applications with a note and
# without any privileged command; a re-run recognizes the per-user install.
# Root ignores permission bits (-w is always true), so these two cases only
# run for regular users; the remaining cases cover both.
mkdir -p "$SYSTEM_APPDIR"
if [[ "$(id -u)" != "0" ]]; then
    chmod 555 "$SYSTEM_APPDIR"
    run_setup >"$TEST_DIR/user-install.out"
    grep -Fq "requires administrator access — installing to $TEST_HOME/Applications" "$TEST_DIR/user-install.out"
    grep -Fq "LM Studio installed: $TEST_HOME/Applications/LM Studio.app" "$TEST_DIR/user-install.out"
    grep -Fq -- "--appdir=$TEST_HOME/Applications" "$BREW_LOG"
    [[ -d "$TEST_HOME/Applications/LM Studio.app" ]]

    : >"$BREW_LOG"
    run_setup >"$TEST_DIR/rerun.out"
    grep -Fq "LM Studio already installed: $TEST_HOME/Applications/LM Studio.app" "$TEST_DIR/rerun.out"
    ! grep -q '^install ' "$BREW_LOG"
fi

# 3. Writable system dir is preferred on a fresh machine.
chmod 755 "$SYSTEM_APPDIR"
rm -rf "$TEST_HOME/Applications" "$BREW_LOG"
: >"$BREW_LOG"
run_setup >"$TEST_DIR/system-install.out"
grep -Fq "LM Studio installed: $SYSTEM_APPDIR/LM Studio.app" "$TEST_DIR/system-install.out"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"

# 4. An explicit app dir wins over everything and is reported.
CUSTOM="$TEST_DIR/custom-apps"
rm -rf "$SYSTEM_APPDIR/LM Studio.app"
: >"$BREW_LOG"
HOME="$TEST_HOME" BREW_LOG="$BREW_LOG" \
MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
MANAGED_MACHINE_LMSTUDIO_APPDIR="$CUSTOM" \
PATH="$TEST_BIN:/usr/bin:/bin" \
/bin/bash "$ROOT/setup-lmstudio" >"$TEST_DIR/custom-install.out"
grep -Fq "LM Studio installed: $CUSTOM/LM Studio.app" "$TEST_DIR/custom-install.out"
grep -Fq -- "--appdir=$CUSTOM" "$BREW_LOG"

echo 'setup-lmstudio tests passed'
