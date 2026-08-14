#!/usr/bin/env bash
# setup-git-hooks: only wire hooks at the managed-machine toplevel, and chain
# an existing hooksPath instead of replacing it.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
TEST_BIN="$TEST_DIR/bin"
HOOK_LOG="$TEST_DIR/hooks.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME" "$TEST_BIN"

configure_test_repo() {
    git -C "$1" config user.name 'managed-machine test'
    git -C "$1" config user.email 'managed-machine-test@example.invalid'
    git -C "$1" config commit.gpgsign false
    git -C "$1" config tag.gpgsign false
}

install_script_tree() {
    local dest="$1"
    mkdir -p "$dest/git-hooks" "$dest/lib"
    cp "$ROOT/setup-git-hooks" "$dest/setup-git-hooks"
    chmod +x "$dest/setup-git-hooks"
    cp "$ROOT/git-hooks/pre-commit" "$ROOT/git-hooks/setup-gitleaks" "$dest/git-hooks/"
    chmod +x "$dest/git-hooks/pre-commit" "$dest/git-hooks/setup-gitleaks"
    cp "$ROOT/lib/bootstrap.sh" "$ROOT/lib/install.sh" "$ROOT/lib/config-repo.sh" "$ROOT/lib/elevate.sh" "$dest/lib/"
}

cat >"$TEST_BIN/gitleaks" <<EOF
#!/usr/bin/env bash
printf 'gitleaks %s\n' "\$*" >>'$HOOK_LOG'
exit 0
EOF
chmod +x "$TEST_BIN/gitleaks"

export GIT_CONFIG_NOSYSTEM=1
export HOME="$TEST_HOME"
export PATH="$TEST_BIN:/usr/bin:/bin"

# 1. Nested libexec under a parent git repo must not write the parent's config
# (Homebrew walk-up). A generic parent with core.hooksPath=git-hooks is left
# alone — that value is not unique to this script.
PARENT="$TEST_DIR/generic-parent"
git init --quiet "$PARENT"
configure_test_repo "$PARENT"
git -C "$PARENT" commit --quiet --allow-empty -m seed
install_script_tree "$PARENT/libexec"
git -C "$PARENT" config --local core.hooksPath git-hooks
set +e
"$PARENT/libexec/setup-git-hooks" >"$TEST_DIR/skip.out" 2>&1
skip_status=$?
set -e
[[ "$skip_status" -eq 76 ]]
grep -Fq 'Skipped:' "$TEST_DIR/skip.out"
grep -Fq 'leaving it unchanged' "$TEST_DIR/skip.out"
[[ "$(git -C "$PARENT" config --local --get core.hooksPath)" == 'git-hooks' ]]
[[ -z "$(git -C "$PARENT" config --local --get managed-machine.priorHooksPath || true)" ]]

# 1b. The same fingerprint on a Homebrew prefix is the historical walk-up
# write and is safe to remove.
BREW_PREFIX="$TEST_DIR/opt-homebrew"
git init --quiet "$BREW_PREFIX"
configure_test_repo "$BREW_PREFIX"
git -C "$BREW_PREFIX" commit --quiet --allow-empty -m seed
mkdir -p "$BREW_PREFIX/bin"
cat >"$BREW_PREFIX/bin/brew" <<'EOF'
#!/bin/sh
exit 0
EOF
chmod +x "$BREW_PREFIX/bin/brew"
install_script_tree "$BREW_PREFIX/libexec"
git -C "$BREW_PREFIX" config --local core.hooksPath git-hooks
set +e
"$BREW_PREFIX/libexec/setup-git-hooks" >"$TEST_DIR/brew-skip.out" 2>&1
brew_skip_status=$?
set -e
[[ "$brew_skip_status" -eq 76 ]]
grep -Fq 'Removed stray core.hooksPath=git-hooks' "$TEST_DIR/brew-skip.out"
[[ -z "$(git -C "$BREW_PREFIX" config --local --get core.hooksPath || true)" ]]

# 2. A real clone with no prior hooksPath uses the repo git-hooks directory.
CLONE="$TEST_DIR/managed-machine"
git init --quiet "$CLONE"
configure_test_repo "$CLONE"
install_script_tree "$CLONE"
git -C "$CLONE" add setup-git-hooks git-hooks lib
git -C "$CLONE" commit --quiet -m seed
"$CLONE/setup-git-hooks" >"$TEST_DIR/clone.out" 2>&1
[[ "$(git -C "$CLONE" config --local --get core.hooksPath)" == 'git-hooks' ]]
[[ -z "$(git -C "$CLONE" config --local --get managed-machine.priorHooksPath || true)" ]]
grep -Fq 'core.hooksPath=git-hooks' "$TEST_DIR/clone.out"

