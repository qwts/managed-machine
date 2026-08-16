#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
DEVIN_LOG="$TEST_ROOT/devin.log"
CURL_LOG="$TEST_ROOT/curl.log"
DEVIN_INSTALL_LOG="$TEST_ROOT/install.log"
ORIGINAL_PATH="$PATH"
export DEVIN_LOG CURL_LOG DEVIN_INSTALL_LOG
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"
CONFIG_REPO="$TEST_ROOT/managed-machine-config"
mkdir -p "$CONFIG_REPO"
cp "$ROOT/tests/fixtures/apps.json" "$CONFIG_REPO/apps.json"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'test'
git -C "$CONFIG_REPO" config user.email 'test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed
: >"$DEVIN_LOG"
: >"$CURL_LOG"
: >"$DEVIN_INSTALL_LOG"

cat >"$TEST_BIN/curl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$CURL_LOG"

output=''
while [[ $# -gt 0 ]]; do
    if [[ "$1" == '-o' ]]; then
        shift
        output="$1"
        break
    fi
    shift
done
[[ -n "$output" ]]

if [[ "${MOCK_BAD_DEVIN_INSTALLER:-0}" == '1' ]]; then
    cat >"$output" <<'INSTALLER'
#!/usr/bin/env bash
printf 'unexpected installer executed\n' >>"$DEVIN_INSTALL_LOG"
echo 'unexpected final command'
INSTALLER
    exit 0
fi

cat >"$output" <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
printf 'installer executed\n' >>"$DEVIN_INSTALL_LOG"
mkdir -p "$HOME/.local/bin"
cat >"$HOME/.local/bin/devin" <<'DEVIN'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$DEVIN_LOG"
if [[ "${1:-}" == '--version' ]]; then
    echo 'devin test-version'
    exit 0
fi
if [[ "${1:-}" == 'auth' && "${2:-}" == 'status' ]]; then
    [[ -f "$HOME/.mock-devin-authenticated" ]]
    exit
fi
if [[ "${1:-}" == 'setup' ]]; then
    if IFS= read -r setup_input; then
        printf 'setup input: %s\n' "$setup_input" >>"$DEVIN_LOG"
    fi
    if [[ "${MOCK_DEVIN_SETUP_RESULT:-0}" == '0' ]]; then
        touch "$HOME/.mock-devin-authenticated"
        exit 0
    fi
    exit "$MOCK_DEVIN_SETUP_RESULT"
fi
exit 0
DEVIN
chmod +x "$HOME/.local/bin/devin"
VERSION_DIR="$HOME/.local/share/devin-test"
COMPILED_BIN_NAME='devin'
mkdir -p "$VERSION_DIR/bin"
cp "$HOME/.local/bin/devin" "$VERSION_DIR/bin/devin"
"$VERSION_DIR/bin/$COMPILED_BIN_NAME" setup
INSTALLER
EOF
chmod +x "$TEST_BIN/curl"

# A fresh noninteractive run installs the binary, does not invoke setup, and
# returns the shared deferral status for bootstrap to record as pending.
set +e
HOME="$TEST_HOME" \
PATH="$TEST_BIN:/usr/bin:/bin" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive \
/bin/bash "$ROOT/setup-devin" >"$TEST_ROOT/noninteractive.out" 2>&1
result=$?
set -e
[[ "$result" -eq 76 ]]
[[ -x "$TEST_HOME/.local/bin/devin" ]]
grep -qxF 'installer executed' "$DEVIN_INSTALL_LOG"
grep -qxF 'auth status' "$DEVIN_LOG"
! grep -qxF 'setup' "$DEVIN_LOG"
grep -Fq 'Skipped:' "$TEST_ROOT/noninteractive.out"
! grep -Fq 'managed-machine setup devin' "$TEST_ROOT/noninteractive.out"

# An existing authenticated install is complete and never redownloads or
# reopens the setup wizard, even without a terminal.
touch "$TEST_HOME/.mock-devin-authenticated"
: >"$CURL_LOG"
: >"$DEVIN_LOG"
HOME="$TEST_HOME" \
PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
CONFIG_REPO_ROOT="$CONFIG_REPO" \
MANAGED_MACHINE_BOOTSTRAP_MODE=noninteractive \
/bin/bash "$ROOT/setup-devin" >"$TEST_ROOT/authenticated.out" 2>&1
[[ ! -s "$CURL_LOG" ]]
grep -qxF 'auth status' "$DEVIN_LOG"
! grep -qxF 'setup' "$DEVIN_LOG"
grep -Fq 'authentication is already configured' "$TEST_ROOT/authenticated.out"

# Interactive setup completes authentication and verifies the resulting state.
HOME="$TEST_HOME"
PATH="$TEST_HOME/.local/bin:$TEST_BIN:$ORIGINAL_PATH"
export HOME PATH
rm -f "$TEST_HOME/.mock-devin-authenticated"
: >"$DEVIN_LOG"
# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"
# shellcheck source=lib/bootstrap.sh
source "$ROOT/lib/bootstrap.sh"
# shellcheck source=lib/devin.sh
source "$ROOT/lib/devin.sh"
printf 'wizard-input\n' >"$TEST_ROOT/interactive-input"
bootstrap_interactive_input() { printf '%s\n' "$TEST_ROOT/interactive-input"; }
MOCK_DEVIN_SETUP_RESULT=0
export MOCK_DEVIN_SETUP_RESULT
ensure_devin_authentication
grep -qxF 'setup' "$DEVIN_LOG"
grep -qxF 'setup input: wizard-input' "$DEVIN_LOG"
[[ "$(grep -c '^auth status$' "$DEVIN_LOG")" -eq 2 ]]
[[ -f "$TEST_HOME/.mock-devin-authenticated" ]]

# A canceled interactive setup remains a real failure.
rm -f "$TEST_HOME/.mock-devin-authenticated"
MOCK_DEVIN_SETUP_RESULT=1
export MOCK_DEVIN_SETUP_RESULT
if ensure_devin_authentication >"$TEST_ROOT/canceled.out" 2>&1; then
    echo 'expected canceled Devin setup to fail' >&2
    exit 1
fi
grep -Fq 'interactive setup did not complete' "$TEST_ROOT/canceled.out"

# Upstream installer drift fails before any downloaded code executes.
: >"$DEVIN_INSTALL_LOG"
MOCK_BAD_DEVIN_INSTALLER=1
export MOCK_BAD_DEVIN_INSTALLER
if install_devin_cli >"$TEST_ROOT/drift.out" 2>&1; then
    echo 'expected changed Devin installer contract to fail closed' >&2
    exit 1
fi
[[ ! -s "$DEVIN_INSTALL_LOG" ]]
grep -Fq 'refusing to modify or execute it' "$TEST_ROOT/drift.out"

echo 'setup-devin tests passed'
