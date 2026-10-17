---
status: completed
overview: Remove managed-machine's responsibility for installing, upgrading, pinning, bootstrapping, or repairing agent-bot while preserving unrelated setup and existing user data.
related_prs: []
---

# Remove agent-bot setup ownership

## Intent

Managed-machine provisions machines and agent accounts without taking responsibility for the agent-bot identity runtime. Issue #179's GeniusBar supplier change does not satisfy this intent if managed-machine retains a Homebrew fallback or continues wiring it. This change removes that ownership completely while leaving independently installed runtimes, credentials, markers, and interposer files intact.

## Target

- Bootstrap, update, catalog auto-install, and setup commands never install, upgrade, pin, bootstrap, repair, or interpose agent-bot. Retired setup names fail with a clear message.
- `setup-gh` resolves the Homebrew `gh` path directly and no longer manages interposition. Update does not invoke the old interposer repair helper.
- Account creation and `--with-harness` may prepare the account, public roster, avatar, and harness. They do not copy App credentials or invoke identity bootstrap/doctor.
- Account readiness covers account, shell, and harness checks only; it makes no identity-ready claim. Account status ignores old identity-only success markers.
- Other machine setup remains independent. No existing Homebrew installation, credentials, key files, or user data is deleted.
- The existing `agent-account-harness-install` plan remains the spec for unrelated account and harness behavior.

## Acceptance

- Bootstrap fixtures prove no agent-bot step or wiring occurs with a pre-existing executable on `PATH`.
- Update fixtures prove no runtime upgrade or gh-interposer repair occurs; the safe setup steps still run.
- CLI setup names for `agent-bot` and `agent-bot-gh` explain retirement.
- Account setup/readiness fixtures prove no identity CLI invocation, credential seeding, or positive identity readiness; account and harness checks continue to run.
- Catalog app install cannot reintroduce the runtime through automatic or explicit catalog setup.
- Existing stale markers and local runtime data are read-only and remain untouched.
- README, skill, AGENTS.md, and account plan describe the changed ownership. Existing identity policy remains unchanged.

## Replay

From the repository root, run `bash tests/bootstrap.test.sh`, `bash tests/update-reexec.test.sh`, `bash tests/cli.test.sh`, `bash tests/add-agent.test.sh`, `bash tests/account.test.sh`, `bash tests/account-readiness.test.sh`, `bash tests/agent-accounts-status.test.sh`, and `bash tests/setup-list-status.test.sh`. Run the repository plain-bash suite from `/tmp` using the command documented in `AGENTS.md`.

## Solution as built

Bootstrap and update no longer include the installer or interposer repair. The retired setup scripts and runtime helper libraries are removed; the CLI returns an explicit retirement error, and catalog paths filter auto-install and reject explicit agent-bot installs. `setup-gh` resolves Homebrew `gh` directly. Agent account creation still supports roster validation, account records, pictures, shared coordination, and optional harness setup, but never copies App keys or runs identity commands. Account doctor/readiness now covers account environment and harness only, and status ignores legacy key/doctor markers. Existing identities and files are retained. The new boundary tests verify removed invocations alongside the still-active account and harness checks.
