#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
FIXTURE="$TEST_DIR/repo"
REMOTE="$TEST_DIR/origin.git"
trap 'rm -rf "$TEST_DIR"' EXIT

# The real repo must not drift: formula and skill carry the same version.
FORMULA_VERSION="$(sed -nE 's/^[[:space:]]*version "([0-9.]+)"$/\1/p' "$ROOT/Formula/managed-machine.rb" | head -1)"
SKILL_VERSION="$(sed -nE 's/^[[:space:]]*version "([0-9.]+)"$/\1/p' "$ROOT/skills/SKILL.md" | head -1)"
[[ -n "$FORMULA_VERSION" && "$FORMULA_VERSION" == "$SKILL_VERSION" ]]

# Fixture clone with the real formula and skill files, pushing to a local
# bare remote.
git init --quiet --bare "$REMOTE"
git init --quiet "$FIXTURE"
git -C "$FIXTURE" config user.name 'managed-machine test'
git -C "$FIXTURE" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE" config commit.gpgsign false
git -C "$FIXTURE" config tag.gpgsign false
mkdir -p "$FIXTURE/Formula" "$FIXTURE/skills"
cp "$ROOT/Formula/managed-machine.rb" "$FIXTURE/Formula/"
cp "$ROOT/skills/SKILL.md" "$FIXTURE/skills/"
git -C "$FIXTURE" add . && git -C "$FIXTURE" commit --quiet -m seed
git -C "$FIXTURE" branch -M main
git -C "$FIXTURE" remote add origin "$REMOTE"
git -C "$FIXTURE" push --quiet -u origin main

run_release() {
    (cd "$FIXTURE" && /bin/bash "$ROOT/scripts/release" "$@")
}

# 1. A release bumps formula tag+version and skill version together, commits,
# tags, and pushes both.
run_release v9.9.9 >"$TEST_DIR/release.out"
grep -qE '^[[:space:]]*tag:[[:space:]]*"v9\.9\.9"' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*version "9\.9\.9"' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*version "9\.9\.9"' "$FIXTURE/skills/SKILL.md"
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
git --git-dir="$REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null
[[ "$(git --git-dir="$REMOTE" rev-parse main)" == "$(git -C "$FIXTURE" rev-parse HEAD)" ]]
[[ "$(git -C "$FIXTURE" log -1 --format=%s)" == 'Release v9.9.9' ]]

# The tagged commit contains the formula pointing at its own tag.
git -C "$FIXTURE" show v9.9.9:Formula/managed-machine.rb | grep -qE '"v9\.9\.9"'

# 2. Re-releasing the same version fails: published tags are immutable.
if run_release v9.9.9 >"$TEST_DIR/duplicate.out" 2>&1; then
    echo 'expected duplicate release to fail' >&2
    exit 1
fi
grep -Fq 'tag v9.9.9 already exists on origin' "$TEST_DIR/duplicate.out"

# 2b. A release whose push failed resumes: local commit+tag exist, the remote
# has neither, and rerunning the same command completes the publish.
FIXTURE2="$TEST_DIR/repo2"
REMOTE2="$TEST_DIR/origin2.git"
git init --quiet --bare "$REMOTE2"
git clone --quiet "$REMOTE" "$FIXTURE2" 2>/dev/null || {
    git init --quiet "$FIXTURE2"
    mkdir -p "$FIXTURE2/Formula" "$FIXTURE2/skills"
    cp "$ROOT/Formula/managed-machine.rb" "$FIXTURE2/Formula/"
    cp "$ROOT/skills/SKILL.md" "$FIXTURE2/skills/"
    git -C "$FIXTURE2" add .
}
git -C "$FIXTURE2" config user.name 'managed-machine test'
git -C "$FIXTURE2" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE2" config commit.gpgsign false
git -C "$FIXTURE2" config tag.gpgsign false
git -C "$FIXTURE2" checkout --quiet -B main
git -C "$FIXTURE2" commit --quiet -m seed --allow-empty
git -C "$FIXTURE2" remote remove origin 2>/dev/null || true
git -C "$FIXTURE2" remote add origin "$REMOTE2"
git -C "$FIXTURE2" push --quiet -u origin main
# Simulate the failed-push state: release commit + local tag, nothing pushed.
(cd "$FIXTURE2" \
    && sed -i '' -E 's|^([[:space:]]*version ")[0-9.]+(")|\18.8.8\2|' Formula/managed-machine.rb skills/SKILL.md \
    && sed -i '' -E 's|^([[:space:]]*tag:[[:space:]]*")v[0-9.]+(")|\1v8.8.8\2|' Formula/managed-machine.rb \
    && git add -A && git commit --quiet -m 'Release v8.8.8' && git tag v8.8.8)
(cd "$FIXTURE2" && /bin/bash "$ROOT/scripts/release" v8.8.8 >"$TEST_DIR/resume.out")
grep -Fq 'Resuming unpublished release v8.8.8' "$TEST_DIR/resume.out"
git --git-dir="$REMOTE2" rev-parse --verify --quiet refs/tags/v8.8.8 >/dev/null
[[ "$(git --git-dir="$REMOTE2" rev-parse main)" == "$(git -C "$FIXTURE2" rev-parse HEAD)" ]]

# 3. Malformed versions and dirty trees are rejected before any change.
if run_release 9.9.10 >"$TEST_DIR/badversion.out" 2>&1; then
    echo 'expected bad version format to fail' >&2
    exit 1
fi
grep -Fq 'must look like v1.2.3' "$TEST_DIR/badversion.out"

printf 'wip\n' >"$FIXTURE/dirty.txt"
if run_release v9.9.10 >"$TEST_DIR/dirty.out" 2>&1; then
    echo 'expected dirty working tree to fail' >&2
    exit 1
fi
grep -Fq 'working tree must be clean' "$TEST_DIR/dirty.out"
rm "$FIXTURE/dirty.txt"

echo 'release tests passed'
