# local-bin Cellar ownership

## Intent

`setup-bin` must work from a Homebrew install. The bundled `libexec/local-bin` is prefix-owned; Git 2.35+ refuses that from the invoking user, and mutating git operations (`clone`, `fetch`, `checkout`, `pull`) fail with permission denied unless run as the prefix owner.

## Target
 
 - Trust that exact path for git commands only (`-c safe.directory`). Do not write `safe.directory` into the user gitconfig.
 - When `local-bin` is owned by another user (e.g. `admin` in a Homebrew install) and mutating git operations are needed (repo missing or unsatisfied pin), escalate via `elevate_as_user_with_github_auth` through macOS Authorization Services (`osascript` with administrator privileges) to run mutating commands as the owner.
 - Securely forward the invoking user's GitHub credentials to the elevated runner via `brew-github-auth-run` and a temporary credential helper without placing tokens in argv.
 - When the pin is already satisfied, skip fetch and do not prompt for administrator authorization.
  - Downstream `install` (symlinking into `~/.local/bin` and updating `~/.zshrc`) and pin manifest recording always run unprivileged as the invoking user.
  - `setup-zsh-functions` mirrors this entire contract for `qwts/zsh-functions` (pin `zsh-functions.ref`, manifest `zsh-functions.manifest`): same safe.directory trust, same elevation path, same satisfied-pin skip. Covered by `tests/setup-zsh-functions.test.sh` (pin mechanics); the elevation paths are shared-helper code covered once by `tests/setup-bin.test.sh`.
 
 ## Acceptance
 
 - `tests/setup-bin.test.sh` passes, including:
   - Stub that fails unless `-c safe.directory` is set.
   - Foreign/prefix-owned repo with unsatisfied pin escalates to the owner via `elevate_as_user_with_github_auth` to update, forwarding credentials via tokenfile and avoiding argv leaks.
   - Foreign/prefix-owned repo with satisfied pin skips fetch without elevating.
 - `tests/elevate.test.sh` covers `elevate_as_user_with_github_auth` for self, other with token, and no-token fallback.
  - A non-admin invoking user can complete `setup-bin` against `/opt/homebrew/Cellar/managed-machine/*/libexec/local-bin`.
  - `tests/setup-zsh-functions.test.sh` covers the mirror: tag/SHA pins, satisfied skip, branch rejection + override, unknown pin, no-git archive.

## Replay

`/bin/bash tests/setup-bin.test.sh`
`/bin/bash tests/setup-zsh-functions.test.sh`

## Status

completed

