---
name: managed-machine
description: "Bootstrap, update, and manage a Mac machine via the managed-machine Homebrew formula. USE FOR: fresh Mac setup, install managed-machine, run bootstrap, update machine, run a setup script, brew ownership fix, local-bin pin, fleet SSH keys, gitleaks hooks, Codex Meta config, Devin CLI install, LM Studio, Rust, Proton Pass. DO NOT USE FOR: editing dotfiles (use managed-machine-config), writing utility scripts (use local-bin), general Homebrew usage."
license: MIT
metadata:
  author: qwts
  version "0.3.3"
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

## If the install fails

If the curl installer fails because `Formula/managed-machine.rb` has no released tag, do not clone or run from a local copy. Stop and ask the user to create a release first.

## CLI

```bash
managed-machine              # full bootstrap (all setup-* in order)
managed-machine --bootstrap  # same
managed-machine --update     # brew update/upgrade + safe setup re-runs
managed-machine setup bin       # preferred: run setup-bin
managed-machine setup setup-bin # compatible explicit script-name form
managed-machine fleet list   # list registered machines
managed-machine fleet remove <machine-id> [--yes] [--revoke-github]
managed-machine --help
```

Setup accepts either a bare name such as `devin` or the full script name `setup-devin`. Invalid names print the available setup list.

## Setup scripts

All idempotent; safe to re-run.

| Script | Purpose |
|---|---|
| setup-brew | Install Homebrew if missing |
| setup-zsh | Starter zsh dotfiles (only if missing) |
| setup-nvm | Install NVM and the current Node.js LTS release |
| setup-git-hooks | gitleaks pre-commit for this repo |
| setup-gh | GitHub CLI, passphrase-protected SSH key gen/upload, private fleet registration, git signing, authorized_keys sync |
| setup-bin | Keep local-bin at the pinned ref and link tools into ~/.local/bin |
| setup-proton-pass | Proton Pass CLI |
| setup-codex | Codex with Meta Muse Spark config (no secrets) |
| setup-devin | Devin CLI |
| setup-lmstudio | LM Studio (Homebrew Cask) |
| setup-rust | rustup + cargo PATH |

## Dependencies

- Dotfiles/config sourced from a persistent private checkout under `$XDG_DATA_HOME/managed-machine/` when set, or `~/.local/share/managed-machine/` otherwise; the brew-bundled copy is read-only seed data
- local-bin kept at the pinned ref from `managed-machine-config/local-bin.ref`
- No secrets in repo; auth/keys generated per machine or from macOS Keychain

## Fleet

New GitHub SSH keys require a usable terminal and a non-empty passphrase; on macOS the encrypted key is added to Keychain. If no terminal is available, stop and have the user run `managed-machine setup gh` interactively. Never silently choose an empty passphrase. The explicit override `MANAGED_MACHINE_ALLOW_EMPTY_SSH_PASSPHRASE=1 managed-machine setup gh` creates an unencrypted key and records that opt-in locally in `~/.config/managed-machine/ssh-key-policy.toml`.

`setup-gh` creates local `~/.config/managed-machine/machine.toml` state and a versioned machine entry in the persistent private `managed-machine-config/fleet/machines/` registry. It imports legacy `authorized_keys` records before generating the fleet key file, so existing hosts are preserved. Managed fleet paths are committed and pushed automatically; unrelated private-config edits are never staged.

Use `managed-machine fleet list` to inspect registered machines. To decommission one, pass the exact machine ID to `managed-machine fleet remove`; add `--yes` for noninteractive confirmation and `--revoke-github` only when the matching authentication/signing keys should also be deleted from the active GitHub account. Successful registration and removal synchronize the private config repository automatically.

## Update

```bash
managed-machine --update
```

Runs `brew update`, upgrades `managed-machine`, then re-runs `setup-gh` and `setup-bin` so the private checkout is synchronized before its local-bin pin is consumed.

## Release

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
# Update Formula/managed-machine.rb with the new version, commit, push
```
