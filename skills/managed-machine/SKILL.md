---
name: managed-machine
description: "Bootstrap, update, and manage a Mac machine via the managed-machine Homebrew formula. USE FOR: fresh Mac setup, install managed-machine, run bootstrap, report installed versions with status, update machine, run a setup script, adopt vendor-installed desktop apps, brew ownership fix, local-bin pin, fleet SSH keys, gitleaks hooks, Codex Meta config, Meta Muse Code, Claude Code, Codex CLI, Antigravity CLI, Grok Build, Aider, Droid CLI, Goose CLI, OpenCode, OpenCode Desktop, Devin CLI install, LM Studio, VS Code, Cursor, Claude app, Antigravity, Rust, Proton Pass. DO NOT USE FOR: editing dotfiles (use managed-machine-config), writing utility scripts (use local-bin), general Homebrew usage."
license: MIT
metadata:
  author: qwts
  version "0.7.4"
---

# managed-machine Skill

Bootstrap and manage a macOS machine using the `managed-machine` Homebrew formula and its setup scripts.

## Install (fresh Mac)

The repository is private; fetch the installer through an authenticated GitHub CLI (`gh auth login` first):

```bash
gh api -H "Accept: application/vnd.github.raw" repos/qwts/managed-machine/contents/install.sh | bash
```

The installer checks brew ownership, verifies gh authentication and wires gh as the git credential helper (private repos clone over HTTPS; no SSH key is required or created — SSH enrollment is the explicit `managed-machine ssh enroll` step), taps `qwts/managed-machine`, trusts the tap when Homebrew requires it, installs the formula, and runs `managed-machine --bootstrap`. Administrator dialogs during that run are part of the install. Steps that cannot finish are skipped, not failed, and do not assign a follow-up command.

