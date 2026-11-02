# ADR: agent-machine executor

Status: proposed. The owning copy belongs in managed-machine-documentation.
This file is the review draft because that repository is not visible from
this worktree. The durable behavior spec remains
[machine-daemon.md](../plans/machine-daemon.md).

## Decision

agent-machine is a separate executor. It is not a component of
agent-daemon. agent-daemon is the local control plane and owns
agent-identity, agent-soul, and agent-comms. Terminals reach agent-daemon
through agent-bot-cli. HQ (supervision, grants, enforcement) stays in
agent-daemon. This daemon has no HQ phases.

"Privileged" means the account that owns the package-manager prefixes. On
macOS that is the `admin` account that owns Homebrew. The daemon never runs
as root or SYSTEM and never uses `su`, `sudo`, a sudoers rule, a root helper,
or UAC.

A catalog row whose published install needs elevation is refused with
`MM_DAEMON_PRIVILEGE_REQUIRED` and left as a human action. On this Mac that
row is `docker`, whose cask postflight writes `/usr/local/bin`. Remote
`install.sh` rows and `config/<name>` scripts are not offered. Settings go
through the Go config providers.

The service is per-user: a LaunchAgent, a systemd user unit, or a Windows
service description that is written and not registered. A system service
manager needs root. `daemon install`, `daemon uninstall`, `daemon run`,
`daemon pair`, and `daemon unpair` refuse root, agent accounts, agent
sessions, a foreign `HOME`, and any account that does not own the shared
prefix when that prefix exists. `daemon pair` and `daemon unpair` manage the
Terminal `--update` client key (`client.key` and `peers/terminal.*`).

Two clients, `terminal` and `agent-daemon`. Each has `peers/<client>.pub`
and its own `.next`, `.confirm`, and `.revoked`. The gate accepts either
live key. The Terminal `--update` thin client uses the `terminal` slot when
the socket and `client.key` exist, and otherwise leaves the legacy update
script and its re-exec rule unchanged. An existing `peer.pub` is moved to
`peers/terminal.pub`. If the old `peer.*` files and `peers/` are both
present, the daemon does not listen. The channel is a local socket (mode
`0700` directory, mode `0600` socket). On Windows the channel is the named pipe
`\\.\pipe\agent-machine`, limited to the creating user, with remote clients
rejected. Peer identity is required and never sufficient. Requests and
responses are signed with separate Ed25519 keys. A request is one JSON
object, bounded to 64KiB, signed over the verb, the catalog identifiers, the
soul, the harness, a nullable session id, a timestamp inside five minutes,
and a single-use nonce. A staged replacement key does not become live until
it is confirmed. The daemon reads the pairing files before each request, so
`peers/<client>.revoked` drops that client's key without a restart. The
other client stays live.

The verbs are `status`, `receipts`, `plan`, `apply`, `update`, and `catalog`.
There is no shell verb and no SSH verb. Requests carry identifiers only.
Unknown fields, unknown names, undeclared options, and undeclared targets are
refused before a process starts. `catalog_key`, `floor`, and `envelope` are
declared only on `catalog`.

