# managed-machine

Machine setup and orchestration, distributed as a self-tapped Homebrew formula. Idempotent; safe to re-run.

## Agent migration protocol

If asked to migrate a machine with this repo:

1. Do not explore this repo.
2. Do not clone this repo.
3. Copy `skills/SKILL.md` to your agent's skill directory:
   - Devin: `~/.config/devin/skills/managed-machine/SKILL.md`
4. Invoke the `managed-machine` skill and follow it.
5. If the curl install fails because the Homebrew formula has no released tag/sha, stop and tell the user the repo needs a release before migration can proceed.

## Agent contribution mode

- If contributing changes to this repo: read the constraints below, then explore as needed.

## Constraints

- no secrets in repo; auth/keys generated per machine or from macOS Keychain
- setup-* scripts source lib/install.sh; keep helpers reusable
- dotfiles/config live in managed-machine-config; development uses the sibling repo, while Homebrew installs materialize a persistent writable checkout outside the Cellar
- state manifests live in ~/.config/managed-machine/*.manifest; never commit *.manifest
- local machine identity lives in ~/.config/managed-machine/machine.toml; never commit machine.toml to this repo
- SSH passphrase policy lives in ~/.config/managed-machine/ssh-key-policy.toml; never commit ssh-key-policy.toml
- versioned fleet records and public SSH keys live only in the private managed-machine-config repo
- local-bin.ref in managed-machine-config pins qwts/local-bin ref
- git-hooks/ runs gitleaks protect --staged; setup-git-hooks wires core.hooksPath
- bin/managed-machine is the CLI entry point; resolves libexec via HOMEBREW_PREFIX or git clone
- Formula/managed-machine.rb is tag/sha256 pinned; update both on release
- install.sh is curlable; checks brew ownership before proceeding
- confirm before destructive/repo-wide actions
