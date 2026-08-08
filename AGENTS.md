# managed-machine

Machine setup and orchestration scripts. Idempotent; safe to re-run.

- no secrets in repo; auth/keys generated per machine or from macOS Keychain
- setup-* scripts source lib/install.sh; keep helpers reusable
- dotfiles/ are starter templates; setup scripts install only if missing or already in manifest
- state manifests live in ~/.config/managed-machine/*.manifest; never commit *.manifest
- home-bin.ref pins qwts/home-bin ref; setup-bin clones/checks out/installs it
- git-hooks/ runs gitleaks protect --staged; setup-git-hooks wires core.hooksPath
- confirm before destructive/repo-wide actions
