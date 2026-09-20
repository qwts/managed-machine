#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
ZSH_FUNCTIONS_DIR="$TEST_DIR/zsh-functions"
INSTALL_LOG="$TEST_DIR/install.log"
trap 'rm -rf "$TEST_DIR"' EXIT

mkdir -p "$TEST_HOME"

configure_test_repo() {
    export GIT_AUTHOR_NAME='managed-machine test' GIT_COMMITTER_NAME='managed-machine test'
    export GIT_AUTHOR_EMAIL='managed-machine-test@example.invalid' GIT_COMMITTER_EMAIL='managed-machine-test@example.invalid'
    git -C "$1" config commit.gpgsign false
    git -C "$1" config tag.gpgsign false
}

# Config checkout carrying the pin file.
git init --quiet "$CONFIG_REPO_ROOT"
configure_test_repo "$CONFIG_REPO_ROOT"
printf 'v0.1.0\n' >"$CONFIG_REPO_ROOT/zsh-functions.ref"
git -C "$CONFIG_REPO_ROOT" add . && git -C "$CONFIG_REPO_ROOT" commit --quiet -m seed

# zsh-functions checkout with a tag, a later commit, and its own installer.
git init --quiet "$ZSH_FUNCTIONS_DIR"
configure_test_repo "$ZSH_FUNCTIONS_DIR"
cat >"$ZSH_FUNCTIONS_DIR/install" <<EOF
#!/usr/bin/env bash
echo ran >>'$INSTALL_LOG'
EOF
chmod +x "$ZSH_FUNCTIONS_DIR/install"
git -C "$ZSH_FUNCTIONS_DIR" add . && git -C "$ZSH_FUNCTIONS_DIR" commit --quiet -m 'v1'
git -C "$ZSH_FUNCTIONS_DIR" tag v0.1.0
TAG_COMMIT="$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)"
printf 'change\n' >"$ZSH_FUNCTIONS_DIR/extra"
git -C "$ZSH_FUNCTIONS_DIR" add . && git -C "$ZSH_FUNCTIONS_DIR" commit --quiet -m 'v2'
HEAD_COMMIT="$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)"
git -C "$ZSH_FUNCTIONS_DIR" branch -M main

run_setup() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    ZSH_FUNCTIONS_DIR="$ZSH_FUNCTIONS_DIR" \
    PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-zsh-functions" "$@"
}

# 1. Tag pin checks out the tag, records the exact commit, runs install.
run_setup >"$TEST_DIR/tag.out"
[[ "$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)" == "$TAG_COMMIT" ]]
grep -Fq 'ran' "$INSTALL_LOG"
MANIFEST="$TEST_HOME/.config/managed-machine/zsh-functions.manifest"
grep -qxF 'ref=v0.1.0' "$MANIFEST"
grep -qxF "commit=$TAG_COMMIT" "$MANIFEST"
grep -qxF "checkout=$(cd "$ZSH_FUNCTIONS_DIR" && pwd -P)" "$MANIFEST"

# 2. Re-run with the pin already satisfied skips the fetch.
run_setup >"$TEST_DIR/satisfied.out"
grep -Fq 'already at pin v0.1.0 — skipping fetch' "$TEST_DIR/satisfied.out"
! grep -Fq 'Fetching zsh-functions' "$TEST_DIR/satisfied.out"

# 3. A commit SHA is an accepted immutable pin.
printf '%s\n' "$HEAD_COMMIT" >"$CONFIG_REPO_ROOT/zsh-functions.ref"
run_setup >"$TEST_DIR/sha.out"
[[ "$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)" == "$HEAD_COMMIT" ]]
grep -qxF "commit=$HEAD_COMMIT" "$MANIFEST"
run_setup >"$TEST_DIR/sha-rerun.out"
grep -Fq "already at pin $HEAD_COMMIT — skipping fetch" "$TEST_DIR/sha-rerun.out"

# 4. A moving branch is rejected with remediation, before any checkout.
printf 'main\n' >"$CONFIG_REPO_ROOT/zsh-functions.ref"
if run_setup >"$TEST_DIR/branch.out" 2>&1; then
    echo 'expected a branch pin to fail' >&2
    exit 1
fi
grep -Fq "moving branch, not an immutable ref" "$TEST_DIR/branch.out"
grep -Fq 'MANAGED_MACHINE_ALLOW_BRANCH_PIN=1' "$TEST_DIR/branch.out"

# 5. The explicit override warns and proceeds.
if ! MANAGED_MACHINE_ALLOW_BRANCH_PIN=1 run_setup >"$TEST_DIR/override.out" 2>&1; then
    echo 'expected the explicit branch override to succeed' >&2
    exit 1
fi
grep -Fq "warning: 'main' is a moving branch" "$TEST_DIR/override.out"

# 6. A tag colliding with a pinned short SHA fails closed, not redirected.
# (Full 40-hex pins are immune by git's own rule — 40-hex refs are ignored
# in favor of the object — but short SHAs resolve to the tag silently.)
printf 'c1\n' >"$ZSH_FUNCTIONS_DIR/collide"
git -C "$ZSH_FUNCTIONS_DIR" add . && git -C "$ZSH_FUNCTIONS_DIR" commit --quiet -m 'c1'
C1="$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)"
SHORT="${C1:0:12}"
printf 'c2\n' >>"$ZSH_FUNCTIONS_DIR/collide"
git -C "$ZSH_FUNCTIONS_DIR" add . && git -C "$ZSH_FUNCTIONS_DIR" commit --quiet -m 'c2'
C2="$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)"
git -C "$ZSH_FUNCTIONS_DIR" tag "$SHORT"
printf '%s\n' "$SHORT" >"$CONFIG_REPO_ROOT/zsh-functions.ref"
: >"$INSTALL_LOG"
if run_setup >"$TEST_DIR/collide.out" 2>&1; then
    echo 'expected a shadowed SHA pin to fail' >&2
    exit 1
fi
grep -Fq "shadows this SHA" "$TEST_DIR/collide.out"
[[ "$(git -C "$ZSH_FUNCTIONS_DIR" rev-parse HEAD)" == "$C2" ]]
[[ ! -s "$INSTALL_LOG" ]] || { echo 'install ran on a shadowed pin' >&2; exit 1; }
grep -qxF "commit=$HEAD_COMMIT" "$MANIFEST"

# 7. An unresolvable pin is a clear error.
printf 'v9.9.9\n' >"$CONFIG_REPO_ROOT/zsh-functions.ref"
if run_setup >"$TEST_DIR/unknown.out" 2>&1; then
    echo 'expected an unknown pin to fail' >&2
    exit 1
fi
grep -Fq "does not resolve to a tag, commit, or branch" "$TEST_DIR/unknown.out"

# 8. A checkout without git metadata records the pin with an empty commit.
ARCHIVE_DIR="$TEST_DIR/archive"
mkdir -p "$ARCHIVE_DIR"
cp "$ZSH_FUNCTIONS_DIR/install" "$ARCHIVE_DIR/install"
ZSH_FUNCTIONS_DIR="$ARCHIVE_DIR" run_setup >"$TEST_DIR/archive.out"
grep -qxF 'commit=' "$MANIFEST"
grep -qxF "checkout=$(cd "$ARCHIVE_DIR" && pwd -P)" "$MANIFEST"

echo 'setup-zsh-functions tests passed'
