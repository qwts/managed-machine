---
name: onboard-harness
description: "Onboard a new agent/IDE harness into managed-machine and managed-machine-config. Use when the user asks to add a new harness, onboard a new agent tool, add a new CLI/TUI and desktop/IDE pair, or extend the catalog with a new tool. Guides catalog rows, setup scripts, tests, docs, and optional dotfiles/config without secrets."
license: MIT
metadata:
  author: qwts
---

# Onboard a new harness

Use this skill when the user wants to add a new agent/IDE harness that usually
has a CLI/TUI and a desktop/IDE side. It loads the durable runbook and repo
constraints so the work lands correctly across `managed-machine` and
`managed-machine-config`.

## Before you start

1. Open `.cursor/plans/onboard-new-harness.md`.
2. Open `managed-machine/AGENTS.md` and `managed-machine-config/AGENTS.md`.
3. Confirm the harness name and which components it has:
   - CLI/TUI only
   - Desktop/IDE only
   - Both CLI/TUI and desktop/IDE

## Constraints

- No secrets, tokens, or private keys in any file you commit.
- `managed-machine-config/config/<name>` and dotfiles only configure; they do not
  install software.
- Do not edit live home files directly; use `install_home_file` and managed
  blocks from `lib/install.sh`.
- Cask rows must use `homebrew/cask`, have a real `sha256`, allowed vendor
  download/homepage hosts, and the correct Developer ID Team ID.
- Do not commit `*.manifest`, `machine.toml`, or ssh-key-policy files.
- Commit with `GIT_AUTHOR_NAME=qwts` and
  `GIT_AUTHOR_EMAIL=91036491+qwts@users.noreply.github.com`; never run
  `git config user.name` or `git config user.email`.

## Workflow

For each component, pick an existing catalog kind. If no existing kind fits,
stop and create a managed-machine release plan for a new engine before
continuing.

1. **Catalog rows** — add to `managed-machine-config/apps.json` and
   `tests/fixtures/apps.json`.
2. **Setup scripts** — in `managed-machine/`, add `setup-<name>` only when the
   README/skill table or direct invocation needs it.
3. **Config/dotfiles** — in `managed-machine-config/`, add `config/<name>` and
   `dotfiles/<name>` only if post-install configuration is needed.
4. **Tests** — add `tests/setup-<name>.test.sh` and update
   `tests/status.test.sh` if the fixture lists the harness.
5. **Docs** — update `README.md` and `skills/managed-machine/SKILL.md`
   description, setup table, adopt list, and layout tree.
6. **Commit** — use the managed-machine commit identity; do not commit manifests
   or secrets.

## Checklist

- [ ] CLI/TUI component has a catalog row with the right kind
      (`official-cli`, `opencode`, `devin`, `brew-formula`).
- [ ] Desktop/IDE component has a `signed-cask` row with `token`, `app_name`,
      `team_id`, `url_hosts`, `homepage_hosts`, and `aliases`.
- [ ] Any new `setup-*` script uses the boilerplate and calls
      `install_catalog_app <name>` or `apply_config_script <name>`.
- [ ] `tests/fixtures/apps.json` includes the new rows.
- [ ] A component-specific test exists and passes.
- [ ] `README.md` and `skills/managed-machine/SKILL.md` mention both components.
- [ ] No secrets and no `git config` in the diff.

## When a new engine is needed

If the harness cannot use `official-cli`, `opencode`, `devin`, `brew-formula`,
or `signed-cask`, create a plan for a new engine in `lib/apps.sh` or
`lib/cask-app.sh` and a managed-machine formula release.
