#!/usr/bin/env bash
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TEST_DIR="$(mktemp -d)"
FIXTURE="$TEST_DIR/repo"
REMOTE="$TEST_DIR/origin.git"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

# Stub gh, brew, and ssh-keygen on PATH so no real external tool or network
# operation can ever run.
STUB_BIN="$TEST_DIR/bin"
mkdir -p "$STUB_BIN"
for tool in gh brew; do
    cat >"$STUB_BIN/$tool" <<EOF
#!/usr/bin/env bash
echo "$tool invoked unexpectedly: \$*" >"$TEST_DIR/$tool.called"
exit 99
EOF
    chmod +x "$STUB_BIN/$tool"
done

cat >"$STUB_BIN/ssh-keygen-signer" <<'EOF'
#!/usr/bin/env bash
# Stub gpg.ssh.program for git commit/tag SSH signing in tests.
if [[ "${1:-}" == "-Y" && "${2:-}" == "sign" ]]; then
    buf_file="${@: -1}"
    cat >"${buf_file}.sig" <<'SIG'
-----BEGIN SSH SIGNATURE-----
U1NIU0lHAAAAAQAAADMAAAALc3NoLWVkMjU1MTkAAAAgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
AAAAAAAAAAAAAA==
-----END SSH SIGNATURE-----
SIG
    exit 0
fi
exit 0
EOF
chmod +x "$STUB_BIN/ssh-keygen-signer"
export PATH="$STUB_BIN:$PATH"

# The real repo must not drift: the formula and VERSION name the same release.
FORMULA_VERSION="$(sed -nE 's/^[[:space:]]*version "([0-9.]+)"$/\1/p' "$ROOT/Formula/managed-machine.rb" | head -1)"
[[ -n "$FORMULA_VERSION" && "$FORMULA_VERSION" == "$(tr -d '[:space:]' <"$ROOT/VERSION")" ]]

# Seed a fixture with the real formula, VERSION, and skill. The fixture
# skill's range is widened to cover the test versions (7.x-9.x) so the range
# check passes; case 6 narrows it again.
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
SEED_MAIN_SHA="$(git -C "$FIXTURE" rev-parse HEAD)"

# Sibling managed-machine-config fixture and bare remote.
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

# 0. Bare `scripts/release vX.Y.Z` form is retired and exits 1 with usage.
if run_release v9.9.9 >"$TEST_DIR/bare.out" 2>&1; then
    fail "expected bare scripts/release v9.9.9 to exit 1"
fi
grep -Fq 'Usage: scripts/release <prepare|tag> vX.Y.Z' "$TEST_DIR/bare.out"

if run_release >"$TEST_DIR/noargs.out" 2>&1; then
    fail "expected scripts/release with no args to exit 1"
fi
grep -Fq 'Usage: scripts/release <prepare|tag> vX.Y.Z' "$TEST_DIR/noargs.out"

# 1. `prepare` refusal checks before any change: malformed version, dirty tree,
# non-main branch, main ahead or behind origin/main, and version outside
# qwts-versions.
if run_release prepare v9.9 >"$TEST_DIR/badversion.out" 2>&1; then
    fail "expected bad version format to fail"
fi
grep -Fq 'must look like v1.2.3' "$TEST_DIR/badversion.out"

printf 'wip\n' >"$FIXTURE/dirty.txt"
if run_release prepare v9.9.9 >"$TEST_DIR/dirty.out" 2>&1; then
    fail "expected dirty working tree to fail"
fi
grep -Fq 'working tree must be clean' "$TEST_DIR/dirty.out"
rm "$FIXTURE/dirty.txt"

git -C "$FIXTURE" checkout --quiet -b feature
if run_release prepare v9.9.9 >"$TEST_DIR/branch.out" 2>&1; then
    fail "expected prepare from a feature branch to fail"
fi
grep -Fq "releases are cut from main; this checkout is on 'feature'" "$TEST_DIR/branch.out"
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
git -C "$FIXTURE" checkout --quiet main
git -C "$FIXTURE" branch --quiet -D feature

