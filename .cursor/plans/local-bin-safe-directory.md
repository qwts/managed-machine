# local-bin Cellar ownership

## Intent

`setup-bin` must work from a Homebrew install. The bundled `libexec/local-bin` is prefix-owned; Git 2.35+ refuses that from the invoking user, and mutating git operations (`clone`, `fetch`, `checkout`, `pull`) fail with permission denied unless run as the prefix owner.

## Target

- Trust that exact path for git commands only (`-c safe.directory`). Do not write `safe.directory` into the user gitconfig.
- When `local-bin` is owned by another user (e.g. `admin` in a Homebrew install) and mutating git operations are needed (repo missing or unsatisfied pin), escalate via `elevate_as_user` through macOS Authorization Services (`osascript` with administrator privileges) to run mutating commands as the owner.
- When the pin is already satisfied, skip fetch and do not prompt for administrator authorization.
- Downstream `install` (symlinking into `~/.local/bin` and updating `~/.zshrc`) and pin manifest recording always run unprivileged as the invoking user.

## Acceptance

- `tests/setup-bin.test.sh` passes, including:
  - Stub that fails unless `-c safe.directory` is set.
  - Foreign/prefix-owned repo with unsatisfied pin escalates to the owner via `elevate_as_user` to update.
  - Foreign/prefix-owned repo with satisfied pin skips fetch without elevating.
- A non-admin invoking user can complete `setup-bin` against `/opt/homebrew/Cellar/managed-machine/*/libexec/local-bin`.

## Replay

`/bin/bash tests/setup-bin.test.sh`

## Status

completed

