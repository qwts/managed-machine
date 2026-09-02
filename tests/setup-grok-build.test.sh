#!/usr/bin/env bash
# setup-grok-build: official installer, managed PATH, idempotent when present,
# and the vendor installer never edits ~/.zshrc (#87).
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

# The fake installer mirrors the real one's PATH handling: it symlinks into
# ~/.local/bin, and picks a shell rc file to append its block to from $SHELL
# (zsh -> ~/.zshrc). With MOCK_INSTALLER_IGNORES_SHELL=1 it appends the block
# whatever $SHELL says, standing in for an installer that cannot be steered.
cat >"$TEST_BIN/curl" <<EOF
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$*" >>'$CURL_LOG'
cat <<'INSTALLER'
#!/usr/bin/env bash
set -euo pipefail
printf 'url-args=%s shell=%s\n' "\$*" "\${SHELL:-}" >>'$INSTALL_LOG'
mkdir -p "\$HOME/.grok/bin" "\$HOME/.local/bin"
cat >"\$HOME/.grok/bin/grok" <<'GROK'
#!/usr/bin/env bash
echo 'grok 0.1.0-test'
GROK
chmod +x "\$HOME/.grok/bin/grok"
ln -sf "\$HOME/.grok/bin/grok" "\$HOME/.local/bin/grok"
case "\$(basename "\${SHELL:-}")" in
    zsh) rc="\$HOME/.zshrc" ;;
    bash) rc="\$HOME/.bashrc" ;;
    *) rc="" ;;
esac
[[ -n "\$rc" || "\${MOCK_INSTALLER_IGNORES_SHELL:-0}" == 1 ]] || exit 0
printf '\n# >>> grok installer >>>\nexport PATH="\$HOME/.grok/bin:\$PATH"\n# <<< grok installer <<<\n' >>"\${rc:-\$HOME/.zshrc}"
echo "  Updated \$HOME/.grok/bin in PATH in \${rc:-\$HOME/.zshrc}." >&2
INSTALLER
EOF
chmod +x "$TEST_BIN/curl"

run_setup() {
    HOME="$TEST_HOME" SHELL=/bin/zsh PATH="$TEST_BIN:/usr/bin:/bin" CONFIG_REPO_ROOT="$CONFIG_REPO" /bin/bash "$ROOT/setup-grok-build"
}

# 1. Missing grok: official URL, binary lands through ~/.local/bin, managed
# PATH is present, and the vendor installer saw no login shell to edit — so
# ~/.zshrc carries managed-machine's guard and no vendor block, and the run
# does not warn.
run_setup >"$TEST_ROOT/install.out" 2>&1
grep -Fq 'https://x.ai/cli/install.sh' "$CURL_LOG"
[[ -x "$TEST_HOME/.local/bin/grok" ]]
grep -Fq 'Grok Build installed:' "$TEST_ROOT/install.out"
grep -Fq 'grok 0.1.0-test' "$TEST_ROOT/install.out"
grep -Fq 'shell=/bin/sh' "$INSTALL_LOG"
grep -Fq '# BEGIN local-bin' "$TEST_HOME/.zshrc"
! grep -Fq 'grok installer' "$TEST_HOME/.zshrc"
[[ ! -e "$TEST_HOME/.bashrc" ]]
! grep -Fq 'edited' "$TEST_ROOT/install.out"

# 2. Re-run is a no-op: curl is not invoked again.
: >"$CURL_LOG"
: >"$INSTALL_LOG"
HOME="$TEST_HOME" PATH="$TEST_HOME/.local/bin:$TEST_BIN:/usr/bin:/bin" \
    CONFIG_REPO_ROOT="$CONFIG_REPO" \
    /bin/bash "$ROOT/setup-grok-build" >"$TEST_ROOT/rerun.out"
[[ ! -s "$CURL_LOG" ]]
[[ ! -s "$INSTALL_LOG" ]]
grep -Fq 'Grok Build already installed:' "$TEST_ROOT/rerun.out"

# 3. An installer that edits ~/.zshrc regardless is not silent: the run
# names the leak and the remedy, and still succeeds.
rm -f "$TEST_HOME/.local/bin/grok"
: >"$CURL_LOG"
MOCK_INSTALLER_IGNORES_SHELL=1 run_setup >"$TEST_ROOT/leak.out" 2>&1
grep -Fq 'grok installer' "$TEST_HOME/.zshrc"
grep -Fq "warn: Grok Build's installer edited $TEST_HOME/.zshrc outside managed-machine's guards" "$TEST_ROOT/leak.out"
grep -Fq 'managed-machine setup zsh' "$TEST_ROOT/leak.out"
grep -Fq 'Grok Build installed:' "$TEST_ROOT/leak.out"

echo 'setup-grok-build tests passed'