git -C "$FIXTURE" commit --quiet --allow-empty -m 'unpushed'
if run_release prepare v9.9.9 >"$TEST_DIR/ahead.out" 2>&1; then
    fail "expected prepare from an unpushed main to fail"
fi
grep -Fq 'main is not at origin/main' "$TEST_DIR/ahead.out"
git -C "$FIXTURE" reset --quiet --hard origin/main

git -C "$FIXTURE" commit --quiet --allow-empty -m 'published-advance'
git -C "$FIXTURE" push --quiet origin main
ADVANCE_MAIN_SHA="$(git -C "$FIXTURE" rev-parse HEAD)"
git -C "$FIXTURE" reset --quiet --hard HEAD~1
if run_release prepare v9.9.9 >"$TEST_DIR/behind.out" 2>&1; then
    fail "expected prepare from a stale main to fail"
fi
grep -Fq 'main is not at origin/main' "$TEST_DIR/behind.out"
git -C "$FIXTURE" reset --quiet --hard origin/main
[[ "$(git -C "$FIXTURE" rev-parse HEAD)" == "$ADVANCE_MAIN_SHA" ]]

if run_release prepare v10.0.0 >"$TEST_DIR/range-pre.out" 2>&1; then
    fail "expected prepare outside qwts-versions to fail"
fi
grep -Fq "is outside the skill's qwts-versions (>=7.0.0 <10.0.0)" "$TEST_DIR/range-pre.out"

# 2. `scripts/release prepare v9.9.9`: creates release/v9.9.9, writes VERSION
# and Formula/managed-machine.rb (tag, version, arm64/sequoia, config tag+sha,
# core URL+sha256 from real mm-release build), tags managed-machine-config,
# commits "Release v9.9.9" on release/v9.9.9, pushes the branch only (origin
# main and tags of this repository unchanged), and prints the gh pr create line.
run_release prepare v9.9.9 >"$TEST_DIR/prepare.out"
[[ "$(git -C "$FIXTURE" rev-parse --abbrev-ref HEAD)" == 'release/v9.9.9' ]]
[[ "$(cat "$FIXTURE/VERSION")" == '9.9.9' ]]
grep -qE '^[[:space:]]*tag:[[:space:]]*"v9\.9\.9"' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*version "9\.9\.9"' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*depends_on arch: :arm64$' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*depends_on macos: :sequoia$' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'url "https://github.com/qwts/managed-machine/releases/download/v9.9.9/managed-machine_9.9.9_darwin_arm64.tar.gz"' "$FIXTURE/Formula/managed-machine.rb"
grep -qE '^[[:space:]]*sha256 "[0-9a-f]{64}"$' "$FIXTURE/Formula/managed-machine.rb"
grep -qE "^[[:space:]]*revision:[[:space:]]*\"$(config_sha)\"" "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'legacy = libexec / "legacy"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq '(legacy / "bin").install "legacy/bin/managed-machine"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'resource("managed-machine-config").stage(legacy / "managed-machine-config")' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'resource("local-bin").stage(legacy / "local-bin")' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq '(libexec / "scripts/update").write' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'exec "$(dirname "$0")/../legacy/scripts/update" "$@"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'chmod 0755, libexec / "scripts/update"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'bin.install "managed-machine"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'chmod 0755, bin / "managed-machine"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'cp "VERSION", legacy / "VERSION"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'assert_match "[legacy] adopt", help_output' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'assert_predicate libexec/"legacy/setup-hostname", :executable?' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'assert_predicate libexec/"scripts/update", :executable?' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'assert_equal version.to_s, shell_output("#{bin}/managed-machine --version").strip' "$FIXTURE/Formula/managed-machine.rb"
for leftover in \
    'if File.exist?("legacy/bin/managed-machine")' \
    'if (libexec / "legacy").exist?' \
    '(libexec / "core")' \
    '#{libexec}/core/managed-machine --version' \
    'bin.install "bin/managed-machine"'; do
    if grep -Fq "$leftover" "$FIXTURE/Formula/managed-machine.rb"; then
        fail "expected prepare to strip leftover '$leftover' from Formula/managed-machine.rb"
    fi
