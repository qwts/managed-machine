#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEMP="$(mktemp -d)"
TEMP="$(cd "$TEMP" && pwd -P)"
trap 'rm -rf "$TEMP"' EXIT
export HOME="$TEMP/home"
export GIT_AUTHOR_NAME=fixture GIT_COMMITTER_NAME=fixture
export GIT_AUTHOR_EMAIL=fixture@example.invalid GIT_COMMITTER_EMAIL=fixture@example.invalid
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null
unset CONFIG_REPO_ROOT XDG_DATA_HOME LOCAL_BIN_DIR ZDOTDIR
mkdir -p "$HOME" "$TEMP/runtime/managed-machine-config/dotfiles/zsh" "$TEMP/runtime/local-bin"
cp -R "$ROOT/lib" "$TEMP/runtime/lib"
cp "$ROOT/setup-bin" "$TEMP/runtime/setup-bin"
source "$ROOT/lib/account-setup.sh"
seed="$TEMP/runtime/managed-machine-config"
binseed="$TEMP/runtime/local-bin"
for name in .zshrc .zprofile .zshenv; do
    printf 'export FIXTURE_SHELL=1\n' >"$seed/dotfiles/zsh/$name"
done
cat >"$binseed/install" <<'EOF'
#!/bin/bash
set -eu
CATEGORIES=(images rename files media utils dns)
SKIP_NAMES=(trash_util.py)
[[ -z "${GH_TOKEN:-}${SSH_AUTH_SOCK:-}${BASH_ENV:-}${MANAGED_MACHINE_ALLOW_BRANCH_PIN:-}" ]]
[[ "$LOCAL_BIN_DIR" == "$HOME/"* ]]
mkdir -p "$HOME/.local/bin" "$HOME/.config/local-bin"
ln -sf "$LOCAL_BIN_DIR/utils/fixture-tool" "$HOME/.local/bin/fixture-tool"
printf 'fixture-tool\n' >"$HOME/.config/local-bin/linked-commands"
printf 'installed\n' >"$HOME/installer-ran"
EOF
for category in images rename files media utils dns; do
    mkdir -p "$binseed/$category"
    touch "$binseed/$category/.keep"
done
printf 'fixture\n' >"$binseed/utils/fixture-tool"
chmod +x "$binseed/install" "$binseed/utils/fixture-tool"
git init -q "$binseed"
git -C "$binseed" add .
git -C "$binseed" -c commit.gpgsign=false commit -qm initial
git -C "$binseed" tag v1
pin="$(git -C "$binseed" rev-parse HEAD)"
printf 'new fixture\n' >"$binseed/tool"
git -C "$binseed" add .
git -C "$binseed" -c commit.gpgsign=false commit -qm newer
git -C "$binseed" branch -M main
seed_head="$(git -C "$binseed" rev-parse HEAD)"
printf 'v1\n' >"$seed/local-bin.ref"
git init -q "$seed"
git -C "$seed" add .
git -C "$seed" -c commit.gpgsign=false commit -qm config
[[ "$(account_config_source "$TEMP/runtime")" == "$seed" ]]
[[ ! -e "$HOME/.local" ]]
printf 'exit 99\n' >"$TEMP/inherited-bash-env"
export BASH_ENV="$TEMP/inherited-bash-env"
export GH_TOKEN=must-not-leak SSH_AUTH_SOCK=/human/ssh MANAGED_MACHINE_ALLOW_BRANCH_PIN=1
account_prepare_config "$TEMP/runtime" >"$TEMP/config.out"
[[ "$CONFIG_REPO_ROOT" == "$HOME/.local/share/managed-machine/managed-machine-config" ]]
[[ "$(cat "$TEMP/config.out")" == "$CONFIG_REPO_ROOT" ]]
[[ "$(account_config_source "$TEMP/runtime")" == "$CONFIG_REPO_ROOT" ]]
export ZDOTDIR="$HOME/zsh"
mkdir -p "$ZDOTDIR"
printf 'alias custom=true\nexport PATH="custom:$PATH"\n' >"$ZDOTDIR/.zprofile"
cp "$ZDOTDIR/.zprofile" "$TEMP/custom"
account_prepare_shell "$TEMP/runtime"
head -n 2 "$ZDOTDIR/.zprofile" | cmp - "$TEMP/custom"
for name in .zshenv .zprofile .zshrc; do
    grep -q 'BEGIN local-bin' "$ZDOTDIR/$name"
