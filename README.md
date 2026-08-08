# managed-machine

Fresh-Mac bootstrap and fleet setup: Homebrew, zsh starter dotfiles, GitHub CLI + SSH identity/signing, gitleaks git hooks, Proton Pass CLI, Devin CLI, LM Studio, Rust (rustup), and host-to-host `authorized_keys` sync.

This repo is the machine manager, distributed as a self-tapped Homebrew formula. Dotfiles/config live in [`qwts/managed-machine-config`](https://github.com/qwts/managed-machine-config), which the formula installs as a working git repo under `$(brew --prefix)/opt/managed-machine/libexec/managed-machine-config` and setup scripts source from there. Utility scripts live in [`qwts/local-bin`](https://github.com/qwts/local-bin), which the formula installs as a working git repo under `$(brew --prefix)/opt/managed-machine/libexec/local-bin` and `setup-bin` keeps at the pinned ref.

---

## Install on a fresh Mac

```bash
curl -fsSL https://raw.githubusercontent.com/qwts/managed-machine/main/install.sh | bash
```

The installer:
1. Installs Homebrew if missing.
2. Verifies Homebrew is owned by the current user (fails with a fix command if not).
3. Taps `qwts/managed-machine` and installs the formula.
4. Runs `managed-machine --bootstrap` (all setup scripts in order).

If `managed-machine` is already installed, the installer updates it and tells you to use the CLI directly.

---

## Usage

```bash
managed-machine              # run full bootstrap (all setup-* scripts)
managed-machine --update     # brew update/upgrade + re-run safe setup steps
managed-machine setup <name> # run a single setup script, e.g. setup-bin
managed-machine --help       # show usage
```

---

## Setup scripts

All setup scripts are safe to re-run.

| Script | Purpose |
|---|---|
| `setup-brew` | Install Homebrew if missing (wires `brew shellenv` into your shell). |
| `setup-zsh` | Install starter `~/.zshenv`, `~/.zprofile`, `~/.zshrc` only when missing; existing files are never overwritten. |
| `setup-git-hooks` | Install gitleaks via brew, set `core.hooksPath=git-hooks` for this repo so pre-commit runs `gitleaks protect --staged`. |
| `setup-gh` | Install GitHub CLI via brew; set `git_protocol=ssh`; generate a per-machine RSA 4096 key at `~/.ssh/id_rsa_github`; append it to the bundled `managed-machine-config/ssh/authorized_keys` (commit/push `managed-machine-config` so other machines see it); sync that file into a managed block in `~/.ssh/authorized_keys`; wire `Host github.com` in `~/.ssh/config`; run `gh auth login`/`refresh` requesting `admin:public_key` and `admin:ssh_signing_key`; upload the key for auth + signing; set global `user.name` (login) and `user.email` (private noreply); configure SSH commit/tag signing. |
| `setup-bin` | Keep local-bin at the pinned ref (read from the bundled `managed-machine-config/local-bin.ref`) under the brew-managed prefix and run its `install` (links tools into `~/.local/bin`, prunes renames, ensures `~/.local/bin` on `PATH`). |
| `setup-proton-pass` | Install the [Proton Pass CLI](https://proton.me/pass/cli) when missing (lands in `~/.local/bin`). |
| `setup-codex` | Install Codex *with* Meta's Muse Spark config (`meta-models.json` + `model_catalog_json`, no secrets, auth stays in Keychain) |
| `setup-devin` | Install the [Devin CLI](https://docs.devin.ai/cli) when missing (lands in `~/.local/bin`). |
| `setup-lmstudio` | Install [LM Studio](https://lmstudio.ai/) via Homebrew Cask when missing (lands in `/Applications`). |
| `setup-rust` | Install [rustup](https://rustup.rs/) when missing (default profile: stable + rustfmt/clippy); ensure `${CARGO_HOME:-~/.cargo}/bin` on `PATH`. |

---

## Pinning local-bin

`managed-machine-config/local-bin.ref` records the local-bin ref this machine should run (a git tag, e.g. `v0.1.0`). `setup-bin` clones/pulls local-bin and checks out that ref, then runs local-bin's `install`.

Override the pin for a single run:

```bash
HOME_BIN_REF=v0.2.0 managed-machine setup bin
```

Bump the pin by editing `managed-machine-config/local-bin.ref` and committing it in `managed-machine-config`. Publish new local-bin versions as git tags; managed-machine tracks them by ref.

---

## Update an existing machine

```bash
managed-machine --update
```

Runs `brew update`, upgrades `managed-machine` if a new version is available, then re-runs safe setup steps (`setup-bin`, `setup-gh`).

---

## Releasing a new version

The formula is pinned to a git tag. To release:

```bash
git tag vX.Y.Z
git push origin vX.Y.Z
# Update Formula/managed-machine.rb with the new version, commit, push.
```

---

## Layout

```
managed-machine/
├── Formula/
│   └── managed-machine.rb    # self-tapped Homebrew formula (tag/sha pinned)
├── bin/
│   └── managed-machine       # CLI entry point
├── scripts/
│   ├── bootstrap             # run all setup-* in order
│   └── update                # brew update/upgrade + safe setup re-runs
├── install.sh                # curlable one-shot installer
├── setup-brew
├── setup-zsh                 # sources dotfiles from managed-machine-config/dotfiles/zsh
├── setup-gh                  # sources ssh/authorized_keys from managed-machine-config
├── setup-bin                 # local-bin orchestrator; pin from managed-machine-config
├── setup-proton-pass
├── setup-devin
├── setup-codex               # sources dotfiles from ../managed-machine-config/dotfiles/codex/meta
├── setup-lmstudio
├── setup-rust
├── setup-git-hooks
├── lib/install.sh            # shared bootstrap helpers
└── git-hooks/                # gitleaks pre-commit for this repo
```

State lives under `~/.config/managed-machine/`. The `~/.local/bin` PATH block in `~/.zshrc` uses the `# BEGIN local-bin` markers (shared with local-bin's `install`) so existing machines need no PATH migration. The cargo PATH block uses `# BEGIN rustup` markers and honors `CARGO_HOME` (default `~/.cargo`). The `~/.ssh/authorized_keys` block uses `# BEGIN managed-machine` markers; `setup-gh` rewrites the legacy `# BEGIN local-bin new-machine` block in place on first sync.