# 3. An existing hooksPath is chained, not replaced. Re-run stays stable.
PRIOR="$TEST_DIR/agent-bot-hooks"
mkdir -p "$PRIOR"
cat >"$PRIOR/pre-commit" <<EOF
#!/bin/sh
printf 'prior-pre-commit\n' >>'$HOOK_LOG'
exit 0
EOF
cat >"$PRIOR/commit-msg" <<EOF
#!/bin/sh
printf 'prior-commit-msg\n' >>'$HOOK_LOG'
exit 0
EOF
cat >"$PRIOR/reference-transaction" <<EOF
#!/bin/sh
printf 'prior-reference-transaction\n' >>'$HOOK_LOG'
exit 0
EOF
chmod +x "$PRIOR/pre-commit" "$PRIOR/commit-msg" "$PRIOR/reference-transaction"

CHAIN="$TEST_DIR/chain-repo"
git init --quiet "$CHAIN"
configure_test_repo "$CHAIN"
install_script_tree "$CHAIN"
git -C "$CHAIN" add setup-git-hooks git-hooks lib
git -C "$CHAIN" commit --quiet -m seed
git -C "$CHAIN" config --global core.hooksPath "$PRIOR"

: >"$HOOK_LOG"
"$CHAIN/setup-git-hooks" >"$TEST_DIR/chain.out" 2>&1
dispatcher="$(git -C "$CHAIN" rev-parse --path-format=absolute --git-common-dir)/managed-machine-hooks"
[[ "$(git -C "$CHAIN" config --local --get core.hooksPath)" == "$dispatcher" ]]
[[ "$(git -C "$CHAIN" config --local --get managed-machine.priorHooksPath)" == "$PRIOR" ]]
[[ -x "$dispatcher/pre-commit" && -x "$dispatcher/commit-msg" && -x "$dispatcher/reference-transaction" ]]
grep -Fq "chaining $PRIOR" "$TEST_DIR/chain.out"

git -C "$CHAIN" commit --quiet --allow-empty -m 'exercise hooks'
grep -Fq 'gitleaks protect --staged --redact' "$HOOK_LOG"
grep -Fq 'prior-pre-commit' "$HOOK_LOG"

# Re-run must not record the dispatcher as the prior path.
"$CHAIN/setup-git-hooks" >"$TEST_DIR/chain-rerun.out" 2>&1
[[ "$(git -C "$CHAIN" config --local --get managed-machine.priorHooksPath)" == "$PRIOR" ]]
[[ "$(git -C "$CHAIN" config --local --get core.hooksPath)" == "$dispatcher" ]]

# 4. An older exclusive local git-hooks path is migrated to chain a global hooksPath.
MIGRATE="$TEST_DIR/migrate-repo"
git init --quiet "$MIGRATE"
configure_test_repo "$MIGRATE"
install_script_tree "$MIGRATE"
git -C "$MIGRATE" add setup-git-hooks git-hooks lib
git -C "$MIGRATE" commit --quiet -m seed
git -C "$MIGRATE" config --local core.hooksPath git-hooks
git -C "$MIGRATE" config --global core.hooksPath "$PRIOR"
"$MIGRATE/setup-git-hooks" >"$TEST_DIR/migrate.out" 2>&1
migrate_dispatcher="$(git -C "$MIGRATE" rev-parse --path-format=absolute --git-common-dir)/managed-machine-hooks"
[[ "$(git -C "$MIGRATE" config --local --get core.hooksPath)" == "$migrate_dispatcher" ]]
[[ "$(git -C "$MIGRATE" config --local --get managed-machine.priorHooksPath)" == "$PRIOR" ]]
grep -Fq "chaining $PRIOR" "$TEST_DIR/migrate.out"

# 5. A ~ hooksPath is expanded before chaining so generated wrappers can find it.
TILDE_PRIOR="$TEST_HOME/.hooks"
mkdir -p "$TILDE_PRIOR"
cat >"$TILDE_PRIOR/pre-commit" <<EOF
#!/bin/sh
printf 'tilde-pre-commit\n' >>'$HOOK_LOG'
exit 0
EOF
chmod +x "$TILDE_PRIOR/pre-commit"
TILDE="$TEST_DIR/tilde-repo"
git init --quiet "$TILDE"
configure_test_repo "$TILDE"
install_script_tree "$TILDE"
git -C "$TILDE" add setup-git-hooks git-hooks lib
git -C "$TILDE" commit --quiet -m seed
git -C "$TILDE" config --global core.hooksPath '~/.hooks'
: >"$HOOK_LOG"
"$TILDE/setup-git-hooks" >"$TEST_DIR/tilde.out" 2>&1
tilde_dispatcher="$(git -C "$TILDE" rev-parse --path-format=absolute --git-common-dir)/managed-machine-hooks"
[[ "$(git -C "$TILDE" config --local --get managed-machine.priorHooksPath)" == "$TILDE_PRIOR" ]]
[[ "$(git -C "$TILDE" config --local --get core.hooksPath)" == "$tilde_dispatcher" ]]
git -C "$TILDE" commit --quiet --allow-empty -m 'exercise tilde hooks'
grep -Fq 'tilde-pre-commit' "$HOOK_LOG"

echo 'setup-git-hooks tests passed'