The control plane publishes the admin catalog. The signing key does not live
next to the catalog. A present `catalog.json` is refused and the daemon does
not listen until that key arrives from the paired channel. The daemon does
not pin `catalog.pub` and does not write `catalog.pub.pin` or
`catalog.version`. Missing `catalog.json` is an empty catalog. A version
missing or below 1 is refused. The paired delivery is the
true-line/agent-bot#71 envelope (`agent-bot-catalog/1`): a catalog public
key, a per-key floor, and the envelope. `AcceptCatalog` checks the format,
the key id, the signature, the floor, and that the version is strictly newer
than the last version this process applied for that key. `ReceiveCatalog`
treats a null envelope as nothing published under that key. It writes nothing.
The `catalog` verb carries them, signed with the request, and only the
paired agent-daemon key is accepted. A null envelope does not clear a
catalog this process already accepted. A verified envelope replaces that
catalog. The last version for a key id is kept in memory. A present
`catalog.json` stays refused. Publishing a catalog version is the approval.
Agents may install pinned items once a catalog is accepted. A brew row
names its formula token, and apply passes that token to the brew provider.
An npm row names a package token and a uv row names a tool token, and both
name the pinned `version`. A uv row may also name `python`, a CPython 3
major.minor. Apply passes the token, the pin, and the Python pin, and does
not pass the request option. The provider plans exactly the pinned release,
which the registry must publish under that version, and never the
registry's latest. An npm or uv row without a pin, or a `python` pin on any
other provider, is refused before a process starts. A brew cask row
names the cask token, the app bundle, the Team ID, and the allowed hosts.
An app-copy row names the url, the sha256, the allowed hosts, the app
bundle, the Team ID, the version, and the format. A winget row names its
`package_id` and runs un-elevated, so machine scope (`global`) is refused
with `MM_DAEMON_PRIVILEGE_REQUIRED`. A go row names its `module` and
`package`, a cargo row names its `crate`, and an archive row names its
`url`, `sha256`, `url_hosts`, `version`, `format`, and `files`. Apply runs
and verifies each planned operation in order. A token on the wrong
provider, more than one recipe on a row, an incomplete cask, app-copy, or
archive recipe, or a missing recipe for brew, npm, uv, winget, go, cargo,
app-copy, or archive is refused before a process starts. npm and uv run only when
that executable is already on the daemon's fixed PATH.
`update`
reapplies one declared flavor.

Status uses the read-only status report. Receipts record the catalog
software `name`, the account, the soul, the harness, a nullable session id,
the plan id, the state, and the outcome. Journal evidence and command output
are not returned. No secrets are written into status, receipts, or manifests.

The daemon diagnostics, each exit 1, are `MM_DAEMON_CALLER_REFUSED`,
`MM_DAEMON_UNSIGNED`, `MM_DAEMON_UNKNOWN_KEY`, `MM_DAEMON_TAMPERED`,
`MM_DAEMON_STALE`, `MM_DAEMON_REPLAY`, `MM_DAEMON_PEER_REFUSED`,
`MM_DAEMON_VERB_UNKNOWN`, `MM_DAEMON_PRIVILEGE_REQUIRED`,
`MM_DAEMON_UNDECLARED`, `MM_DAEMON_NOT_IN_CATALOG`,
`MM_DAEMON_PLAN_FAILED`, `MM_DAEMON_PROVIDER_MISSING`,
`MM_DAEMON_STATUS_FAILED`, and
`MM_DAEMON_UNIMPLEMENTED`. They are registered identifiers. An unknown
identifier panics in tests instead of reaching a caller.

## macOS catalog check (2026-10-29, this machine)

Catalog is managed-machine-config `apps.json` schema 1, 52 rows. Homebrew
prefix `/opt/homebrew` is `admin:admin` mode `0755`. `/Applications` is
`root:admin` mode `0775`, so the `admin` group can write it. `/usr/local/bin`
is `root:wheel` mode `0755`. The session account `user` is not in the `admin`
group and can write neither `/Applications` nor the prefix. The `admin`
account is uid 501 and is in the `admin` group. The daemon runs as `admin`.

Cask artifacts were read from the Homebrew API, not by installing. No catalog
cask has a `pkg` artifact.

| Result | Rows |
|---|---|
| Installs as `admin` with no elevation | 20 `signed-cask` rows whose artifacts are `app` / `binary` / `zap` / `uninstall`: warp, opencode-app, lmstudio, vscode, vscode-insiders, cursor, grokbot, claude-app, chatgpt-app, devin-app, antigravity-app, antigravity-ide, kiro, kiro-cli, zcode, brave, chrome, discord, slack, telegram. Both `vendor-dmg` rows: muse-app, qwen-desktop. All 7 `brew-formula` rows: qwen, minikube, taplo, ripgrep, fd, ast-grep, uv. All 6 `npm` rows: commandcode, copilot, cline, pi, deepseek, mcode. The `uv-tool` row: mistral-vibe. |
| Needs elevation, refused, left as a human action | `docker` (`signed-cask` token `docker-desktop`). The app copy does not, but the published cask postflight symlinks `kubectl` into `/usr/local/bin` with `sudo` if needed, and that directory is not writable by `admin`. |
| Not offered, because the install is a remote shell script | 13 `official-cli` rows (proton-pass, muse, claude, codex, antigravity, grok-build, amp, aider, roocode, kilocode, droid, goose, hermes), plus `opencode` and `devin`. |