If brew is installed but owned by another admin-group user (typically `admin`),
the installer leaves that ownership in place and runs mutating brew commands as
that owner through the macOS authorization dialog. It never `chown`s the prefix
to a non-admin invoking user.

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
managed-machine setup agent-bot-gh # explicit Codex desktop Homebrew gh interposition
managed-machine setup agent-bot-gh --restore # restore stock Homebrew gh
managed-machine adopt           # adopt vendor-installed desktop apps into Homebrew
managed-machine adopt vscode    # one app (cask token or alias)
managed-machine fleet list   # list registered machines
managed-machine fleet remove <machine-id> [--yes] [--revoke-github]
managed-machine ssh enroll --authentication|--signing|--fleet   # explicit human-only SSH enrollment
managed-machine ssh status   # this account's SSH enrollment state (read-only)
managed-machine --help
```

Setup accepts either a bare name such as `devin` or the full script name `setup-devin`. `managed-machine setup` with no name — like `--help` or an invalid name — prints the available setup list with a ✓ beside names already installed (cask receipt plus bundle, Team ID-verified vendor apps, formula receipts, or the CLI command on PATH).

`managed-machine adopt` takes over vendor-installed signed-cask apps (`antigravity`/`antigravity-app`, `antigravity-ide`, `brave-browser`/`brave`, `chatgpt`/`chatgpt-app`, `claude`/`claude-app`, `cursor`, `devin-desktop`/`devin-app`, `discord`, `docker-desktop`/`docker`, `google-chrome`/`chrome`, `grok-bot`/`grokbot`, `kiro`, `kiro-cli`, `lm-studio`/`lmstudio`, `opencode-desktop`/`opencode-app`, `slack`, `telegram`, `visual-studio-code`/`vscode`, `warp`, `zcode`) without mutating a running agent. Unknown names print that token/alias list. Skip (do not fail the run) when the app has a Homebrew receipt, is running, is missing, or fails Team ID verification. `setup-*` also skips a vendor occupier instead of failing the install.

## Setup scripts

All idempotent; safe to re-run.

A `signed-cask` row always requires a `Developer ID Application` signature whose Team ID matches the row. Bundle integrity passes on `codesign --verify --deep --strict` or, when extraction detritus makes that fail (Chromium-based apps such as Brave), on a Gatekeeper assessment reporting `source=Notarized Developer ID` for the same Team ID. A row with `"allow_rolling_url": true` accepts `sha256 :no_check` for vendors serving one rolling URL (Google Chrome) and must then pass that Gatekeeper notarization check, since no checksum stands behind it; every other refusal still applies.

A `vendor-dmg` row installs a desktop app with no `homebrew/cask` token by fetching the vendor DMG directly (pinned `https` URL on an allowlisted host, or per-arch `url_arm64`/`url_x86_64` pairs; matching DMG `sha256`, or `"no_check"` behind `allow_rolling_url` with mandatory notarization). The staged bundle is signature-verified before anything under `/Applications` moves; a bundle already on disk must first prove its Team ID, and a pinned `version` converges drift. `adopt` stays cask-only: vendor-DMG occupiers converge in place.

Full bootstrap detects whether a controlling terminal is available before any setup step runs. Interactive runs present the macOS administrator dialog when a step needs it. A step that cannot finish in this run is skipped and is not part of the install; it does not fail bootstrap and does not print a follow-up command. Outcomes are written to `~/.config/managed-machine/bootstrap.manifest`. Catalog rows in `apps.json` install on bootstrap and `--update` unless they set `"auto": false`; those names stay available as `managed-machine setup <name>` only. `brew-formula` rows install official `homebrew/core` formulae.

| Script | Purpose |
|---|---|
| setup-brew | Install Homebrew if missing |
| setup-hostname | Prompt for a Mac hostname and set LocalHostName, ComputerName, and HostName via scutil |
| setup-zsh | Starter zsh dotfiles; backs up stale unguarded/vendor PATH profiles and rewrites guarded blocks |
| setup-nvm | Install NVM and the current Node.js LTS release |
| setup-git-hooks | gitleaks pre-commit for this repo; composes with an existing hooksPath |
| setup-gh | GitHub CLI install + HTTPS auth, git credential helper, git identity, pull-only config-checkout refresh. Never touches SSH — enrollment is `managed-machine ssh enroll` |
| setup-agent-bot-gh | Explicit, restorable agent-bot interposition for Codex desktop; never part of initial bootstrap |
| setup-bin | Keep local-bin at the pinned ref and link tools into ~/.local/bin |
| setup-proton-pass | Proton Pass CLI |
| setup-muse | Meta Muse Code (`muse` CLI) |
| setup-claude | Claude Code (`claude`) |
| setup-codex-cli | OpenAI Codex CLI (`codex`) |
| setup-antigravity | Antigravity CLI (`agy`) |
| setup-grok-build | Grok Build (`grok`) |
| setup-aider | Aider (`aider`); managed `~/.aider.conf.yml` installed only when missing |
| setup-droid | Droid CLI (`droid`) |
| setup-goose | Goose CLI (`goose`); installer runs with `CONFIGURE=false` |
| setup-opencode | OpenCode (`opencode`) |
| setup-opencode-app | OpenCode desktop app from official homebrew/cask only; verified Team ID |
| setup-codex | Codex managed config: synced defaults in `config.toml` plus opt-in `--profile <name>` variants installed as `~/.codex/<name>.config.toml` (e.g. `muse`); no secrets |
| setup-devin | Devin CLI install plus interactive or deferred authentication |
| setup-lmstudio | LM Studio (Homebrew Cask into `/Applications`; `MANAGED_MACHINE_LMSTUDIO_APPDIR` override) |
| setup-vscode | VS Code from official homebrew/cask only; verified Team ID |
| setup-cursor | Cursor from official homebrew/cask only; verified Team ID |
| setup-claude-app | Claude desktop app from official homebrew/cask only; verified Team ID |
| setup-antigravity-app | Antigravity hub from official homebrew/cask only; verified Team ID |
| setup-antigravity-ide | Antigravity IDE from official homebrew/cask only; verified Team ID |
| setup-kiro | Kiro from official homebrew/cask only; verified Team ID |
| setup-kiro-cli | Kiro CLI from official homebrew/cask only; verified Team ID |
| setup-rust | rustup + cargo PATH |

Catalog-only harnesses have no script; `managed-machine setup <name>` resolves catalog names and aliases directly: `amp`, `cline`, `copilot`, `deepseek`, `grok` (alias of `grok-build`), `hermes`, `pi`, `qwen`, `warp`, `zcode`.

## Dependencies

- Dotfiles/config sourced from a persistent private checkout under `$XDG_DATA_HOME/managed-machine/` when set, or `~/.local/share/managed-machine/` otherwise; the brew-bundled copy is read-only seed data
- local-bin kept at the pinned ref from `managed-machine-config/local-bin.ref`
- No secrets in repo; auth/keys generated per machine or from macOS Keychain

## SSH enrollment and fleet

Default bootstrap, `--update`, `setup-gh`, and `account setup` never create, upload, or register SSH keys — existing SSH state is always preserved and agent accounts are refused. Enrollment is an explicit human-only action: `managed-machine ssh enroll` requires at least one of `--authentication` (machine key + `~/.ssh` github.com block + agent/keychain import + GitHub authentication-key upload + `git_protocol=ssh`), `--signing` (signing-key upload + global SSH commit/tag signing + `allowed_signers`), or `--fleet` (fleet registry + `authorized_keys` sync). Before mutating, it prints the local account, GitHub login, key fingerprint or creation intent, and selected purposes, then requires the macOS authorization dialog (which may be satisfied by Touch ID or a recent grant — an owner-accepted limit, not a guaranteed fresh password). Cancellation or a headless session leaves state unchanged; there is no `--yes`.

Enrolled keys are created unencrypted so the flow cannot hang on a passphrase prompt. The policy is recorded locally in `~/.config/managed-machine/ssh-key-policy.toml`.

`ssh enroll --fleet` creates local `~/.config/managed-machine/machine.toml` state and a versioned machine entry in the persistent private `managed-machine-config/fleet/machines/` registry. It imports legacy `authorized_keys` records before generating the fleet key file, so existing hosts are preserved. Managed fleet paths are committed and pushed automatically; unrelated private-config edits are never staged. Default setup only refreshes that checkout (fetch + rebase when clean) and never publishes fleet state.

Use `managed-machine fleet list` to inspect registered machines. To decommission one, pass the exact machine ID to `managed-machine fleet remove`; add `--yes` for noninteractive confirmation and `--revoke-github` only when the matching authentication/signing keys should also be deleted from the active GitHub account. Successful registration and removal synchronize the private config repository automatically.

## Update

```bash
managed-machine --update
```

Runs `brew update`, upgrades `managed-machine`, then re-runs `setup-gh`, `setup-zsh`, and `setup-bin` so the private checkout is refreshed before zsh templates and the local-bin pin are consumed. No SSH enrollment happens in this path. If the machine explicitly enabled `setup-agent-bot-gh`, update runs it again last to repair Homebrew relinks and PATH refreshes; otherwise stock Homebrew `gh` is untouched.

## Release

```bash
scripts/release vX.Y.Z
```

Bumps `Formula/managed-machine.rb` (tag + version) and this skill's metadata version together, commits `Release vX.Y.Z`, tags, and pushes. Requires a clean working tree; refuses existing tags.
