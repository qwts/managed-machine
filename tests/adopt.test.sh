#!/usr/bin/env bash
# Adopt vendor-installed signed-cask apps into Homebrew.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
SYSTEM_APPDIR="$TEST_DIR/system-applications"
USER_APPDIR="$TEST_HOME/Applications"
BREW_LOG="$TEST_DIR/brew.log"
OSA_LOG="$TEST_DIR/osascript.log"
RECEIPTS="$TEST_DIR/receipts"
CASK_SHA="aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
trap 'chmod -R u+w "$TEST_DIR" 2>/dev/null || true; rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$SYSTEM_APPDIR" "$RECEIPTS" "$USER_APPDIR"
CONFIG_REPO="$TEST_DIR/managed-machine-config"
mkdir -p "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed

write_cask_json() {
    local token="$1" url_host="$2" home_host="$3"
    cat >"$TEST_DIR/cask-$token.json" <<EOF
{"casks":[{"token":"$token","tap":"homebrew/cask","sha256":"$CASK_SHA","url":"https://$url_host/pkg","homepage":"https://$home_host/"}]}
EOF
}

write_cask_json visual-studio-code update.code.visualstudio.com code.visualstudio.com
write_cask_json cursor downloads.cursor.com cursor.com
write_cask_json claude downloads.claude.ai claude.com
write_cask_json antigravity storage.googleapis.com antigravity.google
write_cask_json antigravity-ide edgedl.me.gvt1.com antigravity.google

cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$BREW_LOG'
case "\$1" in
    info)
        token="\${*: -1}"
        token="\${token##*/}"
        cat '$TEST_DIR/cask-'"\$token"'.json'
        ;;
    install)
        token=""
        fail_token="\${BREW_FAIL_TOKEN:-}"
        for arg in "\$@"; do
            case "\$arg" in
                homebrew/cask/*) token="\${arg##*/}" ;;
            esac
        done
        if [[ -n "\$fail_token" && "\$token" == "\$fail_token" ]]; then
            echo "brew install failed for \$token" >&2
            exit 1
        fi
        if [[ -n "\$token" ]]; then
            printf '%s\n' "\$token" >>'$RECEIPTS/list'
        fi
        ;;
    list)
        token=""
        for arg in "\$@"; do
            case "\$arg" in
                --cask|--versions) ;;
                *) token="\$arg" ;;
            esac
        done
        if [[ -f '$RECEIPTS/list' ]] && grep -qx "\$token" '$RECEIPTS/list'; then
            echo "\$token 1.2.3"
            exit 0
        fi
        exit 1
        ;;
esac
EOF
chmod +x "$TEST_BIN/brew"

cat >"$TEST_BIN/codesign" <<'EOF'
#!/usr/bin/env bash
app="${*: -1}"
if [[ "${CODESIGN_FAIL:-0}" == "1" ]]; then
    echo "failed" >&2
    exit 1
fi
if [[ "$1" == "--verify" ]]; then
    exit 0
fi
team="${CODESIGN_TEAM:-}"
if [[ -z "$team" ]]; then
    case "$app" in
        *'Visual Studio Code.app'*) team='UBF8T346G9' ;;
        *'Cursor.app'*) team='VDXQ22DGB9' ;;
        *'Claude.app'*) team='Q6L2SF6YDW' ;;
        *'Antigravity IDE.app'*) team='EQHXZ8M8AV' ;;
        *'Antigravity.app'*) team='EQHXZ8M8AV' ;;
        *) team='AAAAAAAAAA' ;;
    esac
fi
echo "Authority=Developer ID Application: Vendor ($team)"
echo "TeamIdentifier=$team"
EOF
chmod +x "$TEST_BIN/codesign"

cat >"$TEST_BIN/lsof" <<'EOF'
#!/usr/bin/env bash
path="${*: -1}"
recurse=0
for arg in "$@"; do
    [[ "$arg" == "+D" ]] && recurse=1
done
if [[ -n "${LSOF_RUNNING_PATH:-}" ]]; then
    if [[ "$path" == "$LSOF_RUNNING_PATH"* ]]; then
        echo 12345
        exit 0
    fi
    if [[ "$recurse" == "1" && "$LSOF_RUNNING_PATH" == "$path"* ]]; then
        echo 12345
        exit 0
    fi
fi
exit 1
EOF
chmod +x "$TEST_BIN/lsof"

cat >"$TEST_BIN/osascript" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>'$OSA_LOG'
if [[ "\${OSA_EXIT:-0}" != "0" ]]; then
    exit "\$OSA_EXIT"
