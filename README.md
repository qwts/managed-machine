# managed-machine

Fresh-Mac bootstrap and fleet setup: Homebrew, zsh starter dotfiles, GitHub CLI over HTTPS, explicit opt-in SSH identity/signing/fleet enrollment, gitleaks git hooks, Proton Pass CLI, Meta Muse Code, Claude Code, Codex CLI, Antigravity CLI, Grok Build, Aider, Droid CLI, OpenCode, OpenCode Desktop, Devin CLI, LM Studio, VS Code, Cursor, Claude, Antigravity, Rust (rustup), and host-to-host `authorized_keys` sync.

This repo is the machine manager, distributed as a self-tapped Homebrew formula. Dotfiles and fleet state live in [`qwts/managed-machine-config`](https://github.com/qwts/managed-machine-config). The formula bundles a read-only bootstrap seed, then setup scripts create and use a persistent writable checkout under `$XDG_DATA_HOME/managed-machine/` when set, or `~/.local/share/managed-machine/` otherwise. Utility scripts live in [`qwts/local-bin`](https://github.com/qwts/local-bin), which the formula installs under `$(brew --prefix)/opt/managed-machine/libexec/local-bin` and `setup-bin` keeps at the pinned ref.

---

## Install on a fresh Mac

This repository is private, so the installer is fetched through an
authenticated GitHub CLI ([install `gh`](https://cli.github.com) and run
`gh auth login` first):

```bash
gh api -H "Accept: application/vnd.github.raw" repos/qwts/managed-machine/contents/install.sh | bash
```

Without authentication the fetch fails with gh's explicit login instructions
(a plain `curl` of a private repository returns a misleading 404).

The installer:
1. Installs Homebrew if missing.
2. Verifies Homebrew prefix ownership: an admin-group owner (typically `admin`)
   is preserved. The installer never `chown`s `/opt/homebrew` to a non-admin
   invoking user. Mutating `brew` commands run as the prefix owner through the
   macOS authorization dialog.
3. Installs `gh` if missing, verifies GitHub authentication, and wires gh as the
   git credential helper so private repositories clone over HTTPS — no SSH key
   is needed, and none is provisioned; SSH enrollment is a separate explicit
   step (`managed-machine ssh enroll`).
4. Taps `qwts/managed-machine` over authenticated HTTPS, trusts the tap when
   Homebrew requires explicit tap trust (announced, scoped to this tap), and
   installs the formula.
5. Runs `managed-machine --bootstrap`. Administrator dialogs during this run are part of the install. Steps that cannot finish in this run are skipped, not failed, and do not assign a follow-up command.

If `managed-machine` is already installed, the installer updates it and tells you to use the CLI directly.

---

## Usage

```bash
managed-machine              # full bootstrap; terminal mode is auto-detected
managed-machine --bootstrap --interactive
managed-machine --bootstrap --non-interactive
managed-machine --update     # brew update/upgrade + re-run safe setup steps
managed-machine status       # installed versions and pins (read-only)
managed-machine setup bin       # preferred: run setup-bin
managed-machine setup setup-bin # compatible explicit script-name form
managed-machine adopt           # adopt vendor-installed desktop apps into Homebrew
managed-machine adopt vscode    # one app (cask token or alias)
managed-machine fleet list   # list registered machines
managed-machine fleet remove <machine-id> [--yes] [--revoke-github]
managed-machine add-agent qwts-goose-agent   # provision a per-harness agent account
managed-machine add-agent qwts-claude-agent --with-harness   # …and install its harness as the account
managed-machine account setup qwts-claude-agent [--json]
managed-machine account doctor qwts-claude-agent [--json]
managed-machine --help       # show usage
```

`managed-machine status` is read-only: it prints the formula version, last bootstrap and `--update` outcomes, the local-bin pin, and versions of the tools setup scripts manage. Missing tools are listed as missing; nothing is installed or upgraded.

---

## Setup scripts

All setup scripts are safe to re-run.

Which desktop apps and CLIs to install is declared in `managed-machine-config/apps.json`. managed-machine ships the install engines (signed cask, direct vendor DMG, official CLI, `brew-formula` for `homebrew/core`, and so on). After an app is present, bootstrap runs `managed-machine-config/config/<name>` when that script exists — configuration only, never install. Catalog rows install on bootstrap and `--update` unless they set `"auto": false`; those are `managed-machine setup <name>` only. Adding ChatGPT is a catalog row (and an optional config script); it does not require a managed-machine release. A new *kind* of installer does.

### Signed-cask verification

A `signed-cask` row is trusted only after the cask resolves to the exact `homebrew/cask` token, the download and homepage hosts match the row's allowlists, and the installed bundle proves its identity: a `Developer ID Application` authority whose Team ID equals the row's `team_id`. Identity is never waived.

Bundle integrity is satisfied by either check:

1. `codesign --verify --deep --strict`, or
2. the Gatekeeper assessment (`spctl -a -t exec`), which must report `source=Notarized Developer ID` and name the same Team ID.

The second path exists because `--deep --strict` rejects a bundle whose nested helpers carry `com.apple.FinderInfo`/quarantine attributes — *"resource fork, Finder information, or similar detritus not allowed"*. Homebrew Cask stamps those attributes during extraction, so Chromium-based apps such as Brave fail a check their signature and notarization both pass. Gatekeeper additionally proves notarization, which `codesign` never checks, so the fallback is stricter than the primary path in that respect.

### Rolling vendor URLs

Casks normally must publish a real `sha256`; `:no_check` is refused. Some vendors (Google Chrome, for one) serve a single rolling "latest" URL, so `homebrew/cask` has no per-build checksum to publish. Such a row opts in explicitly:

```json
{ "name": "chrome", "kind": "signed-cask", "token": "google-chrome",
  "team_id": "EQHXZ8M8AV", "allow_rolling_url": true }
```

The flag accepts `:no_check` and nothing else — a malformed digest, an off-allowlist host, an unnotarized bundle, or a Team ID mismatch is still refused. It trades the install-time checksum for notarized Developer ID verification, so a row carrying it **must** pass the Gatekeeper assessment: the `codesign --deep --strict` shortcut above does not apply, because passing it alone would install a build with neither a checksum nor a notarization behind it.

Note that every auto-updating app in the catalog already relies on that same guarantee for each update after the first, since the install-time checksum covers only the initial download.

### Direct vendor DMGs

A `vendor-dmg` row installs a desktop app with no `homebrew/cask` token by
fetching the vendor's DMG directly — no Homebrew involved at any step:

```json
{ "name": "qwen-desktop", "kind": "vendor-dmg",
  "app_name": "Qwen Code Desktop.app", "team_id": "NF4574S59H",
  "url_arm64": "https://github.com/.../Qwen-Code-Desktop-arm64.dmg",
  "url_x86_64": "https://github.com/.../Qwen-Code-Desktop-x64.dmg",
  "url_hosts": ["github.com"], "sha256": "no_check",
  "allow_rolling_url": true }
```

The URL must be `https` on a `url_hosts` allowlist entry, and the download's
`sha256` must match — or be `"no_check"` behind `allow_rolling_url`, with the
same mandatory-notarization trade as above. Per-architecture builds use
`url_arm64`/`url_x86_64` (plus `sha256_arm64`/`sha256_x86_64` when the digests
differ); a single `url` serves every architecture. The staged bundle is
signature-verified before anything under `/Applications` moves, and a bundle
already on disk must first prove its Team ID — an impostor is reported, never
replaced. A pinned `version` converges drift (reinstalls on mismatch);
without one, presence plus a valid signature is installed. `managed-machine
adopt` stays cask-only: vendor-DMG occupiers converge in place, so there is
nothing to adopt.

### Interactive and noninteractive bootstrap

Bootstrap uses interactive mode when it can open the current terminal, including a controlling terminal behind a curl pipe. Otherwise it uses noninteractive mode. `--interactive` requires a terminal. `--non-interactive` never presents a dialog.

Privileged work uses the standard macOS administrator dialog (`osascript` / Authorization Services). That dialog is part of auto-install. Mutating Homebrew commands run as the prefix owner (`admin` when that account exists).

A step that cannot finish in the current run is skipped and is not part of that install. Bootstrap does not fail that step and does not print a `managed-machine setup <name>` follow-up. Failed is reserved for unexpected errors. Vendor-installed desktop apps already in `/Applications` are skipped. Bootstrap, `--update`, and `setup-gh` never create, upload, or register SSH keys; enrolling SSH identity is the explicit human-only `managed-machine ssh enroll` action.

The latest machine-readable result is atomically written with mode-600 permissions to `~/.config/managed-machine/bootstrap.manifest`. It contains only step names, statuses, short reasons, and timestamps—never command output or secrets.

`setup-devin` installs the CLI in every mode. Browser authentication is skipped when no interactive terminal is available; an already-authenticated install is left alone.

| Script | Purpose |
|---|---|
| `setup-brew` | Install Homebrew if missing (wires `brew shellenv` into your shell). |
| `setup-hostname` | Prompt (macOS dialog) for a hostname and set `LocalHostName`, `ComputerName`, and `HostName` (`name.lan`) via `scutil`. Re-run `managed-machine setup hostname` to correct a bad name. |
| `setup-zsh` | Install starter `~/.zshenv`, `~/.zprofile`, `~/.zshrc`. Unguarded or vendor PATH fragments are moved to `<name>.<epoch>.bak` and rewritten with duplicate-entry guards; already-guarded files are left in place. |
| `setup-nvm` | Install upstream NVM, add a managed zsh initialization block, install the current Node.js LTS release, and make it the default. |
| `setup-git-hooks` | Install gitleaks via brew. On a managed-machine git clone, wire pre-commit scanning without replacing an existing `core.hooksPath` (agent-bot is chained). From a Homebrew install this step skips hook wiring — libexec is not the git toplevel. |
| `setup-gh` | Install GitHub CLI via brew; authenticate gh over HTTPS; wire gh as the git credential helper; configure the Git identity (login + private noreply); refresh the persistent private config checkout (pull-only). Never touches SSH keys, signing, or the fleet registry — see `managed-machine ssh enroll`. |
| `setup-agent-bot` | Install the reviewed [agent-bot](https://github.com/qwts/agent-bot-identity) identity runtime from its self-tap, `brew pin` it, and wire the machine (`agent-bot bootstrap --machine-only`, then `doctor`) so agents commit as their own App instead of inheriting the human git identity. Runs after `setup-gh`, whose GitHub auth the private tap fetch rides on; re-runs verify, and upgrade the pinned runtime when the tap publishes a newer tag. A noninteractive bootstrap skips it until the runtime is installed. |
| `setup-agent-bot-gh` | Explicitly interpose Homebrew `gh` for Codex desktop through agent-bot; pass `--restore` to restore stock `gh`. Opt-in only: never runs during initial bootstrap, though its precondition (the runtime, from `setup-agent-bot`) is met by then. |
| `setup-bin` | Keep local-bin at the pin read from the persistent `managed-machine-config/local-bin.ref`, then run its `install` (links tools into `~/.local/bin`, prunes renames, ensures `~/.local/bin` on `PATH`). |
| `setup-proton-pass` | Install the [Proton Pass CLI](https://proton.me/pass/cli) when missing (lands in `~/.local/bin`). |
| `setup-muse` | Install [Meta Muse Code](https://dev.meta.ai/) (`muse` CLI) when missing via the official installer. Lands in `~/.local/bin`; skips the installer's PATH edit because that directory is already managed. |
| `setup-claude` | Install [Claude Code](https://code.claude.com/docs/en/quickstart) (`claude`) when missing via the official installer. |
| `setup-codex-cli` | Install the [OpenAI Codex CLI](https://github.com/openai/codex) (`codex`) when missing. Distinct from `setup-codex`, which only merges Muse Spark config. |
| `setup-antigravity` | Install the [Antigravity CLI](https://antigravity.google/docs/cli/install) (`agy`) when missing via the official installer. Lands in `~/.local/bin`. |
| `setup-grok-build` | Install [Grok Build](https://x.ai/build) (`grok`) when missing via the official installer. Lands in `~/.local/bin`; the installer runs with no login shell to edit, so its PATH block never lands in `~/.zshrc` (every official installer runs that way, and one that edits `~/.zshrc` anyway is reported). |
| `setup-aider` | Install [Aider](https://aider.chat/) (`aider`) when missing via the official installer, which is the uv bootstrapper (runs with `UV_NO_MODIFY_PATH=1`; lands in `~/.local/bin`). Then installs a managed `~/.aider.conf.yml` when you have none — it sets `git-commit-verify: true`, because aider otherwise commits with `--no-verify` and skips the gitleaks pre-commit hook. An existing config is never touched. |
| `setup-droid` | Install [Droid CLI](https://docs.factory.ai/droid-cli/quickstart) (`droid`) when missing via the official installer. Lands a checksum-verified binary in `~/.local/bin`; the installer takes no prompts and edits no shell rc file. |
| `setup-opencode` | Install [OpenCode](https://opencode.ai/) (`opencode`) when missing. Links `~/.opencode/bin` into `~/.local/bin` and skips the installer's PATH edit. |
| `setup-opencode-app` | Install the [OpenCode](https://opencode.ai/download) desktop app from `homebrew/cask/opencode-desktop` with the same signed-cask checks (Anomaly Team ID `5NZ4Q7NXJ4`). |
| `setup-codex` | Install Codex *with* Meta's Muse Spark config (`meta-models.json` + `model_catalog_json`, no secrets, auth stays in Keychain). The provider fragment merges idempotently into a managed block of `~/.codex/config.toml`; conflicting user-set keys are never clobbered — setup reports the exact manual merge and defers instead. |
| `setup-devin` | Install the [Devin CLI](https://docs.devin.ai/cli) into `~/.local/bin`; preserve authenticated sessions, run setup interactively when needed, or report authentication as deferred. |
| `setup-lmstudio` | Install [LM Studio](https://lmstudio.ai/) via Homebrew Cask when missing. Installs to `/Applications` (override with `MANAGED_MACHINE_LMSTUDIO_APPDIR`). |
| `setup-vscode` | Install [Visual Studio Code](https://code.visualstudio.com/) from `homebrew/cask/visual-studio-code` only after verifying tap, sha256, vendor download host, and Microsoft Team ID `UBF8T346G9`. |
| `setup-cursor` | Install [Cursor](https://www.cursor.com/) from `homebrew/cask/cursor` with the same signed-cask checks (Anysphere Team ID `VDXQ22DGB9`). |
| `setup-claude-app` | Install the [Claude](https://claude.com/download) desktop app from `homebrew/cask/claude` (Anthropic Team ID `Q6L2SF6YDW`). |
| `setup-antigravity-app` | Install the [Antigravity](https://antigravity.google/) hub from `homebrew/cask/antigravity` (Google Team ID `EQHXZ8M8AV`). |
| `setup-antigravity-ide` | Install [Antigravity IDE](https://antigravity.google/product/antigravity-ide) from `homebrew/cask/antigravity-ide` (same Google Team ID). |
| `setup-kiro` | Install [Kiro](https://kiro.dev/) from `homebrew/cask/kiro` with the same signed-cask checks (Team ID `94KV3E626L`). |
| `setup-kiro-cli` | Install the [Kiro CLI](https://kiro.dev/) from `homebrew/cask/kiro-cli` (same Team ID). |
| `setup-rust` | Install [rustup](https://rustup.rs/) when missing (default profile: stable + rustfmt/clippy); ensure `${CARGO_HOME:-~/.cargo}/bin` on `PATH`. |

Existing vendor-installed desktop apps are not mutated by `setup-*`. Use `managed-machine adopt` to take them over with Homebrew:

```bash
managed-machine adopt              # every allowlisted app that is safe to adopt
managed-machine adopt <name>       # one app (cask token or alias)
```

Canonical names are cask tokens; setup-name aliases are accepted. `--help` and unknown names print this list:

- `visual-studio-code` (alias: `vscode`)
- `cursor`
- `claude` (alias: `claude-app`) — desktop app, not Claude Code CLI
- `antigravity` (alias: `antigravity-app`) — hub, not `agy` CLI
- `antigravity-ide`
- `kiro`
- `kiro-cli`
- `opencode-desktop` (alias: `opencode-app`) — desktop app, not OpenCode CLI

Adopt skips (does not fail the whole run) when the app already has a Homebrew receipt, is running, is missing, or fails Developer ID / Team ID verification. A running Cursor helper that still has `/Applications/Cursor.app` mapped is treated as running: quit the app and re-run. Apps stay in `/Applications`; brew runs as the prefix owner when this user cannot write the prefix. `setup-*` / catalog config is re-run afterward so signature checks pass. Adopt covers signed-cask rows only; `vendor-dmg` rows converge a vendor-installed bundle in place during install, so there is nothing to adopt.

---

## SSH enrollment

SSH identity is **never** provisioned by default setup: `install.sh`, `--bootstrap`, `--update`, `setup-gh`, and `account setup` do not generate keys, upload them, load `ssh-agent`/Keychain, request key-upload scopes, switch `git_protocol` to SSH, configure SSH signing, or register fleet identity. Existing keys, GitHub registrations, `~/.ssh/config`, and signing settings are always preserved — nothing is removed, revoked, or rotated automatically. Agent accounts cannot enroll at all; `account setup` never gives them SSH keys.

Enrollment is a separate, explicit, human-authorized action:

```bash
managed-machine ssh enroll --authentication
managed-machine ssh enroll --signing
managed-machine ssh enroll --fleet
managed-machine ssh enroll --authentication --signing --fleet   # any combination
managed-machine ssh status                                       # read-only report
```

At least one purpose flag is required; purposes are never silently enabled together:

- `--authentication` creates (or reuses) `~/.ssh/id_rsa_github`, adds the `Host github.com` block to `~/.ssh/config` when absent, loads the key into `ssh-agent`/Keychain, uploads the public key as a GitHub *authentication* key, and sets `gh`'s `git_protocol` to `ssh`.
- `--signing` uploads the same public key as a GitHub *signing* key, enables global SSH commit/tag signing (`gpg.format`, `user.signingkey`, `commit.gpgsign`, `tag.gpgSign`, `gpg.ssh.allowedSignersFile`), refreshes the managed `allowed_signers` block from the fleet registry, and adds the local key outside that block so this machine's own commits verify even without fleet enrollment.
- `--fleet` writes local identity state to `~/.config/managed-machine/machine.toml`, registers the machine in the private `managed-machine-config/fleet/machines/` registry, publishes managed fleet state, and syncs local `authorized_keys`.

Before any change the command prints the plan — local account, home, GitHub login, key fingerprint or creation intent, and the selected purposes — then asks through the macOS authorization dialog, which names the same account, login, and purposes. Cancellation, a headless/noninteractive session, or an identity mismatch leaves all state unchanged and reports that no enrollment occurred. There is no `--yes` and no environment opt-out; an agent session, an account in the OS-level `agents` group, or an account named in the agent roster is refused before any dialog.

The authorization gate uses `osascript`'s administrator dialog (`Authorization Services`). Per the owner's decision on #120, the ~5-minute grant cache and Touch ID are accepted: the dialog is an OS-native human confirmation, not a guaranteed fresh-password challenge. Every mutation still runs unprivileged as the invoking account, so an administrator approving for a standard human user cannot land keys under the admin home.

`ssh enroll` is retry-safe: whether a key is already registered is read from the account's public key listings (which need no scope), the match is an exact key-body match, and a listing failure stops the run rather than uploading a duplicate. Upload scopes (`admin:public_key`, `admin:ssh_signing_key`, each reported with the operation that needs it) are requested only here and only when a key actually has to be uploaded — never during default setup. Partial failures report which purposes completed; re-running finishes the rest.

### SSH key policy

Enrolled GitHub SSH keys are created unencrypted (`ssh-keygen -N ''`) so the flow cannot hang on a passphrase prompt. The choice is recorded locally in mode-600 `~/.config/managed-machine/ssh-key-policy.toml` and is never committed. Existing complete key pairs are reused unchanged.

Machine IDs are stable, filesystem-safe forms of the SSH public-key SHA-256 fingerprint. Initial registration timestamps and bootstrap refs are preserved on reruns.

Commit signatures verify locally as well as on GitHub: `--signing` sets `gpg.ssh.allowedSignersFile` to `~/.ssh/allowed_signers` and rewrites a `# BEGIN managed-machine` block in that file from the fleet registry, one entry per machine key under the configured git email principal. Lines outside the block are never touched, and re-running `ssh enroll --signing` (or `fleet remove`) refreshes the block after fleet changes, so `git log --show-signature` verifies commits from every registered machine.

On the first fleet enrollment, existing keys in `managed-machine-config/ssh/authorized_keys` are imported as legacy machine records before that file is regenerated. Registration and removal stage only fleet machine records and the generated allowlist, create a commit using the configured Git identity, and push automatically. Concurrent joins rebase unique machine records and regenerate `authorized_keys`; unrelated changes or non-generated conflicts stop without being staged. Default setup only *refreshes* the config checkout (fetch + rebase when clean); publishing managed fleet state always goes through `ssh enroll --fleet` or `managed-machine fleet`.

The default writable checkout is `$XDG_DATA_HOME/managed-machine/managed-machine-config` when `XDG_DATA_HOME` is set, or `~/.local/share/managed-machine/managed-machine-config` otherwise. Set `CONFIG_REPO_ROOT` to use an explicit existing checkout, or `MANAGED_MACHINE_CONFIG_REPO_URL` to change the repository cloned from the bundled seed. Embedded credentials in HTTP remote URLs are rejected.

```bash
managed-machine fleet list
managed-machine fleet remove sha256-...             # prompts for confirmation
managed-machine fleet remove sha256-... --yes       # explicit noninteractive removal
managed-machine fleet remove sha256-... --revoke-github
```

Removal deletes and publishes the exact registry entry, regenerates fleet/local `authorized_keys`, and removes local `machine.toml` when decommissioning the current machine. GitHub authentication and signing keys are retained unless `--revoke-github` is supplied. Revocation runs only after the fleet removal is pushed; if GitHub revocation fails, the public key is retained under `~/.config/managed-machine/pending-github-key-revocations/` so rerunning the same command can finish safely.

The private config repository is the supported fleet backend. Gist, synced-folder, and database backends are intentionally deferred.

---

## Agent accounts

[ENG-0339](https://github.com/qwts/playbook-engineering/blob/main/docs/decisions/ENG-0339-os-account-determines-persona.md) moves the agent/human persona boundary to the macOS account: one **standard** account per harness, short name = the harness-level roster slug, full name = the persona. The account name is the whole mapping — no registry file exists beyond the roster plus that convention.

```bash
managed-machine add-agent qwts-goose-agent
managed-machine add-agent qwts-devin-agent --full-name Devin
managed-machine add-agent qwts-claude-agent --with-harness
```

The slug is validated against the organization roster (the installed agent-bot `config.json`, a profile at `~/.config/managed-machine/organization-profile.json`, or `MANAGED_MACHINE_ORG_PROFILE`); unknown and retired slugs fail closed. Creation is one elevated phase (`sysadminctl` + `createhomedir`, then `dseditgroup` and `dscl`) with a random throwaway password generated inside the elevated shell and never shown or stored — agent accounts sign into nothing (ENG-0339 §6), so set a real login password afterward in System Settings before fast-user-switching into the account. The uid and gid are the OS's to assign: nothing in the identity chain keys on them, and macOS refuses to change or delete a local record afterwards even for root reached through the authorization dialog, so `add-agent` never depends on a particular number. The same phase adds the account to the `agents` group (created on first use) and installs the App's GitHub avatar as the account picture under `/Library/User Pictures/agents/<slug>.png`. The avatar URL comes from what agent-bot cached in your own `~/.config/<slug>/bot-avatar-url`, or failing that the public GitHub users API for `<slug>[bot]` over plain `curl` (no `gh`, so it works from an agent session too; `gh` is the last resort when the anonymous API is rate-limited), and a URL the API supplied is cached in that same file for later runs; when nothing resolves the account is still provisioned and the report warns. A pre-existing record whose home directory is missing gets it created (`createhomedir`) before the key is seeded; if that is impossible the elevated phase says so instead of failing silently. The same phase seeds the App's key material from your own `~/.config/<slug>/` (`app-id` and `private-key.pem`, fetched from the secret provider beforehand with `agent-bot ensure-private-key --app <slug>`; you keep every harness's key so tasks can be delegated from one account) into the agent home, owned by the account at 0700/0600, then runs `agent-bot bootstrap --machine-only --scope-app <slug>` and `doctor` as the account (roster scoped to that one App; the launchd supervisor load is skipped because a never-logged-in account has no session to load it into, so its unit starts on first login and doctor reports the daemon as a warning). This needs agent-bot 0.3.3 or newer. The agent home is opaque to you afterwards, so convergence is judged from non-secret markers root records under `/Library/Application Support/managed-machine/agents/`: a fingerprint of the seeded key and the account's own doctor verdict. Reruns verify and report instead of recreating, and only prompt for authorization again when the group membership, picture, key material, or wiring verdict has drifted — rotating the key in your `~/.config/<slug>` is enough to have the next rerun reseed it.

The compliance report checks: account exists, is standard (admin membership is a hard failure), is in the `agents` group (a failure), has a picture (a warning), name and home converge, `agent-bot` is installed, the App key is seeded and matches yours, the account's doctor verdict is ready (a not-ready verdict is reported with agent-bot's own code and fix), and the shared coordination space (`/Users/Shared/Public` with its non-sticky `agent-locks` area) exists. Identity wiring itself stays behind the `agent-bot` contract — this command never mints, pins, or resolves identity. When Little Snitch is installed the report reminds you to pre-seed allow rules: its alerts render only in the running user's GUI session, so an unseeded switched-out account hangs silently on first network access.

`managed-machine status` lists every roster account with a recorded snapshot and compliance flags. A legacy agent-bot success is labeled `identity-only ready snapshot` with account/harness checks unverified; only a complete account-setup report can produce an `account ready snapshot`. The header counts full ready snapshots, not live readiness. Admin/group membership, missing picture/home, and key-seeding drift remain visible, alongside unprovisioned and retired accounts. Status reads directory records and non-secret markers only — no dialog, nothing from an agent home. Use `account doctor` for a fresh result.

### Account setup and live readiness

ENG-0339 also gives every agent account its **own harness** and account-local environment. For an already existing, active roster account, run:

```bash
managed-machine account setup qwts-claude-agent
managed-machine account doctor qwts-claude-agent --json
```

The public forms are `managed-machine account setup <active-existing-roster-account> [--json]` and `managed-machine account doctor <account> [--json]`. These commands require the target account's UID and HOME, or explicit administrator authorization through the existing cross-account mechanism. Merely changing HOME does not authorize another account, and there is no passwordless `su` path.

`account setup` prepares only the selected account's shell/local-bin configuration and roster-selected harness, using the bundled config seed where available without requiring human GitHub login. A harness with a separate CLI setup uses that CLI target (`codex-cli` or `kiro-cli` rather than its dotfile or IDE setup). Shared Homebrew, formulae, and casks remain **admin-owned prerequisites**: the account flow does not mutate Homebrew as the agent or change its ownership. Desktop casks remain machine-wide in `/Applications`. Missing bundles or unsupported catalog installers report pending work or failure rather than silently succeeding. The account flow does not run human `setup-gh`, SSH setup, fleet registration, full bootstrap, or `--update`, and it does not install unrelated harnesses. Generic `config/<name>` post-install scripts are not run in account mode: the harness uses its native defaults plus explicit agent-bot wiring, rather than inheriting potentially human-specific setup actions.

`account doctor` is a **live, read-only** per-account check of the shell environment, selected harness, and agent-bot identity readiness; it does not install or repair anything. Both commands use the same readiness report, with optional machine-readable JSON. Exit status is `0` for `ready`, `75` for `pending_user_action`, and `1` for `not_ready` (failed required checks). In a headless session, readiness requiring a GUI login remains `pending_user_action`, not an installation failure or a false ready result. Identity operations stay behind agent-bot's stable CLI, including its live App credential verification; managed-machine does not reimplement minting, pinning, or identity resolution. Vendor sign-in is explicitly not tested. Complete automated readiness currently covers supported account-local CLI harnesses; shared-formula and desktop-only rows require admin/attended verification and are not certified ready here.

On later runs, the default persistent account config can fast-forward from a newer clean bundled seed using local-only Git transport. Dirty or divergent target changes are preserved and reported for reconciliation; an explicit target-owned `CONFIG_REPO_ROOT` is an override, not silently refreshed. If no seed is available, an existing valid checkout is retained with an explicit no-refresh notice.

Doctor requires local-bin's recorded commit and canonical checkout path, verifies the checkout is still at that commit with clean Git state, and checks managed command links against its tracked executable files. Older ref-only manifests need account setup to establish this evidence. Credential readiness is scoped to the requested account; unrelated App failures do not block it, while genuine machine failures and malformed runtime reports still do.

Setup reruns its preparation and live checks even if an older harness success marker exists: cached success is not proof of current readiness. `add-agent <slug> --with-harness` delegates to this account setup phase through its existing administrator authorization. Without `--with-harness`, provisioning retains its opt-in behavior and does not run account setup. `managed-machine status` remains a recorded summary, not a substitute for `account doctor`. IDE-bound harnesses stay attended-only (ENG-0339 §8); log into the account to finish GUI-dependent readiness.

Identity, not directory, decides whose work a checkout is: inside a `qwts-*-agent` account every checkout is bot work; in the owner's account a harness is the owner's delegate unless told to act as the bot (`GH_AGENT_APP`, `--app`, or a worktree pin). Worktree directories are layout only. The runtime-side retirement of the old directory guards is tracked in qwts/agent-bot-identity#187.

Decommissioning an account (`remove-agent`) is deliberately deferred: stop sessions, revoke key material, archive audit metadata, remove the account, and retire the roster row in governance — tracked in qwts/managed-machine#80.

---

## Pinning local-bin

`managed-machine-config/local-bin.ref` records the local-bin ref this machine should run. The pin must be an **immutable ref** — a git tag (e.g. `v0.1.0`) or a commit SHA — so re-running setup with the same pin is reproducible. A moving branch fails with a clear error; the explicit escape hatch for a one-off run is:

```bash
MANAGED_MACHINE_ALLOW_BRANCH_PIN=1 managed-machine setup bin
```

`setup-bin` checks out the pinned ref (skipping `git fetch` entirely when the immutable pin is already checked out) and runs local-bin's `install`. Every run records the pin and the exact commit it resolved to in mode-600 `~/.config/managed-machine/local-bin.manifest`, which is never committed.

Override the pin for a single run:

```bash
HOME_BIN_REF=v0.2.0 managed-machine setup bin
```

Bump the pin by editing `managed-machine-config/local-bin.ref` and committing it in `managed-machine-config`. Publish new local-bin versions as git tags; managed-machine tracks them by ref.

---

## Update an existing machine

```bash
managed-machine --update
```

Runs `brew update`, upgrades `managed-machine` if a new version is available, then re-runs safe setup steps (`setup-gh`, `setup-agent-bot`, `setup-zsh`, `setup-bin`). Every step's outcome is recorded in `~/.config/managed-machine/update.manifest` (same shape as the bootstrap manifest) and shown by `managed-machine status`; a failed step no longer stops the steps after it, and the run exits nonzero at the end naming what failed. An update authenticates to GitHub as you throughout (brew fetches the private fleet taps with your `gh` token, `setup-gh` checks and refreshes your account), so it refuses to run from an agent session (Claude Code, Codex, Cursor, Copilot, Devin, and the other harness markers agent-bot's gh shim reads, or a `qwts-*-agent` account) before spending any dialog: the human's `gh` is not available there — the shim refuses it to a harness, and an agent account signs into nothing as the human ([ENG-0339](https://github.com/qwts/playbook-engineering/blob/main/docs/decisions/ENG-0339-os-account-determines-persona.md)) — so brew would fetch the private taps unauthenticated and `setup-gh` would abort. Run `--update` from a human Terminal in the owner's account. When brew does fail to authenticate a private tap (as the prefix owner without your token), managed-machine says so and tells you not to untap it, instead of relaying brew's untap advice. The persistent private checkout survives formula upgrades and is synchronized before zsh templates and the local-bin pin are read. `setup-zsh` refreshes stale PATH profiles and is a no-op on already-guarded files. Machines that explicitly enabled agent-bot Homebrew interposition run `setup-agent-bot-gh` last, repairing Homebrew relinks and restoring the shell shim after zsh refreshes. Machines without the opt-in marker remain unchanged.

The agent-bot runtime is deliberately not part of `--update`'s `brew upgrade`: it is `brew pin`ned so an unrelated update never moves the identity runtime. Moving it is `setup-agent-bot`'s job, which `--update` runs right after `setup-gh` (and `managed-machine setup agent-bot` runs alone). When the tap repo publishes a newer tag than the installed version, that step refreshes the tap checkout if it is behind (`brew update`), unpins, upgrades to the tagged release, and re-pins, all behind one authorization prompt, then re-runs the machine wiring so the identity daemon restarts on the new runtime. The version check fetches the formula file from the tap repo's main branch (falling back to the tap checkout on disk when offline), never loads the formula (Homebrew's tap trust is per user), and needs no prompt, so `setup agent-bot` works on its own without a preceding `--update`. A developer-checkout link left at `~/.local/bin/agent-bot` is parked next to itself as `agent-bot.devlink-<timestamp>` (the checkout is untouched; move the link back to undo) and the install continues, since the wiring relinks that path to the reviewed runtime anyway.

### Codex desktop GitHub identity

Stock Homebrew `gh` remains the human CLI by default. After the reviewed
agent-bot runtime is installed, explicitly enable direct Codex desktop coverage:

```bash
managed-machine setup agent-bot-gh
```

This preserves stock `gh` beside the interposer as `gh.agent-bot-real` and
records the opt-in under `~/.config/managed-machine/`. Human shells and
unrelated applications still pass through to that exact stock executable;
agent-bot supplies the configured App identity only for recognized agent and
Codex desktop contexts. `managed-machine --update` and later `setup-gh` runs
repair the interposer only when this marker exists.

Restore the preserved Homebrew CLI and remove the opt-in marker with:

```bash
managed-machine setup agent-bot-gh --restore
```

---

## Releasing a new version

The formula is pinned to a git tag. One command bumps the formula tag/version and the skill metadata version together, commits, tags, and pushes — so the two can never drift and the tagged commit contains the formula pointing at its own tag:

```bash
scripts/release vX.Y.Z
```

The script requires a clean working tree and refuses to reuse an existing tag.

---

## Layout

```
managed-machine/
├── Formula/
│   └── managed-machine.rb    # self-tapped Homebrew formula (tag/sha pinned)
├── bin/
│   └── managed-machine       # CLI entry point
├── scripts/
│   ├── adopt                 # take over vendor-installed signed-cask apps
│   ├── bootstrap             # run all setup-* in order
│   ├── fleet                 # list and decommission fleet machines
│   ├── release               # bump formula+skill versions, tag, push
│   ├── status                # read-only installed versions and pins
│   └── update                # brew update/upgrade + safe setup re-runs
├── install.sh                # curlable one-shot installer
├── setup-brew
├── setup-zsh                 # sources dotfiles from the persistent private checkout
├── setup-nvm                 # installs NVM and the current Node.js LTS release
├── setup-gh                  # synchronizes private fleet records and authorized_keys
├── setup-bin                 # local-bin orchestrator; pin from managed-machine-config
├── setup-proton-pass
├── setup-muse
├── setup-claude
├── setup-codex-cli           # OpenAI Codex CLI binary
├── setup-antigravity
├── setup-grok-build
├── setup-aider
├── setup-droid
├── setup-opencode
├── setup-opencode-app
├── setup-devin
├── setup-codex               # sources dotfiles from ../managed-machine-config/dotfiles/codex/meta
├── setup-lmstudio
├── setup-vscode
├── setup-cursor
├── setup-claude-app
├── setup-antigravity-app
├── setup-antigravity-ide
├── setup-kiro
├── setup-kiro-cli
├── setup-rust
├── setup-git-hooks
├── lib/
│   ├── install.sh            # shared bootstrap helpers
│   ├── cask-app.sh           # signed Homebrew cask app installs and adopt
│   ├── vendor-dmg.sh         # direct vendor DMG downloads and installs
│   ├── config-repo.sh        # persistent private checkout + safe git synchronization
│   └── fleet.sh              # machine identity and private fleet registry
├── git-hooks/                # gitleaks pre-commit; setup-git-hooks chains an existing hooksPath
└── skills/
    ├── managed-machine/SKILL.md    # agent skill for bootstrap and update
    └── onboard-harness/SKILL.md    # agent skill for onboarding new harnesses
```

Local identity and manifests live under `~/.config/managed-machine/`; the private config git checkout lives under `$XDG_DATA_HOME/managed-machine/` when set, or `~/.local/share/managed-machine/` otherwise. The `~/.local/bin` PATH block in `~/.zshrc` uses the `# BEGIN local-bin` markers (shared with local-bin's `install`) so existing machines need no PATH migration. The NVM initialization block uses `# BEGIN nvm` markers and manages `NVM_DIR` (default `~/.nvm`). The cargo PATH block uses `# BEGIN rustup` markers and honors `CARGO_HOME` (default `~/.cargo`). All three blocks are guarded so nested shells that inherit PATH never prepend a duplicate entry (the nvm block loads `nvm.sh --no-use` when `node` already resolves under `NVM_DIR`, and activates normally otherwise so the configured version wins over a system node). The `~/.ssh/authorized_keys` block uses `# BEGIN managed-machine` markers; `ssh enroll --fleet` rewrites the legacy `# BEGIN local-bin new-machine` block in place on first sync.
