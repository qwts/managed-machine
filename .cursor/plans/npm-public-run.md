---
name: npm-public-run
status: active
overview: npm catalog installs run elevated as the npm global-prefix owner through a root helper when the prefix is not owned by the invoking user, so a Homebrew-managed node (prefix under /opt/homebrew, owned by admin) does not fail global installs with EACCES.
related_prs: []
---

# Elevated npm global installs

## Intent

`npm` catalog rows install with `npm install --global`. When node came from
Homebrew, the npm global prefix is the Homebrew prefix itself (usually
`/opt/homebrew`), owned by `admin`; a non-admin user's `npm i -g` then fails
with EACCES on `/opt/homebrew/lib/node_modules`. The brew-formula, cask, and
official-cli sites already escalate to the prefix owner through the
Authorization Services dialog; the npm site was the only kind that ran its
install entirely unprivileged.

## Target

- New `lib/npm-public-run` root helper (the npm analogue of
  `lib/brew-github-auth-run`): given `<owner> <npm> [args...]`, drops to the
  prefix owner via `sudo -u` under a single osascript administrator dialog,
  with an owner-owned workdir under `/tmp` (never the invoking user's
  TMPDIR) and empty user/global npm config so the isolation `npm_public`
  gives lookup and receipt queries is preserved on the elevated install.
- `npm_run` in `lib/apps.sh` mirrors `brew_run`: when `npm_prefix_owner`
  resolves to someone other than the invoking user (and is resolvable), the
  `install --global` step routes through the helper; otherwise it runs
  in-process via `npm_public` exactly as today. Read-only calls (`view`,
  `prefix`, `ls`) stay in-process — they never need the owner.
- Numeric owners (the `(502)` form some `stat` outputs report) are resolved
  to an account name before `sudo -u`.
- A noninteractive bootstrap still skips the dialog with the existing
  skipped-exit, exactly like the other elevated sites.

## Acceptance

- `npm install --global` on a prefix owned by another user escalates once via
  osascript and does not EACCES; the package lands under the prefix.
- An in-process prefix (nvm and friends) installs with no dialog.
- The elevated run keeps empty user/global npm config and a throwaway
  workdir, and still pins `--registry` to the public registry.
- `bash tests/npm.test.sh`, `setup-agent-clis`, `catalog`, `status`, `brew`
  all pass.

## Replay

`/bin/bash tests/npm.test.sh`. On a Homebrew-node machine,
`managed-machine setup commandcode` installs `command-code` as the prefix
owner and reports `cmd` from the global prefix.