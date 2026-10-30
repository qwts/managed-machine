---
status: active
overview: >-
  Turn managed-machine into a least-privilege machine daemon for macOS, Linux
  and Windows that installs only catalog-declared software and reports signed
  status, driven by agent-bot's agent-machine component over a mutually signed
  local channel.
related_prs: [386]
related_issues:
  - qwts/managed-machine#387
  - qwts/managed-machine#388
  - qwts/managed-machine#389
  - qwts/managed-machine#390
  - qwts/managed-machine#391
  - qwts/managed-machine#392
  - qwts/managed-machine#393
  - qwts/managed-machine#394
  - qwts/agent-bot-identity#1254
  - qwts/agent-bot-identity#1256
---

# Machine daemon (agent-machine)

## Intent

Owner direction (2026-10-29): turn managed-machine into a privileged daemon.
Commands can be run against it to install predetermined software, and it
reports status updates about the machine. Carry over as much of the existing
managed-machine and managed-machine-config code as possible. Model it on the
agent-bot daemon work (agent-bot-identity, 0.15.x).

The machines are developer machines. The owner approves software by adding it
to the catalog; nothing else is approval and nothing else is installable.

The layering is `agent-bot` > `agent-identity`, `agent-comms`,
`agent-machine`. agent-machine is the agent-bot component that talks to this
daemon and the name of the curated catalog it exposes. managed-machine remains
the engine and the daemon; the repository is not renamed.

## Decisions (owner, 2026-10-29)

1. **Platforms.** macOS, Linux and Windows are all in scope from the first
   slice. OS-specific code is confined to the service manager, the local
   channel and peer identity, behind build-tagged files (as
   `internal/plan/lock_unix.go` and `lock_windows.go` already do).
2. **Least privilege.** The daemon has only the permissions needed to install
   catalog software (casks, brew formulae, npm packages, and the other
   existing providers). It runs as the account that owns the package-manager
   prefixes, never as root or SYSTEM, and never uses `su`, `setuid`, `sudo` or
   UAC. A catalog row that would need elevation (a `.pkg` installer cask,
   system settings, `/Library` or machine-scope writes) is not offered and is
   refused with `MM_DAEMON_PRIVILEGE_REQUIRED`.
3. **No approval gates on verbs.** Adding an entry to the agent-machine
   catalog is the owner's approval. No catalog entry may need approval or
   elevation at install time. Installing and uninstalling the service itself
   remains a human action from the owner's account; agent accounts are refused.
4. **One client.** Only the agent-bot daemon on the same machine
   (agent-machine) talks to the privileged daemon. Remote requests are MCP
   calls through agents.true-line.ai, relayed to that machine's agent-bot.
   Local agents also go through agent-bot.
5. **Agents may install** from the agent-machine catalog, not only read
   status. The catalog is curated and pinned like agent-comms and the
   extension catalog.
6. **`update` replaces `managed-machine --update`.** The Terminal command
   becomes a thin, separately paired client of the daemon, for machines where
   the agent-bot runtime is not installed or not wanted. The legacy path
   remains only as the fallback when the daemon itself is absent.
7. **Status** is readable by anything through agent-bot. The daemon publishes
   a signed digest after every receipt and every 15 minutes.
8. **No dynamic input.** Nothing a caller sends is processed as privileged
   input. A request may only select among options the catalog declares (for
   example a declared version `10.6` of a row that declares `10.5` and
   `10.6`). An undeclared version, or any URL, path, package name, flag or
   script, is refused before anything runs.
9. **Mutual key registration and signed requests.** The two daemons register
   keys with each other and sign their requests, so an impostor is refused even
   when it runs under an allowed account. Keys are held and registered through
   existing agent-bot infrastructure (agent-bot-keyd and/or
   agents.true-line.ai).

## Target

