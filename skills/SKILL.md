---
name: managed-machine
description: "Bootstrap, update, and manage a Mac machine via the managed-machine Homebrew formula. USE FOR: fresh Mac setup, install managed-machine, run bootstrap, report installed versions with status, update machine, run a setup script, brew ownership fix, local-bin pin, fleet SSH keys, gitleaks hooks, Codex Meta config, Meta Muse Code, Devin CLI install, LM Studio, VS Code, Cursor, Claude app, Antigravity, Rust, Proton Pass. DO NOT USE FOR: editing dotfiles (use managed-machine-config), writing utility scripts (use local-bin), general Homebrew usage."
license: MIT
metadata:
  author: qwts
  version "0.3.5"
---

# managed-machine Skill

Bootstrap and manage a macOS machine using the `managed-machine` Homebrew formula and its setup scripts.

## Install (fresh Mac)

The repository is private; fetch the installer through an authenticated GitHub CLI (`gh auth login` first):

```bash
gh api -H "Accept: application/vnd.github.raw" repos/qwts/managed-machine/contents/install.sh | bash
```

The installer checks brew ownership, verifies gh authentication and wires gh as the git credential helper (private repos clone over HTTPS; no SSH key required before `setup-gh`), taps `qwts/managed-machine`, trusts the tap when Homebrew requires it, installs the formula, and runs `managed-machine --bootstrap`. If no terminal is available, prompt-dependent setup is deferred and reported instead of attempted.

If brew is installed but not owned by the current user, the installer fails with:
```
sudo chown -R $(whoami) $(brew --prefix)
```

## If the install fails

If the curl installer fails because `Formula/managed-machine.rb` has no released tag, do not clone or run from a local copy. Stop and ask the user to create a release first.

## CLI

```bash
managed-machine              # full bootstrap; terminal mode is auto-detected
managed-machine --bootstrap --interactive
managed-machine --bootstrap --non-interactive
managed-machine --update     # brew update/upgrade + safe setup re-runs
managed-machine status       # installed versions and pins (read-only)
managed-machine setup bin       # preferred: run setup-bin
managed-machine setup setup-bin # compatible explicit script-name form
managed-machine fleet list   # list registered machines
managed-machine fleet remove <machine-id> [--yes] [--revoke-github]
managed-machine --help
```

Setup accepts either a bare name such as `devin` or the full script name `setup-devin`. Invalid names print the available setup list.

## Setup scripts

All idempotent; safe to re-run.

Full bootstrap detects whether a controlling terminal is available before any setup step runs. Noninteractive mode defers steps that may require passphrases, browser authorization, SSH authentication, or administrator approval, continues independent work, and writes complete/deferred/skipped/failed outcomes to `~/.config/managed-machine/bootstrap.manifest`. Deferred steps are completed later with the reported `managed-machine setup <name>` command.

| Script | Purpose |
|---|---|
| setup-brew | Install Homebrew if missing |
| setup-zsh | Starter zsh dotfiles (only if missing) |
| setup-nvm | Install NVM and the current Node.js LTS release |
| setup-git-hooks | gitleaks pre-commit for this repo; composes with an existing hooksPath |
| setup-gh | GitHub CLI, passphrase-protected SSH key gen/upload, private fleet registration, git signing, authorized_keys sync |
| setup-bin | Keep local-bin at the pinned ref and link tools into ~/.local/bin |
| setup-proton-pass | Proton Pass CLI |
| setup-muse | Meta Muse Code (`muse` CLI) |
| setup-codex | Codex with Meta Muse Spark config (no secrets) |
| setup-devin | Devin CLI install plus interactive or deferred authentication |
| setup-lmstudio | LM Studio (Homebrew Cask; `~/Applications` fallback when `/Applications` needs admin, `MANAGED_MACHINE_LMSTUDIO_APPDIR` override) |
| setup-vscode | VS Code from official homebrew/cask only; verified Team ID |
| setup-cursor | Cursor from official homebrew/cask only; verified Team ID |
| setup-claude-app | Claude desktop app from official homebrew/cask only; verified Team ID |
| setup-antigravity-app | Antigravity hub from official homebrew/cask only; verified Team ID |
| setup-antigravity-ide | Antigravity IDE from official homebrew/cask only; verified Team ID |
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
scripts/release vX.Y.Z
```

Bumps `Formula/managed-machine.rb` (tag + version) and this skill's metadata version together, commits `Release vX.Y.Z`, tags, and pushes. Requires a clean working tree; refuses existing tags.
