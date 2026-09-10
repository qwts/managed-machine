---
status: completed
overview: ENG-0339 and issue 119 — explicit account-local setup, live readiness, and honest cached status.
related_prs: [121, 132]
---

# Agent account harness install (ENG-0339)

## Intent

ENG-0339 supersedes ENG-0045: the macOS account running a harness determines its persona. Inside a `qwts-<harness>-agent` account every checkout is bot work; in the owner's account the harness is the owner's delegate unless told to act as the bot (`GH_AGENT_APP`, `--app`, a worktree pin). Worktree directories are layout only. Each agent account needs its own environment, selected harness, agent-bot runtime, and App key. Issue #119 extends the original opt-in harness installation with independently replayable account setup and live readiness, without running the human machine bootstrap.

Implementation is completed in `scripts/account`, `lib/account-setup.sh`, and `lib/account-readiness.py`. All 44 shell test files pass from a neutral working directory, including 41 Python readiness cases. Real cross-account authorization and the Claude-account pilot remain release validation; development tests use temporary homes, fake accounts/installers, and privilege stubs.

## Target

- Public commands: `managed-machine account setup <active-existing-roster-account> [--json]` and `managed-machine account doctor <account> [--json]`. Root help lists both; `bin/managed-machine` passes every argument after `account` unchanged to `$ROOT/scripts/account`, preserving output and exit status.
- Setup targets an existing active roster account, not account creation. Unknown or retired identities fail closed. Run with the target UID and HOME, or explicit administrator authorization using the existing cross-account mechanism; changing HOME alone is insufficient. No passwordless `su` or new privilege bypass.
- Setup prepares account-local shell/local-bin configuration from the bundled config seed where available and only the roster-selected harness. Prefer the separate CLI setup where shipped (`codex-cli`, `kiro-cli`). Missing bundles and unsupported catalog installers produce actionable pending/failure outcomes, never fabricated success.
- Shared Homebrew, formulae, and casks are admin-owned prerequisites. Do not mutate Homebrew as the agent, change prefix ownership, or relocate desktop casks out of `/Applications`. No human `setup-gh`, SSH setup, fleet registration, full bootstrap, full `--update`, or unrelated harness installs.
- Doctor performs live, read-only per-account shell, harness, and identity checks. Identity wiring/resolution and live App credential verification remain behind the agent-bot CLI; managed-machine does not implement minting or pinning.
- Setup and doctor use the same readiness report, optionally JSON: `ready` exits 0, `pending_user_action` exits 75, and `not_ready` exits 1. A headless account lacking required GUI-session readiness is pending user action, not failed installation or ready.
- Setup reruns preparation and live checks even after stale success markers. The old rule that recorded harness success makes later installs a no-op is superseded.
- `add-agent <slug> --with-harness` delegates to the new setup phase within existing administrator authorization. Without the flag, account provisioning preserves opt-in behavior. Global `status` remains a recorded summary, not live account doctor.
- `add-agent --all [--with-harness]` (PR #132, issue #112) provisions every active roster identity in roster order for fleet Macs: retired identities are skipped, each account keeps its own administrator prompt, per-account failures are collected into a summary, and the run exits nonzero naming every failed slug. The loop re-execs the untouched single-slug flow, so no elevation or verdict semantics change.
- Existing ENG-0339 identity wording and security constraints remain unchanged. No skill or security-rule changes are part of #119.

## Acceptance

- `tests/cli.test.sh` uses a hermetic `scripts/account` stub to verify setup/doctor dispatch with and without `--json`, forwarding of help, empty arguments and argument boundaries, clean output, exit statuses 0/1/75, and both public forms in root help. No account or installer operation runs in this test.
- `tests/account.test.sh` validates UID/HOME, non-root and standard-account checks, active roster targeting, failed/deferred stages, authorization refusal/cancellation, argument quoting, and GUI-context entry. The AppleScript test returns the generated command without executing privilege changes.
- Setup/readiness module tests validate selected-harness isolation, bundled seed preparation, admin-owned Homebrew prerequisites, missing/unsupported installers, read-only doctor, shared report semantics, and headless GUI pending status.
- Add-agent tests validate delegation with `--with-harness`, preserved opt-in without it, and reruns despite stale success markers. Existing provisioning/key/identity boundaries remain intact.
- Generic human `config/<name>` scripts are skipped in account mode; reserved identity/auth environment overrides are refused. Account-local CLI checks reject shared/human-home executables and shadowing aliases. Local-bin checks cover only package-managed destinations and preserve unrelated commands.
- Status counts complete account-ready snapshots separately from legacy identity-only snapshots; it never presents them as live doctor results. Failed/pending setup messages name the non-secret account JSON snapshot as well as the live doctor command.
- Default persistent configuration fast-forwards from newer clean bundled seeds over local-file transport only. Dirty/divergent changes and explicit target-owned overrides are preserved, with stale/no-refresh outcomes reported instead of reset.
- Local-bin readiness requires the recorded commit and canonical checkout, clean Git state, and command links to tracked executable files. Missing historical evidence is not accepted as a verified immutable installation.
- Desktop/shared catalog rows remain pending admin verification even when they declare a command. Credential readiness uses only the target App; genuine machine failures and invalid aggregate reports remain blocking.

## Replay

From the repository root, run `root="$(pwd)"; (cd /tmp && for t in "$root"/tests/*.test.sh; do bash "$t" || exit "$?"; done)`. The neutral working directory prevents the credential fixture from reading this checkout's local bot helper. Focused checks are `account.test.sh`, `account-readiness.test.sh`, `account-setup-env.test.sh`, `add-agent.test.sh`, `agent-accounts-status.test.sh`, and `setup-agent-clis.test.sh`.

On an authorized macOS test account already present in the active roster, run `managed-machine account doctor <account> --json`, then `managed-machine account setup <account> --json`, and doctor again. Verify UID/HOME targeting, selected account/harness scope, read-only doctor, and exits 0/75/1 matching readiness. Repeat setup after a previous success and after introducing safe test-fixture drift to prove markers cannot short-circuit live checks. In a headless session with required GUI readiness absent, verify `pending_user_action` and exit 75; finish the required login action and recheck. Exercise `add-agent <slug> --with-harness` through its existing administrator authorization and verify the same setup phase; omit the flag to verify no setup is triggered.

## Not done here

Making harness setup the default (not opt-in), redefining global `status` as live readiness, broad machine updates or shared Homebrew ownership changes, `remove-agent` (qwts/managed-machine#80), and runtime-side guard retirement (qwts/agent-bot-identity#187).
