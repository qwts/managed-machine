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
- desktop apps install from homebrew/cask only into `/Applications`; lib/cask-app.sh verifies tap, sha256, vendor hosts, and Developer ID Team ID before trusting the bundle
- which apps to install is declared in managed-machine-config/apps.json; optional config/<name> scripts apply settings after install
- Homebrew prefix stays with an admin-group owner (typically `admin`); never chown it to a non-admin invoking user
- dotfiles/config live in managed-machine-config; development uses the sibling repo, while Homebrew installs materialize a persistent writable checkout outside the Cellar
- state manifests live in ~/.config/managed-machine/*.manifest; never commit *.manifest
- bootstrap outcomes live in ~/.config/managed-machine/bootstrap.manifest; never record command output or secrets there
- local machine identity lives in ~/.config/managed-machine/machine.toml; never commit machine.toml to this repo
- SSH passphrase policy lives in ~/.config/managed-machine/ssh-key-policy.toml; never commit ssh-key-policy.toml
- versioned fleet records and public SSH keys live only in the private managed-machine-config repo
- local-bin.ref in managed-machine-config pins qwts/local-bin ref
- git-hooks/ runs gitleaks protect --staged; setup-git-hooks wires hooks only when the script directory is the git toplevel, and chains an existing core.hooksPath instead of replacing it
- bin/managed-machine is the CLI entry point; resolves libexec via HOMEBREW_PREFIX or git clone
- Formula/managed-machine.rb is tag/sha256 pinned; update both on release
- install.sh is curlable; checks brew ownership before proceeding
- confirm before destructive/repo-wide actions
- never `git config` user.name/user.email; for every commit set `GIT_AUTHOR_NAME`/`GIT_COMMITTER_NAME` to `qwts` and `GIT_AUTHOR_EMAIL`/`GIT_COMMITTER_EMAIL` to `91036491+qwts@users.noreply.github.com`. Refuse to commit if git would otherwise use a macOS full name or a `*.local`/`*.lan` hostname email