done
if grep -qE '^[[:space:]]*else$' "$FIXTURE/Formula/managed-machine.rb"; then
    fail "expected prepare to collapse dual-branch else block in Formula/managed-machine.rb"
fi
python3 - "$FIXTURE/Formula/managed-machine.rb" <<'PY'
import sys
text = open(sys.argv[1], encoding="utf-8").read()
if "\n  end\n\n  def install\n" not in text:
    raise SystemExit("blank line before def install was not preserved")
if "\n  end\n\n  test do\n" not in text:
    raise SystemExit("blank line before test do was not preserved")
PY
if command -v ruby >/dev/null 2>&1; then
    ruby -c "$FIXTURE/Formula/managed-machine.rb" >/dev/null
fi
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
[[ "$(git -C "$FIXTURE" log -1 --format=%s)" == 'Release v9.9.9' ]]
grep -Fq 'gh pr create' "$TEST_DIR/prepare.out"
grep -Fq 'release/v9.9.9' "$TEST_DIR/prepare.out"

# Branch release/v9.9.9 is pushed to origin; origin/main and tags of this repo are unchanged.
PREPARE_SHA="$(git -C "$FIXTURE" rev-parse HEAD)"
[[ "$(git --git-dir="$REMOTE" rev-parse refs/heads/release/v9.9.9)" == "$PREPARE_SHA" ]]
[[ "$(git --git-dir="$REMOTE" rev-parse refs/heads/main)" == "$ADVANCE_MAIN_SHA" ]]
if git --git-dir="$REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null; then
    fail "prepare must not push tag v9.9.9 to origin"
fi
if git -C "$FIXTURE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null; then
    fail "prepare must not create local tag v9.9.9"
fi

# Private config repo was tagged at v9.9.9 and pushed to config-origin.git.
git --git-dir="$CONFIG_REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null
[[ "$(git -C "$CONFIG_FIXTURE" rev-parse "refs/tags/v9.9.9^{commit}")" == "$(config_sha)" ]]

# 2b. A second run of `prepare v9.9.9` resumes idempotently (both from the
# release branch and from main, and when the remote branch push had not landed).
run_release prepare v9.9.9 >"$TEST_DIR/prepare2.out"
[[ "$(git -C "$FIXTURE" rev-parse HEAD)" == "$PREPARE_SHA" ]]
grep -Fq 'gh pr create' "$TEST_DIR/prepare2.out"

git --git-dir="$REMOTE" update-ref -d refs/heads/release/v9.9.9
git -C "$FIXTURE" checkout --quiet main
run_release prepare v9.9.9 >"$TEST_DIR/prepare3.out"
[[ "$(git -C "$FIXTURE" rev-parse --abbrev-ref HEAD)" == 'release/v9.9.9' ]]
[[ "$(git -C "$FIXTURE" rev-parse HEAD)" == "$PREPARE_SHA" ]]
[[ "$(git --git-dir="$REMOTE" rev-parse refs/heads/release/v9.9.9)" == "$PREPARE_SHA" ]]
[[ "$(git --git-dir="$REMOTE" rev-parse refs/heads/main)" == "$ADVANCE_MAIN_SHA" ]]
grep -Fq 'gh pr create' "$TEST_DIR/prepare3.out"

# 3. `scripts/release tag v9.9.9` refusal checks:
# 3a. Refuses when HEAD != origin/main (release/v9.9.9 is not merged to main yet).
if run_release tag v9.9.9 >"$TEST_DIR/tag-unmerged.out" 2>&1; then
    fail "expected tag before PR merge (HEAD != origin/main) to fail"
fi
grep -Fq 'origin/main' "$TEST_DIR/tag-unmerged.out"

