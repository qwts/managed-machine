---
name: zsh-profile-refresh
status: active
overview: Refresh stale zsh PATH profiles, re-run setup-zsh on --update. Antigravity catalog args were dropped; the official installer does not accept --skip-path.
related_prs: [54, 152, 158, 159]
---

# Zsh profile refresh and catalog installer args

## Intent

Closed PR #51 refreshed unguarded/vendor zsh PATH files and passed `--skip-path` to Antigravity. After the catalog/config-script split, `setup-zsh` only copies missing files and `--update` skips zsh. Vendor installers keep appending `~/.local/bin`.

## Target

- Helpers in `lib/install.sh`: `zsh_profile_needs_refresh`, `backup_existing_home_file`, `preserve_zsh_profile_extras` (plus `preserve_managed_blocks`), `install_zsh_startup_file`.
- `ensure_brew_path_block` in `lib/brew.sh` writes the dedup `# BEGIN brew` block via shared `ensure_zsh_block`; `setup-brew` calls it on every success exit.
- `setup-brew` runs after `setup-zsh` in bootstrap so the `.zshenv` template lands before the brew block is appended — a brew-only file must never shadow configured template content.
- One shared `ensure_zsh_block <file> <name>` (body on stdin) under `ensure_local_bin_in_zshrc`, `ensure_cargo_bin_in_zshrc`, `ensure_nvm_in_zshrc` (public names and `# BEGIN/END` markers unchanged). Delegates to `zsh-profile ensure-block` when it resolves on PATH; the internal fallback rewrites in place, strips trailing blanks before appending, and commits same-dir atomic with mode preservation — bootstrap order never hard-depends on zsh-functions.
- `managed-machine-config/config/zsh` uses `install_zsh_startup_file`. `setup-zsh` still runs `apply_config_script zsh`, then the guarded PATH writers.
- Catalog `args` (one per line) forwarded by `install_official_cli_from_catalog`. Antigravity has no args; its installer only accepts `--dir` / `--help`.
- `scripts/update` `SAFE_STEPS` includes `setup-zsh` after `setup-gh`.
- Vendor-run leak contract: `install_official_cli` snapshots `.zshrc`/`.zprofile`/`.zshenv` from both `${ZDOTDIR:-$HOME}` and `$HOME` before running the installer (`snapshot_startup_files`) and calls `report_vendor_startup_edits <before> <after> <label>`. Only PATH-predicate lines count: `zsh_unguarded_path_line` (shared with `zsh_profile_needs_refresh` so they never diverge) plus a bare `export PATH=` fallback; `brew shellenv` / `.cargo/env` carry-over lines never count. The report is durable — it re-runs on every `setup <name>`, so it must stay quiet when nothing new leaked.
- Per-line warn names the file and the offending line. Managed-dir lines carry the `managed-machine setup zsh` remedy; foreign vendor dirs carry a manual-review remedy; a run adding both emits both. `# BEGIN <name>`…`# END <name>` ranges must be closed with a matching name before they suppress lines — an orphan `# BEGIN` never hides the export after it.
- "Newly unguarded" is an occurrence-count comparison, not a set difference: a line reports when its unguarded count in the file rose since the snapshot (additions, guard-to-open relocation, duplicate appends) and stays silent on an unchanged re-run. Whole-file hashing is never used, so a shadowing edit elsewhere cannot mask or fabricate a leak.
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
- `setup brew` leaves a `typeset -U` brew block so bare non-login shells resolve `brew` with no duplicates across nesting.
- Repeated `setup zsh` runs are byte-identical: no blank-line growth with or without `zsh-profile` on PATH.
- `zsh-profile` on PATH is delegated to; markers and helper messages unchanged.
- Antigravity installer is invoked with no extra args.
- Malformed catalog `args` (not an array) fails the install.
- `--update` runs `setup-zsh`.
- An installer adding a bare unguarded `export PATH=` to any of the three startup files (under `$HOME` or a ZDOTDIR root) warns once per line, naming the file and the line; an identical re-run warns nothing.
- A vendor moving an identical export out of a closed guarded block, or appending a second copy of an existing unguarded export, warns again (occurrence-count compare); an orphan `# BEGIN` does not suppress the following export.
- Guarded-only or non-PATH-only changes print the ok note ("added no unguarded PATH line"); a change to no file prints nothing; `brew shellenv` / `.cargo/env` carry-over lines never warn.
- A run adding a managed dir and a foreign dir emits both remedies.

## Replay

Any change to the vendor-diff warning/remedy wording must update the affected assertions in `tests/setup-aider.test.sh`, `tests/setup-grok-build.test.sh`, `tests/setup-goose.test.sh`, `tests/setup-droid.test.sh`, and `tests/vendor-rc-diff.test.sh` in the same change, and this plan's acceptance list — the full suite is mandated by AGENTS.md.

Read this file and `AGENTS.md`. Do not explore for a migration; this is contribution work. After both PRs merge and a `v0.3.9` tag exists, bootstrap this machine from the released formula (`install.sh` via `gh api`), not this worktree. Preserve admin-owned Homebrew. Hostname is already `MacStudioM2`.
