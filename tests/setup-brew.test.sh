#!/usr/bin/env bash
# setup-brew writes a dedup brew PATH block so every shell reaches Homebrew.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"
unset ZDOTDIR
export HOME="$TEST_HOME"

PREFIX="$TEST_DIR/prefix"
mkdir -p "$PREFIX/bin" "$PREFIX/sbin"

# shellcheck source=lib/install.sh
source "$ROOT/lib/install.sh"

fail() { echo "FAIL: $1" >&2; exit 1; }

# 1. Explicit prefix writes the exact block.
HOME="$TEST_HOME" ensure_brew_path_block "$PREFIX"
grep -qxF '# BEGIN brew' "$TEST_HOME/.zshenv" || fail "missing BEGIN marker"
grep -qxF 'typeset -U path PATH' "$TEST_HOME/.zshenv" || fail "missing typeset guard"
grep -qxF "path=($PREFIX/bin $PREFIX/sbin \$path)" "$TEST_HOME/.zshenv" || fail "wrong path body"
grep -qxF '# END brew' "$TEST_HOME/.zshenv" || fail "missing END marker"

# 2. Missing prefix leaves profiles untouched.
HOME2="$TEST_DIR/home2"
mkdir -p "$HOME2"
HOME="$HOME2" ensure_brew_path_block "$TEST_DIR/no-such-prefix"
[[ ! -e "$HOME2/.zshenv" ]] || fail "wrote a block with no prefix"

# 3. Nested double-source keeps exactly one entry per dir.
zsh -f -c '
  source "$0/.zshenv"
  source "$0/.zshenv"
  for d in "$1/bin" "$1/sbin"; do
    c=0
    for p in "$path[@]"; do [[ "$p" == "$d" ]] && (( c++ )); done
    (( c == 1 )) || { print -u2 "duplicated: $d"; exit 1 }
  done
' "$TEST_HOME" "$PREFIX" || fail "nested sourcing duplicated entries"

# 4. Script level: brew on PATH exits 0 with the block written.
cat >"$TEST_BIN/brew" <<EOF
#!/usr/bin/env bash
if [[ "\${1:-}" == "--prefix" ]]; then printf '%s\n' '$PREFIX'; exit 0; fi
echo 'Homebrew 9.9.9-test'
exit 0
EOF
chmod +x "$TEST_BIN/brew"
cp "$TEST_BIN/brew" "$PREFIX/bin/brew"
chmod +x "$PREFIX/bin/brew"
rm -f "$TEST_HOME/.zshenv"
HOME="$TEST_HOME" PATH="$TEST_BIN:/usr/bin:/bin" \
  /bin/bash "$ROOT/setup-brew" >"$TEST_DIR/setup.out" 2>&1 \
  || fail "setup-brew failed with brew on PATH"
grep -qxF '# BEGIN brew' "$TEST_HOME/.zshenv" || fail "script did not write the block"
grep -qxF "path=($PREFIX/bin $PREFIX/sbin \$path)" "$TEST_HOME/.zshenv" || fail "script block has wrong prefix"

# 5. Bare non-login zsh resolves brew through the block alone.
out="$(HOME="$TEST_HOME" PATH="/usr/bin:/bin" zsh -c 'command -v brew')"
[[ "$out" == "$PREFIX/bin/brew" ]] || fail "bare zsh cannot reach brew: $out"

echo 'setup-brew tests passed'
