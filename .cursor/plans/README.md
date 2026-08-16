# Plans as living specs

`.cursor/plans/` is the durable spec for work in this repo. Chat is disposable; these files are not.

## Naming

Use a stable kebab-case name (`zsh-profile-refresh.md`). Do not add hash suffixes for new specs. Cursor-generated `*.plan.md` files may be kept as history after they land; do not treat the hash as the canonical name.

## Frontmatter

Every plan has:

- `status`: `active` | `completed` | `superseded`
- `overview`: one or two sentences
- `related_prs`: GitHub PR numbers, when they exist

## Shape

Include **Intent**, **Target**, **Acceptance**, and **Replay**. Update the file in place when behavior changes. Never delete a completed spec; mark it `completed` or `superseded` and point at the replacement.

## Replay

A later model should be able to open the matching plan plus `AGENTS.md` and continue without the original chat. If the plan and the code disagree, update the plan in the same change.
