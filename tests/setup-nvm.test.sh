#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
NO_CURL_BIN="$TEST_DIR/no-curl-bin"
NO_GIT_BIN="$TEST_DIR/no-git-bin"
TEST_LOG="$TEST_DIR/nvm.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN" "$NO_CURL_BIN" "$NO_GIT_BIN"
printf '# user-owned setting\nexport FOO=bar\n' >"$TEST_HOME/.zshrc"

cat >"$TEST_BIN/git" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

cat >"$TEST_BIN/node" <<'EOF'
#!/usr/bin/env bash
printf 'v22.0.0\n'
EOF

cat >"$TEST_BIN/npm" <<'EOF'
#!/usr/bin/env bash
printf '10.0.0\n'
EOF

cat >"$TEST_BIN/curl" <<'EOF'
#!/usr/bin/env bash
cat <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
mkdir -p "$NVM_DIR"
cat >"$NVM_DIR/nvm.sh" <<'NVM'
nvm() {
    printf '%s\n' "$*" >>"$NVM_TEST_LOG"
    case "$1" in
        --version) printf '0.40.4\n' ;;
    esac
}
NVM
INSTALLER
EOF

chmod +x "$TEST_BIN"/*

cp "$TEST_BIN/git" "$NO_CURL_BIN/git"
cat >"$NO_CURL_BIN/dirname" <<'EOF'
#!/usr/bin/env bash
exec /usr/bin/dirname "$@"
EOF
chmod +x "$NO_CURL_BIN"/*

cp "$TEST_BIN/curl" "$NO_GIT_BIN/curl"
cp "$NO_CURL_BIN/dirname" "$NO_GIT_BIN/dirname"
chmod +x "$NO_GIT_BIN"/*

run_setup() {
    HOME="$TEST_HOME" \
    NVM_DIR="$TEST_HOME/.nvm" \
    PATH="$TEST_BIN:$PATH" \
    NVM_TEST_LOG="$TEST_LOG" \
    /bin/bash "$ROOT/setup-nvm"
}

run_setup
run_setup

[[ -s "$TEST_HOME/.nvm/nvm.sh" ]]
[[ "$(grep -c '^# BEGIN nvm$' "$TEST_HOME/.zshrc")" == "1" ]]
grep -qxF 'export FOO=bar' "$TEST_HOME/.zshrc"
grep -qxF 'install --lts' "$TEST_LOG"
grep -qxF 'alias default lts/*' "$TEST_LOG"
if command -v zsh >/dev/null 2>&1; then
    zsh -n "$TEST_HOME/.zshrc"
fi

CUSTOM_NVM_DIR="$TEST_HOME/custom-nvm"
HOME="$TEST_HOME" \
NVM_DIR="$CUSTOM_NVM_DIR" \
PATH="$TEST_BIN:$PATH" \
NVM_TEST_LOG="$TEST_LOG" \
/bin/bash "$ROOT/setup-nvm"
grep -qxF "export NVM_DIR=$CUSTOM_NVM_DIR" "$TEST_HOME/.zshrc"

if HOME="$TEST_HOME" NVM_DIR="$TEST_HOME/.nvm" PATH="$NO_CURL_BIN:/bin" /bin/bash "$ROOT/setup-nvm" >"$TEST_DIR/missing.out" 2>&1; then
    echo "expected setup-nvm to fail when curl is unavailable" >&2
    exit 1
fi
grep -Fq 'curl is required' "$TEST_DIR/missing.out"

if HOME="$TEST_HOME" NVM_DIR="$TEST_HOME/.nvm" PATH="$NO_GIT_BIN:/bin" /bin/bash "$ROOT/setup-nvm" >"$TEST_DIR/missing-git.out" 2>&1; then
    echo "expected setup-nvm to fail when git is unavailable" >&2
    exit 1
fi
grep -Fq 'git is required' "$TEST_DIR/missing-git.out"

echo "setup-nvm tests passed"
