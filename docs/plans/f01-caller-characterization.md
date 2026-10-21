---
status: active
overview: Migration plan F01 — revision-pinned inventory of callers, protected read-only behavior, catalog and test fixtures that compatibility B must route explicitly, plus byte-exact offline goldens for the read-only command surface.
related_prs: [244]
---

# F01: characterize current callers and protected behavior

## Intent

Compatibility B (ADR-0016 in managed-machine-documentation) routes every unmigrated supported invocation to a legacy adapter explicitly, and new-core failure never falls back to legacy. Each legacy route later needs a caller inventory, characterization tests and documented differences before removal. This plan records the F01 inventory by static inspection, pinned to exact revisions, plus the first fixture slice (byte goldens for the read-only command surface), so later fixture and routing work has a reviewed starting point. Nothing here changes behavior or runs an existing test.

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
| In-repo contract plan | `docs/plans/agent-cli-contract.md` (documents the `skill path --json` record shape and SIGPIPE handling) | contract record |
| Shared CI gate | `qwts/qwts-agent-ci` action `cli-skill-gate`, invoked from `.github/workflows/ci.yml` with `bundle-args-json: ["skill", "path"]` against the packaged executable | runtime caller (every CI run) |
| Command-family contract | `qwts/qwts-agent-sop` ENG-0055 (every CLI ships its skill and a read-only location command), ENG-0064 (`skill` command family) and `docs/reference/cli-skill-command-contract.md` | contract owner |
| Org operations docs | `qwts/qwts-agent-org` `docs/agent-bot-operations.md`, `docs/config.md` mention `skill path` provenance generically | reference only |
| Private config repository | none found by text search at the pinned revision | — |

GitHub code search over the `qwts` organization (2026-10-21) found no invocation of `managed-machine skill` outside this repository; the other `skill path` hits (`agent-bot-identity`, `agent-comms`) are those CLIs' own commands under the same ENG-0064 family. The search covers default branches of repositories visible to the bot token only; private repositories outside that grant and non-GitHub callers remain unverified.

### `status` section names

`scripts/status` validates requested sections (`status_valid_section`, `status_want_section`). Its own `status-invalid-section` error lists the 19 valid sections in order: `managed-machine`, `machine`, `ssh`, `bootstrap`, `update`, `agent-accounts`, `local-bin`, `zsh-functions`, `pins`, `toolchain`, `homebrew`, `gh`, `nvm`, `node`, `hostname`, `rustup`, `rustc`, `cargo`, `catalog`. (A first pass that grepped `section "..."` literals missed `pins` and `toolchain`; the error message is the authoritative list and is captured byte-for-byte in the draft status goldens.) The v1 `status` contract maps these to observation records; the mapping table itself is owned by the documentation repository and is not restated here.

### Fixtures delivered: read-only command surface goldens

`tests/readonly-characterization.test.sh` runs a copied `bin/managed-machine` in a throwaway tree (`env -i`, empty `HOME`, minimal `PATH`, fixture `VERSION`, `RELEASE_COMMIT`, skill bundle and one executable `setup-alpha`) and compares exit code, stdout and stderr byte-for-byte against 17 goldens in `tests/fixtures/readonly-characterization/`, with the fixture root normalized to `<ROOT>`:

- ENG-0055 routes that must never source `lib/`: `--version` (plain, extra argument, extra argument with `--json`), `skill path` (plain, `--json`, bogus flag plain and `--json`), `skill` without a subcommand (plain and `--json`). The stub `lib/install.sh` records being sourced, and the test fails if it is.
- Help routes that may source `lib/install.sh` but must not refresh the config checkout or probe installs (qwts/managed-machine#173): `--help`, `-h`, `help`, no command (all four identical), `--help extra --json`, unknown command (plain and `--json`), `--update extra --json`. The stub records any config-directory resolution, refresh or install probe, and the test fails if one happens.

Observed contract captured by the goldens: every `--json` failure is the `schema_version: 1` envelope with `ready: false`, `status: "not_ready"`, an `error` object and one `checks` entry whose `id` is the command with dashes turned into dots plus `.preflight`; only `unknown-command` carries evidence (`{"command": ...}`). Plain failures write one `Error:` line to stderr (help and unknown command also print usage to stderr) and exit 1. `skill path --json` is `ready: true` with `path`, `skill_file` and `commit`. Regenerate with `--record` only after a reviewed contract change; a tampered golden fails the run (checked).

What these fixtures do not cover: `status`, `setup --list`, `catalog`, `fleet`, `account`, `ssh` and `adopt` routes, which source `lib/` and depend on the config checkout or host state; those need stubbed fixtures like `tests/cli.test.sh` builds, not this empty-tree harness.

### Catalog shape (private, parsed as data only)

`apps.json` at `70582e8a`: `schema_version` plus 52 rows. Kinds: signed-cask 21, official-cli 13, brew-formula 7, npm 6, vendor-dmg 2, uv-tool 1, opencode 1, devin 1. 24 rows carry `aliases`; 5 rows carry an `auto` field; 2 carry a `version`. Trust metadata fields present on signed rows: `team_id`, `url_hosts`, `homepage_hosts`. No row names, hosts, team identifiers or URLs are recorded here. F05 must account for all 52 rows; this inventory only confirms the counts the migration plan assumed.

### Catalog routes already characterized

`tests/catalog.test.sh` (#215) already drives `scripts/catalog` through the dispatcher against a fixture `apps.json` in an isolated `CONFIG_REPO_ROOT` with a fake `HOME` and fake binaries: `auto` filtering (omitted/true install on bootstrap, false is setup-only, string `"false"` rejected), retired runtime rows rejected, `catalog resolve` for canonical names, cask tokens, aliases, wrapper scripts and `setup-` prefixes, error envelopes for unknown, invalid and retired names including evidence, `catalog-invalid-arguments` for bad flags in both orders, fresh-`HOME` and legacy-origin non-materialization of a config checkout, and SIGPIPE tolerance. These are assertion tests, not byte goldens; `catalog list` output also depends on install probes (`catalog_app_installed`, `setup_name_installed`) and config provenance, so a golden slice for it needs those seams stubbed in a fixture `lib/`, which is a harness design to review before building.

### Trust rejection cases already characterized

`tests/cask-verify.test.sh` covers: fallback acceptance on notarized Developer ID, rejection when Gatekeeper rejects, rejection when not notarized, Team ID mismatch in Gatekeeper origin (with and without checksum), and wrong signature Team ID refused before any integrity check. `tests/vendor-dmg.test.sh` covers the vendor DMG engine with stubs. These are the rejection fixtures compatibility routing must keep reachable.

### Test classification (static markers only)

61 pre-existing files under `tests/*.test.sh` (this plan's own `readonly-characterization.test.sh` is a 62nd, isolated by construction and excluded from the counts). Markers: isolation = `HOME=`, `mktemp`, `PATH=`, fake/stub; real = `brew install`, `sudo`, `hdiutil`, `installer -pkg`, `osascript`.

- **Isolated, no real-install markers (45):** account-readiness, account-setup-env, agent-accounts-status, agent-tools, brew-formula, brew-github-auth, cask-verify, catalog, cli, config-repo, fleet, fleet-remove, github-ssh-keys, harness-coverage, install-fetch, migrate, npm-install-scripts, release, setup-agent-clis, setup-aider, setup-brew, setup-codex, setup-devin, setup-droid, setup-git-hooks, setup-goose, setup-grok-build, setup-hostname, setup-kiro, setup-list-status, setup-lmstudio, setup-muse, setup-nvm, setup-rust, setup-zsh-functions, setup-zsh, skill-workflows, ssh-key, status, taplo, uv-tool, vendor-rc-diff, zshrc-path-guards, gitconfig-isolation and onboard-harness-skill; the last two carry no markers of either kind.
- **Contain real-install or elevation markers (16), review isolation before any run:** account, add-agent, add-agent-all, adopt, agent-comms-boundary, bootstrap, brew, elevate, install-docs, npm, setup-bin, setup-ides, setup-muse-app, ssh-enroll, update-reexec, vendor-dmg.
- Known failing on macOS per open issues: `status.test.sh` (#229) and `elevate.test.sh` (#228). Their current pass state is unverified here.

Marker counts are a triage aid, not proof a test is safe; a stubbed `sudo` string counts as a real marker and a real network call may carry no marker.

## Acceptance

- Every invocation, error id, section name, caller and count above exists at the pinned revisions (reviewer spot-check by grep).
- For the inventory, no existing test was executed and no catalog row was evaluated; only `git`, `grep`, `sed` and a JSON parse were used. The new characterization test runs only a copied dispatcher in an empty tree and passes; a tampered golden makes it fail.
- Downstream-caller work and the remaining fixtures are explicitly listed as not done.

## Replay

`bash tests/readonly-characterization.test.sh` (offline; needs bash and python3). For the inventory, from a checkout at `e617a0ff`: `grep -rn "skill path" bin lib scripts skills tests Formula`; `managed-machine status --section nothing-here --json` in an isolated tree (the error lists the valid sections); `grep -o "assert_cli_json_error '[a-z-]*'" tests/cli.test.sh | sort -u`; the marker counts with `grep -c` over `tests/*.test.sh`; `git diff --stat ecce0fdf e617a0ff -- bin lib scripts tests Formula skills`. For the private catalog, parse `apps.json` with a JSON library and count fields and kinds only.

## Not done here

- Byte goldens for the routes that source `lib/` (`catalog list/resolve`, `setup --list`, `status` sections, trust rejection replays). Catalog alias/`auto` behavior is already asserted by `tests/catalog.test.sh`; turning it into goldens needs the install-probe and provenance seams stubbed, which is a harness decision for review. The read-only command surface is covered above.
- Any run of the existing shell tests, or verification of #228/#229.
- Callers in repositories the bot token cannot see, and non-GitHub callers; the org code search above is the only cross-repository evidence.
- Invocation-level transition rules and the status section mapping; those are owner decisions in the documentation repository.
