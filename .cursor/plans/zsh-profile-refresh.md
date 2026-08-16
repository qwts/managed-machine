---
name: zsh-profile-refresh
status: active
overview: Refresh stale zsh PATH profiles, re-run setup-zsh on --update. Antigravity catalog args were dropped; the official installer does not accept --skip-path.
related_prs: [54]
---

# Zsh profile refresh and catalog installer args

## Intent

Closed PR #51 refreshed unguarded/vendor zsh PATH files and passed `--skip-path` to Antigravity. After the catalog/config-script split, `setup-zsh` only copies missing files and `--update` skips zsh. Vendor installers keep appending `~/.local/bin`.

## Target

- Helpers in `lib/install.sh`: `zsh_profile_needs_refresh`, `backup_existing_home_file`, `preserve_zsh_profile_extras`, `install_zsh_startup_file`.
- `managed-machine-config/config/zsh` uses `install_zsh_startup_file`. `setup-zsh` still runs `apply_config_script zsh`, then the guarded PATH writers.
- Catalog `args` (one per line) forwarded by `install_official_cli_from_catalog`. Antigravity has no args; its installer only accepts `--dir` / `--help`.
- `scripts/update` `SAFE_STEPS` includes `setup-zsh` after `setup-gh`.
- Stale profiles move to `~/.zshrc.<epoch>.bak` (and the same for `.zprofile` / `.zshenv`). `brew shellenv` and `.cargo/env` are copied forward. Clean custom files without unmanaged PATH lines stay put.

## Acceptance

- Fresh install rewrites an unguarded template local-bin block to the guarded `case` form.
- Re-run on a guarded profile creates no backup.
- Antigravity / unguarded PATH lines are backed up and stripped; `brew shellenv` and `.cargo/env` survive.
- nvm present restores the guarded nvm block.
- A clean custom `.zprofile` is left alone.
- A vendor installer comment without a PATH mutation is left alone.
- Antigravity installer is invoked with no extra args.
- Malformed catalog `args` (not an array) fails the install.
- `--update` runs `setup-zsh`.

## Replay

Read this file and `AGENTS.md`. Do not explore for a migration; this is contribution work. After both PRs merge and a `v0.3.9` tag exists, bootstrap this machine from the released formula (`install.sh` via `gh api`), not this worktree. Preserve admin-owned Homebrew. Hostname is already `MacStudioM2`.
