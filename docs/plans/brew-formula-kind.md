---
name: brew-formula-kind
status: active
overview: Catalog kind for official homebrew/core formulae so setup-only tools like minikube do not need a signed-cask row.
related_prs: [65]
---

# Brew-formula catalog kind

## Intent

Some requested setup-only tools are Homebrew formulae, not desktop casks. Minikube is `homebrew/core` only. The catalog already has `auto: false`; it still needs an install engine that refuses tap-shadowed formulae.

## Target

- Kind `brew-formula` with a required `formula` field (`[a-z0-9][a-z0-9-]*`).
- `brew info --json=v2 --formula homebrew/core/<name>` must report tap `homebrew/core`.
- Install with `brew_run install homebrew/core/<name>`. Already-installed receipts skip install.
- `managed-machine status` reports `brew list --versions`, falling back to `cmd_version` on `PATH` across the catalog `command`, `formula`, or `aliases` (when installed outside Homebrew or when `Cellar/<formula>` is unreadable) or `installed (<prefix>/Cellar/<formula>)` when an unreadable Cellar directory exists, and `missing` otherwise.
- On-demand apps (minikube, Docker Desktop, Discord, Slack, Telegram) live in managed-machine-config with `"auto": false`. Docker Desktop is catalog name `docker` / token `docker-desktop`.

## Acceptance

- A `brew-formula` row installs from `homebrew/core` and is a no-op on re-run.
- A missing `formula`, invalid name, or non-core tap fails before `brew install`.
- Unknown kinds still fail closed with an upgrade message.
- Bootstrap and `--update` skip `"auto": false` rows (existing optional-catalog-apps behavior).
- `managed-machine status --section catalog` reports the `PATH` fallback version for a `brew-formula` row whose only on-`PATH` binary is a catalog alias (`ripgrep` via `rg`); `tests/status.test.sh` adds that row to its private catalog copy, because the shared `tests/fixtures/apps.json` is the snapshot the Go core pins and a converted `ripgrep` row would conflict with the built-in `internal/catalog/ripgrep.yaml` (#229).

## Replay

`/bin/bash tests/brew-formula.test.sh` and `/bin/bash tests/status.test.sh`. After a formula release, `managed-machine setup minikube` installs the core formula; `setup docker` installs Docker Desktop from the signed-cask row.
