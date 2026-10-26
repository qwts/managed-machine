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

# Parse a Formula/managed-machine.rb (either the transitional dual-branch
# formula or a prepared collapsed D3-only formula) and assert all D4/S8 D3
# requirements.
verify_formula_structure() {
    local target_formula="$1" target_version="$2"
    python3 - "$target_formula" "$target_version" <<'PY'
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

# 5. The install block is either the collapsed D3-only layout (produced by
#    `scripts/release prepare`) or the transitional P01 dual-branch layout
#    guarded by `if File.exist?("legacy/bin/managed-machine") ... else ... end`.
install_match = re.search(
    r'^\s*def install\s*$(.*?)^\s*test do\s*$', text, re.MULTILINE | re.DOTALL
)
if not install_match:
    raise SystemExit("missing def install block")
install_block = install_match.group(1)
has_install_guard = 'if File.exist?("legacy/bin/managed-machine")' in install_block
if has_install_guard:
    if "\n    else\n" not in install_block:
        raise SystemExit('def install missing else branch for if File.exist?("legacy/bin/managed-machine")')
    inverted_install, pre_p01_install = install_block.split("\n    else\n", 1)
else:
    if "\n    else\n" in install_block:
        raise SystemExit("collapsed D3 install block must not have an else branch")
    for forbidden in ('(libexec / "core")', 'bin.install "bin/managed-machine"'):
        if forbidden in install_block:
            raise SystemExit(f"collapsed D3 install block still contains {forbidden!r}")
    inverted_install = install_block
    pre_p01_install = None

for snippet in (
    'legacy = libexec / "legacy"',
    'legacy.mkpath',
    '(legacy / "bin").install "legacy/bin/managed-machine"',
    'legacy.install Dir["legacy/setup-*"]',
    'legacy.install "legacy/lib"',
    '(legacy / "scripts").install Dir["legacy/scripts/*"]',
    '(legacy / "git-hooks").install Dir["legacy/git-hooks/*"]',
    '(legacy / "managed-machine-config").mkpath',
    'resource("managed-machine-config").stage(legacy / "managed-machine-config")',
    '(legacy / "local-bin").mkpath',
    'resource("local-bin").stage(legacy / "local-bin")',
    '(libexec / "scripts").mkpath',
    '(libexec / "scripts/update").write',
    'exec "$(dirname "$0")/../legacy/scripts/update" "$@"',
    'chmod 0755, libexec / "scripts/update"',
    'bin.mkpath',
    'resource("core").stage',
    'bin.install "managed-machine"',
    'chmod 0755, bin / "managed-machine"',
    'cp "VERSION", legacy / "VERSION"',
    'libexec.install "VERSION"',
    '(libexec / "skills").install "skills/managed-machine"',
    '(libexec / "RELEASE_COMMIT").write',
):
    if snippet not in inverted_install:
        raise SystemExit(f"inverted install branch missing {snippet!r}")

if pre_p01_install is not None:
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
        if snippet not in pre_p01_install:
            raise SystemExit(f"pre-P01 install branch missing {snippet!r}")

# 6. test do is either the collapsed D3-only block or branches on
#    `if (libexec / "legacy").exist?` while the guard exists.
test_match = re.search(r"^\s*test do\s*$(.*?)^\s*end\s*\Z", text, re.MULTILINE | re.DOTALL)
if not test_match:
    raise SystemExit("missing test do block")
test_block = test_match.group(1)
if 'if (libexec / "legacy").exist?' in test_block:
    test_branch_match = re.search(
        r'if \(libexec / "legacy"\)\.exist\?(.*?)^\s*else\s*$(.*?)^\s*end\s*$',
        test_block,
        re.MULTILINE | re.DOTALL,
    )
    if not test_branch_match:
        raise SystemExit('test do missing if (libexec / "legacy").exist? ... else ... end')
    inverted_test, pre_p01_test = test_branch_match.group(1), test_branch_match.group(2)
else:
    if "\n    else\n" in test_block:
        raise SystemExit("collapsed D3 test do block must not have an else branch")
    inverted_test = test_block
    pre_p01_test = None

for want_common in (
    'assert_match "managed-machine", help_output',
    'assert_match "status", help_output',
    'assert_match "[legacy] adopt", help_output',
):
    if want_common not in test_block:
        raise SystemExit(f"test do missing common assertion {want_common!r}")

for want_inverted in (
    'assert_predicate libexec/"legacy/setup-hostname", :executable?',
    'assert_predicate libexec/"scripts/update", :executable?',
    'invalid = shell_output("#{bin}/managed-machine setup no-such-setup 2>&1", 1)',
    'assert_match "unknown setup name", invalid',
    'assert_equal version.to_s, shell_output("#{bin}/managed-machine --version").strip',
    'shell_output("#{bin}/managed-machine version")',
):
    if want_inverted not in inverted_test:
        raise SystemExit(f"inverted test do branch missing {want_inverted!r}")
if '#{libexec}/core/managed-machine' in inverted_test:
    raise SystemExit("inverted test do branch still references #{libexec}/core/managed-machine")

if pre_p01_test is not None:
    for want_pre_p01 in (
        'assert_predicate libexec/"setup-hostname", :executable?',
        'invalid = shell_output("#{bin}/managed-machine setup no-such-setup 2>&1", 1)',
        'assert_match "unknown setup name", invalid',
        'assert_equal version.to_s, shell_output("#{libexec}/core/managed-machine --version").strip',
        'shell_output("#{bin}/managed-machine version")',
    ):
        if want_pre_p01 not in pre_p01_test:
            raise SystemExit(f"pre-P01 test do branch missing {want_pre_p01!r}")
PY
}

