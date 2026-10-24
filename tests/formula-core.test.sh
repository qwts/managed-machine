#!/usr/bin/env bash
# S6 R04/R05 (D3, D4, D11): `scripts/release prepare v<VERSION>` on a temp copy
# of the repository writes Formula/managed-machine.rb with arm64 + macOS 15
# Sequoia constraints, pins the darwin_arm64 core archive resource by URL and
# sha256, stages it to libexec/core/managed-machine (mode 0755), keeps the two
# private resources and every installed path unchanged, asserts the core and
# dispatcher version checks in test do, and passes mm-release verify
# -require-pin against a local build.
set -euo pipefail
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
VERSION="$(tr -d '[:space:]' <"$ROOT/VERSION")"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

fail() {
    echo "FAIL: $*" >&2
    exit 1
}

[[ -n "$VERSION" ]] || fail "VERSION is empty"
[[ -f "$ROOT/Formula/managed-machine.rb" ]] || fail "Formula/managed-machine.rb is missing"

# Stub gh and brew so neither can ever run against the developer machine.
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
export PATH="$STUB_BIN:$PATH"

# Seed a temp copy of the repository and a temp sibling managed-machine-config
# repository with local bare remotes.
REMOTE="$TEST_DIR/origin.git"
REPO="$TEST_DIR/repo"
CONFIG_REMOTE="$TEST_DIR/config-origin.git"
CONFIG_REPO="$TEST_DIR/managed-machine-config"

git init --quiet --bare --initial-branch=main "$REMOTE"
git init --quiet "$REPO"
git -C "$REPO" config user.name 'managed-machine test'
git -C "$REPO" config user.email 'managed-machine-test@example.invalid'
git -C "$REPO" config commit.gpgsign false
git -C "$REPO" config tag.gpgsign false
mkdir -p "$REPO/Formula" "$REPO/skills/managed-machine"
cp "$ROOT/Formula/managed-machine.rb" "$REPO/Formula/"
cp "$ROOT/VERSION" "$REPO/"
cp "$ROOT/skills/managed-machine/SKILL.md" "$REPO/skills/managed-machine/"
git -C "$REPO" add . && git -C "$REPO" commit --quiet -m seed
git -C "$REPO" branch -M main
git -C "$REPO" remote add origin "$REMOTE"
git -C "$REPO" push --quiet -u origin main

git init --quiet --bare --initial-branch=main "$CONFIG_REMOTE"
git init --quiet "$CONFIG_REPO"
git -C "$CONFIG_REPO" config user.name 'managed-machine test'
git -C "$CONFIG_REPO" config user.email 'managed-machine-test@example.invalid'
git -C "$CONFIG_REPO" config commit.gpgsign false
git -C "$CONFIG_REPO" config tag.gpgsign false
printf '{}\n' >"$CONFIG_REPO/apps.json"
git -C "$CONFIG_REPO" add . && git -C "$CONFIG_REPO" commit --quiet -m seed
git -C "$CONFIG_REPO" branch -M main
git -C "$CONFIG_REPO" remote add origin "$CONFIG_REMOTE"
git -C "$CONFIG_REPO" push --quiet -u origin main

(cd "$REPO" && MANAGED_MACHINE_CONFIG_DIR="$CONFIG_REPO" /bin/bash "$ROOT/scripts/release" prepare "$VERSION" >"$TEST_DIR/prepare.out")

FORMULA="$REPO/Formula/managed-machine.rb"
[[ -f "$FORMULA" ]] || fail "prepared Formula/managed-machine.rb is missing"

if command -v ruby >/dev/null 2>&1; then
    ruby -c "$FORMULA" >/dev/null
fi

# Parse the prepared Formula/managed-machine.rb and assert all D4 requirements.
python3 - "$FORMULA" "$VERSION" <<'PY'
import re
import sys

formula_path, version = sys.argv[1], sys.argv[2]
with open(formula_path, encoding="utf-8") as fh:
    text = fh.read()
lines = text.splitlines()

def require_line(pattern: str, desc: str) -> None:
    if not re.search(pattern, text, re.MULTILINE):
        raise SystemExit(f"missing {desc} ({pattern}) in {formula_path}")

# 1. Platform constraints (arm64 and macOS 15 Sequoia).
require_line(r"^\s*depends_on\s+arch:\s*:arm64\s*$", "depends_on arch: :arm64")
require_line(r"^\s*depends_on\s+macos:\s*:sequoia\s*$", "depends_on macos: :sequoia")

# 2. Top-level tag and version match VERSION.
require_line(rf'^\s*tag:\s*"v{re.escape(version)}"\s*$', f'tag: "v{version}"')
require_line(rf'^\s*version\s+"{re.escape(version)}"\s*$', f'version "{version}"')

# 3. Parse resource blocks.
resources: dict[str, str] = {}
current_name = None
current_lines: list[str] = []
for line in lines:
    m = re.match(r'^\s*resource\s+"([^"]+)"\s+do\s*$', line)
    if m:
        current_name = m.group(1)
        current_lines = []
        continue
    if current_name is not None:
        if re.match(r"^\s*end\s*$", line):
            resources[current_name] = "\n".join(current_lines)
            current_name = None
            continue
        current_lines.append(line)

