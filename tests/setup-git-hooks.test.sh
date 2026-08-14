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
    cp "$ROOT/lib/bootstrap.sh" "$dest/lib/bootstrap.sh"
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
# (Homebrew walk-up).
PARENT="$TEST_DIR/opt-homebrew"
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
grep -Fq 'Removed stray core.hooksPath=git-hooks' "$TEST_DIR/skip.out"
[[ -z "$(git -C "$PARENT" config --local --get core.hooksPath || true)" ]]
[[ -z "$(git -C "$PARENT" config --local --get managed-machine.priorHooksPath || true)" ]]

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

echo 'setup-git-hooks tests passed'