# 3b. Refuses on main when VERSION != 9.9.9 (main still has the current VERSION before merge).
git -C "$FIXTURE" checkout --quiet main
if run_release tag v9.9.9 >"$TEST_DIR/tag-oldmain.out" 2>&1; then
    fail "expected tag on main with old VERSION to fail"
fi
grep -Fq "VERSION is '$(tr -d '[:space:]' <"$ROOT/VERSION")', expected '9.9.9'" "$TEST_DIR/tag-oldmain.out"

# Simulate merging the release PR to origin/main.
git -C "$FIXTURE" merge --quiet --ff-only release/v9.9.9
git -C "$FIXTURE" push --quiet origin main

# 3c. Refuses when local main is ahead or behind origin/main.
git -C "$FIXTURE" commit --quiet --allow-empty -m 'unpushed-after-merge'
if run_release tag v9.9.9 >"$TEST_DIR/tag-ahead.out" 2>&1; then
    fail "expected tag when main is ahead of origin/main to fail"
fi
grep -Fq 'not at origin/main' "$TEST_DIR/tag-ahead.out"
git -C "$FIXTURE" reset --quiet --hard origin/main

# 3d. Refuses when VERSION != X.Y.Z.
if run_release tag v9.9.8 >"$TEST_DIR/tag-wrongver.out" 2>&1; then
    fail "expected tag with mismatched version argument to fail"
fi
grep -Fq "VERSION is '9.9.9', expected '9.9.8'" "$TEST_DIR/tag-wrongver.out"

# 3e. Refuses when the formula disagrees with VERSION.
SAVED_MAIN_SHA="$(git -C "$FIXTURE" rev-parse HEAD)"
sed -i '' -E 's|^([[:space:]]*version ")9\.9\.9(")|\19.9.1\2|' "$FIXTURE/Formula/managed-machine.rb"
git -C "$FIXTURE" commit --quiet -am 'Break formula version'
git -C "$FIXTURE" push --quiet origin main
if run_release tag v9.9.9 >"$TEST_DIR/tag-badformula.out" 2>&1; then
    fail "expected tag with disagreeing formula to fail"
fi
grep -Fq 'Formula/managed-machine.rb disagrees with v9.9.9' "$TEST_DIR/tag-badformula.out"
git -C "$FIXTURE" reset --quiet --hard "$SAVED_MAIN_SHA"
git -C "$FIXTURE" push --quiet --force origin main

# 3f. Refuses when the private origin lacks tag v9.9.9 or points at another revision.
git --git-dir="$CONFIG_REMOTE" update-ref -d refs/tags/v9.9.9
if run_release tag v9.9.9 >"$TEST_DIR/tag-noconfigtag.out" 2>&1; then
    fail "expected tag when private origin lacks tag v9.9.9 to fail"
fi
grep -Fq 'managed-machine-config tag v9.9.9 was not found on origin' "$TEST_DIR/tag-noconfigtag.out"

git -C "$CONFIG_FIXTURE" commit --quiet --allow-empty -m 'other-config-commit'
WRONG_CONFIG_SHA="$(git -C "$CONFIG_FIXTURE" rev-parse HEAD)"
git -C "$CONFIG_FIXTURE" push --quiet --force origin "$WRONG_CONFIG_SHA:refs/tags/v9.9.9"
git -C "$CONFIG_FIXTURE" reset --quiet --hard HEAD~1
if run_release tag v9.9.9 >"$TEST_DIR/tag-wrongconfigtag.out" 2>&1; then
    fail "expected tag when private origin tag points at wrong revision to fail"
fi
grep -Fq 'managed-machine-config tag v9.9.9 on origin points at' "$TEST_DIR/tag-wrongconfigtag.out"

# Restore the valid config tag on private origin.
git -C "$CONFIG_FIXTURE" push --quiet --force origin "refs/tags/v9.9.9"

