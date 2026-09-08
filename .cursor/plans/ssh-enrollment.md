---
status: completed
issue: https://github.com/qwts/managed-machine/issues/120
---

# Explicit human-authorized SSH enrollment

## Intent

The owner discovered that `setup-gh` automatically provisions SSH keys while
preparing separate agent accounts. SSH key creation, upload, signing
enrollment, and fleet enrollment must be a separate, explicit,
human-authorized action — never a side effect of normal machine setup — and
agent accounts must never receive SSH keys.

## Target

- Default `install.sh`, `managed-machine --bootstrap`, `managed-machine
  --update`, `setup-gh`, and `account setup` perform zero SSH-enrollment
  operations: no `ssh-keygen`, no `ssh-add`/Keychain import, no `gh ssh-key
  add`, no `admin:public_key`/`admin:ssh_signing_key` scope requests, no
  `git_protocol=ssh` switch, no SSH signing configuration, no fleet
  registration. They keep gh installation, HTTPS credential helper, git
  identity, and a pull-only refresh of the private config checkout
  (`refresh_managed_machine_config_repo` — fetch + rebase when clean, never
  commits or pushes).
- `managed-machine ssh enroll --authentication|--signing|--fleet` is the only
  enrollment path. At least one purpose flag is required; purposes are never
  silently combined. Authentication and signing both require successful
  private-key loading into ssh-agent (with Keychain integration on macOS);
  loading failure fails that purpose before upload or enabling Git signing
  or the SSH remote protocol. Signing
  resolves the private config checkout independently of fleet enrollment,
  so its allowed-signers refresh also works without an exported override.
- Before any mutation, the command validates the invoking account (never
  root; never an agent context: harness env markers, OS `agents`-group
  membership, or a rostered identity slug; HOME must be owned by and
  registered to that account in the directory), requires an active gh login
  (no login flow is opened), prints the plan (account, home, GitHub login,
  key fingerprint or creation intent, purposes), and runs one OS-native
  osascript authorization whose prompt names the same account, login, and
  purposes. Identity queries, directory-output parsers, and the consent
  executable are pinned to `/usr/bin`; the shared session-marker check runs
  with a system-only PATH. An unavailable registered-home lookup refuses
  enrollment. Tests substitute these binaries only in a disposable fixture
  copy; production has no command-path override for the authorization gate.
- The elevated side of the authorization is `/usr/bin/true` — a consent gate
  only. All mutations run unprivileged as the invoking account, so an
  administrator approving for a standard human user cannot land keys under
  the admin home.
- Cancellation, unavailable GUI authorization, or noninteractive execution
  leave all state unchanged and report that no enrollment occurred. There is
  no `--yes`, no environment marker, and no cached installation record that
  substitutes for consent.
- Existing SSH state is always preserved: keys, GitHub registrations, custom
  `~/.ssh/config`, and signing configuration are never deleted, revoked,
  rotated, or disabled by the default path. Explicit `--signing` writes the
  managed signing config but never strips an existing one silently.
- Diagnostics distinguish "not enrolled" from failure: `managed-machine
  status` prints an `ssh` line (`not enrolled (explicit opt-in: ...)` /
  `enrolled: key signing fleet`), and `managed-machine ssh status` gives a
  read-only per-account report including an agent-account policy note.

## Owner decisions on record (#120)

- Authorization mechanism: `osascript` `with administrator privileges`.
  macOS Authorization Services may satisfy it via Touch ID or a recently
  cached grant (~5 min), and it authorizes "an administrator" rather than a
  verified named human — the owner accepted this: it is an OS-native human
  confirmation, not a guaranteed fresh-password challenge. No script-level or
  LocalAuthentication API offers a password-only challenge; a `sudo -kv`
  terminal check was rejected because it would exclude standard (non-admin)
  human accounts.
- Purpose selection follows the app's flag convention (`fleet remove --yes`,
  `add-agent --with-harness`).

## Acceptance

- `tests/ssh-enroll.test.sh` covers: zero SSH operations in stubbed
  `setup-gh`; HTTPS path intact; argument contract; agent/session/roster/
  root/HOME-mismatch refusal before any mutation or dialog; missing gh auth;
  cancellation; noninteractive skip; plan + prompt binding; per-purpose and
  combined enrollment; scope-refresh ordering; retry with no duplicate
  upload; listing failure never uploading blind; `ssh status` read-only; no
  secret material in output; config refresh fast-forward/dirty-skip/never-
  push; production identity rejection with spoofed PATH commands; pinned
  consent rejection despite a successful PATH stub; directory-home lookup
  failure refusing before mutation or dialog.
- `tests/cli.test.sh` covers `ssh` dispatch and help text;
  `tests/status.test.sh` covers the `ssh` status line.
- Full suite passes from a neutral directory:

  ```bash
  root="$(pwd)"
  ( cd /tmp && for t in "$root"/tests/*.test.sh; do bash "$t" || exit "$?"; done )
  ```

## Replay

```bash
root="$(pwd)"
( cd /tmp && for t in "$root"/tests/*.test.sh; do bash "$t" || exit "$?"; done )
```

Operator flow: `managed-machine setup gh` (gh + HTTPS + identity), then
`managed-machine ssh enroll --authentication [--signing] [--fleet]` from a
human Terminal in the owner's account.
