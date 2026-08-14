#!/usr/bin/env bash
# Official curl|bash CLIs: install once, skip PATH edits, no-op when present.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_ROOT="$(mktemp -d)"
TEST_HOME="$TEST_ROOT/home"
TEST_BIN="$TEST_ROOT/bin"
CURL_LOG="$TEST_ROOT/curl.log"
INSTALL_LOG="$TEST_ROOT/install.log"
trap 'rm -rf "$TEST_ROOT"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"

write_curl_stub() {
    local dest_rel="$1"
    local cmd="$2"
    cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>'$CURL_LOG'
cat <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
printf 'url-args=%s\n' "\$*" >>'$INSTALL_LOG'
printf 'CODEX_NON_INTERACTIVE=%s\n' "\${CODEX_NON_INTERACTIVE-}" >>'$INSTALL_LOG'
mkdir -p "\$HOME/$(dirname "$dest_rel")"
cat >"\$HOME/$dest_rel" <<'BIN'
#!/usr/bin/env bash
echo '$cmd 0.1.0-test'
BIN
chmod +x "\$HOME/$dest_rel"
INSTALLER
EOF
    chmod +x "$TEST_BIN/curl"
}

run_setup() {
    local script="$1"
    HOME="$TEST_HOME" PATH="$TEST_BIN:/usr/bin:/bin" /bin/bash "$ROOT/$script"
}

assert_install() {
    local script="$1"
    local url="$2"
    local cmd="$3"
    local dest="$4"
    : >"$CURL_LOG"
    : >"$INSTALL_LOG"
    rm -rf "$TEST_HOME/.local" "$TEST_HOME/.opencode" "$TEST_HOME/.zshrc"
    run_setup "$script" >"$TEST_ROOT/out"
    grep -Fq "$url" "$CURL_LOG"
    [[ -x "$dest" ]]
    grep -Fq "$cmd 0.1.0-test" "$TEST_ROOT/out"
    grep -Fq '# BEGIN local-bin' "$TEST_HOME/.zshrc"
    ! grep -Fq '>>> Codex installer >>>' "$TEST_HOME/.zshrc"
    ! grep -Fq '# opencode' "$TEST_HOME/.zshrc"

    : >"$CURL_LOG"
    HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
        /bin/bash "$ROOT/$script" >"$TEST_ROOT/rerun.out"
    [[ ! -s "$CURL_LOG" ]]
    grep -Fq 'already installed' "$TEST_ROOT/rerun.out"
}

write_curl_stub '.local/bin/claude' claude
assert_install setup-claude 'https://claude.ai/install.sh' claude "$TEST_HOME/.local/bin/claude"

write_curl_stub '.local/bin/codex' codex
assert_install setup-codex-cli 'https://chatgpt.com/codex/install.sh' codex "$TEST_HOME/.local/bin/codex"
grep -Fxq 'CODEX_NON_INTERACTIVE=1' "$INSTALL_LOG"

write_curl_stub '.local/bin/agy' agy
assert_install setup-antigravity 'https://antigravity.google/cli/install.sh' agy "$TEST_HOME/.local/bin/agy"

write_curl_stub '.opencode/bin/opencode' opencode
assert_install setup-opencode 'https://opencode.ai/install' opencode "$TEST_HOME/.local/bin/opencode"
[[ -L "$TEST_HOME/.local/bin/opencode" ]]
[[ "$(readlink "$TEST_HOME/.local/bin/opencode")" == "$TEST_HOME/.opencode/bin/opencode" ]]
grep -Fq -- '--no-modify-path' "$INSTALL_LOG"

echo 'setup-agent-clis tests passed'
