---
name: managed-machine
description: "Bootstrap, update, and manage a Mac machine via the managed-machine Homebrew formula. USE FOR: fresh Mac setup, install managed-machine, run bootstrap, update machine, run a setup script, brew ownership fix, home-bin pin, fleet SSH keys, gitleaks hooks, Codex Meta config, Devin CLI install, LM Studio, Rust, Proton Pass. DO NOT USE FOR: editing dotfiles (use managed-machine-config), writing utility scripts (use home-bin), general Homebrew usage."
license: MIT
metadata:
  author: qwts
  version: "0.2.0"
---

# managed-machine Skill

Bootstrap and manage a macOS machine using the `managed-machine` Homebrew formula and its setup scripts.

## Install (fresh Mac)

```bash
curl -fsSL https://raw.githubusercontent.com/qwts/managed-machine/main/install.sh | bash
```

The installer checks brew ownership, taps `qwts/managed-machine`, installs the formula, and runs `managed-machine --bootstrap`.

If brew is installed but not owned by the current user, the installer fails with:
```
sudo chown -R $(whoami) $(brew --prefix)
```

## CLI

```bash
managed-machine              # full bootstrap (all setup-* in order)
managed-machine --bootstrap  # same
managed-machine --update     # brew update/upgrade + safe setup re-runs
managed-machine setup <name> # single setup script, e.g. setup-bin
managed-machine --help
```

## Setup scripts

All idempotent; safe to re-run.

| Script | Purpose |
|---|---|
| setup-brew | Install Homebrew if missing |
| setup-zsh | Starter zsh dotfiles (only if missing) |
| setup-git-hooks | gitleaks pre-commit for this repo |
| setup-gh | GitHub CLI, SSH key gen/upload, git signing, authorized_keys sync |
| setup-bin | Clone/pin home-bin, link tools into ~/.local/bin |
| setup-proton-pass | Proton Pass CLI |
| setup-codex | Codex with Meta Muse Spark config (no secrets) |
| setup-devin | Devin CLI |
| setup-lmstudio | LM Studio (Homebrew Cask) |
| setup-rust | rustup + cargo PATH |

## Dependencies

- Dotfiles/config sourced from `../managed-machine-config` (clone alongside)
- home-bin cloned at pinned ref from `managed-machine-config/home-bin.ref`
- No secrets in repo; auth/keys generated per machine or from macOS Keychain

## Update

```bash
managed-machine --update
```

Runs `brew update`, upgrades `managed-machine`, then re-runs `setup-bin` and `setup-gh`.

## Release

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
curl -sL https://github.com/qwts/managed-machine/archive/refs/tags/vX.Y.Z.tar.gz | shasum -a 256
# Update Formula/managed-machine.rb with version + sha256, commit, push
```