verify_formula_structure "$ROOT/Formula/managed-machine.rb" "$VERSION"
verify_formula_structure "$FORMULA" "$VERSION"

# Exercise `def install` and `test do` in Ruby for both the repository's
# current formula (covering both branches while the guard exists) and the
# prepared collapsed D3-only formula.
if command -v ruby >/dev/null 2>&1; then
    CORE_BIN="$TEST_DIR/core-managed-machine"
    (cd "$ROOT" && go build -ldflags "-X main.version=$VERSION" -o "$CORE_BIN" ./cmd/managed-machine)

    for target_formula in "$ROOT/Formula/managed-machine.rb" "$FORMULA"; do
        ruby - "$target_formula" "$ROOT" "$TEST_DIR" "$CORE_BIN" "$VERSION" "$CONFIG_REPO" <<'RB'
require "fileutils"
require "open3"
require "pathname"
require "tmpdir"

formula_path, repo_root, test_dir, core_bin, version_str, config_repo = ARGV
formula_src = File.read(formula_path)
has_guard = formula_src.include?('if File.exist?("legacy/bin/managed-machine")')
ENV["HOME"] = File.join(test_dir, "home")
ENV["CONFIG_REPO_ROOT"] = config_repo
ENV["GIT_TERMINAL_PROMPT"] = "0"
ENV["PATH"] = "/usr/bin:/bin"
FileUtils.mkdir_p(ENV["HOME"])

class Pathname
  def install(*sources)
    mkpath
    sources.flatten.each do |src|
      src_path = Pathname.new(src.to_s)
      dst_path = self / src_path.basename
      FileUtils.rm_rf(dst_path.to_s)
      FileUtils.cp_r(src_path.to_s, dst_path.to_s)
    end
  end
end

module Utils
  def self.safe_popen_read(*cmd)
    out, _status = Open3.capture2(*cmd.map(&:to_s))
    out
  end
end

class StubResource
  def initialize(name, core_bin)
    @name = name
    @core_bin = core_bin
  end

  def stage(target = nil)
    if target
      Pathname.new(target.to_s).mkpath
      File.write((Pathname.new(target.to_s) / ".staged").to_s, @name)
    else
      Dir.mktmpdir("core-stage") do |dir|
        FileUtils.cp(@core_bin, File.join(dir, "managed-machine"))
        Dir.chdir(dir) { yield }
      end
    end
  end
end

class Formula
  class << self
    attr_accessor :test_block, :version_val
    def desc(*); end
    def homepage(*); end
    def url(*); end
    def version(v = nil)
      @version_val = v if v
      @version_val
    end
    def license(*); end
    def depends_on(*); end
    def resource(*); end
    def test(&blk)
      @test_block = blk
    end
  end

  attr_reader :libexec, :bin, :buildpath

  def initialize(prefix_dir, build_dir, core_bin, ver)
    @libexec = Pathname.new(prefix_dir) / "libexec"
    @bin = Pathname.new(prefix_dir) / "bin"
    @buildpath = Pathname.new(build_dir)
    @core_bin = core_bin
    @ver = ver
  end

  def version
    @ver
  end

  def resource(name)
    StubResource.new(name, @core_bin)
  end

  def cp(src, dst)
    FileUtils.cp(src.to_s, dst.to_s)
  end

  def chmod(mode, path)
    File.chmod(mode, path.to_s)
  end

  def shell_output(cmd, want_status = 0)
    out, status = Open3.capture2("/bin/bash", "-c", cmd)
    unless status.exitstatus == want_status
      raise "shell_output(#{cmd.inspect}) exited #{status.exitstatus}, expected #{want_status}: #{out}"
    end
    out
  end

  def assert_match(pattern, text)
    ok = pattern.is_a?(Regexp) ? pattern.match?(text) : text.include?(pattern.to_s)
    raise "assert_match(#{pattern.inspect}) failed on #{text.inspect}" unless ok
  end

  def assert_predicate(path, pred)
    raise "assert_predicate(#{path}, #{pred}) failed" unless path.public_send(pred)
  end

  def assert_equal(expected, actual)
    raise "assert_equal failed: expected #{expected.inspect}, got #{actual.inspect}" unless expected == actual
  end
