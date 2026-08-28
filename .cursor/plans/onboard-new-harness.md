---
name: onboard-new-harness
status: active
overview: Standard pattern for adding a new agent/IDE harness with CLI/TUI and desktop/IDE components to managed-machine and managed-machine-config.
related_prs: []
---

# Onboard a new harness

## Intent

New agent/IDE tools usually ship as a CLI/TUI plus a desktop IDE app. managed-machine
contains the install engines (`signed-cask`, `official-cli`, `opencode`, `devin`,
`brew-formula`); `managed-machine-config` contains the catalog and optional
post-install config scripts. This plan is the runbook for adding both sides of a
new harness without a managed-machine release when an existing kind fits, and for
recognizing when a release is required.

## Target

1. **Pick the catalog kind for each component.**

   - CLI/TUI:
     - `official-cli` for a vendor `curl | bash` installer that lands in `~/.local/bin`.
     - `opencode` when the installer needs a custom symlink from a vendor dir into `~/.local/bin`.
     - `devin` when the installer must remain non-interactive and defer browser auth.
     - `brew-formula` when the tool is in `homebrew/core`.
     - A new kind requires a new engine in `lib/apps.sh` and a managed-machine release.

   - Desktop/IDE:
     - `signed-cask` for a signed `.app` in `homebrew/cask` where the Team ID and vendor hosts are known.
     - `cask` only when no signature gate is acceptable (rare; prefer `signed-cask`).
     - A new desktop kind requires a new engine and a release.

   - Config-only: if the harness only needs dotfiles, use a catalog row with an
     existing install kind (or a core setup script) plus
     `managed-machine-config/config/<name>`.

2. **Add catalog row(s) to `managed-machine-config/apps.json`.**

   - CLI/TUI: `name`, `kind`, `display`, `command`, `url`, optional `env`, `args`,
     `aliases`, `auto`.
   - Desktop/IDE: `name`, `kind: "signed-cask"`, `token` (cask token), `app_name`
     (exact `.app` bundle name), `team_id`, `url_hosts`, `homepage_hosts`,
     `aliases`, optional `auto`.
   - Use `"auto": false` for setup-only components that must not run on every
     `managed-machine --update` / bootstrap.

3. **Add a `setup-*` script in managed-machine when the README/skill table or
   direct invocation needs one.** Catalog-only rows can also be reached through
   `managed-machine setup <name>`, but a named script is the convention.

   Boilerplate for an install catalog app:

   ```bash
   #!/usr/bin/env bash
   set -euo pipefail
   REPO_ROOT="$(cd "$(dirname "$0")" && pwd)"
   # shellcheck source=lib/install.sh
   source "$REPO_ROOT/lib/install.sh"
   # shellcheck source=lib/bootstrap.sh
   source "$REPO_ROOT/lib/bootstrap.sh"
   # shellcheck source=lib/apps.sh
   source "$REPO_ROOT/lib/apps.sh"
   CONFIG_REPO_ROOT="${CONFIG_REPO_ROOT:-$(managed_machine_config_repo_dir)}"
   install_catalog_app <name>
   ```

   For a config-only step that runs after a sibling catalog row installs the
   binary, use `apply_config_script <name>` instead of `install_catalog_app <name>`.

   Make the script executable (`chmod +x setup-<name>`).

4. **Add optional post-install config to `managed-machine-config`.**

   - `config/<name>` runs after the app is installed if it exists. It must only
     configure; it may not install software.
   - It can use `install_home_file` to copy dotfiles from `dotfiles/<name>/...`
     into the home directory.
   - No secrets; auth/keys come from macOS Keychain or are generated per machine.

5. **Update managed-machine test fixtures and tests.**

   - Add the row(s) to `tests/fixtures/apps.json`.
   - Add a test like `tests/setup-<name>.test.sh` following the same-kind test
     (`setup-kiro.test.sh` for `signed-cask`, `setup-agent-clis.test.sh` for
     `official-cli`, `setup-devin.test.sh` for `devin`, etc.).
   - If `scripts/status` should report the new component, add the corresponding
     `grep` assertion to `tests/status.test.sh` when the fixture is populated.