for required_res in ("managed-machine-config", "local-bin", "core"):
    if required_res not in resources:
        raise SystemExit(f'missing resource "{required_res}" in {formula_path}')

# Private resources stay unchanged.
cfg_block = resources["managed-machine-config"]
if 'url "https://github.com/qwts/managed-machine-config.git"' not in cfg_block:
    raise SystemExit("managed-machine-config url changed")
if f'tag:      "v{version}"' not in cfg_block:
    raise SystemExit("managed-machine-config tag missing")
if not re.search(r'revision:\s*"[0-9a-f]{40}"', cfg_block):
    raise SystemExit("managed-machine-config revision missing")

lb_block = resources["local-bin"]
if 'url "https://github.com/qwts/local-bin.git"' not in lb_block:
    raise SystemExit("local-bin url changed")
if 'branch:  "main"' not in lb_block:
    raise SystemExit("local-bin branch changed")

# 4. Core resource URL names v<VERSION> and managed-machine_<VERSION>_darwin_arm64.tar.gz,
#    and pins a 64-hex sha256.
core_block = resources["core"]
url_match = re.search(r'^\s*url\s+"([^"]+)"\s*$', core_block, re.MULTILINE)
if not url_match:
    raise SystemExit('resource "core" has no url')
core_url = url_match.group(1)
want_archive = f"managed-machine_{version}_darwin_arm64.tar.gz"
want_url = f"https://github.com/qwts/managed-machine/releases/download/v{version}/{want_archive}"
if f"/v{version}/" not in core_url:
    raise SystemExit(f'core resource url {core_url!r} does not name v{version}')
if not core_url.endswith("/" + want_archive):
    raise SystemExit(f'core resource url {core_url!r} does not name {want_archive}')
if core_url != want_url:
    raise SystemExit(f'core resource url = {core_url!r}, want {want_url!r}')

sha_match = re.search(r'^\s*sha256\s+"([0-9a-f]{64})"\s*$', core_block, re.MULTILINE)
if not sha_match:
    raise SystemExit('resource "core" has no valid 64-hex sha256')

# 5. Every existing installed path is preserved, and core is staged to
#    libexec/core/managed-machine with mode 0755.
for snippet in (
    'libexec.install Dir["setup-*"]',
    'libexec.install "lib"',
    '(libexec / "scripts").install Dir["scripts/*"]',
    '(libexec / "git-hooks").install Dir["git-hooks/*"]',
    '(libexec / "managed-machine-config").mkpath',
    'resource("managed-machine-config").stage(libexec / "managed-machine-config")',
    '(libexec / "local-bin").mkpath',
    'resource("local-bin").stage(libexec / "local-bin")',
    '(libexec / "core").mkpath',
    'resource("core").stage',
    '(libexec / "core").install "managed-machine"',
    'chmod 0755, libexec / "core/managed-machine"',
    'libexec.install "VERSION"',
    '(libexec / "skills").install "skills/managed-machine"',
    '(libexec / "RELEASE_COMMIT").write',
    'bin.install "bin/managed-machine"',
):
    if snippet not in text:
        raise SystemExit(f"install block missing {snippet!r}")

# 6. test do requires #{libexec}/core/managed-machine --version to equal version
#    and managed-machine version through bin/managed-machine to exit 0.
test_match = re.search(r"^\s*test do\s*$(.*?)^\s*end\s*$", text, re.MULTILINE | re.DOTALL)
if not test_match:
    raise SystemExit("missing test do block")
test_block = test_match.group(1)
if '#{libexec}/core/managed-machine --version' not in test_block:
    raise SystemExit("test do missing #{libexec}/core/managed-machine --version check")
if "assert_equal version.to_s" not in test_block:
    raise SystemExit("test do missing assert_equal version.to_s on core --version")
if '#{bin}/managed-machine version' not in test_block:
    raise SystemExit("test do missing #{bin}/managed-machine version check")
PY

# Build locally with mm-release and verify -require-pin passes when the pin is right.
DIST_DIR="$TEST_DIR/dist"
(cd "$ROOT" && go run ./cmd/mm-release build -version "$VERSION" -out "$DIST_DIR")
(cd "$ROOT" && go run ./cmd/mm-release verify -dist "$DIST_DIR" -formula "$FORMULA" -require-pin)

# Verify -require-pin fails when the pin disagrees.
BAD_FORMULA="$TEST_DIR/bad-managed-machine.rb"
sed -E '/resource "core"/,/^  end/ s/sha256 "[0-9a-f]{64}"/sha256 "0000000000000000000000000000000000000000000000000000000000000000"/' \
    "$FORMULA" >"$BAD_FORMULA"
if (cd "$ROOT" && go run ./cmd/mm-release verify -dist "$DIST_DIR" -formula "$BAD_FORMULA" -require-pin >/dev/null 2>&1); then
    fail "expected mm-release verify -require-pin to fail on wrong core sha256"
fi

if [[ -f "$TEST_DIR/gh.called" || -f "$TEST_DIR/brew.called" ]]; then
    fail "gh or brew was unexpectedly invoked"
fi

echo 'formula core tests passed'
