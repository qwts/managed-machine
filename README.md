# managed-machine

Fresh-Mac bootstrap and fleet setup: Homebrew, zsh starter dotfiles, GitHub CLI + SSH identity/signing, gitleaks git hooks, Proton Pass CLI, Devin CLI, and host-to-host `authorized_keys` sync.

This repo is the machine manager. It does **not** contain the utility scripts themselves — those live in [`qwts/home-bin`](https://github.com/qwts/home-bin), which `setup-bin` clones at a pinned ref and installs via home-bin's own `install` script.

---

## Setup on a fresh Mac

```bash
git clone git@github.com:qwts/managed-machine.git ~/managed-machine
~/managed-machine/setup-brew
~/managed-machine/setup-zsh
~/managed-machine/setup-git-hooks
~/managed-machine/setup-gh
~/managed-machine/setup-bin          # clones/pins ~/.bin, runs home-bin/install
~/managed-machine/setup-proton-pass
~/managed-machine/setup-codex         # Codex configured for Muse Spark via Meta (safe, no secrets)
~/managed-machine/setup-devin
```

All setup scripts are safe to re-run.

| Script | Purpose |
|---|---|
| `setup-brew` | Install Homebrew if missing (wires `brew shellenv` into your shell). |
| `setup-zsh` | Install starter `~/.zshenv`, `~/.zprofile`, `~/.zshrc` only when missing; existing files are never overwritten. |
| `setup-git-hooks` | Install gitleaks via brew, set `core.hooksPath=git-hooks` for this repo so pre-commit runs `gitleaks protect --staged`. |
| `setup-gh` | Install GitHub CLI via brew; set `git_protocol=ssh`; generate a per-machine RSA 4096 key at `~/.ssh/id_rsa_github`; append it to `ssh/authorized_keys` (commit/push so other machines see it); sync that file into a managed block in `~/.ssh/authorized_keys`; wire `Host github.com` in `~/.ssh/config`; run `gh auth login`/`refresh` requesting `admin:public_key` and `admin:ssh_signing_key`; upload the key for auth + signing; set global `user.name` (login) and `user.email` (private noreply); configure SSH commit/tag signing. |
| `setup-bin` | Clone home-bin at the pinned ref into `~/.bin` and run its `install` (links tools into `~/.local/bin`, prunes renames, ensures `~/.local/bin` on `PATH`). |
| `setup-proton-pass` | Install the [Proton Pass CLI](https://proton.me/pass/cli) when missing (lands in `~/.local/bin`). |
| `setup-codex` | Install Codex *with* Meta's Muse Spark config (`meta-models.json` + `model_catalog_json`, no secrets, auth stays in Keychain) |
| `setup-devin` | Install the [Devin CLI](https://docs.devin.ai/cli) when missing (lands in `~/.local/bin`). |

---

## Pinning home-bin

`home-bin.ref` records the home-bin ref this machine should run (a git tag, e.g. `v0.1.0`). `setup-bin` clones/pulls home-bin and checks out that ref, then runs home-bin's `install`.

Override the pin for a single run:

```bash
HOME_BIN_REF=v0.2.0 ~/managed-machine/setup-bin
```

Bump the pin by editing `home-bin.ref` and committing it. Publish new home-bin versions as git tags; managed-machine tracks them by ref.

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
├── setup-zsh
├── setup-gh
├── setup-bin            # home-bin orchestrator (clone/pin/install)
├── setup-proton-pass
├── setup-devin
├── setup-git-hooks
├── home-bin.ref         # pinned home-bin ref
├── lib/install.sh       # shared bootstrap helpers
├── dotfiles/zsh/         # starter .zshenv / .zprofile / .zshrc
├── dotfiles/codex/       # Muse Spark catalog (meta-models.json, no secrets)
├── ssh/authorized_keys   # fleet host-to-host public keys
└── git-hooks/            # gitleaks pre-commit for this repo
```

State lives under `~/.config/managed-machine/`. The `~/.local/bin` PATH block in `~/.zshrc` uses the `# BEGIN home-bin` markers (shared with home-bin's `install`) so existing machines need no PATH migration. The `~/.ssh/authorized_keys` block uses `# BEGIN managed-machine` markers; `setup-gh` rewrites the legacy `# BEGIN home-bin new-machine` block in place on first sync.