6. **Update `README.md` and `skills/managed-machine/SKILL.md` in managed-machine.**

   - First-line description lists the harness.
   - Setup scripts table includes the new `setup-<name>` entries.
   - `managed-machine adopt` allowlist includes the desktop cask token and alias.
   - Layout tree lists new `setup-*` scripts.
   - Skill description line mentions the harness.

7. **If a new install engine/kind was added, release managed-machine.**

   - If only catalog rows and existing engines are used, no managed-machine
     release is needed; release `managed-machine-config` first.
   - A new engine requires a formula release: run `scripts/release vX.Y.Z`.
     The release bumps `Formula/managed-machine.rb` and the skill version
     together.

8. **Security and repo constraints.**

   - No secrets, tokens, or private keys in either repo.
   - Cask installs must verify `homebrew/cask` tap, a real `sha256`, download host,
     homepage host, and Developer ID Team ID.
   - `setup-*` scripts source `lib/install.sh` and keep helpers reusable.
   - Commit with `GIT_AUTHOR_NAME/EMAIL` set to `qwts` /
     `91036491+qwts@users.noreply.github.com`; never run `git config user.name/user.email`.
   - Do not commit `*.manifest`, `machine.toml`, or transient state.

## Acceptance

- `managed-machine setup <cli-name>` and `managed-machine setup <desktop-name>`
  (or their `setup-*` forms) are idempotent, install from the expected source, and
  fail closed on a shadowed tap, missing sha256, unexpected host, or wrong Team ID.
- `managed-machine status` reports versions/paths for both components.
- `managed-machine adopt <desktop-token>` recognizes the token and alias and can
  adopt a vendor install.
- `tests/setup-<name>.test.sh` passes and covers fresh install, re-run
  idempotency, and negative cases (bad tap, bad sha, bad host, bad team).
- `tests/status.test.sh` still passes after fixture changes.
- `README.md` and `skills/managed-machine/SKILL.md` stay in sync and mention both
  the CLI/TUI and desktop/IDE.
- No new `*.manifest`, `machine.toml`, secrets, or `git config` changes land in the
  commit.
- If a new engine was added, the release smoke test passes.

## Example: OpenCode Desktop

The current repo state is a concrete instance of this runbook. OpenCode CLI is
already catalogued (`name: opencode`, `kind: opencode`) and has `setup-opencode`
in managed-machine. OpenCode Desktop is in flight:

- Add to `managed-machine-config/apps.json`:

  ```json
  {
    "name": "opencode-app",
    "kind": "signed-cask",
    "token": "opencode-desktop",
    "app_name": "OpenCode.app",
    "team_id": "5NZ4Q7NXJ4",
    "url_hosts": ["github.com"],
    "homepage_hosts": ["opencode.ai"],
    "aliases": ["opencode-desktop"]
  }
  ```

- `setup-opencode-app` in managed-machine already calls `install_catalog_app opencode-app`.
- Add `opencode-app` to `tests/fixtures/apps.json` and write
  `tests/setup-opencode-app.test.sh` mirroring `setup-kiro.test.sh` (same cask
  tap, sha, host, and Team ID gates).
- Ensure `README.md` and `skills/managed-machine/SKILL.md` mention
  `setup-opencode-app` and `opencode-desktop`/`opencode-app` in the adopt list.
- No managed-machine release is needed because the `signed-cask` engine already
  exists; the change is a catalog and test update.

## Replay

1. Open this plan plus `managed-machine/AGENTS.md` and `managed-machine-config/AGENTS.md`.
2. Choose the concrete harness name and which components it has (CLI/TUI,
   desktop/IDE, or both).
3. For each component, pick an existing catalog kind or decide a new engine is
   required.
4. Add catalog row(s) to `managed-machine-config/apps.json` and
   `tests/fixtures/apps.json`.
5. Add or update `setup-*` and `managed-machine-config/config/<name>` scripts.
6. Add tests and update fixtures/status assertions.
7. Update `README.md`, `skills/managed-machine/SKILL.md`, and the adopt list.
8. If a new engine was added, run `scripts/release vX.Y.Z`.
9. Run the relevant tests:

   ```bash
   /bin/bash tests/setup-<name>.test.sh
   /bin/bash tests/status.test.sh
   /bin/bash tests/catalog.test.sh   # if auto or catalog shape changed
   ```

10. Commit with the managed-machine commit identity. Do not commit manifests or
    secrets.
