---
status: active
overview: ENG-0339 alignment — the account, not the worktree directory, decides bot versus human work; each agent account installs its own harness via add-agent --with-harness.
related_prs: []
---

# Agent account harness install (ENG-0339)

## Intent

ENG-0339 supersedes ENG-0045: the macOS account running a harness determines its persona. Inside a `qwts-<harness>-agent` account every checkout is bot work; in the owner's account the harness is the owner's delegate unless told to act as the bot (`GH_AGENT_APP`, `--app`, a worktree pin). Worktree directories are layout only. The same record requires each agent account to have its harness installed, `agent-bot` installed, and its App key provisioned in its own home; `add-agent` covered the last two.

## Target

- `AGENTS.md` and `.cursor/rules/commit-identity.mdc` state the account rule, not the worktree rule.
- Operator-facing text (`lib/brew.sh`, `scripts/update`, `README.md`, `lib/agent-account.sh`) explains the refused human `gh` in account terms. `tests/brew.test.sh` keeps echoing the installed shim's literal refusal until qwts/agent-bot-identity#187 changes it.
- `managed-machine add-agent <slug> --with-harness` installs the roster harness as the account inside the existing elevated phase: `sudo -u <slug> managed-machine setup <name>` headless, `<name>` = the roster harness key, or `<harness>-cli` when the repo ships that script. The verdict is a world-readable marker (`<slug>.harness`: `ok <name>` / `failed <name> <exit>`) with `.harness.log` and `.harness.err` beside it.
- The compliance report always names the expected harness install (ok / not installed / failed with the installer's first error line); `status` flags only a recorded failure. A roster row without a harness fails `--with-harness` closed before any dialog.
- `.claude/worktrees/` is ignored; the leftover ENG-0045 worktree under the repo root is gone.

## Acceptance

- `tests/add-agent.test.sh`: `--with-harness` runs the stubbed `managed-machine setup goose` once as the account with stdin closed and noninteractive mode; a second run is a no-op; a failure records `failed goose 7`, is reported with the log path, flags the status summary, and is retried; `codex` installs through `codex-cli`; a bare roster row fails closed.
- `tests/brew.test.sh`, `tests/update-reexec.test.sh`: the reworded notes still match.
- `tests/setup-aider.test.sh`: the sibling-checkout comment no longer assumes the worktree workflow.

## Replay

Run `tests/*.test.sh`. On a real machine: `managed-machine add-agent qwts-claude-agent --with-harness`, approve the one dialog, then `managed-machine status` shows the account and `/Library/Application Support/managed-machine/agents/qwts-claude-agent.harness` reads `ok claude`.

## Not done here

Making the harness install the default (not opt-in) and counting it in `status`'s "ready" verdict; `remove-agent` (qwts/managed-machine#80); the runtime-side guard retirement (qwts/agent-bot-identity#187).