```mermaid
flowchart LR
  mcp["MCP via agents.true-line.ai"] --> agentMachine["agent-bot: agent-machine"]
  localAgents[local agents] --> agentMachine
  keyd[agent-bot-keyd] -.->|"signs"| agentMachine
  terminal["--update thin client"] -->|"own pairing"| mmd
  agentMachine -->|"signed, local channel"| mmd["managed-machined as prefix owner"]
  mmd --> resolve["resolve identifiers against pinned catalog"]
  resolve --> planStore["plan, journal, receipt"]
  planStore --> providers["existing providers"]
  planStore --> digest["signed status digest"]
  digest --> agentMachine
```

### Carried over unchanged

| Piece | Where | Role in the daemon |
|---|---|---|
| Catalog | `managed-machine-config/apps.json`, `software/` sources, `internal/catalog`, `internal/definition`, `internal/schema` | The only installable set |
| Providers | `internal/provider/*` | The install engine; host allowlists, sha256, Team ID and no-elevation rules stay |
| Plan, journal, receipt | `internal/plan` (ADR-0012), `internal/execute`, `internal/statedir` | Every state change is plan, then apply by plan ID, then receipt |
| Status | `internal/status` | The status body |
| Diagnostics | `internal/diag` | `MM_DAEMON_*` codes in responses |
| Output contract | `internal/output` | Response encoding |
| Extension protocol | `internal/protocol` (`mm-extension-1`) | Template for the bounded one-JSON-line wire format |
| Config apply | `internal/config`, `config/<name>` | Settings after install, configure-only |
| Fleet identity | `internal/fleet`, `machine.toml` | Identity the daemon reports under |

### Closed verb set

- `status`: machine, software, config, flavor and fleet sections and the last
  receipts. Read-only.
- `receipts`: list and read receipts.
- `plan software <catalog-name> [--option <declared>] --<declared target>` and
  `plan config`.
- `apply <plan-id>`: only a saved plan ID (ADR-0012).
- `update`: re-run the declared flavor.

Anything else is refused with `MM_DAEMON_VERB_UNKNOWN` or
`MM_DAEMON_NOT_IN_CATALOG`. There is no command-execution verb and no SSH
verb. Requests carry identifiers only; the daemon resolves them against the
catalog it loaded itself from the pinned managed-machine-config revision and
pinned bundles, and fails closed on anything unknown, including extra fields.

### Per-OS shape (proposed; confirmed in P1)

| | macOS | Linux | Windows |
|---|---|---|---|
| Service | launchd LaunchDaemon, `UserName` = `admin` | systemd unit, `User=` prefix owner, `NoNewPrivileges=yes`, `ProtectSystem=strict`, limited `ReadWritePaths` | Windows service under the developer account, not LocalSystem |
| Channel | Unix socket, directory `0700` | Unix socket, directory `0700` | Named pipe, DACL for that account only |
| Peer identity | `getpeereid` | `SO_PEERCRED` | client PID plus token SID |
| Scope | Homebrew prefix, `/Applications` (admin group) | Homebrew/Linuxbrew or user prefixes | winget user scope, user prefixes |

Peer identity is checked first and is never sufficient alone; every request
must also carry a valid signature from a registered key.

### Pairing and signing

