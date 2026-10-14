---
status: completed
overview: Show the bootstrap and update plan up front, confirm once in the terminal, authorize once via the macOS administrator dialog, and execute all privileged steps under that single approval.
related_prs: [202]
---

# Bootstrap and update plan with single administrator approval (#174)

## Intent

Interactive `managed-machine --bootstrap` and the post-re-exec phase of `managed-machine --update` must never surprise the owner with repeated macOS authorization dialogs mid-run. Instead, managed-machine inspects the planned steps read-only, prints the full execution plan (including which steps need administrator access and what packages, `/Applications` bundles, owners, and paths they change), asks the owner to confirm the plan once in the terminal, requests a single macOS authorization dialog (`osascript ... with administrator privileges`) for the approved plan, and runs every privileged action under that one approval while refusing any privileged call outside the plan.

## Target

- Before any mutation, interactive `bootstrap` and post-re-exec `--update` build and print an execution plan naming every step, marking `[admin]` vs `[user]`, and listing each privileged action's target, destination path, and execution owner.
- The owner confirms the plan once in the terminal (`Approve plan <plan-id> and continue? [Y/n]`), then sees at most **one** macOS authorization dialog for the phase, whose prompt references `<plan-id>`. Declining the terminal confirmation or cancelling the dialog makes no privileged changes.
- `lib/elevate-plan-run` verifies the SHA-256 of the approved plan file in a `700` session directory (rejecting symlinks, temporarily owning the session directory as `root:wheel` during elevated execution, and restoring caller ownership on exit) and executes only allowlisted action kinds (`chown-prefix`, `migrate-app-mv`, `migrate-cask-adopt`, `scutil-hostname`, `install-homebrew`, `brew-formula`, `signed-cask`, `vendor-dmg-retire-cask`, `vendor-dmg`, `git-checkout-owner`, `npm-global`). Homebrew, git, and npm actions drop to the prefix owner (`sudo -u "$owner"`); Homebrew never runs as root. For `migrate-cask-adopt`, `elevate-plan-run` normalizes the destination `.app` bundle ownership to `$owner:admin` (and mode `u+w,a+rX,go-w`) as root before dropping to `$owner` so Homebrew Cask's post-adopt `chmod`/`chgrp -hR admin` succeeds without invoking `sudo`.
- `scripts/adopt` pre-verifies adoptable cask bundles and authorizes all privileged moves, ownership normalizations, and `brew install --cask --adopt` calls under a single `osascript` approval via `adopt_stage_and_authorize_plan`.
- `vendor-dmg-retire-cask` verifies the staged vendor DMG bundle before retiring a Homebrew cask receipt, and steps with recorded plan statuses (`elevate_plan_step_has_recorded_status`) consume their recorded exit status without re-downloading or re-executing on failure.
- While `MANAGED_MACHINE_APPROVED_PLAN_FILE` is active, `elevate_run` serves recorded results for approved actions without opening another dialog and refuses any privileged call outside the plan (`Error: refusing privileged step outside the approved plan: <label>`).
- Noninteractive bootstrap preserves its existing behavior: no terminal prompt, no authorization dialog, and privileged steps recorded as `skipped`.
- Privileged steps are logged in `bootstrap.manifest` and `update.manifest` with `plan_id=<plan-id>`, `plan\t<plan-id>\t...` rows, and `complete\t<step>\tplan=<plan-id>: <summary>`.
- Catalog rows whose privileged install is declined are recorded in `~/.config/managed-machine/declined-apps.manifest` (`600`), shown in subsequent plans as `not installed (declined)`, and skipped on future batch runs until the owner explicitly succeeds at `managed-machine setup <name>`.

## Acceptance

- `tests/elevate.test.sh` verifies single-`osascript` execution of a multi-action approved plan, refusal of an out-of-plan `elevate_run` call without invoking `osascript`, and `MANAGED_MACHINE_ELEVATION_DECLINED_FILE` suppression.
- `tests/adopt.test.sh` verifies that `managed-machine adopt` across multiple apps authorizes at most once via `osascript`, normalizes `.app` bundle ownership to `$owner:admin` before `brew install --cask --adopt`, and stops prompting when the dialog is cancelled.
- `tests/bootstrap.test.sh` verifies interactive plan output, a single `osascript` invocation across multiple privileged steps (dropping to the brew owner via `sudo -u`), a declined plan making zero changes, refusal of an out-of-plan privileged call, and `plan_id` logging in `bootstrap.manifest`.
- `tests/update-reexec.test.sh` verifies that `--update` uses at most one `osascript` before re-exec (`brew update && brew upgrade managed-machine`) and at most one `osascript` after re-exec for all privileged catalog steps, stops privileged work when a dialog is cancelled, and persists declined catalog rows in `declined-apps.manifest` until `managed-machine setup <name>` clears them.

## Replay

Run `tests/elevate.test.sh`, `tests/adopt.test.sh`, `tests/bootstrap.test.sh`, and `tests/update-reexec.test.sh` (or the full suite from `/tmp`).
