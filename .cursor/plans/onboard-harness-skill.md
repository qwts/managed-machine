---
name: onboard-harness-skill
status: active
overview: Create an agent skill at `skills/onboard-harness/SKILL.md` that loads the `onboard-new-harness` runbook and guides the agent through adding a new CLI/TUI + desktop/IDE harness to managed-machine and managed-machine-config.
related_prs: []
---

# Create the `onboard-harness` agent skill

## Intent

The `onboard-new-harness` runbook is a durable spec, but agents still need a
skill that knows when to load that spec and how to execute it step by step.
Create `skills/onboard-harness/SKILL.md` so an agent asked to "onboard a new
harness" (or add a new CLI/TUI and desktop/IDE tool) does not have to re-derive
the catalog kinds, repo split, security constraints, or commit rules from
`AGENTS.md` and the plan each time.

The skill is documentation the agent loads; it must not itself contain secrets,
host-specific paths, or runnable install code. It delegates all work to the
runbook and to managed-machine's existing engines.

## Target

1. **Place the skill source under the repo's `skills/` convention.**

   - Canonical source: `skills/onboard-harness/SKILL.md`.
   - Optional: `skills/onboard-harness/examples.md` for concrete OpenCode/Kiro
     walkthroughs, and `skills/onboard-harness/reference.md` for a catalog-field
     cheat sheet.
   - Keep `SKILL.md` under 500 lines; put long reference material in the optional
     files.
   - If the active IDE requires project skills under `.cursor/skills/` or
     `~/.agents/skills/`, add a symlink or copy there; for Devin, the canonical
     install path is `~/.config/devin/skills/onboard-harness/SKILL.md` per
     `AGENTS.md`.

2. **Match the existing `skills/managed-machine/SKILL.md` frontmatter style.**

   ```markdown
   ---
   name: onboard-harness
   description: "Onboard a new agent/IDE harness into managed-machine and managed-machine-config. Use when the user asks to add a new harness, onboard a new agent tool, or add a new CLI/TUI and desktop/IDE pair. Guides catalog rows, setup scripts, tests, docs, and optional dotfiles/config without secrets."
   license: MIT
   metadata:
     author: qwts
   ---
   ```

   - Do **not** add a `version` field. The skill is agent documentation, not a
     shipped product artifact, so it should not be coupled to `scripts/release`.
     If it is later promoted to a versioned product skill, add `version` and
     update `scripts/release` in a separate plan.

3. **Body: a concise agent workflow.**

   The skill body should be a checklist-style guide, not a copy of the runbook.
   It must tell the agent to:

   - Open `.cursor/plans/onboard-new-harness.md` and follow it.
   - Open both `managed-machine/AGENTS.md` and `managed-machine-config/AGENTS.md`
     and honor their constraints.
   - Determine the harness name and whether it has a CLI/TUI, a desktop/IDE, or
     both.
   - Pick catalog kinds from existing engines; if a new engine is required, stop
     and ask for a managed-machine release plan.
   - Add catalog row(s) to `managed-machine-config/apps.json` and
     `tests/fixtures/apps.json`.
   - Add or update `setup-*` scripts in `managed-machine/` using the existing
     boilerplate and `install_catalog_app` / `apply_config_script`.
   - Add `managed-machine-config/config/<name>` and `dotfiles/<name>` only when
     post-install configuration is needed and contains no secrets.
   - Add tests (e.g. `tests/setup-<name>.test.sh`) and update `tests/status.test.sh`
     if the fixture now lists the harness.
   - Update `README.md` and `skills/managed-machine/SKILL.md` description/table/
     adopt list when a desktop cask is added.
   - Commit using the repo's required `GIT_AUTHOR_NAME` and `GIT_AUTHOR_EMAIL`;
     never run `git config user.name/user.email`.

4. **Include hard constraints the agent must not miss.**

   - No secrets, tokens, or private keys in any committed file.
   - Dotfiles and config scripts only configure; they do not install software.
   - Do not edit live home files directly; use `install_home_file` / managed
     blocks and manifests.
   - Cask rows require `homebrew/cask` tap, a real `sha256`, allowed download and
     homepage hosts, and the correct Developer ID Team ID.
   - Do not commit `*.manifest`, `machine.toml`, or ssh-key-policy files.

5. **Do not auto-invoke in contexts where it would conflict.**

   - Omit `disable-model-invocation` so the skill auto-loads when the user asks
     to onboard a new harness, add a new agent/IDE tool, or mentions a CLI/TUI
     and desktop/IDE pair.
   - Do not include `managed-machine` operational commands in the skill; those
     belong to `skills/managed-machine/SKILL.md`. The two skills may load
     together; keep their scopes separate.

6. **Update repo documentation for the new skill.**

   - Add `skills/onboard-harness/SKILL.md` (and any examples/reference files) to
     the README "Layout" tree.
   - Optionally add a note in `AGENTS.md` migration protocol that the
     `onboard-harness` skill can be copied to an agent's skill directory when
     onboarding harnesses repeatedly.

7. **Add a lightweight test.**

   - Add `tests/onboard-harness-skill.test.sh` that verifies:
     - `skills/onboard-harness/SKILL.md` exists and is readable.
     - It has valid YAML frontmatter with `name` and `description`.
     - The description is non-empty and mentions "harness".
     - It does not contain `git config user.name` or `git config user.email`.
     - It does not contain `/Users/<name>/` or other host-specific paths.
     - It is under 500 lines (or under a published limit if optional files are
       used).

8. **No release or formula changes.**

   - Because the skill carries no `version` field and the formula does not bundle
     `skills/`, no `scripts/release` or `Formula/managed-machine.rb` change is
     required.
   - If the skill is later promoted to versioned/shipped status, create a new
     plan to couple it to the release script.

## Acceptance

- `skills/onboard-harness/SKILL.md` exists and follows the repo's skill
  frontmatter style.
- The skill description is in third person, mentions trigger terms, and clearly
  states what and when.
- The skill body under 500 lines and points at `.cursor/plans/onboard-new-harness.md`
  and both `AGENTS.md` files.
- The skill checklist covers catalog rows, `setup-*`, tests, docs, dotfiles/config,
  and the commit identity constraint.
- No secrets, no `git config` instructions, and no host-specific paths in the
  skill.
- `tests/onboard-harness-skill.test.sh` passes.
- README layout tree lists the new skill files.
- `scripts/release` and the formula are unchanged.

## Replay

1. Open this plan, `.cursor/plans/onboard-new-harness.md`,
   `skills/managed-machine/SKILL.md`, and both `AGENTS.md` files.
2. Create `skills/onboard-harness/SKILL.md` with the frontmatter and body above.
3. Optionally create `skills/onboard-harness/examples.md` (OpenCode/Kiro) and
   `skills/onboard-harness/reference.md` (catalog fields).
4. Update `README.md` layout and, optionally, `AGENTS.md` migration protocol.
5. Add `tests/onboard-harness-skill.test.sh`.
6. Run the tests:

   ```bash
   /bin/bash tests/onboard-harness-skill.test.sh
   /bin/bash tests/release.test.sh   # to verify release is unaffected
   ```

7. Stage the skill and test, commit with the managed-machine commit identity,
   and do not commit manifests or secrets.
