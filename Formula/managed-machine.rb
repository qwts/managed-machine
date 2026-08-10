# typed: false
# frozen_string_literal: true

# Managed-machine Homebrew formula.
# Self-tap: brew tap qwts/managed-machine git@github.com:qwts/managed-machine.git
# Then: brew install managed-machine
#
# The formula installs the managed-machine orchestration files and bundles
# managed-machine-config and local-bin as working git repos under libexec.
# All symlinks created by the setup scripts point back into this brew-managed
# directory, so `brew update && brew upgrade managed-machine` updates the
# orchestration, dotfiles config, and utility scripts.
#
# To release a new version:
#   1. Commit the code changes and update the :tag below.
#   2. Push the new tag.
#   3. Push the formula update.
class ManagedMachine < Formula
  desc "Fresh-Mac bootstrap and fleet setup orchestration"
  homepage "https://github.com/qwts/managed-machine"
  url "git@github.com:qwts/managed-machine.git",
      using: :git,
      tag:   "v0.3.3"
  version "0.3.3"
  license "MIT"

  # Dotfiles/config repo. Installed as a working git clone so setup-gh can
  # append new machine SSH keys and the owner can commit/push.
  resource "managed-machine-config" do
    url "git@github.com:qwts/managed-machine-config.git",
        using:   :git,
        branch:  "main",
        shallow: false
  end

  # Utility scripts repo. Installed as a working git clone so setup-bin can
  # pull new commands and keep the ~/.local/bin symlinks pointed here.
  resource "local-bin" do
    url "git@github.com:qwts/local-bin.git",
        using:   :git,
        branch:  "main",
        shallow: false
  end

  # No depends_on — setup scripts handle their own deps (brew, gh, etc.)

  def install
    libexec.mkpath

    # Install managed-machine orchestration files into libexec
    %w[setup-brew setup-zsh setup-nvm setup-git-hooks setup-gh setup-bin
       setup-proton-pass setup-codex setup-devin setup-lmstudio setup-rust].each do |s|
      libexec.install s
    end
    libexec.install "lib"
    (libexec / "scripts").install Dir["scripts/*"]
    (libexec / "git-hooks").install Dir["git-hooks/*"]

    # Bundle managed-machine-config and local-bin as working git repos under libexec.
    (libexec / "managed-machine-config").mkpath
    resource("managed-machine-config").stage(libexec / "managed-machine-config")

    (libexec / "local-bin").mkpath
    resource("local-bin").stage(libexec / "local-bin")

    # Install the CLI entry point
    bin.install "bin/managed-machine"
  end

  test do
    assert_match "managed-machine", shell_output("#{bin}/managed-machine --help")
  end
end
