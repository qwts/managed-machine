#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
TEST_HOME="$TEST_DIR/home"
CONFIG_REPO_ROOT="$TEST_DIR/managed-machine-config"
LOCAL_BIN_DIR="$TEST_DIR/local-bin"
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
printf 'v1.0.0\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
git -C "$CONFIG_REPO_ROOT" add . && git -C "$CONFIG_REPO_ROOT" commit --quiet -m seed

# local-bin checkout with a tag, a later commit, and its own installer.
git init --quiet "$LOCAL_BIN_DIR"
configure_test_repo "$LOCAL_BIN_DIR"
cat >"$LOCAL_BIN_DIR/install" <<EOF
#!/usr/bin/env bash
echo ran >>'$INSTALL_LOG'
EOF
chmod +x "$LOCAL_BIN_DIR/install"
git -C "$LOCAL_BIN_DIR" add . && git -C "$LOCAL_BIN_DIR" commit --quiet -m 'v1'
git -C "$LOCAL_BIN_DIR" tag v1.0.0
TAG_COMMIT="$(git -C "$LOCAL_BIN_DIR" rev-parse HEAD)"
printf 'change\n' >"$LOCAL_BIN_DIR/extra"
git -C "$LOCAL_BIN_DIR" add . && git -C "$LOCAL_BIN_DIR" commit --quiet -m 'v2'
HEAD_COMMIT="$(git -C "$LOCAL_BIN_DIR" rev-parse HEAD)"
git -C "$LOCAL_BIN_DIR" branch -M main

run_setup() {
    HOME="$TEST_HOME" \
    CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    LOCAL_BIN_DIR="$LOCAL_BIN_DIR" \
    PATH="/usr/bin:/bin" \
    /bin/bash "$ROOT/setup-bin" "$@"
}

# 1. Tag pin checks out the tag, records the exact commit, runs install.
run_setup >"$TEST_DIR/tag.out"
[[ "$(git -C "$LOCAL_BIN_DIR" rev-parse HEAD)" == "$TAG_COMMIT" ]]
grep -Fq 'ran' "$INSTALL_LOG"
MANIFEST="$TEST_HOME/.config/managed-machine/local-bin.manifest"
grep -qxF 'ref=v1.0.0' "$MANIFEST"
grep -qxF "commit=$TAG_COMMIT" "$MANIFEST"
grep -qxF "checkout=$(cd "$LOCAL_BIN_DIR" && pwd -P)" "$MANIFEST"

# 2. Re-run with the pin already satisfied skips the fetch.
run_setup >"$TEST_DIR/satisfied.out"
grep -Fq 'already at pin v1.0.0 — skipping fetch' "$TEST_DIR/satisfied.out"
! grep -Fq 'Fetching local-bin' "$TEST_DIR/satisfied.out"

# 3. A commit SHA is an accepted immutable pin.
printf '%s\n' "$HEAD_COMMIT" >"$CONFIG_REPO_ROOT/local-bin.ref"
run_setup >"$TEST_DIR/sha.out"
[[ "$(git -C "$LOCAL_BIN_DIR" rev-parse HEAD)" == "$HEAD_COMMIT" ]]
grep -qxF "commit=$HEAD_COMMIT" "$MANIFEST"
run_setup >"$TEST_DIR/sha-rerun.out"
grep -Fq "already at pin $HEAD_COMMIT — skipping fetch" "$TEST_DIR/sha-rerun.out"

# 4. A moving branch is rejected with remediation, before any checkout.
printf 'main\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
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

# 6. A prefix-owned checkout is trusted for one command; do not write gitconfig.
TEST_BIN="$TEST_DIR/bin"
mkdir -p "$TEST_BIN"
REAL_GIT="$(command -v git)"
cat >"$TEST_BIN/git" <<EOF
#!/usr/bin/env bash
has_safe=0
for arg in "\$@"; do
    case "\$arg" in
        safe.directory=*) has_safe=1 ;;
    esac
done
if [[ "\$has_safe" -eq 0 ]]; then
    for arg in "\$@"; do
        if [[ "\$arg" == "$LOCAL_BIN_DIR" || "\$arg" == "$LOCAL_BIN_DIR/.git" ]]; then
            echo "fatal: detected dubious ownership in repository at '$LOCAL_BIN_DIR'" >&2
            exit 128
        fi
    done
    if [[ "\$PWD" == "$LOCAL_BIN_DIR" ]]; then
        echo "fatal: detected dubious ownership in repository at '$LOCAL_BIN_DIR'" >&2
        exit 128
    fi
fi
exec '$REAL_GIT' "\$@"
EOF
chmod +x "$TEST_BIN/git"
printf 'v1.0.0\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
git -C "$LOCAL_BIN_DIR" checkout --quiet v1.0.0
: >"$INSTALL_LOG"
PATH="$TEST_BIN:/usr/bin:/bin" HOME="$TEST_HOME" CONFIG_REPO_ROOT="$CONFIG_REPO_ROOT" \
    LOCAL_BIN_DIR="$LOCAL_BIN_DIR" /bin/bash "$ROOT/setup-bin" >"$TEST_DIR/foreign.out" 2>&1
grep -Fq 'already at pin v1.0.0 — skipping fetch' "$TEST_DIR/foreign.out"
! grep -Fq 'dubious ownership' "$TEST_DIR/foreign.out"

# 7. An unresolvable pin is a clear error.
printf 'v9.9.9\n' >"$CONFIG_REPO_ROOT/local-bin.ref"
if run_setup >"$TEST_DIR/unknown.out" 2>&1; then
    echo 'expected an unknown pin to fail' >&2
    exit 1
fi
grep -Fq "does not resolve to a tag, commit, or branch" "$TEST_DIR/unknown.out"

ARCHIVE_DIR="$TEST_DIR/archive"
mkdir -p "$ARCHIVE_DIR"
cp "$LOCAL_BIN_DIR/install" "$ARCHIVE_DIR/install"
LOCAL_BIN_DIR="$ARCHIVE_DIR" run_setup >"$TEST_DIR/archive.out"
grep -qxF 'commit=' "$MANIFEST"
grep -qxF "checkout=$(cd "$ARCHIVE_DIR" && pwd -P)" "$MANIFEST"

echo 'setup-bin tests passed'
