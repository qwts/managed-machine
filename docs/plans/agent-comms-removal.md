---
status: completed
overview: Managed-machine owns no agent-comms install, update, service, pairing, or membership lifecycle. Bootstrap and update must remain independent of agent-comms, including when a machine already has an independently managed copy.
related_prs: []
---

# Keep agent-comms outside managed-machine setup

## Intent

Managed-machine bootstrap and update must finish without installing, upgrading, linking, starting, or configuring agent-comms. A separately managed agent-comms installation and its service, pairings, and data remain untouched.

Issue [#179](https://github.com/qwts/managed-machine/issues/179) proposed adding a managed-machine GeniusBar CLI/service setup that would provision agent-comms, with a Homebrew fallback. That provisioning proposal conflicts with this ownership boundary and must not be implemented as managed-machine setup. Independently installing the GeniusBar desktop app is outside this boundary.

## Target

- Bootstrap, update, account setup, explicit setup commands, and catalog installation have no agent-comms provisioning edge.
- The pinned local-bin installer remains independently versioned and contains no agent-comms provisioning.
- Existing independent agent-comms installations and their state are preserved.
- Historical references may remain; new setup behavior must not be introduced through them.

## Acceptance

- `tests/agent-comms-boundary.test.sh` checks that fixture scheduling matches the real bootstrap and update setup lists, then runs both entrypoints with agent-comms absent and with a pre-existing external executable.
- The fixture records direct agent-comms commands and Homebrew install, upgrade, and tap requests for agent-comms. Mutation checks inject direct service setup and `brew install agent-comms` into a scheduled fixture step and confirm both are rejected.
- The entrypoint fixture uses test doubles for individual setup scripts. It verifies scheduling and command-recording behavior; it does not execute production setup-script internals. The local-bin and catalog findings below are audit evidence, not automated test coverage.
- Audit evidence: `managed-machine-config/local-bin.ref` is pinned to `v0.2.1`, which resolves to local-bin commit `34b38897002b8f0042958ea4d2c47a40b3961bca`; its installer contains no agent-comms, GeniusBar CLI, broker, or pairing references. The managed-machine-config app catalog and managed-machine setup/account integration also contain no agent-comms provisioning path.

## Solution as built

No production setup edge existed to remove. The change adds an isolated command-recording regression fixture for the actual bootstrap/update entrypoint control flow and records the dependency audit here. The fixture intentionally stubs setup implementations, so future changes to a setup script's internals require their own focused regression test and renewed audit.

## Validation

- The scope-refined `tests/agent-comms-boundary.test.sh` passed, including direct-command and Homebrew mutation checks for agent-comms.
- The scope-refined boundary test passed against the agent-bot-removal worktree's current `scripts/bootstrap` and `scripts/update` via a temporary copy.
- All 60 repository `*.test.sh` scripts passed from `/tmp` after clearing the inherited `CODEX_*` harness markers so the suite's human-mode fixtures run as intended.

## Replay

Run `bash tests/agent-comms-boundary.test.sh`, then the repository test suite from `/tmp` as prescribed in `AGENTS.md`. Review any new agent-comms provisioning path, including proposed GeniusBar CLI or service setup, against this plan before adding it. Independently installed GeniusBar desktop apps are outside this plan.
