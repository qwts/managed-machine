#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
FIXTURE="$TEST_DIR/repo"
REMOTE="$TEST_DIR/origin.git"
trap 'rm -rf "$TEST_DIR"' EXIT

# The real repo must not drift: formula and skill carry the same version.
FORMULA_VERSION="$(sed -nE 's/^[[:space:]]*version "([0-9.]+)"$/\1/p' "$ROOT/Formula/managed-machine.rb" | head -1)"
SKILL_VERSION="$(sed -nE 's/^[[:space:]]*version "([0-9.]+)"$/\1/p' "$ROOT/skills/managed-machine/SKILL.md" | head -1)"
[[ -n "$FORMULA_VERSION" && "$FORMULA_VERSION" == "$SKILL_VERSION" ]]

# Fixture clone with the real formula and skill files, pushing to a local
# bare remote.
git init --quiet --bare "$REMOTE"
git init --quiet "$FIXTURE"
git -C "$FIXTURE" config user.name 'managed-machine test'
git -C "$FIXTURE" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE" config commit.gpgsign false
git -C "$FIXTURE" config tag.gpgsign false
mkdir -p "$FIXTURE/Formula" "$FIXTURE/skills/managed-machine"
cp "$ROOT/Formula/managed-machine.rb" "$FIXTURE/Formula/"
cp "$ROOT/skills/managed-machine/SKILL.md" "$FIXTURE/skills/managed-machine/"
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
grep -qE '^[[:space:]]*version "9\.9\.9"' "$FIXTURE/skills/managed-machine/SKILL.md"
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
git --git-dir="$REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null
[[ "$(git --git-dir="$REMOTE" rev-parse main)" == "$(git -C "$FIXTURE" rev-parse HEAD)" ]]
[[ "$(git -C "$FIXTURE" log -1 --format=%s)" == 'Release v9.9.9' ]]

# The tagged commit contains the formula pointing at its own tag.
git -C "$FIXTURE" show v9.9.9:Formula/managed-machine.rb | grep -qE '"v9\.9\.9"'

# 1b. The tag is annotated and carries the release message. A lightweight tag
# would abort the release outright wherever tag.gpgSign is set (see 1c).
[[ "$(git -C "$FIXTURE" cat-file -t v9.9.9)" == 'tag' ]]
[[ "$(git -C "$FIXTURE" tag -l --format='%(contents:subject)' v9.9.9)" == 'Release v9.9.9' ]]

# 1c. setup-gh sets tag.gpgSign globally, so a release run on a machine this
# repo provisioned signs its tag — and git refuses to write a signed tag with
# no message. Release under that exact configuration.
FIXTURE_SIGNED="$TEST_DIR/repo-signed"
REMOTE_SIGNED="$TEST_DIR/origin-signed.git"
ssh-keygen -q -t ed25519 -N '' -C 'release test' -f "$TEST_DIR/tagkey"
git init --quiet --bare "$REMOTE_SIGNED"
git init --quiet "$FIXTURE_SIGNED"
git -C "$FIXTURE_SIGNED" config user.name 'managed-machine test'
git -C "$FIXTURE_SIGNED" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE_SIGNED" config gpg.format ssh
git -C "$FIXTURE_SIGNED" config user.signingkey "$TEST_DIR/tagkey.pub"
git -C "$FIXTURE_SIGNED" config commit.gpgsign true
git -C "$FIXTURE_SIGNED" config tag.gpgsign true
mkdir -p "$FIXTURE_SIGNED/Formula" "$FIXTURE_SIGNED/skills/managed-machine"
cp "$ROOT/Formula/managed-machine.rb" "$FIXTURE_SIGNED/Formula/"
cp "$ROOT/skills/managed-machine/SKILL.md" "$FIXTURE_SIGNED/skills/managed-machine/"
git -C "$FIXTURE_SIGNED" add . && git -C "$FIXTURE_SIGNED" commit --quiet -m seed
git -C "$FIXTURE_SIGNED" branch -M main
git -C "$FIXTURE_SIGNED" remote add origin "$REMOTE_SIGNED"
git -C "$FIXTURE_SIGNED" push --quiet -u origin main

(cd "$FIXTURE_SIGNED" && /bin/bash "$ROOT/scripts/release" v7.7.7 >"$TEST_DIR/signed.out" 2>&1) || {
    echo 'expected a release to succeed with tag.gpgsign enabled' >&2
    cat "$TEST_DIR/signed.out" >&2
    exit 1
}
git --git-dir="$REMOTE_SIGNED" rev-parse --verify --quiet refs/tags/v7.7.7 >/dev/null
[[ "$(git -C "$FIXTURE_SIGNED" cat-file -t v7.7.7)" == 'tag' ]]
git -C "$FIXTURE_SIGNED" cat-file tag v7.7.7 | grep -Fq 'BEGIN SSH SIGNATURE'

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
    mkdir -p "$FIXTURE2/Formula" "$FIXTURE2/skills/managed-machine"
    cp "$ROOT/Formula/managed-machine.rb" "$FIXTURE2/Formula/"
    cp "$ROOT/skills/managed-machine/SKILL.md" "$FIXTURE2/skills/managed-machine/"
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
    && sed -i '' -E 's|^([[:space:]]*version ")[0-9.]+(")|\18.8.8\2|' Formula/managed-machine.rb skills/managed-machine/SKILL.md \
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