- Each side holds its own Ed25519 keypair; private keys never leave their
  holder (agent-bot's side is held by agent-bot-keyd). The two exchange public
  keys; there is no shared secret.
- Each request is signed over the verb, its catalog identifiers, the
  originating soul and harness, a timestamp and a single-use nonce. The daemon
  refuses a bad signature, an unknown key, a stale timestamp and a replayed
  nonce.
- Every response and status digest is signed by the daemon.
- Re-registration stages the new key and keeps the live pairing until the new
  one is confirmed (agent-bot #1243). Either side can revoke; a revoked pairing
  refuses everything until re-registered.
- The Terminal `--update` thin client and any later `managed-machine-cli` each
  get their own pairing.

### Execution environment

- Every child process gets an explicit minimal environment: the account's
  `HOME`/`USERPROFILE`, a fixed `PATH`, and no inherited `HOMEBREW_*`,
  `npm_config_*` or `UV_*` variables. Arguments come only from the catalog;
  no shell is involved.
- Socket or pipe and state directory are owned by the daemon account
  (`0700`/`0600`).
- Receipts record the account an operation ran as and who asked (soul, agent,
  harness, or the Terminal client). Receipts, digests and manifests never hold
  command output or secrets.

### Constraints kept

Casks only into `/Applications` with tap, sha256, vendor host and Team ID
verified; codesign/notarization checks never skipped; Homebrew prefix stays
`admin`-owned and brew never runs as root; SSH enrollment stays human-only; no
config-repo content, machine or key records as fixtures; two-step release
flow; ENG-0339 identity rules.

### AGENTS.md changes (land in P2)

- The daemon runs as the prefix owner, never elevated, on all three OSes; the
  providers keep "never elevate".
- Agent accounts may install from the agent-machine catalog through the
  daemon; this is how today's "`--update` refuses agent sessions" maps onto
  daemon verbs.
- Adding a catalog entry is the approval; no entry may need elevation.
- `--update` is a thin daemon client with a legacy fallback.

## Phases

```mermaid
flowchart LR
  P0[P0 privilege audit] --> P1[P1 ADR]
  P1 --> P2[P2 service lifecycle]
  P1 --> P3[P3 channel and signing]
  P2 --> P4[P4 status and receipts]
  P3 --> P4
  P4 --> P5[P5 plan and apply]
  P5 --> P6[P6 update and thin client]
  P3 --> P7[P7 agent-machine in agent-bot]
  P5 --> P7
  P7 --> P8[P8 hosted relay and attestation]
```

Every managed-machine phase passes `gofmt -l . && go vet ./... && go test
./...` on macOS, Linux and Windows, and `tests/machine-daemon.test.sh`, using
synthetic catalog fixtures only and no real installs.

### P0: privilege audit (managed-machine-config, managed-machine)

On a real macOS, Linux and Windows machine, classify every catalog row as
installable by the prefix owner without elevation, or needing elevation.

Acceptance: a checked per-OS table recorded in the P1 ADR; rows needing
elevation are excluded from the agent-machine catalog (mechanism decided in
P1) and unreachable through the daemon.

#### macOS check (2026-10-29, this machine)

Catalog is `managed-machine-config` `apps.json` schema 1, 52 rows. Homebrew
prefix `/opt/homebrew` is `admin:admin` mode `0755`. `/Applications` is
`root:admin` mode `0775`, so the `admin` group can write it. `/usr/local/bin`
is `root:wheel` mode `0755`. The session account `user` is not in the `admin`
group and can write neither `/Applications` nor the prefix. The `admin`
account is uid 501 and is in the `admin` group. The daemon runs as `admin`.

Cask artifacts were read from the Homebrew API (`formulae.brew.sh`), not by
installing. No catalog cask has a `pkg` artifact.

| Result | Rows |
|---|---|
| Installs as `admin` with no elevation | 20 `signed-cask` rows whose artifacts are `app` / `binary` / `zap` / `uninstall`: warp, opencode-app, lmstudio, vscode, vscode-insiders, cursor, grokbot, claude-app, chatgpt-app, devin-app, antigravity-app, antigravity-ide, kiro, kiro-cli, zcode, brave, chrome, discord, slack, telegram. Both `vendor-dmg` rows: muse-app, qwen-desktop. All 7 `brew-formula` rows: qwen, minikube, taplo, ripgrep, fd, ast-grep, uv. All 6 `npm` rows: commandcode, copilot, cline, pi, deepseek, mcode. The `uv-tool` row: mistral-vibe. |
| Needs elevation | `docker` (`signed-cask` token `docker-desktop`). The app copy does not, but the published cask postflight symlinks `kubectl` into `/usr/local/bin` with `sudo` if needed, and that directory is not writable by `admin`. |
| Not offered, because the install is a remote shell script | 13 `official-cli` rows (proton-pass, muse, claude, codex, antigravity, grok-build, amp, aider, roocode, kilocode, droid, goose, hermes), plus `opencode` and `devin`. managed-machine runs these as `curl \| bash`. The daemon does not execute scripts. |

`config/<name>` (aider, claude, codex, commandcode, grok-build, grokbot, kilocode, muse, muse-app, opencode, roocode, zsh) are scripts too. The daemon applies config only through the Go config providers.

Linux and Windows were not checked on a real machine. This catalog has no winget rows. `signed-cask` and `vendor-dmg` are macOS-only.

### P1: ADR (managed-machine-documentation)

Acceptance: an ADR covering the per-OS privilege model, the client and pairing
model, the closed verb set, no dynamic input, no approval gates, the
`MM_DAEMON_*` diagnostics, the wire format (bounded JSON lines modelled on
`mm-extension-1`), the P0 table, and the exclusion mechanism for rows needing
elevation.

### P2: service lifecycle (managed-machine)

`managed-machine daemon run|install|uninstall|status` for launchd, systemd and
Windows services.

Acceptance: install and uninstall are idempotent, run from the owner's account,
and refuse agent accounts, root/SYSTEM and foreign-HOME callers; the AGENTS.md
changes above land; `.github/workflows/go-core.yml` runs on all three OSes.

### P3: local channel and signing (managed-machine)

Unix socket (macOS, Linux) and named pipe (Windows), peer identity, Ed25519
pairing, nonce and timestamp checks, signed responses, staged re-registration,
revocation.

Acceptance tests: a correctly signed request from the registered key is
accepted; refused are an unsigned request, an unknown key, a tampered request,
a stale timestamp, a replayed nonce, and an unregistered process under the same
account; re-registration keeps the old key until the new one is confirmed.

### P4: status and receipts (managed-machine)

Acceptance: `status` and `receipts` served from `internal/status` and the
receipts; a signed digest after each receipt and every 15 minutes; digests
contain no command output and no secrets.

### P5: plan and apply (managed-machine)

One catalog item through the existing providers, as the prefix owner.

Acceptance tests: the child's account, home and environment are the expected
ones on each OS; a declared option is accepted; an undeclared version, a URL, a
path, an extra field, an unknown catalog name and a row needing elevation are
refused before any process starts; the receipt is written and status reflects
it.

### P6: update and the Terminal thin client (managed-machine)

Acceptance: the `update` verb re-runs the declared flavor; `managed-machine
--update` is a paired thin client of the daemon and falls back to the legacy
path when no daemon is installed; the `legacy/scripts/update` re-exec rule is
preserved.

### P7: agent-machine component (agent-bot-identity)

Acceptance: daemon routes and MCP tools for the closed verbs, pairing through
agent-bot-keyd, receipts on the agent-bot side, and install and wiring next to
agent-identity and agent-comms.

### P8: hosted relay and key attestation (agents.true-line.ai server)

Acceptance: MCP requests to agents.true-line.ai are relayed to the machine's
agent-machine; both daemon keys are attested against the enrolled device, and
the pairing is visible and revocable on `/devices/manage`.

### Later

- A standalone `managed-machine-cli` client with its own pairing.

## Still open

- Key registration through agent-bot-keyd, agents.true-line.ai, or both, and
  which is authoritative. Proposed: keyd in P3/P7, server attestation added in
  P8.
- Windows service account: the developer's own account (proposed) or a
  dedicated local account that owns the prefixes.
- Linux prefix owner when Homebrew is absent.
- Exclusion mechanism for rows needing elevation: a schema flag, or a separate
  agent-machine catalog file (P1).

## Replay

Open this plan plus `AGENTS.md`, `docs/go-core.md` and
`docs/plans/README.md`. Sources: managed-machine-config `AGENTS.md`;
managed-machine-documentation v1 spec, output and diagnostics contract, and
ADR-0012, ADR-0014, ADR-0016, ADR-0017; agent-bot-identity `agent-daemon.mjs`,
`docs/daemon-api.md`, `docs/server-mode.md`, `identity/server-client.mjs`,
`identity/relay-events.mjs`, `owner-approval.mjs` and agent-bot-keyd. Each
phase has a GitHub issue linked in `related_issues`; update the phase here when
its acceptance changes.
