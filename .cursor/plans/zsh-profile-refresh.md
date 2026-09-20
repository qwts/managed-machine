---
name: zsh-profile-refresh
status: active
overview: Refresh stale zsh PATH profiles, re-run setup-zsh on --update. Antigravity catalog args were dropped; the official installer does not accept --skip-path.
related_prs: [54, 152]
---

# Zsh profile refresh and catalog installer args

## Intent

Closed PR #51 refreshed unguarded/vendor zsh PATH files and passed `--skip-path` to Antigravity. After the catalog/config-script split, `setup-zsh` only copies missing files and `--update` skips zsh. Vendor installers keep appending `~/.local/bin`.

## Target

- Helpers in `lib/install.sh`: `zsh_profile_needs_refresh`, `backup_existing_home_file`, `preserve_zsh_profile_extras` (plus `preserve_managed_blocks`), `install_zsh_startup_file`.
- One shared `ensure_zsh_block <file> <name>` (body on stdin) under `ensure_local_bin_in_zshrc`, `ensure_cargo_bin_in_zshrc`, `ensure_nvm_in_zshrc` (public names and `# BEGIN/END` markers unchanged). Delegates to `zsh-profile ensure-block` when it resolves on PATH; the internal fallback rewrites in place, strips trailing blanks before appending, and commits same-dir atomic with mode preservation — bootstrap order never hard-depends on zsh-functions.
- `managed-machine-config/config/zsh` uses `install_zsh_startup_file`. `setup-zsh` still runs `apply_config_script zsh`, then the guarded PATH writers.
- Catalog `args` (one per line) forwarded by `install_official_cli_from_catalog`. Antigravity has no args; its installer only accepts `--dir` / `--help`.
- `scripts/update` `SAFE_STEPS` includes `setup-zsh` after `setup-gh`.
- Stale profiles move to `~/.zshrc.<epoch>.bak` (and the same for `.zprofile` / `.zshenv`). `brew shellenv` and `.cargo/env` are copied forward. Clean custom files without unmanaged PATH lines stay put.
- Unguarded agent-bot exports (`$HOME` variants with trailing marker comments for `.local/bin`, `.config/agent-bot/bin`) trigger a refresh; guarded `case` bodies never do.
- Every well-formed managed block in the backup is carried into the rewritten file with name-dedup against the template (template-owned blocks never duplicate) and a single blank separator; orphan `BEGIN`s stay behind.

## Acceptance

- Fresh install rewrites an unguarded template local-bin block to the guarded `case` form.
- Re-run on a guarded profile creates no backup.
- Antigravity / unguarded PATH lines are backed up and stripped; `brew shellenv` and `.cargo/env` survive.
- nvm present restores the guarded nvm block.
- A clean custom `.zprofile` is left alone.
- A vendor installer comment without a PATH mutation is left alone.
- Agent-bot loose exports in `.zshenv` are backed up and stripped.
- Managed blocks (including the `zsh-functions` loader) survive a refresh exactly once; re-refresh is byte-identical; orphans stay in the backup.
- Repeated `setup zsh` runs are byte-identical: no blank-line growth with or without `zsh-profile` on PATH.
- `zsh-profile` on PATH is delegated to; markers and helper messages unchanged.
- Antigravity installer is invoked with no extra args.
- Malformed catalog `args` (not an array) fails the install.
- `--update` runs `setup-zsh`.

## Replay

Read this file and `AGENTS.md`. Do not explore for a migration; this is contribution work. After both PRs merge and a `v0.3.9` tag exists, bootstrap this machine from the released formula (`install.sh` via `gh api`), not this worktree. Preserve admin-owned Homebrew. Hostname is already `MacStudioM2`.
