---
name: managed-machine
description: "Bootstrap, update, and manage a Mac machine via the managed-machine Homebrew formula. USE FOR: fresh Mac setup, install managed-machine, run bootstrap, report installed versions with status, update machine, run a setup script, adopt vendor-installed desktop apps, brew ownership fix, local-bin pin, fleet SSH keys, gitleaks hooks, Codex Meta config, Meta Muse Code, Claude Code, Codex CLI, Antigravity CLI, Grok Build, Aider, Droid CLI, Goose CLI, OpenCode, OpenCode Desktop, Devin CLI install, LM Studio, VS Code, Cursor, Claude app, Antigravity, Rust, Proton Pass. DO NOT USE FOR: editing dotfiles (use managed-machine-config), writing utility scripts (use local-bin), general Homebrew usage."
license: MIT
metadata:
  qwts-contract: "1"
  qwts-cli: "managed-machine"
  qwts-versions: ">=0.10.0 <0.12.0"
  qwts-validated: "0.11.0"
  qwts-side-effects: "destructive"
---

# managed-machine Skill

Bootstrap and manage a macOS machine using the `managed-machine` Homebrew formula and its setup scripts.

## Install (fresh Mac)

Installing needs qwts org access (the formula's config and local-bin resources are private); fetch the installer through an authenticated GitHub CLI (`gh auth login` first):

```bash
(
  installer="$(mktemp "${TMPDIR:-/tmp}/managed-machine-install.XXXXXX")" || exit 1
  trap 'rm -f "$installer"' EXIT
  if gh api -H "Accept: application/vnd.github.raw" repos/qwts/managed-machine/contents/install.sh >"$installer"; then
    bash "$installer"
  else
    fetch_status=$?
    echo "Error: installer download failed; nothing was executed. See gh's error above." >&2
    exit "$fetch_status"
  fi
)
```

A failed download exits before Bash runs and removes the temporary file. For invalid or expired credentials in a human Terminal, check `gh auth status`, sign in again with `gh auth login -h github.com` if needed, and retry. A successful download preserves the installer's exit status.

The installer checks brew ownership, verifies gh authentication and wires gh as the git credential helper (private repos clone over HTTPS; no SSH key is required or created — SSH enrollment is the explicit `managed-machine ssh enroll` step), taps `qwts/managed-machine`, trusts the tap when Homebrew requires it, installs the formula, and runs `managed-machine --bootstrap`. Administrator dialogs during that run are part of the install. Steps that cannot finish are skipped, not failed, and do not assign a follow-up command.

On a Mac where agent-bot's gh shim is on PATH, gh refuses an agent session the human's login (`agent-bot: ... refusing stock human gh`). The install and `setup gh` are then human-Terminal steps: stop and ask the owner to run them. Do not run `gh auth login`. The installer reports this case instead of asking for a login.

If brew is installed but owned by another admin-group user (typically `admin`),
the installer leaves that ownership in place and runs mutating brew commands as
that owner through the macOS authorization dialog. It never `chown`s the prefix
to a non-admin invoking user.

GeniusBar, `agent-bot` and `agent-comms` are not part of this install: the owner installs GeniusBar by hand and uses its **Command-line tools…** for the two CLIs. Do not install or repair them through managed-machine.

## If the install fails

If the curl installer fails because `Formula/managed-machine.rb` has no released tag, do not clone or run from a local copy. Stop and ask the user to create a release first.

## CLI

```bash
# Portable Go core commands (bin/managed-machine on all five targets):
managed-machine software [name] [--available|--install|--refresh] [--category <name>] [--provider <name>|--recipe <id>] [--user|--global|--custom <path>] [--json|--human]
managed-machine config [name] [--available|--status|--check|--plan|--apply] [--from-plan <id>] [--category <name>] [--global|--user|--custom <path>] [--json|--human]
managed-machine status [--section software|config|flavor|fleet [name]] [--global|--user|--custom <path>] [--json|--human]
managed-machine fleet [owner] [--keys|--access|--refresh|--plan|--apply|--check|--publish|--unpublish <fp>] [--owner <id>] [--key <path>] [--label <text>] [--from-plan <id|path>] [--user] [--json|--human]
managed-machine skill [name] [--kind <kind>] [--bundle <bundle>] [--search <term>] [--version <v>] [--next <h>] [--json|--human]
managed-machine skill path [--json|--human]                 # print bundled skill path + source commit
managed-machine platform [--json|--human]
managed-machine version [--json|--human]
managed-machine --version    # print bare release version
managed-machine --help       # also -h, help, setup -h/--help, or no command; changes nothing

# Legacy commands (macOS only; Homebrew install, delegated to libexec/legacy/bin/managed-machine):
managed-machine --bootstrap  # full bootstrap; terminal mode is auto-detected
managed-machine --bootstrap --interactive
managed-machine --bootstrap --non-interactive
managed-machine --update     # brew update/upgrade + safe setup re-runs
managed-machine status --section bootstrap|update [--json]  # last bootstrap or update manifest outcome (read-only)
managed-machine setup --list [--json]                       # list available setup names and install state
managed-machine catalog list [--json] [--no-refresh]        # introspect catalog apps, scripts, aliases, kinds, status
managed-machine catalog resolve <name> [--json]             # resolve any setup or adopt alias to its canonical entry
managed-machine setup bin       # preferred: run setup-bin (19 catalog apps run in the core; others delegate to legacy)
managed-machine setup setup-bin # compatible explicit script-name form
managed-machine adopt           # adopt vendor-installed desktop apps into Homebrew
managed-machine adopt vscode    # one app (cask token or alias)
managed-machine add-agent <slug>|--all [--full-name <persona>] [--with-harness] # create dedicated agent macOS user account(s)
managed-machine fleet remove <machine-id> [--yes] [--revoke-github] [--dry-run] [--json]
managed-machine account doctor <account> [--json]           # live per-account readiness (read-only)
managed-machine account setup <account> [--json]            # prepare account environment + harness
managed-machine ssh enroll --authentication|--signing|--fleet   # explicit human-only SSH enrollment
managed-machine ssh status [--json]                         # this account's SSH enrollment state (read-only)
```

A bootstrap starts only from `--bootstrap`. The bare command, `help`, `-h`, `--help`, `setup -h`, and `setup --help` print usage and exit 0; an unknown command writes an `MM_USAGE` diagnostic to stderr and exits 2.

Compatibility rule (ADR-0016, ADR-0017, S6 D2/D5, S7, S8 D1–D7): `bin/managed-machine` is the portable Go core on all five targets (`darwin/arm64`, `linux/arm64`, `linux/amd64`, `windows/arm64`, `windows/amd64`). It serves `software`, `config`, bare `status` and `status --section software|config|flavor|fleet`, `fleet` (`--keys`, `--access`, `--refresh`, `--plan`, `--apply`, `--check`, `--publish`, `--unpublish`, or an owner name), `skill` and `skill path [--json|--human]`, `platform`, `version`, `--version`, and `--help`/`-h`/`help`/`setup --help`/`setup -h` directly. `skill path [--json|--human]` resolves the skill bundle from `MANAGED_MACHINE_BUNDLE_ROOT` when set or `<executable dir>/../libexec` (`skills/managed-machine/SKILL.md` and `RELEASE_COMMIT`), failing with `MM_SKILL_BUNDLE_MISSING` (exit 1) when the bundle is absent (`skill <flags> path` exits 2 with `MM_LEGACY_ROUTE_UNSUPPORTED`). On `darwin`, when `MANAGED_MACHINE_POLICY_DIR` (or `MANAGED_MACHINE_SOURCE_DIR`) is unset, the core resolves `$CONFIG_REPO_ROOT` else `${XDG_DATA_HOME:-$HOME/.local/share}/managed-machine/managed-machine-config` (and `<checkout>/software`) when that directory exists on disk. For the 19 converted catalog apps (`antigravity-app`, `ast-grep`, `brave`, `chatgpt-app`, `chrome`, `claude-app`, `discord`, `fd`, `grokbot`, `lmstudio`, `minikube`, `opencode-app`, `ripgrep`, `slack`, `taplo`, `telegram`, `uv`, `warp`, `zcode`), `setup <name|alias> [--json|--human]` (with optional `setup-` prefix) runs in the core as `software <name> --install --global` when the core's built-in catalog defines the software or `<name>.yaml` exists in `MANAGED_MACHINE_SOURCE_DIR`; otherwise, and for every other name, extra argument, or `setup --list`, `setup` delegates to legacy Bash on macOS. Bootstrap and `--update` still install from `apps.json`. Surviving legacy routes (`setup` for unconverted names and `setup --list`, `catalog`, `adopt`, `add-agent`, `account`, `account doctor`, `ssh`, `--bootstrap`, `--update`, `fleet remove`, and `status --section bootstrap|update`) are delegated on `darwin` by `exec` to `<legacy root>/bin/managed-machine` (`MANAGED_MACHINE_LEGACY_ROOT` when set, else `../libexec/legacy/bin/managed-machine` in Homebrew installs, else `../legacy/bin/managed-machine` in a checkout) with the original argv and environment, failing with `MM_LEGACY_TREE_MISSING` (exit 1; remedy `brew reinstall managed-machine`) if missing or non-executable, and are refused on Linux and Windows with `MM_LEGACY_ROUTE_UNSUPPORTED` (exit 2). Compatibility-period legacy routes removed in `v0.11.0` fail on all five targets with `MM_LEGACY_ROUTE_REMOVED` (exit 2) naming the route and its replacement: `fleet list` -> `managed-machine fleet --keys`; `status --section managed-machine` -> `managed-machine version`; `status --section ssh` -> `managed-machine ssh status`; `status --section agent-accounts` -> `managed-machine account doctor`; `status --section catalog` -> `managed-machine setup --list and managed-machine software --available`; and `status --section machine|local-bin|zsh-functions|pins|toolchain|homebrew|gh|nvm|node|hostname|rustup|rustc|cargo` -> `no replacement`.

Portable core commands (`software`, `config`, bare `status` and `status --section software|config|flavor|fleet`, `fleet`, `skill`, `skill path`, `platform`, `version`) emit compact JSON Lines by default, a JSON document with `--json`, or readable text with `--human`, and emit `MM_<COMMAND>_<CONDITION>` diagnostics on stderr with exit codes `0` (success), `1` (runtime failure), `2` (usage or validation error), `3` (completed evaluation with findings/drift), and `4` (incomplete evaluation). `status --section <name>` accepts a single section per invocation (repeating `--section` or passing a comma-separated list exits 2 with `MM_USAGE`): `software`, `config`, `flavor`, or `fleet` in the core, or `bootstrap` or `update` via legacy delegation on macOS (`status` with no `--section` reports `software`, `config`, and `flavor`; `fleet` is queried via `--section fleet`). Surviving legacy inspection and inventory commands (`status --section bootstrap|update`, `ssh status`, `account doctor`, `catalog list`, `catalog resolve`, `setup --list` or `setup --json`, `fleet remove --dry-run`) accept `--json` for deterministic machine-readable output (`schema_version: 1`). Config-repo-backed legacy JSON reports (`catalog list --json`, `catalog resolve --json`, `setup --list --json`, `fleet remove --dry-run --json`) include `config_checkout` provenance metadata (`path`, `source`, `branch`, `commit`, `upstream_commit`, `dirty`, `refresh_status`, `stale`) so callers can distinguish a freshly rebased checkout from a dirty or offline checkout. When `--json` is supplied on a failing legacy invocation, stdout receives a structured JSON error envelope containing `status` (`not_ready` or `pending_user_action`), `error` (`code`, `message`, `action`, `evidence`), and `checks`, and exit codes follow a uniform contract (`0` ready/success, `1` not ready or hard failure, `75` `EX_TEMPFAIL` when owner/human/GUI/admin action is required).

Setup accepts a canonical catalog `name`, any entry in `aliases`, a Homebrew cask `token`, a wrapper script name (such as `codex-cli` -> `codex`), or a standalone `setup-<name>` script name, with or without a `setup-` prefix. `managed-machine catalog list [--json]` enumerates every supported catalog app and setup script with its canonical `name`, `aliases`, `adopt_aliases`, `kind`, `command`, `auto`, `declined`, and `installed` state (`status`: `installed` | `declined` | `not_installed`), while `managed-machine catalog resolve <name> [--json]` resolves any alias and reports whether `setup` and/or `adopt` accepts it. Both refresh the `managed-machine-config` checkout pull-only by default (pass `--no-refresh` to inspect the on-disk checkout without fetching). `managed-machine setup --list` (or `managed-machine setup` with no name or an invalid name) lists the available setup names with a ✓ beside names already installed. `--help` (and `setup --help`) does not list setup names: it shows the portable core commands first, marks the macOS-only Bash routes `[legacy]`, and points to `setup --list`.

`managed-machine adopt` takes over vendor-installed signed-cask apps (`antigravity`/`antigravity-app`, `antigravity-ide`, `brave-browser`/`brave`, `chatgpt`/`chatgpt-app`, `claude`/`claude-app`, `cursor`, `devin-desktop`/`devin-app`, `discord`, `docker-desktop`/`docker`, `google-chrome`/`chrome`, `grok-bot`/`grokbot`, `kiro`, `kiro-cli`, `lm-studio`/`lmstudio`, `opencode-desktop`/`opencode-app`, `slack`, `telegram`, `visual-studio-code`/`vscode`, `warp`, `zcode`) without mutating a running agent; canonical names for `adopt` are Homebrew cask tokens, and catalog names/aliases (with or without `setup-`) are also accepted. Unknown names print that token/alias list. Skip (do not fail the run) when the app has a Homebrew receipt, is running, is missing, or fails Team ID verification. `setup-*` also skips a vendor occupier instead of failing the install.

## Side effects and retries

Run `managed-machine --version` first. If it is outside `qwts-versions`,
treat this skill as a hint: take behavior from that version's `--help`, and
change nothing it does not vouch for. `managed-machine skill path` prints the
installed release's copy of this skill.

| Class | Commands | Retry |
|---|---|---|
| read-only | `--version`, `version`, `platform`, `--help` (also -h, help, or no command), `skill`, `skill path`, `status`, `software` (--available/--category/--provider/--recipe/--user/--global/--custom), `config` (--available/--category/--global/--user/--custom/--status/--check), `fleet` (--keys/--access/--check), `ssh status`, `account doctor` | Safe to repeat. |
| local-write | `software` (--install/--refresh), `config` (--plan/--apply/--from-plan), `fleet` (--plan/--apply/--from-plan/--refresh/--publish/--unpublish), `catalog list`, `catalog resolve`, `setup --list`, `setup` (no name), `setup <name>`, `adopt`, `--bootstrap`, `--update`, `add-agent`, `account setup` | Idempotent; rerun to converge. Catalog/setup listings refresh the config checkout pull-only unless `--no-refresh` is passed. `--update` refuses agent sessions. |
| remote-write | `ssh enroll` | Human-only; agent sessions are refused. |
| destructive | `fleet remove` | Pass `--dry-run` first to preview exact changes and exit behavior without mutating; run `managed-machine fleet --keys` before any retry of a live removal. |

## Setup scripts

All idempotent; safe to re-run.

A `signed-cask` row always requires a `Developer ID Application` signature whose Team ID matches the row. Bundle integrity passes on `codesign --verify --deep --strict` or, when extraction detritus makes that fail (Chromium-based apps such as Brave), on a Gatekeeper assessment reporting `source=Notarized Developer ID` for the same Team ID. A row with `"allow_rolling_url": true` accepts `sha256 :no_check` for vendors serving one rolling URL (Google Chrome) and must then pass that Gatekeeper notarization check, since no checksum stands behind it; every other refusal still applies.

A `vendor-dmg` row installs a desktop app with no `homebrew/cask` token by fetching the vendor DMG directly (pinned `https` URL on an allowlisted host, or per-arch `url_arm64`/`url_x86_64` pairs; matching DMG `sha256`, or `"no_check"` behind `allow_rolling_url` with mandatory notarization). The staged bundle is signature-verified before anything under `/Applications` moves; a bundle already on disk must first prove its Team ID, and a pinned `version` converges drift. `adopt` stays cask-only: vendor-DMG occupiers converge in place.

Full bootstrap detects whether a controlling terminal is available before any setup step runs. Interactive runs print the execution plan up front (every step, which steps need administrator access, and what packages, `/Applications` bundles, owners, and paths they change), ask the owner to confirm the plan once in the terminal, request a single macOS administrator dialog for the approved plan, and run all privileged steps under that one approval (refusing any privileged action outside the plan). Noninteractive runs never open a dialog and record privileged steps as `skipped`. A step that cannot finish in this run is skipped and is not part of the install; it does not fail bootstrap and does not print a follow-up command. Outcomes (including the approved `plan_id` for privileged steps) are written to `~/.config/managed-machine/bootstrap.manifest`. Catalog rows in `apps.json` install on bootstrap and `--update` unless they set `"auto": false` or were previously declined (recorded in `~/.config/managed-machine/declined-apps.manifest`, which lists them as `not installed (declined)` until the owner explicitly runs `managed-machine setup <name>`). `brew-formula` rows install official `homebrew/core` formulae.

| Script | Purpose |
|---|---|
| setup-brew | Install Homebrew if missing; dedup brew PATH block in .zshenv |
| setup-hostname | Prompt for a Mac hostname and set LocalHostName, ComputerName, and HostName via scutil |
| setup-zsh | Starter zsh dotfiles; backs up stale unguarded/vendor PATH profiles and rewrites guarded blocks |
| setup-nvm | Install NVM and the current Node.js LTS release |
| setup-git-hooks | gitleaks pre-commit for this repo; composes with an existing hooksPath |
| setup-gh | GitHub CLI install + HTTPS auth, git credential helper, git identity, pull-only config-checkout refresh. Never touches SSH — enrollment is `managed-machine ssh enroll` |
| setup-agent-bot / setup-agent-bot-gh | Retired; identity runtime installation and wiring are outside managed-machine |
| setup-bin | Keep local-bin at the pinned ref and link tools into ~/.local/bin |
| setup-zsh-functions | Keep zsh-functions at the pinned ref and install functions, zsh-profile, and the .zshenv loader |
| setup-proton-pass | Proton Pass CLI |
| setup-muse | Meta Muse Code (`muse` CLI) |
| setup-claude | Claude Code (`claude`) |
| setup-codex-cli | OpenAI Codex CLI (`codex`) |
| setup-antigravity | Antigravity CLI (`agy`) |
| setup-grok-build | Grok Build (`grok`) |
| setup-aider | Aider (`aider`); managed `~/.aider.conf.yml` installed only when missing |
| setup-droid | Droid CLI (`droid`) |
| setup-goose | Goose CLI (`goose`); installer runs with `CONFIGURE=false` |
| setup-opencode | OpenCode v2 (`opencode`, pinned 2.0.18; replaces a managed v1 in place) |
| setup-opencode-app | OpenCode desktop app from official homebrew/cask only; verified Team ID |
| setup-codex | Codex managed config: synced defaults in `config.toml` plus opt-in `--profile <name>` variants installed as `~/.codex/<name>.config.toml` (e.g. `muse`); no secrets |
| setup-devin | Devin CLI install plus interactive or deferred authentication |
| setup-lmstudio | LM Studio (Homebrew Cask into `/Applications`; `MANAGED_MACHINE_LMSTUDIO_APPDIR` override) |
| setup-vscode | VS Code from official homebrew/cask only; verified Team ID |
| setup-cursor | Cursor from official homebrew/cask only; verified Team ID |
| setup-claude-app | Claude desktop app from official homebrew/cask only; verified Team ID |
| setup-muse-app | Muse desktop app from Meta's Sparkle feed; verified Team ID |
| setup-antigravity-app | Antigravity hub from official homebrew/cask only; verified Team ID |
| setup-antigravity-ide | Antigravity IDE from official homebrew/cask only; verified Team ID |
| setup-kiro | Kiro from official homebrew/cask only; verified Team ID |
| setup-kiro-cli | Kiro CLI from official homebrew/cask only; verified Team ID |
| setup-rust | rustup + cargo PATH |

Catalog-only harnesses have no script; `managed-machine setup <name>` resolves catalog names and aliases directly: `amp`, `cline`, `copilot`, `deepseek`, `grok` (alias of `grok-build`), `hermes`, `pi`, `qwen`, `warp`, `zcode`.

## Dependencies

- Dotfiles/config sourced from a persistent private checkout under `$XDG_DATA_HOME/managed-machine/` when set, or `~/.local/share/managed-machine/` otherwise; the brew-bundled copy is read-only seed data
- local-bin kept at the pinned ref from `managed-machine-config/local-bin.ref`
- No secrets in repo; auth/keys generated per machine or from macOS Keychain

## SSH enrollment and fleet

Default bootstrap, `--update`, `setup-gh`, and `account setup` never create, upload, or register SSH keys — existing SSH state is always preserved and agent accounts are refused. Enrollment is an explicit human-only action: `managed-machine ssh enroll` requires at least one of `--authentication` (machine key + `~/.ssh` github.com block + agent/keychain import + GitHub authentication-key upload + `git_protocol=ssh`), `--signing` (signing-key upload + global SSH commit/tag signing + `allowed_signers`), or `--fleet` (fleet registry + `authorized_keys` sync). Before mutating, it prints the local account, GitHub login, key fingerprint or creation intent, and selected purposes, then requires the macOS authorization dialog (which may be satisfied by Touch ID or a recent grant — an owner-accepted limit, not a guaranteed fresh password). Cancellation or a headless session leaves state unchanged; there is no `--yes`.

Enrolled keys are created unencrypted so the flow cannot hang on a passphrase prompt. The policy is recorded locally in `~/.config/managed-machine/ssh-key-policy.toml`.

`ssh enroll --fleet` creates local `~/.config/managed-machine/machine.toml` state and a versioned machine entry in the persistent private `managed-machine-config/fleet/machines/` registry. It imports legacy `authorized_keys` records before generating the fleet key file, so existing hosts are preserved. Managed fleet paths are committed and pushed automatically; unrelated private-config edits are never staged. Default setup only refreshes that checkout (fetch + rebase when clean) and never publishes fleet state.

Use `managed-machine fleet --keys` to inspect registered machines (`fleet list` was removed in `v0.11.0` with `MM_LEGACY_ROUTE_REMOVED`). To decommission one, pass the exact machine ID to `managed-machine fleet remove`; add `--yes` for noninteractive confirmation and `--revoke-github` only when the matching authentication/signing keys should also be deleted from the active GitHub account. Successful registration and removal synchronize the private config repository automatically.

## Update

```bash
managed-machine --update
```

Runs `brew update` and upgrades `managed-machine` under one authorization dialog, re-execs into the upgraded tree, then builds and prints the post-upgrade plan (`migrations`, `setup-gh`, `setup-zsh`, `setup-bin`, `setup-zsh-functions`, and auto catalog rows), confirms once in the terminal, and runs any privileged post-upgrade work under a single authorization dialog (recording outcomes and the approved `plan_id` in `~/.config/managed-machine/update.manifest`). Cancelling an authorization dialog stops privileged work for the rest of the run and records declined catalog rows in `~/.config/managed-machine/declined-apps.manifest` so subsequent `--update` runs list them as `not installed (declined)` instead of prompting again. No SSH enrollment happens in this path. Update does not install, upgrade, pin, bootstrap, or repair agent-bot or its GitHub CLI interposition. Existing installations and user data are left intact.

## Release

```bash
scripts/release prepare vX.Y.Z
scripts/release tag vX.Y.Z
```

Two-step PR-first release flow (`scripts/release vX.Y.Z` is retired). `scripts/release prepare vX.Y.Z` runs from clean `main` at `origin/main` (refuses other branches, dirty trees, existing tags, or a version outside this skill's `qwts-versions` — revalidate the skill in a reviewed PR first, ENG-0055; re-runs resume idempotently): it tags the sibling `managed-machine-config` checkout at `vX.Y.Z`, builds the five deterministic archives with `mm-release build`, creates branch `release/vX.Y.Z`, updates `VERSION` and `Formula/managed-machine.rb` (tag, version, `arm64`/`sequoia` constraints, `managed-machine-config` tag/revision, `core` resource URL/sha256, and the inverted install block), verifies with `mm-release verify -require-pin`, commits `Release vX.Y.Z`, pushes `release/vX.Y.Z` only, and prints the `gh pr create` command. After the release PR is reviewed and merged and the owner explicitly confirms, `scripts/release tag vX.Y.Z` runs on clean `main` at `origin/main` to create and push the annotated tag `refs/tags/vX.Y.Z` (triggering `.github/workflows/release.yml`).
