#!/usr/bin/env bash
# setup-muse: official installer, skip PATH edits, idempotent when present.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
CURL_LOG="$TEST_ROOT/curl.log"
INSTALL_LOG="$TEST_ROOT/install.log"
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
: >"$CURL_LOG"
: >"$INSTALL_LOG"

cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>'$CURL_LOG'
cat <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
printf 'MUSE_NO_MODIFY_PATH=%s\n' "\${MUSE_NO_MODIFY_PATH-}" >>'$INSTALL_LOG'
mkdir -p "\$HOME/.local/bin"
cat >"\$HOME/.local/bin/muse" <<'MUSE'
#!/usr/bin/env bash
echo 'muse 0.1.0-test'
MUSE
chmod +x "\$HOME/.local/bin/muse"
INSTALLER
EOF
chmod +x "$TEST_BIN/curl"

run_setup() {
    HOME="$TEST_HOME" PATH="$TEST_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-muse"
}

# 1. Missing muse: official URL, installer sees MUSE_NO_MODIFY_PATH=1, binary lands.
run_setup >"$TEST_ROOT/install.out"
grep -Fq 'https://dev.meta.ai/install.sh' "$CURL_LOG"
grep -Fxq 'MUSE_NO_MODIFY_PATH=1' "$INSTALL_LOG"
[[ -x "$TEST_HOME/.local/bin/muse" ]]
grep -Fq 'Meta Muse Code installed:' "$TEST_ROOT/install.out"
grep -Fq 'muse 0.1.0-test' "$TEST_ROOT/install.out"
# Managed PATH block is present; installer must not add its own line.
grep -Fq '# BEGIN local-bin' "$TEST_HOME/.zshrc"
! grep -Fq 'added by muse installer' "$TEST_HOME/.zshrc"

# 2. Re-run is a no-op: curl is not invoked again.
: >"$CURL_LOG"
: >"$INSTALL_LOG"
# Prefer the already-installed binary over the curl stub directory.
HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-muse" >"$TEST_ROOT/rerun.out"
[[ ! -s "$CURL_LOG" ]]
[[ ! -s "$INSTALL_LOG" ]]
grep -Fq 'Meta Muse Code already installed:' "$TEST_ROOT/rerun.out"

echo 'setup-muse tests passed'