# 4. `scripts/release tag v9.9.9` creates the annotated tag at HEAD and pushes
# exactly refs/tags/v9.9.9.
MAIN_BEFORE_TAG="$(git --git-dir="$REMOTE" rev-parse refs/heads/main)"
run_release tag v9.9.9 >"$TEST_DIR/tag.out"
grep -Fq 'Released v9.9.9' "$TEST_DIR/tag.out"
git --git-dir="$REMOTE" rev-parse --verify --quiet refs/tags/v9.9.9 >/dev/null
[[ "$(git --git-dir="$REMOTE" rev-parse "refs/tags/v9.9.9^{commit}")" == "$MAIN_BEFORE_TAG" ]]
[[ "$(git --git-dir="$REMOTE" rev-parse refs/heads/main)" == "$MAIN_BEFORE_TAG" ]]
[[ "$(git -C "$FIXTURE" cat-file -t v9.9.9)" == 'tag' ]]
[[ "$(git -C "$FIXTURE" tag -l --format='%(contents:subject)' v9.9.9)" == 'Release v9.9.9' ]]
git -C "$FIXTURE" show v9.9.9:Formula/managed-machine.rb | grep -qE '"v9\.9\.9"'

# 4b. Re-running `tag v9.9.9` or `prepare v9.9.9` refuses when the tag exists.
if run_release tag v9.9.9 >"$TEST_DIR/tag-dup.out" 2>&1; then
    fail "expected duplicate tag v9.9.9 to fail"
fi
grep -Fq 'tag v9.9.9 already exists' "$TEST_DIR/tag-dup.out"

if run_release prepare v9.9.9 >"$TEST_DIR/prepare-dup.out" 2>&1; then
    fail "expected prepare of already-tagged v9.9.9 to fail"
fi
grep -Fq 'tag v9.9.9 already exists' "$TEST_DIR/prepare-dup.out"

# 5. `tag` with tag.gpgSign enabled signs the annotated tag using the stub SSH
# signer without invoking real ssh-keygen or opening an editor.
FIXTURE_SIGNED="$TEST_DIR/repo-signed"
REMOTE_SIGNED="$TEST_DIR/origin-signed.git"
printf 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA release-test\n' >"$TEST_DIR/tagkey.pub"
git init --quiet --bare --initial-branch=main "$REMOTE_SIGNED"
git init --quiet "$FIXTURE_SIGNED"
git -C "$FIXTURE_SIGNED" config user.name 'managed-machine test'
git -C "$FIXTURE_SIGNED" config user.email 'managed-machine-test@example.invalid'
git -C "$FIXTURE_SIGNED" config gpg.format ssh
git -C "$FIXTURE_SIGNED" config gpg.ssh.program "$STUB_BIN/ssh-keygen-signer"
git -C "$FIXTURE_SIGNED" config user.signingkey "$TEST_DIR/tagkey.pub"
git -C "$FIXTURE_SIGNED" config commit.gpgsign true
git -C "$FIXTURE_SIGNED" config tag.gpgsign true
seed_fixture "$FIXTURE_SIGNED"
git -C "$FIXTURE_SIGNED" add . && git -C "$FIXTURE_SIGNED" commit --quiet -m seed
git -C "$FIXTURE_SIGNED" branch -M main
git -C "$FIXTURE_SIGNED" remote add origin "$REMOTE_SIGNED"
git -C "$FIXTURE_SIGNED" push --quiet -u origin main

