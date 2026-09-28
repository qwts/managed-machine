#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
FIXTURE="$TEST_DIR/repo"
REMOTE="$TEST_DIR/origin.git"
trap 'rm -rf "$TEST_DIR"' EXIT

# The real repo must not drift: the formula and VERSION name the same release.
FORMULA_VERSION="$(sed -nE 's/^[[:space:]]*version "([0-9.]+)"$/\1/p' "$ROOT/Formula/managed-machine.rb" | head -1)"
[[ -n "$FORMULA_VERSION" && "$FORMULA_VERSION" == "$(tr -d '[:space:]' <"$ROOT/VERSION")" ]]

# Seed a fixture with the real formula, VERSION, and skill. The fixture
# skill's range is widened to cover the test versions (7.x-9.x) so the range
# check passes; case 5 narrows it again.
seed_fixture() {
    mkdir -p "$1/Formula" "$1/skills/managed-machine"
    cp "$ROOT/Formula/managed-machine.rb" "$1/Formula/"
    cp "$ROOT/VERSION" "$1/"
    sed -E 's|^([[:space:]]*qwts-versions:[[:space:]]*)"[^"]*"|\1">=7.0.0 <10.0.0"|' \
        "$ROOT/skills/managed-machine/SKILL.md" >"$1/skills/managed-machine/SKILL.md"
}

# Fixture pushing to a local bare remote.
git init --quiet --bare --initial-branch=main "$REMOTE"
git init --quiet "$FIXTURE"
git -C "$FIXTURE" config user.name 'managed-machine test'
git -C "$FIXTURE" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE" config commit.gpgsign false
git -C "$FIXTURE" config tag.gpgsign false
seed_fixture "$FIXTURE"
git -C "$FIXTURE" add . && git -C "$FIXTURE" commit --quiet -m seed
git -C "$FIXTURE" branch -M main
git -C "$FIXTURE" remote add origin "$REMOTE"
git -C "$FIXTURE" push --quiet -u origin main

# The release tags and pins the sibling managed-machine-config checkout at
# the same version: the fixture sits at the default sibling path so every
# repo fixture below resolves it.
CONFIG_FIXTURE="$TEST_DIR/managed-machine-config"
CONFIG_REMOTE="$TEST_DIR/config-origin.git"
git init --quiet --bare --initial-branch=main "$CONFIG_REMOTE"
git init --quiet "$CONFIG_FIXTURE"
git -C "$CONFIG_FIXTURE" config user.name 'managed-machine test'
git -C "$CONFIG_FIXTURE" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_FIXTURE" config commit.gpgsign false
git -C "$CONFIG_FIXTURE" config tag.gpgsign false
printf '{}\n' >"$CONFIG_FIXTURE/apps.json"
git -C "$CONFIG_FIXTURE" add . && git -C "$CONFIG_FIXTURE" commit --quiet -m seed
git -C "$CONFIG_FIXTURE" branch -M main
git -C "$CONFIG_FIXTURE" remote add origin "$CONFIG_REMOTE"
git -C "$CONFIG_FIXTURE" push --quiet -u origin main
config_sha() { git -C "$CONFIG_FIXTURE" rev-parse HEAD; }

run_release() {
    (cd "$FIXTURE" && /bin/bash "$ROOT/scripts/release" "$@")
}

# 1. A release bumps formula tag+version and VERSION together, commits,
# tags, and pushes both.
run_release v9.9.9 >"$TEST_DIR/release.out"
grep -qE '^[[:space:]]*tag:[[:space:]]*"v9\.9\.9"' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*version "9\.9\.9"' "$FIXTURE/Formula/managed-machine.rb"
[[ "$(cat "$FIXTURE/VERSION")" == '9.9.9' ]]
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
git --git-dir="$REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null
[[ "$(git --git-dir="$REMOTE" rev-parse main)" == "$(git -C "$FIXTURE" rev-parse HEAD)" ]]
[[ "$(git -C "$FIXTURE" log -1 --format=%s)" == 'Release v9.9.9' ]]

# The tagged commit contains the formula pointing at its own tag.
git -C "$FIXTURE" show v9.9.9:Formula/managed-machine.rb | grep -qE '"v9\.9\.9"'

