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
managed-machine fleet list   # list registered machines
managed-machine fleet remove <machine-id> [--yes] [--revoke-github]
managed-machine --help       # show usage
```

---

## Setup scripts

All setup scripts are safe to re-run.

| Script | Purpose |
|---|---|
| `setup-brew` | Install Homebrew if missing (wires `brew shellenv` into your shell). |
| `setup-zsh` | Install starter `~/.zshenv`, `~/.zprofile`, `~/.zshrc` only when missing; existing files are never overwritten. |
| `setup-nvm` | Install upstream NVM, add a managed zsh initialization block, install the current Node.js LTS release, and make it the default. |
| `setup-git-hooks` | Install gitleaks via brew, set `core.hooksPath=git-hooks` for this repo so pre-commit runs `gitleaks protect --staged`. |
| `setup-gh` | Install GitHub CLI via brew; generate/upload a per-machine SSH key; register immutable bootstrap metadata in the private `managed-machine-config/fleet/machines` registry; generate and sync fleet `authorized_keys`; configure Git identity and SSH signing. |
| `setup-bin` | Keep local-bin at the pinned ref (read from the bundled `managed-machine-config/local-bin.ref`) under the brew-managed prefix and run its `install` (links tools into `~/.local/bin`, prunes renames, ensures `~/.local/bin` on `PATH`). |
| `setup-proton-pass` | Install the [Proton Pass CLI](https://proton.me/pass/cli) when missing (lands in `~/.local/bin`). |
| `setup-codex` | Install Codex *with* Meta's Muse Spark config (`meta-models.json` + `model_catalog_json`, no secrets, auth stays in Keychain) |
| `setup-devin` | Install the [Devin CLI](https://docs.devin.ai/cli) when missing (lands in `~/.local/bin`). |
| `setup-lmstudio` | Install [LM Studio](https://lmstudio.ai/) via Homebrew Cask when missing (lands in `/Applications`). |
| `setup-rust` | Install [rustup](https://rustup.rs/) when missing (default profile: stable + rustfmt/clippy); ensure `${CARGO_HOME:-~/.cargo}/bin` on `PATH`. |

---

## Fleet registry

`setup-gh` writes local identity state to `~/.config/managed-machine/machine.toml` and registers the same machine in the private `managed-machine-config/fleet/machines/` directory. Machine IDs are stable, filesystem-safe forms of the SSH public-key SHA-256 fingerprint. Initial registration timestamps and bootstrap refs are preserved on reruns.

On the first fleet-aware run, existing keys in `managed-machine-config/ssh/authorized_keys` are imported as legacy machine records before that file is regenerated. Registration never commits private config changes automatically; follow the printed `git add`, `commit`, and `push` instructions.

```bash
managed-machine fleet list
managed-machine fleet remove sha256-...             # prompts for confirmation
managed-machine fleet remove sha256-... --yes       # explicit noninteractive removal
managed-machine fleet remove sha256-... --revoke-github
```

Removal deletes the exact registry entry, regenerates fleet/local `authorized_keys`, and removes local `machine.toml` when decommissioning the current machine. GitHub authentication and signing keys are retained unless `--revoke-github` is supplied.

The private config repository is the supported fleet backend. Gist, synced-folder, and database backends are intentionally deferred.

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
│   ├── fleet                 # list and decommission fleet machines
│   └── update                # brew update/upgrade + safe setup re-runs
├── install.sh                # curlable one-shot installer
├── setup-brew
├── setup-zsh                 # sources dotfiles from managed-machine-config/dotfiles/zsh
├── setup-nvm                 # installs NVM and the current Node.js LTS release
├── setup-gh                  # sources ssh/authorized_keys from managed-machine-config
├── setup-bin                 # local-bin orchestrator; pin from managed-machine-config
├── setup-proton-pass
├── setup-devin
├── setup-codex               # sources dotfiles from ../managed-machine-config/dotfiles/codex/meta
├── setup-lmstudio
├── setup-rust
├── setup-git-hooks
├── lib/
│   ├── install.sh            # shared bootstrap helpers
│   └── fleet.sh              # machine identity and private fleet registry
└── git-hooks/                # gitleaks pre-commit for this repo
```

State lives under `~/.config/managed-machine/`, including the uncommitted local `machine.toml`. The `~/.local/bin` PATH block in `~/.zshrc` uses the `# BEGIN local-bin` markers (shared with local-bin's `install`) so existing machines need no PATH migration. The NVM initialization block uses `# BEGIN nvm` markers and manages `NVM_DIR` (default `~/.nvm`). The cargo PATH block uses `# BEGIN rustup` markers and honors `CARGO_HOME` (default `~/.cargo`). The `~/.ssh/authorized_keys` block uses `# BEGIN managed-machine` markers; `setup-gh` rewrites the legacy `# BEGIN local-bin new-machine` block in place on first sync.