(cd "$FIXTURE_SIGNED" && MANAGED_MACHINE_CONFIG_DIR="$CONFIG_FIXTURE" /bin/bash "$ROOT/scripts/release" prepare v7.7.7 >"$TEST_DIR/signed-prepare.out" 2>&1) || {
    echo 'expected prepare to succeed with commit.gpgsign enabled' >&2
    cat "$TEST_DIR/signed-prepare.out" >&2
    exit 1
}
git -C "$FIXTURE_SIGNED" checkout --quiet main
git -C "$FIXTURE_SIGNED" merge --quiet --ff-only release/v7.7.7
git -C "$FIXTURE_SIGNED" push --quiet origin main
(cd "$FIXTURE_SIGNED" && MANAGED_MACHINE_CONFIG_DIR="$CONFIG_FIXTURE" /bin/bash "$ROOT/scripts/release" tag v7.7.7 >"$TEST_DIR/signed-tag.out" 2>&1) || {
    echo 'expected tag to succeed with tag.gpgsign enabled' >&2
    cat "$TEST_DIR/signed-tag.out" >&2
    exit 1
}
git --git-dir="$REMOTE_SIGNED" rev-parse --verify --quiet refs/tags/v7.7.7 >/dev/null
[[ "$(git -C "$FIXTURE_SIGNED" cat-file -t v7.7.7)" == 'tag' ]]
git -C "$FIXTURE_SIGNED" cat-file tag v7.7.7 | grep -Fq 'BEGIN SSH SIGNATURE'

# 6. A second release on the same repository (v9.9.11) rewrites an S6 D4
# (`v0.10.0`-layout) formula to the S8 D3 install block, `tag` refuses if the
# formula's install block is reverted to the old `libexec/core` layout, and a
# version outside a narrowed qwts-versions is refused.
cat >"$FIXTURE/Formula/managed-machine.rb" <<EOF
class ManagedMachine < Formula
  desc "Fresh-Mac bootstrap and fleet setup orchestration"
  homepage "https://github.com/qwts/managed-machine"
  url "https://github.com/qwts/managed-machine.git",
      using: :git,
      tag:   "v9.9.9"
  version "9.9.9"
  license "MIT"

  depends_on arch: :arm64
  depends_on macos: :sequoia

  resource "managed-machine-config" do
    url "https://github.com/qwts/managed-machine-config.git",
        using:    :git,
        tag:      "v9.9.9",
        revision: "$(config_sha)",
        shallow:  false
  end

  resource "local-bin" do
    url "https://github.com/qwts/local-bin.git",
        using:   :git,
        branch:  "main",
        shallow: false
  end

  # Prebuilt portable Go core binary for darwin/arm64 (S6 R04, decisions D3/D4).
  # Staged to libexec/core/managed-machine beside the Bash dispatcher.
  resource "core" do
    url "https://github.com/qwts/managed-machine/releases/download/v9.9.9/managed-machine_9.9.9_darwin_arm64.tar.gz"
    sha256 "1d0e6f77108b25bd67e2011955dc420c0b14a46b4add134a6576dcce1a0b3baa"
  end

  def install
    libexec.mkpath
    libexec.install Dir["setup-*"]
    libexec.install "lib"
    (libexec / "scripts").install Dir["scripts/*"]
    (libexec / "git-hooks").install Dir["git-hooks/*"]
    (libexec / "managed-machine-config").mkpath
    resource("managed-machine-config").stage(libexec / "managed-machine-config")
    (libexec / "local-bin").mkpath
    resource("local-bin").stage(libexec / "local-bin")
    (libexec / "core").mkpath
    resource("core").stage do
      (libexec / "core").install "managed-machine"
      chmod 0755, libexec / "core/managed-machine"
    end
    bin.install "bin/managed-machine"
  end

  test do
    help_output = shell_output("#{bin}/managed-machine --help")
    assert_match "managed-machine", help_output
    assert_predicate libexec/"setup-hostname", :executable?
    invalid = shell_output("#{bin}/managed-machine setup no-such-setup 2>&1", 1)
    assert_match "unknown setup name", invalid
    assert_equal version.to_s, shell_output("#{libexec}/core/managed-machine --version").strip
    shell_output("#{bin}/managed-machine version")
  end
end
EOF
git -C "$FIXTURE" commit --quiet -am 'Seed S6 D4 formula layout'
git -C "$FIXTURE" push --quiet origin main