`config/<name>` scripts are not run. The daemon applies config only through
the Go config providers.

This catalog has no winget rows. `signed-cask` and `vendor-dmg` are
macOS-only.

## Linux catalog check (2026-11-02, Ubuntu 24.04 x86_64, a cloud VM)

The prefix owner was a dedicated account with no sudo rights and no
supplementary groups. Creating `/home/linuxbrew/.linuxbrew` owned by that
account was the one root step, done by a person; the daemon does not create
the prefix. A `sudo`/`su`/`doas`/`pkexec` shim first on the owner's PATH
logged every elevation attempt. Rows were installed for real with the
commands the providers run, and the daemon's own apply path then installed a
brew, a pinned npm and a pinned uv row with its fixed PATH.

| Result | Rows |
|---|---|
| Installs as the prefix owner with no elevation | All 7 `brew-formula` rows from x86_64_linux bottles with no `post_install`: qwen (`qwen-code`), minikube, taplo, ripgrep, fd, ast-grep, uv. npm rows installed into `~/.local` with `--ignore-scripts`: commandcode, copilot, cline, pi, deepseek. The `uv-tool` row mistral-vibe with its `python` pin 3.12. |
| Not checked | `mcode`: its package name is only in the private catalog. |
| Not offered on Linux | `signed-cask`, `vendor-dmg` (macOS-only); `official-cli`, `opencode`, `devin` (remote shell scripts, as on macOS). |

No elevation was attempted and no file under the prefix or the owner's home
belonged to another account afterwards. Without its `python` pin, uv chose
CPython 3.15, for which `pydantic-core` has no wheel, and the source build
failed. That is why the daemon carries the uv row's `python` pin.

## Windows catalog check (2026-11-02, Windows Server 2025, x86_64)

The prefix owner was a new local account in `Users` only, not in
`Administrators`, running at Medium integrity, on a GitHub-hosted
`windows-latest` VM (run 41379375467). Node 22 was the
administrator-installed one under `C:\Program Files\nodejs`. Only the
`npm` and `uv-tool` rows apply on Windows; this catalog has no winget rows.

| Result | Rows |
|---|---|
| Installs as the prefix owner with no elevation | npm rows into `%APPDATA%\npm` with `--ignore-scripts`: commandcode, copilot, cline, pi, deepseek. `uv-tool` mistral-vibe 2.25.8 with `python` 3.12. Through the daemon's apply path: copilot pinned to 1.0.104 and mistral-vibe 2.25.8, uv found in `WinGet\Links`. |
| Not checked | `mcode` (package name only in the private catalog). |
| Not offered on Windows | `signed-cask`, `vendor-dmg`, `brew-formula` (no Homebrew on Windows); `official-cli`, `opencode`, `devin` (scripts). |

No file under the account's npm prefix or `.local` belonged to another
account afterwards. Two defects were found and fixed with this check: the
daemon's fixed PATH (`WindowsApps;System32`) held neither npm nor uv, so no
row could install, and the npm provider required a Unix execute bit that
Windows files never carry, so it never found `npm.cmd`. The fixed PATH is
now `WindowsApps`, `WinGet\Links`, `C:\Program Files\nodejs`, `System32`.
WinGet itself is not registered for a new account on Windows Server, so a
winget row was not exercised.

## Consequences

- Package-manager children see `HOME`, `USERPROFILE`, and `PATH` only.
- A pinned brew row is applied through the brew provider. Tests use a fake
  provider and do not run Homebrew.
- A catalog that publishes an npm or uv row without `version` has that row
  refused, so the control plane must emit the pin.
- The repository rename from managed-machine to agent-machine is specified
  and not performed. Old names stay until one cutover after the service
  lifecycle lands.