done
[[ "$(account_setup_clean /bin/bash -c 'source "$1/.zshenv"; printf "%s\n" "$PATH"' bash "$ZDOTDIR")" == "$HOME/.local/bin:"* ]]
[[ ! -e "$HOME/.zshrc" ]]
mkdir -p "$HOME/.local/bin" "$TEMP/shared"
printf 'shared executable\n' >"$TEMP/shared/agent-bot"
ln -s "$TEMP/shared/agent-bot" "$HOME/.local/bin/agent-bot"
ln -s /unreadable/human/private-tool "$HOME/.local/bin/unrelated-tool"
account_prepare_local_bin "$TEMP/runtime"
[[ "$(readlink "$HOME/.local/bin/agent-bot")" == "$TEMP/shared/agent-bot" ]]
[[ "$(readlink "$HOME/.local/bin/unrelated-tool")" == /unreadable/human/private-tool ]]
[[ "$(cat "$TEMP/shared/agent-bot")" == 'shared executable' ]]
[[ -e "$HOME/installer-ran" && -L "$HOME/.local/bin/fixture-tool" ]]
[[ "$(git -C "$binseed" rev-parse HEAD)" == "$seed_head" ]]
[[ -z "$(git -C "$binseed" status --porcelain)" ]]
[[ "$(git -C "$HOME/.local/share/managed-machine/local-bin/$pin" rev-parse HEAD)" == "$pin" ]]
account_prepare_environment "$TEMP/runtime"
head -n 2 "$ZDOTDIR/.zprofile" | cmp - "$TEMP/custom"
mv "$CONFIG_REPO_ROOT/local-bin.ref" "$TEMP/saved-pin"
status=0
account_prepare_local_bin "$TEMP/runtime" 2>/dev/null || status=$?
[[ "$status" == 75 ]]
mv "$TEMP/saved-pin" "$CONFIG_REPO_ROOT/local-bin.ref"
status=0
CONFIG_REPO_ROOT="$HOME/unseeded-config" account_prepare_config "$TEMP/unbundled/runtime" 2>/dev/null || status=$?
[[ "$status" == 75 && ! -e "$HOME/unseeded-config" ]]
printf 'missing-tag\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
status=0
account_prepare_local_bin "$TEMP/runtime" 2>"$TEMP/pending" || status=$?
[[ "$status" == 75 ]]
grep -q 'update managed-machine bundled local-bin' "$TEMP/pending"
printf 'main\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
status=0
account_prepare_local_bin "$TEMP/runtime" 2>/dev/null || status=$?
[[ "$status" == 1 ]]
printf 'v1\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
rm "$HOME/.local/bin/fixture-tool"
ln -s "$TEMP/shared/agent-bot" "$HOME/.local/bin/fixture-tool"
if account_prepare_local_bin "$TEMP/runtime" 2>"$TEMP/collision"; then exit 1; fi
grep -q 'collision: fixture-tool' "$TEMP/collision"
[[ "$(readlink "$HOME/.local/bin/fixture-tool")" == "$TEMP/shared/agent-bot" ]]
rm "$HOME/.local/bin/fixture-tool"
account_prepare_local_bin "$TEMP/runtime"
printf '../private\n' >"$HOME/.config/local-bin/linked-commands"
if account_prepare_local_bin "$TEMP/runtime" 2>/dev/null; then exit 1; fi
printf 'fixture-tool\n' >"$HOME/.config/local-bin/linked-commands"
if CONFIG_REPO_ROOT="$seed" account_prepare_config "$TEMP/runtime" 2>/dev/null; then exit 1; fi
if ZDOTDIR="$TEMP/human" account_prepare_shell "$TEMP/runtime" 2>/dev/null; then exit 1; fi
rm "$ZDOTDIR/.zshrc"
ln -s "$TEMP/custom" "$ZDOTDIR/.zshrc"
if account_prepare_shell "$TEMP/runtime" 2>/dev/null; then exit 1; fi
head -n 2 "$ZDOTDIR/.zprofile" | cmp - "$TEMP/custom"
if LOCAL_BIN_DIR="$binseed" account_prepare_local_bin "$TEMP/runtime" 2>/dev/null; then exit 1; fi
mkdir -p "$HOME/bad"
ln -s "$TEMP" "$HOME/bad/escape"
if XDG_DATA_HOME="$HOME/bad/escape" CONFIG_REPO_ROOT= account_prepare_config "$TEMP/runtime" 2>/dev/null; then exit 1; fi
mkdir -p "$TEMP/dev/runtime" "$TEMP/dev/managed-machine-config/.git"
[[ "$(CONFIG_REPO_ROOT= XDG_DATA_HOME="$HOME/unused" account_config_source "$TEMP/dev/runtime")" == "$TEMP/dev/managed-machine-config" ]]
[[ ! -e "$HOME/unused" ]]
printf 'account setup environment tests passed\n'