run_release prepare v9.9.11 >"$TEST_DIR/prepare-911.out"
grep -Fq '# Installed as bin/managed-machine; legacy Bash tree staged under libexec/legacy.' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq '(legacy / "bin").install "legacy/bin/managed-machine"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'exec "$(dirname "$0")/../legacy/scripts/update" "$@"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'bin.install "managed-machine"' "$FIXTURE/Formula/managed-machine.rb"
grep -Fq 'assert_equal version.to_s, shell_output("#{bin}/managed-machine --version").strip' "$FIXTURE/Formula/managed-machine.rb"
if grep -Fq '(libexec / "core").install' "$FIXTURE/Formula/managed-machine.rb"; then
    fail "expected prepare to replace S6 D4 libexec/core install block with D3 block"
fi
if grep -Fq '#{libexec}/core/managed-machine --version' "$FIXTURE/Formula/managed-machine.rb"; then
    fail "expected prepare to replace S6 D4 libexec/core --version test line with D3 line"
fi
git -C "$FIXTURE" checkout --quiet main
git -C "$FIXTURE" merge --quiet --ff-only release/v9.9.11
git -C "$FIXTURE" push --quiet origin main

SAVED_911_SHA="$(git -C "$FIXTURE" rev-parse HEAD)"
for leftover_line in \
    '(libexec / "core").mkpath' \
    'assert_equal version.to_s, shell_output("#{libexec}/core/managed-machine --version").strip' \
    'bin.install "bin/managed-machine"' \
    'if File.exist?("legacy/bin/managed-machine")'; do
    git -C "$FIXTURE" reset --quiet --hard "$SAVED_911_SHA"
    python3 - "$FIXTURE/Formula/managed-machine.rb" "$leftover_line" <<'PY'
import sys
path, extra = sys.argv[1], sys.argv[2]
text = open(path, encoding="utf-8").read()
text = text.replace("    libexec.mkpath\n", f"    libexec.mkpath\n    # {extra}\n", 1)
open(path, "w", encoding="utf-8").write(text)
PY
    git -C "$FIXTURE" commit --quiet -am "Inject leftover line: $leftover_line"
    git -C "$FIXTURE" push --quiet --force origin main
    if run_release tag v9.9.11 >"$TEST_DIR/tag-badd3.out" 2>&1; then
        fail "expected tag with leftover line '$leftover_line' to fail"
    fi
    grep -Fq 'Formula/managed-machine.rb disagrees with v9.9.11' "$TEST_DIR/tag-badd3.out"
done
git -C "$FIXTURE" reset --quiet --hard "$SAVED_911_SHA"
git -C "$FIXTURE" push --quiet --force origin main

run_release tag v9.9.11 >"$TEST_DIR/tag-911.out"
grep -Fq 'Released v9.9.11' "$TEST_DIR/tag-911.out"

sed -i '' -E 's|^([[:space:]]*qwts-versions:[[:space:]]*)"[^"]*"|\1">=9.9.0 <9.10.0"|' "$FIXTURE/skills/managed-machine/SKILL.md"
git -C "$FIXTURE" commit --quiet -am 'Narrow the skill range'
git -C "$FIXTURE" push --quiet origin main
if run_release prepare v9.10.0 >"$TEST_DIR/range.out" 2>&1; then
    fail "expected prepare outside narrowed qwts-versions to fail"
fi
grep -Fq "is outside the skill's qwts-versions (>=9.9.0 <9.10.0)" "$TEST_DIR/range.out"
[[ -z "$(git -C "$FIXTURE" status --porcelain)" ]]
[[ "$(cat "$FIXTURE/VERSION")" == '9.9.11' ]]
if git -C "$FIXTURE" rev-parse --verify --quiet refs/tags/v9.10.0 >/dev/null; then
    fail "tag v9.10.0 must not exist"
fi

# 7. Neither subcommand ever invoked gh or brew.
if [[ -f "$TEST_DIR/gh.called" || -f "$TEST_DIR/brew.called" ]]; then
    fail "gh or brew was unexpectedly invoked"
fi

echo 'release tests passed'
