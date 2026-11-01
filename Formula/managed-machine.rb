# typed: false
# frozen_string_literal: true

# Managed-machine Homebrew formula.
# Self-tap: brew tap qwts/managed-machine https://github.com/qwts/managed-machine.git
# Then: brew install managed-machine
#
# All repository URLs are authenticated HTTPS (gh's git credential helper),
# never SSH: the formula and its resources are fetched before setup-gh has
# provisioned any SSH key.
#
# The formula installs the managed-machine orchestration files and bundles
# managed-machine-config as a read-only bootstrap seed and local-bin as a
# working git repo under libexec.
# Formula upgrades replace orchestration and bundled seeds. Persistent private
# config remains outside the Homebrew prefix and synchronizes through git.
#
# To release a new version:
#   1. Commit the code changes and update the :tag below.
#   2. Push the new tag.
#   3. Push the formula update.
class ManagedMachine < Formula
  desc "Fresh-Mac bootstrap and fleet setup orchestration"
  homepage "https://github.com/qwts/managed-machine"
  url "https://github.com/qwts/managed-machine.git",
      using: :git,
      tag:   "v0.11.3"
  version "0.11.3"
  license "MIT"

  depends_on arch: :arm64
  depends_on macos: :sequoia

  # Dotfiles/config repo. Installed as a read-only seed; managed-machine creates
  # the writable persistent checkout outside the Homebrew prefix. Pinned to a
  # tag+revision on that repo — scripts/release tags managed-machine-config at
  # the release version and rewrites both fields, so a release ships a known
  # catalog snapshot instead of whatever main is at install time.
  resource "managed-machine-config" do
    url "https://github.com/qwts/managed-machine-config.git",
        using:    :git,
        tag:      "v0.11.3",
        revision: "33c185503927616a50695dcc1e044c762715afab",
        shallow:  false
  end

  # Utility scripts repo. Installed as a working git clone so setup-bin can
  # pull new commands and keep the ~/.local/bin symlinks pointed here.
  resource "local-bin" do
    url "https://github.com/qwts/local-bin.git",
        using:   :git,
        branch:  "main",
        shallow: false
  end

  # Prebuilt portable Go core binary for darwin/arm64 (S6 R04, S8 D3).
  # Installed as bin/managed-machine; legacy Bash tree staged under libexec/legacy.
  resource "core" do
    url "https://github.com/qwts/managed-machine/releases/download/v0.11.3/managed-machine_0.11.3_darwin_arm64.tar.gz"
    sha256 "7d9745c8e5a27fb051bbac224baeeb09b9d0002685e5c063c885593470422cdc"
  end

  def install
    libexec.mkpath
    legacy = libexec / "legacy"
    legacy.mkpath

    # Install legacy macOS Bash orchestration files into libexec/legacy.
    (legacy / "bin").install "legacy/bin/managed-machine"
    legacy.install Dir["legacy/setup-*"]
    legacy.install "legacy/lib"
    (legacy / "scripts").install Dir["legacy/scripts/*"]
    (legacy / "git-hooks").install Dir["legacy/git-hooks/*"]

    # Bundle a managed-machine-config seed and the local-bin working repo under libexec/legacy.
    (legacy / "managed-machine-config").mkpath
    resource("managed-machine-config").stage(legacy / "managed-machine-config")

    (legacy / "local-bin").mkpath
    resource("local-bin").stage(legacy / "local-bin")

    # Transitional --update re-exec shim across the v0.10.x -> v0.11.0 layout
    # inversion (S8 D3(e)); removed in v0.12.0.
    (libexec / "scripts").mkpath
    (libexec / "scripts/update").write <<~SH
      #!/usr/bin/env bash
      exec "$(dirname "$0")/../legacy/scripts/update" "$@"
    SH
    chmod 0755, libexec / "scripts/update"

    # Install the compiled Go core binary as bin/managed-machine and expose the
    # agent-machine command alias.
    bin.mkpath
    resource("core").stage do
      bin.install "managed-machine"
      chmod 0755, bin / "managed-machine"
    end
    bin.install_symlink "managed-machine" => "agent-machine"

    # ENG-0055 skill bundle: `managed-machine --version` and `skill path`
    # resolve VERSION, skills/managed-machine and RELEASE_COMMIT from libexec.
    if File.exist?("VERSION")
      cp "VERSION", legacy / "VERSION"
      libexec.install "VERSION"
      (libexec / "skills").install "skills/managed-machine"
      commit = Utils.safe_popen_read("git", "-C", buildpath, "rev-parse", "HEAD").strip
      (libexec / "RELEASE_COMMIT").write "#{commit.match?(/\A\h{40}\z/) ? commit : "unknown"}\n"
    end
  end

  test do
    help_output = shell_output("#{bin}/managed-machine --help")
    assert_match "managed-machine", help_output
    assert_match "status", help_output
    assert_match "[legacy] adopt", help_output
    assert_predicate libexec/"legacy/setup-hostname", :executable?
    assert_predicate libexec/"scripts/update", :executable?

    # The failure path resolves libexec/legacy through the core exec seam and
    # must name the bad input.
    invalid = shell_output("#{bin}/managed-machine setup no-such-setup 2>&1", 1)
    assert_match "unknown setup name", invalid

    assert_equal version.to_s, shell_output("#{bin}/managed-machine --version").strip
    assert_equal version.to_s, shell_output("#{bin}/agent-machine --version").strip
    shell_output("#{bin}/managed-machine version")
  end
end
