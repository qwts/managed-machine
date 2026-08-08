# managed-machine

Machine setup and orchestration scripts. Idempotent; safe to re-run.

- no secrets in repo; auth/keys generated per machine or from macOS Keychain
- setup-* scripts source lib/install.sh; keep helpers reusable
- dotfiles/config live in managed-machine-config; scripts source from ../managed-machine-config
- state manifests live in ~/.config/managed-machine/*.manifest; never commit *.manifest
- home-bin.ref in managed-machine-config pins qwts/home-bin ref
- git-hooks/ runs gitleaks protect --staged; setup-git-hooks wires core.hooksPath
- confirm before destructive/repo-wide actions