end

load formula_path

# Branch 1: post-P01 layout (`legacy/bin/managed-machine` exists).
post_build = Dir.mktmpdir("build-post-p01-", test_dir)
post_prefix = Dir.mktmpdir("prefix-post-p01-", test_dir)
FileUtils.cp_r(File.join(repo_root, "legacy"), File.join(post_build, "legacy"))
FileUtils.cp(File.join(repo_root, "VERSION"), File.join(post_build, "VERSION"))
FileUtils.mkdir_p(File.join(post_build, "skills"))
FileUtils.cp_r(File.join(repo_root, "skills", "managed-machine"), File.join(post_build, "skills", "managed-machine"))
system("git", "-C", post_build, "init", "--quiet")
system("git", "-C", post_build, "config", "user.name", "test")
system("git", "-C", post_build, "config", "user.email", "test@example.invalid")
system("git", "-C", post_build, "config", "commit.gpgsign", "false")
File.write(File.join(post_build, ".seed"), "1\n")
system("git", "-C", post_build, "add", ".seed")
system("git", "-C", post_build, "commit", "--quiet", "-m", "seed")

Dir.chdir(post_build) do
  f = ManagedMachine.new(post_prefix, post_build, core_bin, version_str)
  f.install
  raise "expected libexec/legacy to exist in post-P01 install" unless (f.libexec / "legacy").exist?
  raise "expected libexec/legacy/bin/managed-machine to be executable" unless (f.libexec / "legacy/bin/managed-machine").executable?
  raise "expected libexec/scripts/update shim to be executable" unless (f.libexec / "scripts/update").executable?
  f.instance_eval(&ManagedMachine.test_block)
end

# Branch 2: pre-P01 layout (`bin/managed-machine` at root, no `legacy/`),
# exercised while the formula carries the transitional guard.
if has_guard
  pre_build = Dir.mktmpdir("build-pre-p01-", test_dir)
  pre_prefix = Dir.mktmpdir("prefix-pre-p01-", test_dir)
  FileUtils.mkdir_p(File.join(pre_build, "bin"))
  pre_bin = File.join(pre_build, "bin", "managed-machine")
  File.write(pre_bin, <<~'SH')
    #!/usr/bin/env bash
    set -euo pipefail
    libexec="$(cd "$(dirname "$0")/../libexec" && pwd)"
    case "${1:-}" in
      --help|-h|help|version|--version|-v)
        exec "$libexec/core/managed-machine" "$@"
        ;;
      setup)
        name="${2:-}"
        if [[ -f "$libexec/setup-$name" ]]; then
          exec "$libexec/setup-$name" "${@:3}"
        fi
        echo "Error: unknown setup name '$name'" >&2
        exit 1
        ;;
      *)
        exec "$libexec/core/managed-machine" "$@"
        ;;
    esac
  SH
  File.chmod(0o755, pre_bin)
  Dir[File.join(repo_root, "legacy", "setup-*")].each do |p|
    FileUtils.cp(p, File.join(pre_build, File.basename(p)))
  end
  FileUtils.cp_r(File.join(repo_root, "legacy", "lib"), File.join(pre_build, "lib"))
  FileUtils.cp_r(File.join(repo_root, "legacy", "scripts"), File.join(pre_build, "scripts"))
  FileUtils.cp_r(File.join(repo_root, "legacy", "git-hooks"), File.join(pre_build, "git-hooks"))
  FileUtils.cp(File.join(repo_root, "VERSION"), File.join(pre_build, "VERSION"))
  FileUtils.mkdir_p(File.join(pre_build, "skills"))
  FileUtils.cp_r(File.join(repo_root, "skills", "managed-machine"), File.join(pre_build, "skills", "managed-machine"))
  system("git", "-C", pre_build, "init", "--quiet")
  system("git", "-C", pre_build, "config", "user.name", "test")
  system("git", "-C", pre_build, "config", "user.email", "test@example.invalid")
  system("git", "-C", pre_build, "config", "commit.gpgsign", "false")
  File.write(File.join(pre_build, ".seed"), "1\n")
  system("git", "-C", pre_build, "add", ".seed")
  system("git", "-C", pre_build, "commit", "--quiet", "-m", "seed")

  Dir.chdir(pre_build) do
    f = ManagedMachine.new(pre_prefix, pre_build, core_bin, version_str)
    f.install
    raise "expected libexec/legacy NOT to exist in pre-P01 install" if (f.libexec / "legacy").exist?
    raise "expected libexec/core/managed-machine to be executable in pre-P01 install" unless (f.libexec / "core/managed-machine").executable?
    raise "expected libexec/setup-hostname to be executable in pre-P01 install" unless (f.libexec / "setup-hostname").executable?
    f.instance_eval(&ManagedMachine.test_block)
  end
end
RB
    done
fi

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
