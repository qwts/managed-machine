# managed-machine

Machine setup and orchestration, distributed as a self-tapped Homebrew formula. Idempotent; safe to re-run.

## Agent mode

- If aiding a machine setup/migration: install the skill (`skills/SKILL.md`) and follow it. Do NOT explore the repo.
- If contributing changes to this repo: read the constraints below, then explore as needed.

## Constraints

- no secrets in repo; auth/keys generated per machine or from macOS Keychain
- setup-* scripts source lib/install.sh; keep helpers reusable
- dotfiles/config live in managed-machine-config; scripts source from ../managed-machine-config
- state manifests live in ~/.config/managed-machine/*.manifest; never commit *.manifest
- local-bin.ref in managed-machine-config pins qwts/local-bin ref
- git-hooks/ runs gitleaks protect --staged; setup-git-hooks wires core.hooksPath
- bin/managed-machine is the CLI entry point; resolves libexec via HOMEBREW_PREFIX or git clone
- Formula/managed-machine.rb is tag/sha256 pinned; update both on release
- install.sh is curlable; checks brew ownership before proceeding
- confirm before destructive/repo-wide actions
