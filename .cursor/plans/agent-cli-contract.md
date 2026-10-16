---
status: completed
overview: Issue 214 — structured JSON output, section filtering, SIGPIPE tolerance, and machine-actionable error envelopes across read-only and inspection CLI surfaces.
related_prs: []
---

# Agent-friendly CLI contract: structured output and errors (#214)

## Intent

Agents and automation calling `managed-machine` previously had structured JSON output only on `account doctor --json` and `account setup --json`, forcing callers to scrape human-formatted prose from `status`, `fleet list`, `ssh status`, `setup`, and `skill path` or parse unstructured stderr lines on failure. Issue #214 brings all read-only and inspection commands to the same deterministic contract without changing default human-readable output when `--json` is omitted.

## Target

- `--json` support (`schema_version: 1`) across read-only and inspection commands:
  - `managed-machine status [--json] [--section <name>...]`: emits `schema_version: 1`, `command: "status"`, `ready: true`, `status: "ready"`, and `sections` (`managed-machine`, `machine`, `ssh`, `bootstrap`, `update`, `agent-accounts`, `local-bin`, `zsh-functions`, `toolchain`, `catalog`) with no secrets or raw manifest command output.
  - `managed-machine status --section <name>` (repeatable or comma-separated): filters output to the requested sections (`managed-machine`, `machine`, `ssh`, `bootstrap`, `update`, `agent-accounts`, `local-bin`, `zsh-functions`, `pins`, `toolchain`, `homebrew`, `gh`, `nvm`, `node`, `hostname`, `rustup`, `rustc`, `cargo`, `catalog`) in both text and `--json` modes, avoiding unrelated subprocess probes when a narrow slice is requested.
  - `managed-machine setup --list [--json]` and `managed-machine setup --json`: exits `0` and lists available setup scripts and catalog targets in `names` (`name`, `prefixed_name`, `installed`, `auto`, `kind`, `has_script`, `in_catalog`), while preserving existing `managed-machine setup` (no args) exit-1 usage behavior.
  - `managed-machine fleet list [--json]`: emits `schema_version: 1`, `command: "fleet-list"`, `ready: true`, `status: "ready"`, `registry_dir`, and `machines` (`machine_id`, `hostname`, `managed_at`, `managed_machine_ref`, `local_bin_ref`, `ssh_key_fingerprint`).
  - `managed-machine ssh status [--json]`: emits `schema_version: 1`, `command: "ssh-status"`, `ready: true`, `status: "ready"`, `enrolled`, `account`, `key`, `github`, `signing`, and `fleet`.
  - `managed-machine skill path [--json]`: emits `schema_version: 1`, `command: "skill-path"`, `ready: true`, `status: "ready"`, `name`, `path`, `skill_file`, and `commit` from the tree alone without sourcing `lib/` or refreshing the config checkout (ENG-0055).
- Structured JSON error envelopes (`managed_machine_json_failure` in `lib/install.sh` and self-contained `cli_json_failure` in `bin/managed-machine`):
  - When `--json` is present on a failing invocation across `bin/managed-machine`, `scripts/status`, `scripts/fleet`, `scripts/ssh`, or `scripts/account`, stdout receives a single JSON object containing `schema_version: 1`, `command`, `ready: false`, `status` (`"not_ready"` or `"pending_user_action"`), `error: {code, message, action, evidence}`, and `checks: [{id, status, code, message, action, evidence}]`.
  - Exit codes follow the shared contract: `0` ready/success, `1` not ready or hard failure, `75` (`EX_TEMPFAIL`) when human/owner/GUI/admin action is required (`pending_user_action`, including headless GUI unavailability on `ssh enroll` or cross-account `account`).
- Pipeline truncation (`SIGPIPE` / `BrokenPipeError`):
  - Read-only listings (`--help`, `status`, `setup --list`, `fleet list`, `ssh status`, `skill path`, `account doctor`) handle `SIGPIPE` / Python `BrokenPipeError` cleanly and exit `0` when piped to a truncating reader such as `head -n 1`.

## Acceptance

- `tests/skill-workflows.test.sh` verifies `skill path --json`, structured JSON error output on `skill list --json`, and that every command in the `skills/managed-machine/SKILL.md` side-effects table matches `usage()` in `bin/managed-machine`.
- `tests/cli.test.sh` verifies `setup --list`, `setup --list --json`, `setup --json`, structured JSON errors for retired/unknown setup targets (`setup agent-bot --json`, `setup nonexistent --json`) and unknown CLI commands (`managed-machine badcmd --json`), and `SIGPIPE` tolerance (`managed-machine --help | head -n 1`).
- `tests/status.test.sh` verifies `status --json` schema and fields, `--section` filtering in both text and `--json` modes, structured JSON errors on invalid sections/arguments, and `status --json | head -n 1`.
- `tests/fleet.test.sh` verifies `fleet list --json` (populated and empty registries) and structured JSON errors on invalid arguments.
- `tests/ssh-enroll.test.sh` verifies `ssh status --json` and structured JSON error envelopes (including exit `75` `pending_user_action` when `ssh enroll --authentication --json` is refused in an agent session).
- `tests/account.test.sh` verifies top-level `error` (`code`, `message`, `action`, `evidence`) in `account` JSON failure reports and structured JSON errors when `account` is called with invalid arguments and `--json`.

## Replay

From a neutral working directory:
```bash
root="$(pwd)"
(cd /tmp && for t in "$root"/tests/*.test.sh; do bash "$t" </dev/null || exit "$?"; done)
```