fi
args=("\$@")
n=\${#args[@]}
i=0
while [[ \$i -lt \$n ]]; do
    if [[ "\${args[\$i]}" == "/bin/mv" && \$((i + 2)) -lt \$n ]]; then
        src="\${args[\$((i + 1))]}"
        dest="\${args[\$((i + 2))]}"
        chmod u+w "\$(dirname "\$src")" 2>/dev/null || true
        /bin/mv "\$src" "\$dest"
        break
    fi
    i=\$((i + 1))
done
exit 0
EOF
chmod +x "$TEST_BIN/osascript"

cat >"$TEST_BIN/uname" <<'EOF'
#!/usr/bin/env bash
echo Darwin
EOF
chmod +x "$TEST_BIN/uname"

plant_app() {
    mkdir -p "$1"
}

reset_state() {
    rm -rf "$SYSTEM_APPDIR"/* "$USER_APPDIR" "$RECEIPTS/list"
    mkdir -p "$SYSTEM_APPDIR" "$USER_APPDIR" "$RECEIPTS"
    : >"$BREW_LOG"
    : >"$OSA_LOG"
    chmod 755 "$SYSTEM_APPDIR"
    unset LSOF_RUNNING_PATH CODESIGN_FAIL CODESIGN_TEAM BREW_FAIL_TOKEN OSA_EXIT
}

run_adopt() {
    HOME="$TEST_HOME" \
    BREW_LOG="$BREW_LOG" \
    OSA_LOG="$OSA_LOG" \
    RECEIPTS="$RECEIPTS" \
    LSOF_RUNNING_PATH="${LSOF_RUNNING_PATH:-}" \
    CODESIGN_FAIL="${CODESIGN_FAIL:-0}" \
    CODESIGN_TEAM="${CODESIGN_TEAM:-}" \
    BREW_FAIL_TOKEN="${BREW_FAIL_TOKEN:-}" \
    OSA_EXIT="${OSA_EXIT:-0}" \
    MANAGED_MACHINE_SYSTEM_APPDIR="$SYSTEM_APPDIR" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    PATH="$TEST_BIN:/usr/bin:/bin" \
    /bin/bash "$ROOT/scripts/adopt" "$@"
}

# Help lists cask tokens and aliases.
run_adopt --help >"$TEST_DIR/help.out"
grep -Fq 'visual-studio-code' "$TEST_DIR/help.out"
grep -Fq '(vscode)' "$TEST_DIR/help.out"
grep -Fq 'claude' "$TEST_DIR/help.out"
grep -Fq '(claude-app)' "$TEST_DIR/help.out"

# Unknown name prints the token/alias list and exits nonzero.
reset_state
if run_adopt no-such-app >"$TEST_DIR/unknown.out" 2>&1; then
    echo 'expected unknown name to fail' >&2
    exit 1
fi
grep -Fq 'unknown app name: no-such-app' "$TEST_DIR/unknown.out"
grep -Fq 'Available apps (cask token; aliases accepted):' "$TEST_DIR/unknown.out"
grep -Fq 'visual-studio-code' "$TEST_DIR/unknown.out"
grep -Fq '(vscode)' "$TEST_DIR/unknown.out"

# vscode alias resolves to visual-studio-code.
reset_state
plant_app "$SYSTEM_APPDIR/Visual Studio Code.app"
run_adopt vscode >"$TEST_DIR/alias.out"
grep -Fq '==> visual-studio-code' "$TEST_DIR/alias.out"
grep -Fq -- '--adopt' "$BREW_LOG"
grep -Fq 'homebrew/cask/visual-studio-code' "$BREW_LOG"
grep -Fq "complete: visual-studio-code" "$TEST_DIR/alias.out"
grep -Fq "already installed: $SYSTEM_APPDIR/Visual Studio Code.app" "$TEST_DIR/alias.out"

# Writable /Applications adopts in place with no elevation.
reset_state
plant_app "$SYSTEM_APPDIR/Visual Studio Code.app"
run_adopt visual-studio-code >"$TEST_DIR/inplace.out"
grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"
grep -Fq -- '--adopt' "$BREW_LOG"
[[ -d "$SYSTEM_APPDIR/Visual Studio Code.app" ]]
[[ ! -d "$USER_APPDIR/Visual Studio Code.app" ]]
! grep -Fq '/bin/mv' "$OSA_LOG"

# Already-has-receipt, missing, and bad Team ID skip.
reset_state
plant_app "$SYSTEM_APPDIR/Visual Studio Code.app"
printf '%s\n' 'visual-studio-code' >"$RECEIPTS/list"
run_adopt visual-studio-code >"$TEST_DIR/receipt.out"
grep -Fq 'already has a Homebrew cask receipt' "$TEST_DIR/receipt.out"
grep -Fq 'skipped: 1' "$TEST_DIR/receipt.out"
! grep -q '^install ' "$BREW_LOG"

reset_state
run_adopt cursor >"$TEST_DIR/missing.out"
grep -Fq 'Cursor.app is missing from disk' "$TEST_DIR/missing.out"
! grep -q '^install ' "$BREW_LOG"

reset_state
plant_app "$SYSTEM_APPDIR/Visual Studio Code.app"
CODESIGN_TEAM=AAAAAAAAAA run_adopt visual-studio-code >"$TEST_DIR/bad-team.out" 2>&1
grep -Fq 'failed Developer ID / Team ID verification' "$TEST_DIR/bad-team.out"
! grep -q '^install ' "$BREW_LOG"

# Running Cursor skips with a quit-then-re-run message.
reset_state
plant_app "$SYSTEM_APPDIR/Cursor.app"
LSOF_RUNNING_PATH="$SYSTEM_APPDIR/Cursor.app" run_adopt cursor >"$TEST_DIR/running.out"
grep -Fq "Cursor.app is running at $SYSTEM_APPDIR/Cursor.app" "$TEST_DIR/running.out"
grep -Fq 'quit then re-run: managed-machine adopt cursor' "$TEST_DIR/running.out"
! grep -q '^install ' "$BREW_LOG"
! grep -Fq '/bin/mv' "$OSA_LOG"
[[ -d "$SYSTEM_APPDIR/Cursor.app" ]]

# Nested Cursor Helper.app is treated as running even when the top-level
# Contents/MacOS binary is not mapped.
reset_state
plant_app "$SYSTEM_APPDIR/Cursor.app"
helper="$SYSTEM_APPDIR/Cursor.app/Contents/Frameworks/Cursor Helper.app/Contents/MacOS/Cursor Helper"
mkdir -p "$(dirname "$helper")"
: >"$helper"
chmod +x "$helper"
LSOF_RUNNING_PATH="$helper" run_adopt cursor >"$TEST_DIR/helper.out"
grep -Fq "Cursor.app is running at $SYSTEM_APPDIR/Cursor.app" "$TEST_DIR/helper.out"
grep -Fq 'quit then re-run: managed-machine adopt cursor' "$TEST_DIR/helper.out"
! grep -q '^install ' "$BREW_LOG"
[[ -d "$SYSTEM_APPDIR/Cursor.app" ]]

# Unwritable /Applications: adopt in place via brew --appdir; do not move
# the bundle to ~/Applications.
if [[ "$(id -u)" != "0" ]]; then
    reset_state
    plant_app "$SYSTEM_APPDIR/Claude.app"
    chmod 555 "$SYSTEM_APPDIR"
    run_adopt claude >"$TEST_DIR/move.out"
    grep -Fq -- "--appdir=$SYSTEM_APPDIR" "$BREW_LOG"
    grep -Fq -- '--adopt' "$BREW_LOG"
    [[ -d "$SYSTEM_APPDIR/Claude.app" ]]
    [[ ! -d "$USER_APPDIR/Claude.app" ]]
    ! grep -Fq '/bin/mv' "$OSA_LOG"
    chmod 755 "$SYSTEM_APPDIR"
fi

# Bulk adopt: mix of complete, skipped, and failed; exit 1 only on failure.
reset_state
plant_app "$SYSTEM_APPDIR/Claude.app"
plant_app "$SYSTEM_APPDIR/Cursor.app"
plant_app "$SYSTEM_APPDIR/Visual Studio Code.app"
LSOF_RUNNING_PATH="$SYSTEM_APPDIR/Cursor.app" \
BREW_FAIL_TOKEN=visual-studio-code \
    run_adopt >"$TEST_DIR/bulk.out" 2>&1 || bulk_status=$?
[[ "${bulk_status:-0}" -eq 1 ]]
grep -Fq 'complete: claude' "$TEST_DIR/bulk.out"
grep -Fq 'Cursor.app is running' "$TEST_DIR/bulk.out"
grep -Fq 'brew install failed for visual-studio-code' "$TEST_DIR/bulk.out"
grep -Fq 'Adopt summary:' "$TEST_DIR/bulk.out"
grep -Fq 'complete: 1' "$TEST_DIR/bulk.out"
grep -Fq 'failed: 1' "$TEST_DIR/bulk.out"
grep -Fq '    claude' "$TEST_DIR/bulk.out"
grep -Fq '    visual-studio-code' "$TEST_DIR/bulk.out"
grep -Fq '    cursor' "$TEST_DIR/bulk.out"

echo 'adopt tests passed'
