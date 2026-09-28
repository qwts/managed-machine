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
      tag:   "v0.7.10"
  version "0.7.10"
  license "MIT"

  # Dotfiles/config repo. Installed as a read-only seed; managed-machine creates
  # the writable persistent checkout outside the Homebrew prefix. Pinned to a
  # tag+revision on that repo — scripts/release tags managed-machine-config at
  # the release version and rewrites both fields, so a release ships a known
  # catalog snapshot instead of whatever main is at install time.
  resource "managed-machine-config" do
    url "https://github.com/qwts/managed-machine-config.git",
        using:    :git,
        tag:      "v0.7.10",
        revision: "784a66628497f388103ff4f30d0d6f4f0ba718a0",
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

  # No depends_on — setup scripts handle their own deps (brew, gh, etc.)

  def install
    libexec.mkpath

    # Install managed-machine orchestration files into libexec
    libexec.install Dir["setup-*"]
    libexec.install "lib"
    (libexec / "scripts").install Dir["scripts/*"]
    (libexec / "git-hooks").install Dir["git-hooks/*"]

    # Bundle a managed-machine-config seed and the local-bin working repo.
    (libexec / "managed-machine-config").mkpath
    resource("managed-machine-config").stage(libexec / "managed-machine-config")

    (libexec / "local-bin").mkpath
    resource("local-bin").stage(libexec / "local-bin")

    # ENG-0055 skill bundle: `managed-machine --version` reads VERSION and
    # `managed-machine skill path` reports the skill and the commit the tag
    # names. Tags before these files existed skip the block.
    if File.exist?("VERSION")
      libexec.install "VERSION"
      (libexec / "skills").install "skills/managed-machine"
      commit = Utils.safe_popen_read("git", "-C", buildpath, "rev-parse", "HEAD").strip
      (libexec / "RELEASE_COMMIT").write "#{commit.match?(/\A\h{40}\z/) ? commit : "unknown"}\n"
    end

    # Install the CLI entry point
    bin.install "bin/managed-machine"
  end

  test do
    # --help exercises libexec resolution end-to-end; the setup-name listing
    # only appears when the setup scripts actually landed in libexec, which
    # catches install/post_install layout breakage that a bare banner match
    # missed.
    help_output = shell_output("#{bin}/managed-machine --help")
    assert_match "managed-machine", help_output
    assert_match "status", help_output
    assert_match "adopt", help_output
    assert_match(/^  hostname$/, help_output)
    assert_match(/^  zsh$/, help_output)
    assert_match(/^  gh$/, help_output)

    # The failure path resolves libexec too and must name the bad input.
    invalid = shell_output("#{bin}/managed-machine setup no-such-setup 2>&1", 1)
    assert_match "unknown setup name", invalid
  end
end