# The release tagged managed-machine-config at the same version and pinned
# the formula resource to that commit.
git --git-dir="$CONFIG_REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null
[[ "$(git -C "$CONFIG_FIXTURE" rev-parse "refs/tags/v9.9.9^{commit}")" == "$(config_sha)" ]]
grep -qE "^[[:space:]]*revision:[[:space:]]*\"$(config_sha)\"" "$FIXTURE/Formula/managed-machine.rb"
grep -qE "^[[:space:]]*tag:[[:space:]]*\"v9\.9\.9\"" "$FIXTURE/Formula/managed-machine.rb"

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
git init --quiet --bare --initial-branch=main "$REMOTE_SIGNED"
git init --quiet "$FIXTURE_SIGNED"
git -C "$FIXTURE_SIGNED" config user.name 'managed-machine test'
git -C "$FIXTURE_SIGNED" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE_SIGNED" config gpg.format ssh
git -C "$FIXTURE_SIGNED" config user.signingkey "$TEST_DIR/tagkey.pub"
git -C "$FIXTURE_SIGNED" config commit.gpgsign true
git -C "$FIXTURE_SIGNED" config tag.gpgsign true
seed_fixture "$FIXTURE_SIGNED"
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
git init --quiet --bare --initial-branch=main "$REMOTE2"
git clone --quiet "$REMOTE" "$FIXTURE2" 2>/dev/null || {
    git init --quiet "$FIXTURE2"
    seed_fixture "$FIXTURE2"
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
    && sed -i '' -E 's|^([[:space:]]*version ")[0-9.]+(")|\18.8.8\2|' Formula/managed-machine.rb \
    && printf '8.8.8\n' >VERSION \
    && sed -i '' -E 's|^([[:space:]]*tag:[[:space:]]*")v[0-9.]+(")|\1v8.8.8\2|' Formula/managed-machine.rb \
    && sed -i '' -E "s|revision:[[:space:]]*\"[0-9a-f]{40}\"|revision: \"$(config_sha)\"|" Formula/managed-machine.rb \
    && git add -A && git commit --quiet -m 'Release v8.8.8' && git tag v8.8.8)
(cd "$FIXTURE2" && /bin/bash "$ROOT/scripts/release" v8.8.8 >"$TEST_DIR/resume.out")
grep -Fq 'Resuming unpublished release v8.8.8' "$TEST_DIR/resume.out"
git --git-dir="$REMOTE2" rev-parse --verify --quiet refs/tags/v8.8.8 >/dev/null
[[ "$(git --git-dir="$REMOTE2" rev-parse main)" == "$(git -C "$FIXTURE2" rev-parse HEAD)" ]]
# Resume publishes the config tag the pinned revision names.
git --git-dir="$CONFIG_REMOTE" rev-parse --verify --quiet refs/tags/v8.8.8 >/dev/null

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

# 4. A release is cut from main as published on origin: a feature branch is
# refused, and so is a main that is behind or ahead of origin, before any
# file is touched or tag created.
git -C "$FIXTURE" checkout --quiet -b feature
if run_release v9.9.11 >"$TEST_DIR/branch.out" 2>&1; then
    echo 'expected a release from a feature branch to fail' >&2
    exit 1
fi
grep -Fq "releases are cut from main; this checkout is on 'feature'" "$TEST_DIR/branch.out"
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
! git -C "$FIXTURE" rev-parse --verify --quiet refs/tags/v9.9.11 >/dev/null
git -C "$FIXTURE" checkout --quiet main
git -C "$FIXTURE" branch --quiet -D feature

git -C "$FIXTURE" commit --quiet --allow-empty -m 'unpushed'
if run_release v9.9.11 >"$TEST_DIR/ahead.out" 2>&1; then
    echo 'expected a release from an unpushed main to fail' >&2
    exit 1
fi
grep -Fq 'main is not at origin/main' "$TEST_DIR/ahead.out"
! git -C "$FIXTURE" rev-parse --verify --quiet refs/tags/v9.9.11 >/dev/null
git -C "$FIXTURE" reset --quiet --hard origin/main

git -C "$FIXTURE" reset --quiet --hard HEAD~1
if run_release v9.9.11 >"$TEST_DIR/behind.out" 2>&1; then
    echo 'expected a release from a stale main to fail' >&2
    exit 1
fi
grep -Fq 'main is not at origin/main' "$TEST_DIR/behind.out"
git -C "$FIXTURE" reset --quiet --hard origin/main
run_release v9.9.11 >"$TEST_DIR/current.out" 2>&1
grep -Fq 'Released v9.9.11' "$TEST_DIR/current.out"

# 5. A release outside the skill's qwts-versions is refused before any file
# is touched or tag created: the skill must be revalidated first.
sed -i '' -E 's|^([[:space:]]*qwts-versions:[[:space:]]*)"[^"]*"|\1">=9.9.0 <9.10.0"|' "$FIXTURE/skills/managed-machine/SKILL.md"
git -C "$FIXTURE" commit --quiet -am 'Narrow the skill range'
git -C "$FIXTURE" push --quiet origin main
if run_release v9.10.0 >"$TEST_DIR/range.out" 2>&1; then
    echo 'expected a release outside qwts-versions to fail' >&2
    exit 1
fi
grep -Fq "is outside the skill's qwts-versions (>=9.9.0 <9.10.0)" "$TEST_DIR/range.out"
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
[[ "$(cat "$FIXTURE/VERSION")" == '9.9.11' ]]
! git -C "$FIXTURE" rev-parse --verify --quiet refs/tags/v9.10.0 >/dev/null

echo 'release tests passed'
