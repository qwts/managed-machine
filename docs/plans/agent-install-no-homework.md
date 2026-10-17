# Agent install: no homework

## Intent

Auto-install either finishes a step in the same run or that step is not part of the install. The macOS administrator dialog is accepted manual intervention. Printing `managed-machine setup <name>` after a failed or deferred step is not.

## Target

- New GitHub SSH keys are created with an empty passphrase. No prompt.
- Vendor-installed apps already in `/Applications` are skipped (exit 76), not failed.
- When the administrator dialog cannot be shown, the step is skipped, not failed.
- `setup-gh` and `setup-bin` stay in auto-install. They are not preflighted out.
- Bootstrap records skip reasons only. It does not assign a follow-up command.

## Acceptance

- `tests/ssh-key.test.sh` creates a key with no TTY.
- `tests/setup-ides.test.sh` treats a vendor occupier as skip 76.
- `tests/bootstrap.test.sh` runs `setup-gh` and `setup-bin` in noninteractive mode and never prints `managed-machine setup`.
- `tests/elevate.test.sh` returns nonzero without popping a dialog in noninteractive mode.

## Replay

Run the tests above. Interactive bootstrap may show administrator dialogs; that is the install.

## Status

completed
