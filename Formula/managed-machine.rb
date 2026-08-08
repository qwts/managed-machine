# typed: false
# frozen_string_literal: true

# Managed-machine Homebrew formula.
# Self-tap: brew tap qwts/managed-machine https://github.com/qwts/managed-machine
# Then: brew install managed-machine
#
# Version + sha256 are pinned to a git tag tarball. To release a new version:
#   1. Tag the commit: git tag vX.Y.Z
#   2. Push the tag: git push origin vX.Y.Z
#   3. Compute sha256:
#      curl -sL https://github.com/qwts/managed-machine/archive/refs/tags/vX.Y.Z.tar.gz | shasum -a 256
#   4. Update version + sha256 below, commit, and push.
class ManagedMachine < Formula
  desc "Fresh-Mac bootstrap and fleet setup orchestration"
  homepage "https://github.com/qwts/managed-machine"
  url "https://github.com/qwts/managed-machine/archive/refs/tags/v0.2.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  version "0.2.0"
  license "MIT"

  # No depends_on — setup scripts handle their own deps (brew, gh, etc.)

  def install
    libexec.mkpath
    # Install all setup scripts, lib, scripts, git-hooks into libexec
    %w[setup-brew setup-zsh setup-git-hooks setup-gh setup-bin
       setup-proton-pass setup-codex setup-devin setup-lmstudio setup-rust].each do |s|
      (libexec / s).install s
    end
    libexec.install "lib"
    libexec.install Dir["scripts/*"] => "scripts"
    libexec.install Dir["git-hooks/*"] => "git-hooks"

    # Install the CLI entry point
    bin.install "bin/managed-machine"
  end

  test do
    assert_match "managed-machine", shell_output("#{bin}/managed-machine --help")
  end
end
