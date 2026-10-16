---
status: completed
overview: Issue 215 — catalog introspection (`catalog list`, `catalog resolve`), alias resolution docs, config-checkout provenance metadata, and non-mutating `fleet remove --dry-run`.
related_prs: [218]
---

# Catalog introspection and dry-run/decommission ergonomics (#215)

## Intent

Agents previously had to parse `--help` prose or inspect `managed-machine-config/apps.json` and wrapper scripts directly to discover supported setup/adopt names, aliases, kinds, CLI commands, and declined/installed state, and had no non-mutating preview for `managed-machine fleet remove`. Issue #215 adds first-class catalog introspection (`managed-machine catalog list` and `managed-machine catalog resolve`), config-checkout provenance metadata across config-repo-backed JSON reports, documented alias resolution rules, and `managed-machine fleet remove <machine-id> --dry-run`.

## Target

- Catalog introspection (`scripts/catalog`, dispatched by `managed-machine catalog`):
  - `managed-machine catalog list [--json] [--no-refresh]`:
    - Refreshes the persistent config checkout pull-only by default (unless `--no-refresh` is passed).
    - Emits `schema_version: 1`, `command: "catalog-list"`, `ready: true`, `status: "ready"`, `config_checkout`, `alias_resolution`, `apps` (catalog entries from `apps.json`), `setup_scripts` (standalone concrete `setup-*` scripts), and `entries` (combined sorted inventory).
    - Each entry reports `name` (canonical), `aliases` (all accepted `setup` aliases including cask token, explicit aliases, wrapper script names such as `codex-cli`, and `setup-` prefixed forms), `adopt_aliases` (accepted `adopt` names for signed casks), `kind`, `command` (where applicable), `token`, `app_name`, `formula`, `package`, `auto`, `declined`, `installed`, `status` (`"installed"` | `"declined"` | `"not_installed"`), `has_setup_script`, and `adoptable`.
  - `managed-machine catalog resolve <name> [--json] [--no-refresh]`:
    - Resolves any canonical name, alias, cask token, wrapper script name, or `setup-` prefixed name to its canonical entry, reporting `matched_via` (`"canonical_name"` | `"cask_token"` | `"alias"`), `setup_supported`, `adopt_supported`, and `adopt_token`.
    - Fails closed on invalid (`catalog-name-invalid`), retired (`catalog-name-retired`), or unknown (`catalog-name-unknown`) names with structured JSON error envelopes when `--json` is passed.
- Config-checkout provenance metadata (`config_repo_provenance_json` in `lib/config-repo.sh`):
  - Tracks `MANAGED_MACHINE_CONFIG_REFRESH_STATUS` (`"refreshed"` | `"current"` | `"stale_dirty"` | `"fetch_failed"` | `"rebase_failed"` | `"unverified"` | `"not_attempted"`) during `refresh_managed_machine_config_repo`.
  - Exposes `config_checkout` (`path`, `source`, `branch`, `commit`, `upstream_commit`, `dirty`, `refresh_status`, `stale`) on `catalog list --json`, `catalog resolve --json`, `setup --list --json`, `fleet list --json`, and `fleet remove --dry-run --json`.
- Non-mutating fleet decommission preview (`managed-machine fleet remove <machine-id> --dry-run [--revoke-github] [--json]`):
  - Requires no `--yes` and no interactive TTY prompt.
  - Validates `<machine-id>`, checks `--revoke-github` prerequisites (`gh`), verifies target record / pending revocation existence and clean config-repo state, and predicts exit behavior (`would_exit: 0` or structured failure) without mutating the fleet registry, local `machine.toml`, `authorized_keys`, `allowed_signers`, git history/remotes, or GitHub keys.
- Documentation:
  - Documents `catalog list`, `catalog resolve`, `fleet remove --dry-run`, and alias resolution rules in `bin/managed-machine` (`usage`), `scripts/catalog`, and `skills/managed-machine/SKILL.md`.

## Acceptance

- `tests/catalog.test.sh` verifies `catalog list`, `catalog list --json`, `catalog list --no-refresh`, `catalog resolve` (canonical, alias, cask token, wrapper script `codex-cli` -> `codex`, `setup-` prefix, standalone script `bin`), declined/installed status reporting, config-checkout provenance (`refreshed`, `current`, `stale_dirty`, `not_attempted`), structured JSON errors on retired/unknown/invalid names, and `SIGPIPE` tolerance.
- `tests/fleet-remove.test.sh` and `tests/fleet.test.sh` verify `fleet remove <machine-id> --dry-run` (with and without `--revoke-github` and `--json`) performs zero local, git, or GitHub mutations, requires no `--yes` on noninteractive stdin, reports exact planned changes, and fails with matching error envelopes when the machine is missing or the config repo has unrelated dirty files.
- `tests/cli.test.sh` and `tests/skill-workflows.test.sh` verify CLI dispatch for `catalog` and skill table alignment with `usage()`.

## Replay

From a neutral working directory:
```bash
root="$(pwd)"
(cd /tmp && for t in "$root"/tests/*.test.sh; do bash "$t" </dev/null || exit "$?"; done)
```
