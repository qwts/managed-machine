---
status: active
overview: Migration plan F01 — revision-pinned inventory of callers, protected read-only behavior, catalog and test fixtures that compatibility B must route explicitly; inventory half only, no fixtures or test execution yet.
related_prs: [244]
---

# F01: characterize current callers and protected behavior

## Intent

Compatibility B (ADR-0016 in managed-machine-documentation) routes every unmigrated supported invocation to a legacy adapter explicitly, and new-core failure never falls back to legacy. Each legacy route later needs a caller inventory, characterization tests and documented differences before removal. This plan records the inventory half of F01 by static inspection, pinned to exact revisions, so later fixture and routing work has a reviewed starting point. Nothing here changes behavior or runs an existing test.

## Target

### Revisions

| Source | Pinned revision | Notes |
| --- | --- | --- |
| qwts/managed-machine `main` | `e617a0ffeaa34cf34fb23acd45ff240fdcb739a1` | 3 commits after the migration plan's inspection at `ecce0fdf`: `scripts/add-agent` (73 changed lines), `tests/add-agent-all.test.sh`, `Formula/managed-machine.rb` (release v0.8.12 pins), plus the F02 proof under `proofs/`. None touch the dispatcher, catalog, status or skill code inspected below. |
| managed-machine-config (private) | `70582e8a161a524d68209314aaa1f87249489869` | Same as the plan's inspection. The formula's release seed pins `5d8c9e4a2bd5`, so config `HEAD` still differs from the released seed. |
| managed-machine-documentation | not a Git repository | Plan and ADR text read as of 2026-10-21. |

### Protected read-only invocations (bin/managed-machine)

- `--version` and `skill path [--json]` are matched before anything sources `lib/` or refreshes the config checkout (ENG-0055 comment block). `--version` rejects any argument with exit 1, or a JSON failure `version-invalid-arguments` when `--json` is present.
- `skill path` and `skill path --json` print the bundled skill path and source commit; usage errors emit `skill-invalid-arguments`. The bundled skill's command table lists the read-only set as `--version`, `--help` (also `-h`, `help`, no command), `skill path`, `status`, `fleet list`, `ssh status`, `account doctor`.
- JSON error identifiers already asserted by `tests/cli.test.sh`: `unknown-command`, `setup-invalid-arguments`, `setup-name-unknown`, `setup-name-retired`, `setup-name-invalid`, `update-invalid-arguments`, `skill-invalid-arguments`. These are the compatibility behavior a legacy route must preserve byte-for-byte or document as an intentional difference.

### `skill path` callers found

| Caller | Where | Kind |
| --- | --- | --- |
| Dispatcher usage/help text | `bin/managed-machine` (usage line and two usage-error paths) | definition |
| Bundled skill | `skills/managed-machine/SKILL.md` (command table, read-only list, source-commit note) | agent-facing documentation |
| Homebrew formula comment | `Formula/managed-machine.rb` (explains the reported commit) | packaging |
| Tests | `tests/cli.test.sh` (two invalid-argument cases), `tests/skill-workflows.test.sh` (plain output, `--json` schema, SIGPIPE tolerance) | characterization candidates |
| Private config repository | none found by text search at the pinned revision | — |

Downstream callers outside these two repositories (agent harness skills, agent-comms, local-bin) were **not** inspected and must be added before any `skill path` cutover.

### `status` section names

`scripts/status` validates requested sections (`status_valid_section`, `status_want_section`) and emits, in order: `managed-machine`, `machine`, `ssh`, `bootstrap`, `update`, `agent-accounts`, `local-bin`, `zsh-functions`, `homebrew`, `gh`, `nvm`, `node`, `hostname`, `rustup`, `rustc`, `cargo`, `catalog`. The v1 `status` contract maps these to observation records; the mapping table itself is owned by the documentation repository and is not restated here.

### Catalog shape (private, parsed as data only)

`apps.json` at `70582e8a`: `schema_version` plus 52 rows. Kinds: signed-cask 21, official-cli 13, brew-formula 7, npm 6, vendor-dmg 2, uv-tool 1, opencode 1, devin 1. 24 rows carry `aliases`; 5 rows carry an `auto` field; 2 carry a `version`. Trust metadata fields present on signed rows: `team_id`, `url_hosts`, `homepage_hosts`. No row names, hosts, team identifiers or URLs are recorded here. F05 must account for all 52 rows; this inventory only confirms the counts the migration plan assumed.

### Trust rejection cases already characterized

`tests/cask-verify.test.sh` covers: fallback acceptance on notarized Developer ID, rejection when Gatekeeper rejects, rejection when not notarized, Team ID mismatch in Gatekeeper origin (with and without checksum), and wrong signature Team ID refused before any integrity check. `tests/vendor-dmg.test.sh` covers the vendor DMG engine with stubs. These are the rejection fixtures compatibility routing must keep reachable.

### Test classification (static markers only)

61 files under `tests/*.test.sh`. Markers: isolation = `HOME=`, `mktemp`, `PATH=`, fake/stub; real = `brew install`, `sudo`, `hdiutil`, `installer -pkg`, `osascript`.

- **Isolated, no real-install markers (45):** account-readiness, account-setup-env, agent-accounts-status, agent-tools, brew-formula, brew-github-auth, cask-verify, catalog, cli, config-repo, fleet, fleet-remove, github-ssh-keys, harness-coverage, install-fetch, migrate, npm-install-scripts, release, setup-agent-clis, setup-aider, setup-brew, setup-codex, setup-devin, setup-droid, setup-git-hooks, setup-goose, setup-grok-build, setup-hostname, setup-kiro, setup-list-status, setup-lmstudio, setup-muse, setup-nvm, setup-rust, setup-zsh-functions, setup-zsh, skill-workflows, ssh-key, status, taplo, uv-tool, vendor-rc-diff, zshrc-path-guards, gitconfig-isolation and onboard-harness-skill; the last two carry no markers of either kind.
- **Contain real-install or elevation markers (16), review isolation before any run:** account, add-agent, add-agent-all, adopt, agent-comms-boundary, bootstrap, brew, elevate, install-docs, npm, setup-bin, setup-ides, setup-muse-app, ssh-enroll, update-reexec, vendor-dmg.
- Known failing on macOS per open issues: `status.test.sh` (#229) and `elevate.test.sh` (#228). Their current pass state is unverified here.

Marker counts are a triage aid, not proof a test is safe; a stubbed `sudo` string counts as a real marker and a real network call may carry no marker.

## Acceptance

- Every invocation, error id, section name, caller and count above exists at the pinned revisions (reviewer spot-check by grep).
- No test was executed and no catalog row was evaluated; only `git`, `grep`, `sed` and a JSON parse were used.
- Downstream-caller and fixture work are explicitly listed as not done.

## Replay

From a checkout at `e617a0ff`: `grep -rn "skill path" bin lib scripts skills tests Formula`; `grep -o 'section "[a-z-]*"' scripts/status`; `grep -o "assert_cli_json_error '[a-z-]*'" tests/cli.test.sh | sort -u`; the marker counts with `grep -c` over `tests/*.test.sh`; `git diff --stat ecce0fdf e617a0ff -- bin lib scripts tests Formula skills`. For the private catalog, parse `apps.json` with a JSON library and count fields and kinds only.

## Not done here

- Isolated regression fixtures (the second half of F01), including empty-home help fixtures and catalog alias/`auto` filtering captures.
- Any run of the existing shell tests, or verification of #228/#229.
- Callers outside the two inspected repositories.
- Invocation-level transition rules and the status section mapping; those are owner decisions in the documentation repository.
