# managed-machine

Fresh-Mac bootstrap and fleet setup: Homebrew, zsh starter dotfiles, GitHub CLI + SSH identity/signing, gitleaks git hooks, Proton Pass CLI, Devin CLI, LM Studio, Rust (rustup), and host-to-host `authorized_keys` sync.

This repo is the machine manager. Dotfiles/config live in [`qwts/managed-machine-config`](https://github.com/qwts/managed-machine-config), which setup scripts source from `../managed-machine-config` by default. It does **not** contain the utility scripts themselves — those live in [`qwts/home-bin`](https://github.com/qwts/home-bin), which `setup-bin` clones at a pinned ref and installs via home-bin's own `install` script.

---

## Setup on a fresh Mac

```bash
git clone git@github.com:qwts/managed-machine.git ~/managed-machine
git clone git@github.com:qwts/managed-machine-config.git ~/managed-machine-config
~/managed-machine/setup-brew
~/managed-machine/setup-zsh
~/managed-machine/setup-git-hooks
~/managed-machine/setup-gh
~/managed-machine/setup-bin          # clones/pins ~/.bin, runs home-bin/install
~/managed-machine/setup-proton-pass
~/managed-machine/setup-codex         # Codex configured for Muse Spark via Meta (safe, no secrets)
~/managed-machine/setup-devin
~/managed-machine/setup-lmstudio
~/managed-machine/setup-rust
```

All setup scripts are safe to re-run.

| Script | Purpose |
|---|---|
| `setup-brew` | Install Homebrew if missing (wires `brew shellenv` into your shell). |
| `setup-zsh` | Install starter `~/.zshenv`, `~/.zprofile`, `~/.zshrc` only when missing; existing files are never overwritten. |
| `setup-git-hooks` | Install gitleaks via brew, set `core.hooksPath=git-hooks` for this repo so pre-commit runs `gitleaks protect --staged`. |
| `setup-gh` | Install GitHub CLI via brew; set `git_protocol=ssh`; generate a per-machine RSA 4096 key at `~/.ssh/id_rsa_github`; append it to `../managed-machine-config/ssh/authorized_keys` (commit/push `managed-machine-config` so other machines see it); sync that file into a managed block in `~/.ssh/authorized_keys`; wire `Host github.com` in `~/.ssh/config`; run `gh auth login`/`refresh` requesting `admin:public_key` and `admin:ssh_signing_key`; upload the key for auth + signing; set global `user.name` (login) and `user.email` (private noreply); configure SSH commit/tag signing. |
| `setup-bin` | Clone home-bin at the pinned ref (read from `../managed-machine-config/home-bin.ref`) into `~/.bin` and run its `install` (links tools into `~/.local/bin`, prunes renames, ensures `~/.local/bin` on `PATH`). |
| `setup-proton-pass` | Install the [Proton Pass CLI](https://proton.me/pass/cli) when missing (lands in `~/.local/bin`). |
| `setup-codex` | Install Codex *with* Meta's Muse Spark config (`meta-models.json` + `model_catalog_json`, no secrets, auth stays in Keychain) |
| `setup-devin` | Install the [Devin CLI](https://docs.devin.ai/cli) when missing (lands in `~/.local/bin`). |
| `setup-lmstudio` | Install [LM Studio](https://lmstudio.ai/) via Homebrew Cask when missing (lands in `/Applications`). |
| `setup-rust` | Install [rustup](https://rustup.rs/) when missing (default profile: stable + rustfmt/clippy); ensure `${CARGO_HOME:-~/.cargo}/bin` on `PATH`. |

---

## Pinning home-bin

`managed-machine-config/home-bin.ref` records the home-bin ref this machine should run (a git tag, e.g. `v0.1.0`). `setup-bin` clones/pulls home-bin and checks out that ref, then runs home-bin's `install`.

Override the pin for a single run:

```bash
HOME_BIN_REF=v0.2.0 ~/managed-machine/setup-bin
```

Bump the pin by editing `managed-machine-config/home-bin.ref` and committing it in `managed-machine-config`. Publish new home-bin versions as git tags; managed-machine tracks them by ref.

---

## Update an existing machine

```bash
cd ~/managed-machine && git pull
~/managed-machine/setup-bin          # pulls + checks out the pin, re-links tools
```

---

## Layout

```
managed-machine/
├── setup-brew
├── setup-zsh            # sources dotfiles from ../managed-machine-config/dotfiles/zsh
├── setup-gh             # sources ssh/authorized_keys from ../managed-machine-config
├── setup-bin            # home-bin orchestrator (clone/pin/install); pin from ../managed-machine-config
├── setup-proton-pass
├── setup-devin
├── setup-codex          # sources dotfiles from ../managed-machine-config/dotfiles/codex/meta
├── setup-lmstudio
├── setup-rust
├── setup-git-hooks
├── lib/install.sh       # shared bootstrap helpers
└── git-hooks/           # gitleaks pre-commit for this repo
```

State lives under `~/.config/managed-machine/`. The `~/.local/bin` PATH block in `~/.zshrc` uses the `# BEGIN home-bin` markers (shared with home-bin's `install`) so existing machines need no PATH migration. The cargo PATH block uses `# BEGIN rustup` markers and honors `CARGO_HOME` (default `~/.cargo`). The `~/.ssh/authorized_keys` block uses `# BEGIN managed-machine` markers; `setup-gh` rewrites the legacy `# BEGIN home-bin new-machine` block in place on first sync.
