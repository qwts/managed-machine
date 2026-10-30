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
manager needs root.

The only client is agent-daemon, over a local socket (mode `0700` directory,
mode `0600` socket). On Windows the channel is the named pipe
`\\.\pipe\agent-machine`, limited to the creating user, with remote clients
rejected. Peer identity is required and never sufficient. Requests and
responses are signed with separate Ed25519 keys. A request is one JSON
object, bounded to 64KiB, signed over the verb, the catalog identifiers, the
soul, the harness, a timestamp inside five minutes, and a single-use nonce.
A staged replacement key does not become live until it is confirmed.

The verbs are `status`, `receipts`, `plan`, `apply`, and `update`. There is
no shell verb and no SSH verb. Requests carry identifiers only. Unknown
fields, unknown names, undeclared options, and undeclared targets are refused
before a process starts.

The control plane publishes the admin catalog. This daemon loads
`catalog.json` only when `catalog.sha256` matches. Publishing a catalog
version is the approval. Agents may install pinned items. `update` reapplies
one declared flavor. `managed-machine --update` uses the daemon when the
socket and a client key exist, and otherwise leaves the legacy update script
and its re-exec rule unchanged.

Status uses the read-only status report. Receipts list plan id, state, and
outcome. Journal evidence and command output are not returned. No secrets
are written into status, receipts, or manifests.

The daemon diagnostics, each exit 1, are `MM_DAEMON_CALLER_REFUSED`,
`MM_DAEMON_UNSIGNED`, `MM_DAEMON_UNKNOWN_KEY`, `MM_DAEMON_TAMPERED`,
`MM_DAEMON_STALE`, `MM_DAEMON_REPLAY`, `MM_DAEMON_PEER_REFUSED`,
`MM_DAEMON_VERB_UNKNOWN`, `MM_DAEMON_PRIVILEGE_REQUIRED`,
`MM_DAEMON_UNDECLARED`, `MM_DAEMON_NOT_IN_CATALOG`,
`MM_DAEMON_PLAN_FAILED`, `MM_DAEMON_STATUS_FAILED`, and
`MM_DAEMON_UNIMPLEMENTED`. They are registered identifiers. An unknown
identifier panics in tests instead of reaching a caller.

## macOS catalog check (2026-10-29)

52 rows in managed-machine-config `apps.json`. 36 install as `admin` with no
elevation. `docker` needs elevation and is refused. 15 rows are remote shell
scripts and are not offered. Linux and Windows were not checked on a real
machine. This catalog has no winget rows.

## Consequences

- Package-manager children see `HOME`, `USERPROFILE`, and `PATH` only.
- A pinned brew row is applied through the brew provider. Tests use a fake
  provider and do not run Homebrew.
- The repository rename from managed-machine to agent-machine is specified
  and not performed. Old names stay until one cutover after the service
  lifecycle lands.
